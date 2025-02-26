package plugin

import (
	"context"
	"fmt"
	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/plugin/pkg/upstream"
	"github.com/coredns/coredns/request"
	redisCon "github.com/gomodule/redigo/redis"
	"github.com/miekg/dns"
	redis "github.com/polymorpher/coredns-redis"
	"github.com/polymorpher/coredns-redis/record"
	"sort"
	"sync"
	"time"
)

type Result int

const (
	// Success is a successful lookup.
	Success Result = iota
	// NameError indicates a nameerror
	NameError
	// Delegation indicates the lookup resulted in a delegation.
	Delegation
	// NoData indicates the lookup resulted in a NODATA.
	NoData
	// ServerFailure indicates a server failure during the lookup.
	ServerFailure
)

func (r Result) toRcode() int {
	if r == NameError {
		return dns.RcodeNameError
	}
	if r == ServerFailure {
		return dns.RcodeServerFailure
	}
	return dns.RcodeSuccess
}

const name = "redis"

var log = clog.NewWithPlugin("redis")

type Plugin struct {
	Redis *redis.Redis
	Next  plugin.Handler

	loadZoneTicker *time.Ticker
	zones          []string
	lastRefresh    time.Time
	lock           sync.Mutex
	// Upstream for looking up external names during the resolution process.
	Upstream *upstream.Upstream
}

func (p *Plugin) Name() string {
	return name
}

func (p *Plugin) Ready() bool {
	ok, err := p.Redis.Ping()
	if err != nil {
		log.Error(err)
	}
	return ok
}

func (p *Plugin) externalLookup(ctx context.Context, state request.Request, target string, qtype uint16) ([]dns.RR, Result) {
	m, e := p.Upstream.Lookup(ctx, state, target, qtype)
	if e != nil {
		return nil, ServerFailure
	}
	if m == nil {
		return nil, Success
	}
	if m.Rcode == dns.RcodeNameError {
		return m.Answer, NameError
	}
	if m.Rcode == dns.RcodeServerFailure {
		return m.Answer, ServerFailure
	}
	if m.Rcode == dns.RcodeSuccess && len(m.Answer) == 0 {
		return m.Answer, NoData
	}
	return m.Answer, Success
}

func (p *Plugin) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{Req: r, W: w}
	qName := state.Name()
	qType := state.QType()

	if qName == "" || qType == dns.TypeNone {
		return plugin.NextOrFailure(qName, p.Next, ctx, w, r)
	}

	var conn redisCon.Conn
	defer func() {
		if conn == nil {
			return
		}
		_ = conn.Close()
	}()

	var zoneName string
	x := sort.SearchStrings(p.zones, qName)
	if x < len(p.zones) && p.zones[x] == qName {
		zoneName = p.zones[x]
	} else {
		conn = p.Redis.Pool.Get()
		zoneName = plugin.Zones(p.zones).Matches(qName)
	}

	if zoneName == "" {
		log.Debugf("zone not found: %s", qName)
		p.checkCache()
		return plugin.NextOrFailure(qName, p.Next, ctx, w, r)
	} else if conn == nil {
		conn = p.Redis.Pool.Get()
	}

	zone := p.Redis.LoadZoneC(zoneName, false, conn)
	if zone == nil {
		log.Errorf("unable to load zone: %s", zoneName)
		return p.Redis.ErrorResponse(state, zoneName, dns.RcodeServerFailure, nil)
	}

	if qType == dns.TypeAXFR {
		log.Debug("zone transfer request (Handler)")
		return p.handleZoneTransfer(zone, p.zones, w, r, conn)
	}

	location := p.Redis.FindLocation(qName, zone)
	if location == "" {
		log.Debugf("location %s not found for zone: %s", qName, zone)
		p.checkCache()
		return p.Redis.ErrorResponse(state, zoneName, dns.RcodeNameError, nil)
	}

	answers := make([]dns.RR, 0, 0)
	extras := make([]dns.RR, 0, 10)
	zoneRecords := p.Redis.LoadZoneRecordsC(location, zone, conn)
	zoneRecords.MakeFqdn(zone.Name)

	answerCode := dns.RcodeSuccess

	if qType != dns.TypeCNAME && len(zoneRecords.CNAME) > 0 {
		answers, extras = p.Redis.CNAME(qName, zone, zoneRecords)
		targetName := answers[0].(*dns.CNAME).Target
		log.Debugf("Doing external (%s) recursive CNAME lookup for %s in zone %s", targetName, qName, zone)
		rr, result := p.externalLookup(ctx, state, targetName, qType)
		// note that we should still write an answer even if external lookup fails, but we should propagate external lookup errors back to answer as well
		if result != Success {
			log.Debugf("External lookup failed for name %s in zone %s", qName, zone)
		}
		answerCode = result.toRcode()
		answers = append(answers, rr...)
	} else {
		switch qType {
		case dns.TypeSOA:
			answers, extras = p.Redis.SOA(zone, zoneRecords)
		case dns.TypeA:
			answers, extras = p.Redis.A(qName, zone, zoneRecords)
		case dns.TypeAAAA:
			answers, extras = p.Redis.AAAA(qName, zone, zoneRecords)
		case dns.TypeCNAME:
			answers, extras = p.Redis.CNAME(qName, zone, zoneRecords)
		case dns.TypeTXT:
			answers, extras = p.Redis.TXT(qName, zone, zoneRecords)
		case dns.TypeNS:
			answers, extras = p.Redis.NS(qName, zone, zoneRecords, p.zones, conn)
		case dns.TypeMX:
			answers, extras = p.Redis.MX(qName, zone, zoneRecords, p.zones, conn)
		case dns.TypeSRV:
			answers, extras = p.Redis.SRV(qName, zone, zoneRecords, p.zones, conn)
		case dns.TypePTR:
			answers, extras = p.Redis.PTR(qName, zone, zoneRecords, p.zones, conn)
		case dns.TypeCAA:
			answers, extras = p.Redis.CAA(qName, zone, zoneRecords)

		default:
			return p.Redis.ErrorResponse(state, zoneName, dns.RcodeNotImplemented, nil)
		}
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative, m.RecursionAvailable, m.Compress = true, false, true
	m.Answer = append(m.Answer, answers...)
	m.Extra = append(m.Extra, extras...)
	state.SizeAndDo(m)
	m = state.Scrub(m)
	_ = w.WriteMsg(m)
	return answerCode, nil
}

func (p *Plugin) handleZoneTransfer(zone *record.Zone, zones []string, w dns.ResponseWriter, r *dns.Msg, conn redisCon.Conn) (int, error) {
	//todo: check and test zone transfer, implement ip-range check
	records := p.Redis.AXFR(zone, zones, conn)
	ch := make(chan *dns.Envelope)
	tr := new(dns.Transfer)
	tr.TsigSecret = nil
	go func(ch chan *dns.Envelope) {
		j, l := 0, 0

		for i, r := range records {
			l += dns.Len(r)
			if l > redis.MaxTransferLength {
				ch <- &dns.Envelope{RR: records[j:i]}
				l = 0
				j = i
			}
		}
		if j < len(records) {
			ch <- &dns.Envelope{RR: records[j:]}
		}
		close(ch)
	}(ch)

	err := tr.Out(w, r, ch)
	if err != nil {
		fmt.Println(err)
	}
	w.Hijack()
	return dns.RcodeSuccess, nil
}

func (p *Plugin) startZoneNameCache() {

	if err := p.loadCache(); err != nil {
		log.Fatal("unable to load zones to cache", err)
	} else {
		log.Info("zone name cache loaded")
	}
	go func() {
		for {
			select {
			case <-p.loadZoneTicker.C:
				if err := p.loadCache(); err != nil {
					log.Error("unable to load zones to cache", err)
					return
				} else {
					log.Infof("zone name cache refreshed (%v)", time.Now())
				}
			}
		}
	}()
}

func (p *Plugin) loadCache() error {
	z, err := p.Redis.LoadAllZoneNames()
	if err != nil {
		return err
	}
	sort.Strings(z)
	p.lock.Lock()
	p.zones = z
	p.lastRefresh = time.Now()
	p.lock.Unlock()
	return nil
}

// TODO: we should use a heap for p.zones so we don't keep duplicating the slice each time this function is called
func (p *Plugin) loadCacheForZone(fqdn string) (bool, error) {
	exists, err := p.Redis.CheckZoneName(fqdn)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("zone does not exist: %s", fqdn)
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	pos := sort.SearchStrings(p.zones, fqdn)
	if p.zones[pos] == fqdn {
		return false, nil
	}
	p.zones = append(p.zones, "")
	copy(p.zones[pos+1:], p.zones[pos:])
	p.zones[pos] = fqdn
	return true, nil
}

func (p *Plugin) checkCache() {
	if time.Now().Sub(p.lastRefresh).Seconds() > float64(p.Redis.DefaultTtl*2) {
		p.startZoneNameCache()
	}
}

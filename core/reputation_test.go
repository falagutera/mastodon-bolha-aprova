package core

import (
	"context"
	"crypto/md5" // #nosec G501 -- the hash StopForumSpam takes
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeReputation answers like StopForumSpam and AbuseIPDB, from what each
// returned when asked by hand.
func fakeReputation(t *testing.T) (*lookups, *atomic.Int64, *atomic.Value) {
	var calls atomic.Int64
	var lastQuery atomic.Value
	sum := md5.Sum([]byte("spammer@example.org")) // #nosec G401 -- test of the API's hash
	spamHash := hex.EncodeToString(sum[:])
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastQuery.Store(r.URL.RawQuery)
		q := r.URL.Query()
		if !q.Has("json") || !q.Has("unix") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if q.Get("ip") == "198.51.100.66" {
			fmt.Fprint(w, `{"success":0,"error":"rate limited"}`)
			return
		}
		var parts []string
		switch ip := q.Get("ip"); ip {
		case "":
		case "185.220.101.1":
			parts = append(parts, `"ip":{"value":"185.220.101.1","appears":1,"frequency":84,"lastseen":1795210613,"torexit":1,"asn":60729,"country":"de","confidence":94.92}`)
		default:
			parts = append(parts, `"ip":{"value":"`+ip+`","frequency":0,"appears":0,"asn":61890,"country":"br"}`)
		}
		switch h := q.Get("emailhash"); h {
		case "":
		case spamHash:
			parts = append(parts, `"emailhash":{"value":"`+h+`","appears":1,"frequency":3,"lastseen":1793447221,"confidence":1.51}`)
		default:
			parts = append(parts, `"emailhash":{"value":"`+h+`","frequency":0,"appears":0}`)
		}
		fmt.Fprint(w, `{"success":1,`+strings.Join(parts, ",")+`}`)
	})
	mux.HandleFunc("GET /api/v2/check", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Key") != "a" || r.URL.Query().Get("maxAgeInDays") != "90" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch ip := r.URL.Query().Get("ipAddress"); ip {
		case "185.220.101.1":
			fmt.Fprint(w, `{"data":{"ipAddress":"185.220.101.1","isPublic":true,"abuseConfidenceScore":100,"countryCode":"DE","usageType":"Data Center/Web Hosting/Transit","isp":"x","isTor":true,"totalReports":512,"numDistinctUsers":77,"lastReportedAt":"2026-11-21T09:12:00+00:00"}}`)
		case "198.51.100.66":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			fmt.Fprint(w, `{"data":{"ipAddress":"`+ip+`","abuseConfidenceScore":0,"usageType":"Fixed Line ISP","isTor":false,"totalReports":0,"numDistinctUsers":0,"lastReportedAt":null}}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg := LookupsConfig{Reputation: []string{providerSFS, providerAbuse}}
	if err := cfg.validate(func(k string) string { return map[string]string{"APROVA_ABUSEIPDB_KEY": "a"}[k] }); err != nil {
		t.Fatal(err)
	}
	l := newLookups(cfg, srv.Client(), "test")
	l.sfsURL, l.abuseURL = srv.URL, srv.URL
	return l, &calls, &lastQuery
}

func TestReputationReported(t *testing.T) {
	l, calls, lastQuery := fakeReputation(t)
	got := l.reputation(context.Background(), "185.220.101.1", "Spammer@Example.org")
	if len(got) != 2 || got[0].Provider != providerSFS || got[1].Provider != providerAbuse {
		t.Fatalf("%+v", got)
	}
	s, a := got[0], got[1]
	if !s.Listed() || s.IP.Reports != 84 || s.IP.Score != 95 || s.IP.Last != "2026-11-20" || !s.Tor || s.Email == nil || !s.Email.Listed || s.Email.Score != 2 {
		t.Errorf("sfs: %+v ip=%+v email=%+v", s, s.IP, s.Email)
	}
	if !a.Listed() || a.IP.Reports != 512 || a.IP.Reporters != 77 || a.IP.Score != 100 || a.IP.Last != "2026-11-21" || !a.Tor || a.Usage != "Data Center/Web Hosting/Transit" || a.Email != nil {
		t.Errorf("abuseipdb: %+v ip=%+v", a, a.IP)
	}
	if rows := s.Rows(); len(rows) != 2 || rows[0].Kind != "ip" || rows[1].Kind != "email" {
		t.Errorf("rows: %+v", rows)
	}
	if q, _ := lastQuery.Load().(string); strings.Contains(q, "spammer") || strings.Contains(q, "%40") {
		t.Errorf("the e-mail went out in the clear: %s", q)
	}
	n := calls.Load()
	l.reputation(context.Background(), "185.220.101.1", "spammer@example.org")
	if calls.Load() != n {
		t.Error("the second lookup should come from the cache")
	}
}

func TestReputationClean(t *testing.T) {
	l, _, _ := fakeReputation(t)
	got := l.reputation(context.Background(), "138.204.212.120", "someone@example.org")
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	for _, r := range got {
		if r.Down || r.Listed() || r.Tor || r.IP == nil || r.IP.Last != "" {
			t.Errorf("not reported is an answer: %+v %+v", r, r.IP)
		}
	}
	if got[1].Usage != "Fixed Line ISP" {
		t.Errorf("usage: %q", got[1].Usage)
	}
}

func TestReputationWithoutPublicIP(t *testing.T) {
	l, _, _ := fakeReputation(t)
	// A private IP (an edge proxy) is never sent; StopForumSpam still checks
	// the e-mail, AbuseIPDB has nothing to check.
	got := l.reputation(context.Background(), "10.200.4.30", "someone@example.org")
	if len(got) != 1 || got[0].Provider != providerSFS || got[0].IP != nil || got[0].Email == nil {
		t.Fatalf("%+v", got)
	}
	if l.reputation(context.Background(), "", "not-an-email") != nil {
		t.Error("nothing to ask")
	}
}

func TestReputationDown(t *testing.T) {
	l, _, _ := fakeReputation(t)
	got := l.reputation(context.Background(), "198.51.100.66", "")
	if len(got) != 2 || !got[0].Down || !got[1].Down {
		t.Errorf("%+v", got)
	}
}

func TestReputationConfig(t *testing.T) {
	noKey := func(string) string { return "" }
	c := LookupsConfig{Reputation: []string{providerSFS, providerAbuse}}
	if err := c.validate(noKey); err != nil || len(c.Reputation) != 1 || !c.abuseWanted {
		t.Errorf("abuseipdb without a key is left out, not fatal: %v %v", err, c.Reputation)
	}
	l := newLookups(c, http.DefaultClient, "test")
	l.sfsURL = "http://127.0.0.1:1" // nothing listens
	got := l.reputation(context.Background(), "203.0.113.9", "")
	if len(got) != 2 || !got[0].Down || got[1].Provider != providerAbuse || !got[1].Off {
		t.Errorf("%+v", got)
	}
	for _, bad := range []LookupsConfig{{Reputation: []string{"cleantalk"}}, {Reputation: []string{providerSFS, providerSFS}}} {
		if err := bad.validate(noKey); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

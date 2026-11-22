package core

import (
	"context"
	"crypto/md5" // #nosec G501 -- the hash StopForumSpam takes
	"crypto/sha256"
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
	ctSum := sha256.Sum256([]byte("spammer@example.org"))
	ctSpam := "email_" + hex.EncodeToString(ctSum[:])
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if q.Get("method_name") != "spam_check" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if q.Get("auth_key") != "c" {
			w.WriteHeader(http.StatusNotImplemented)
			fmt.Fprint(w, `{"error_message":"Unknown access key","error_no":6}`)
			return
		}
		if strings.Contains(q.Get("email"), "@") {
			t.Error("the e-mail went to CleanTalk in the clear")
		}
		var parts []string
		switch ip := q.Get("ip"); ip {
		case "":
		case "185.220.101.1":
			parts = append(parts, `"185.220.101.1":{"spam_rate":1,"updated":"2026-11-22 15:30:13","frequency":6732,"appears":1,"network_type":"tor","country":"DE"}`)
		case "203.0.113.50":
			parts = append(parts, `"203.0.113.50":{"spam_rate":0.4,"updated":"2026-11-18 10:06:23","frequency":4,"appears":1,"network_type":"hosting"}`)
		case "198.51.100.66":
			fmt.Fprint(w, `{"data":"In progress"}`)
			return
		default:
			parts = append(parts, `"`+ip+`":{"appears":0,"frequency":0,"network_type":"unknown"}`)
		}
		switch e := q.Get("email"); e {
		case "":
		case ctSpam:
			parts = append(parts, `"`+e+`":{"frequency":747,"updated":"2026-11-22 15:29:40","spam_rate":1,"appears":1,"disposable_email":1}`)
		default:
			parts = append(parts, `"`+e+`":{"appears":0,"frequency":0,"disposable_email":0}`)
		}
		fmt.Fprint(w, `{"data":{`+strings.Join(parts, ",")+`}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg := LookupsConfig{Reputation: []string{providerSFS, providerAbuse, providerCT}}
	if err := cfg.validate(func(k string) string {
		return map[string]string{"APROVA_ABUSEIPDB_KEY": "a", "APROVA_CLEANTALK_KEY": "c"}[k]
	}); err != nil {
		t.Fatal(err)
	}
	l := newLookups(cfg, srv.Client(), "test")
	l.sfsURL, l.abuseURL, l.cleantalkURL = srv.URL, srv.URL, srv.URL
	return l, &calls, &lastQuery
}

func TestReputationReported(t *testing.T) {
	l, calls, lastQuery := fakeReputation(t)
	got := l.reputation(context.Background(), "185.220.101.1", "Spammer@Example.org")
	if len(got) != 3 || got[0].Provider != providerSFS || got[1].Provider != providerAbuse || got[2].Provider != providerCT {
		t.Fatalf("%+v", got)
	}
	s, a, c := got[0], got[1], got[2]
	if !c.Listed() || c.IP.Reports != 6732 || c.IP.Score != 100 || c.IP.Last != "2026-11-22" || !c.Tor || c.Network != "" ||
		c.Email == nil || !c.Email.Listed || c.Email.Reports != 747 || !c.Email.Disposable {
		t.Errorf("cleantalk: %+v ip=%+v email=%+v", c, c.IP, c.Email)
	}
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
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	for _, r := range got {
		if r.Down || r.Listed() || r.Tor || r.IP == nil || r.IP.Last != "" {
			t.Errorf("not reported is an answer: %+v %+v", r, r.IP)
		}
	}
	if got[2].Email == nil || got[2].Email.Disposable {
		t.Errorf("cleantalk e-mail: %+v", got[2].Email)
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
	if len(got) != 2 || got[0].Provider != providerSFS || got[0].IP != nil || got[0].Email == nil || got[1].Provider != providerCT || got[1].IP != nil || got[1].Email == nil {
		t.Fatalf("%+v", got)
	}
	if l.reputation(context.Background(), "", "not-an-email") != nil {
		t.Error("nothing to ask")
	}
}

func TestReputationDown(t *testing.T) {
	l, _, _ := fakeReputation(t)
	got := l.reputation(context.Background(), "198.51.100.66", "")
	if len(got) != 3 || !got[0].Down || !got[1].Down || !got[2].Down {
		t.Errorf("%+v", got)
	}
}

func TestReputationConfig(t *testing.T) {
	noKey := func(string) string { return "" }
	c := LookupsConfig{Reputation: []string{providerSFS, providerAbuse, providerCT}}
	if err := c.validate(noKey); err != nil || len(c.Reputation) != 1 || len(c.repOff) != 2 {
		t.Errorf("abuseipdb without a key is left out, not fatal: %v %v", err, c.Reputation)
	}
	l := newLookups(c, http.DefaultClient, "test")
	l.sfsURL = "http://127.0.0.1:1" // nothing listens
	got := l.reputation(context.Background(), "203.0.113.9", "")
	if len(got) != 3 || !got[0].Down || got[1].Provider != providerAbuse || !got[1].Off || got[2].Provider != providerCT || !got[2].Off {
		t.Errorf("%+v", got)
	}
	for _, bad := range []LookupsConfig{{Reputation: []string{"spamhaus"}}, {Reputation: []string{providerSFS, providerSFS}}} {
		if err := bad.validate(noKey); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// A paid provider that turns the key down (unpaid plan, expired or wrong key)
// shows as unavailable, like an outage, and the page still renders.
func TestReputationRefusedKey(t *testing.T) {
	l, _, _ := fakeReputation(t)
	l.cfg.repKeys = map[string]string{providerAbuse: "expired", providerCT: "expired"}
	got := l.reputation(context.Background(), "203.0.113.50", "someone@example.org")
	if len(got) != 3 || got[0].Down || !got[1].Down || !got[2].Down {
		t.Fatalf("%+v", got)
	}
	l.mu.Lock()
	_, a := l.refusedAt[providerAbuse]
	_, c := l.refusedAt[providerCT]
	l.mu.Unlock()
	if !a || !c {
		t.Errorf("a refused key is logged for the operator: %v %v", a, c)
	}
	if _, err := l.cleantalk(context.Background(), "203.0.113.50", ""); err != errRefused {
		t.Errorf("cleantalk 501 + error_no: %v", err)
	}
	if _, err := l.abuseipdb(context.Background(), "203.0.113.50"); err != errRefused {
		t.Errorf("abuseipdb 401: %v", err)
	}
}

func TestCleanTalkNetworkType(t *testing.T) {
	l, _, _ := fakeReputation(t)
	r, err := l.cleantalk(context.Background(), "203.0.113.50", "")
	if err != nil || r.Network != "hosting" || r.Tor || r.IP == nil || r.IP.Score != 40 || r.Email != nil {
		t.Errorf("%v %+v %+v", err, r, r.IP)
	}
}

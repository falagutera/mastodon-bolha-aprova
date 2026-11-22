package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeProviders answers like ip.gutocarvalho.net, Have I Been Pwned and
// XposedOrNot, from what each returned when asked by hand.
func fakeProviders(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/{ip}", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.PathValue("ip") {
		case "203.0.113.9":
			fmt.Fprint(w, `{"ip":"203.0.113.9","hostname":"h.example","city":"Campo Grande","region":"MS","country":"BR","timezone":"America/Campo_Grande",
				"asn":{"asn":"AS28573","name":"Claro S.A.","route":"203.0.113.0/24"}}`)
		default:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	mux.HandleFunc("GET /api/v3/breachedaccount/{email}", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("hibp-api-key") != "k" || !strings.HasPrefix(r.Header.Get("User-Agent"), "aprova/") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.PathValue("email") {
		case "old@example.org":
			fmt.Fprint(w, `[{"Name":"Canva","Title":"Canva","BreachDate":"2019-05-24"},{"Name":"Adobe","Title":"Adobe","BreachDate":"2013-10-04"}]`)
		case "slow@example.org":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("GET /v1/breach-analytics", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Query().Get("email") {
		case "old@example.org":
			fmt.Fprint(w, `{"ExposedBreaches":{"breaches_details":[{"breach":"Neogen","xposed_date":"2026"},{"breach":"LinkedIn","xposed_date":"2012"}]}}`)
		default:
			fmt.Fprint(w, `{"BreachMetrics":null,"ExposedBreaches":null}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testLookups(t *testing.T, breaches ...string) (*lookups, *atomic.Int64) {
	srv, calls := fakeProviders(t)
	cfg := LookupsConfig{IPInfo: srv.URL, Breaches: breaches}
	if err := cfg.validate(func(k string) string { return map[string]string{"APROVA_HIBP_KEY": "k"}[k] }); err != nil {
		t.Fatal(err)
	}
	l := newLookups(cfg, srv.Client(), "test")
	l.hibpURL, l.xonURL = srv.URL, srv.URL
	return l, calls
}

func TestOriginLooksTheIPUp(t *testing.T) {
	l, calls := testLookups(t)
	o := l.origin(context.Background(), "203.0.113.9")
	if o == nil || o.Place != "Campo Grande, MS, BR" || o.ASN != "AS28573" || o.Network != "Claro S.A." || o.Hostname != "h.example" || o.InfoDown {
		t.Fatalf("origin: %+v", o)
	}
	if !strings.HasSuffix(o.InfoURL, "/?ip=203.0.113.9") {
		t.Errorf("info url: %s", o.InfoURL)
	}
	l.origin(context.Background(), "203.0.113.9")
	if calls.Load() != 1 {
		t.Errorf("the second lookup should come from the cache: %d calls", calls.Load())
	}
	if o := l.origin(context.Background(), "198.51.100.1"); o == nil || !o.InfoDown || o.IP != "198.51.100.1" {
		t.Errorf("a failed lookup still shows the IP: %+v", o)
	}
	for _, bad := range []string{"", "10.0.0.1", "127.0.0.1", "::1", "fe80::1", "not-an-ip", "203.0.113.9/../x"} {
		if o := l.origin(context.Background(), bad); o != nil {
			t.Errorf("%q: %+v", bad, o)
		}
	}
}

func TestBreachHistory(t *testing.T) {
	l, calls := testLookups(t, providerHIBP, providerXON)
	got := l.breaches(context.Background(), "Old@Example.org")
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	h, x := got[0], got[1]
	if h.Provider != providerHIBP || h.Count != 2 || h.Oldest != "2013" || h.Newest != "2019" || h.Names[0] != "Adobe" {
		t.Errorf("hibp: %+v", h)
	}
	if x.Provider != providerXON || x.Count != 2 || x.Oldest != "2012" || x.Newest != "2026" || x.Names[0] != "LinkedIn" {
		t.Errorf("xon: %+v", x)
	}
	n := calls.Load()
	l.breaches(context.Background(), "old@example.org")
	if calls.Load() != n {
		t.Error("the second lookup should come from the cache")
	}
	for _, b := range l.breaches(context.Background(), "new@example.org") {
		if b.Down || b.Count != 0 {
			t.Errorf("no breach is an answer, not an outage: %+v", b)
		}
	}
	got = l.breaches(context.Background(), "slow@example.org")
	if !got[0].Down || got[1].Down {
		t.Errorf("a refused provider is down, the other answers: %+v", got)
	}
	if l.breaches(context.Background(), "no-at-sign") != nil {
		t.Error("not an e-mail")
	}
}

func TestLookupErrorsCarryNoEmail(t *testing.T) {
	l := newLookups(LookupsConfig{}, &http.Client{}, "test")
	l.cfg.hibpKey = "k"
	l.hibpURL = "http://127.0.0.1:1" // nothing listens
	_, err := l.hibp(context.Background(), "secret-person@example.org")
	if err == nil || strings.Contains(err.Error(), "secret-person") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error: %v", err)
	}
}

func TestLookupsConfig(t *testing.T) {
	noKey := func(string) string { return "" }
	c := LookupsConfig{Breaches: []string{providerHIBP, providerXON}}
	if err := c.validate(noKey); err != nil || len(c.Breaches) != 1 || c.Breaches[0] != providerXON || !c.hibpWanted {
		t.Errorf("hibp without a key is left out, not fatal: %v %v", err, c.Breaches)
	}
	for _, bad := range []LookupsConfig{
		{IPInfo: "http://ip.example"},
		{IPInfo: "https://ip.example/path"},
		{Breaches: []string{"other"}},
		{Breaches: []string{providerXON, providerXON}},
	} {
		if err := bad.validate(noKey); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	c = LookupsConfig{IPInfo: "https://ip.example/"}
	if err := c.validate(noKey); err != nil || c.IPInfo != "https://ip.example" {
		t.Errorf("%v %q", err, c.IPInfo)
	}
}

// A provider the config lists without its key still shows, as off.
func TestBreachProviderWithoutKeyShowsOff(t *testing.T) {
	srv, _ := fakeProviders(t)
	cfg := LookupsConfig{Breaches: []string{providerHIBP, providerXON}}
	if err := cfg.validate(func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	l := newLookups(cfg, srv.Client(), "test")
	l.xonURL = srv.URL
	got := l.breaches(context.Background(), "old@example.org")
	if len(got) != 2 || got[0].Provider != providerXON || got[0].Count != 2 || got[1].Provider != providerHIBP || !got[1].Off {
		t.Errorf("%+v", got)
	}
}

package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Lookups add context to the page of one pending request: where the sign-up
// came from (an ip-info service) and whether the e-mail shows up in public
// breaches. They run only on that page, never on the queue, digests or
// actions; results live in memory for a day and are never stored.
//
// A breach history is a good sign: an address in old breaches is old and real.
// Fresh throwaway addresses used by bots have none. So it is shown as context,
// never as a check that blocks the quorum.

const (
	lookupTimeout   = 3 * time.Second
	lookupTTL       = 24 * time.Hour
	lookupFailedTTL = 10 * time.Minute
	lookupMaxCache  = 5000
	lookupBodyLimit = 2 << 20
	breachNamesMax  = 12
)

// Breach providers.
const (
	providerHIBP = "hibp"
	providerXON  = "xon"
)

var breachProviders = []string{providerHIBP, providerXON}

// LookupsConfig turns lookups on. Each is off unless set.
type LookupsConfig struct {
	// IPInfo is the base URL of an ip-info service answering GET /api/v1/<ip>
	// (open/gcn/tools/iptools, e.g. https://ip.gutocarvalho.net).
	IPInfo string `yaml:"ip_info"`
	// Breaches lists the e-mail breach providers to ask: hibp (Have I Been
	// Pwned, needs APROVA_HIBP_KEY) and xon (XposedOrNot, no key). They receive
	// the applicant's e-mail: say so in the instance's privacy policy.
	Breaches   []string `yaml:"breaches"`
	hibpKey    string
	hibpWanted bool
}

type ipInfo struct {
	Hostname string `json:"hostname"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Country  string `json:"country"`
	Timezone string `json:"timezone"`
	ASN      *struct {
		ASN   string `json:"asn"`
		Name  string `json:"name"`
		Route string `json:"route"`
	} `json:"asn"`
}

// originView is the "where it came from" card.
type originView struct {
	IP       string `json:"ip"`
	Hostname string `json:"hostname,omitempty"`
	Place    string `json:"place,omitempty"` // city, region, country
	Country  string `json:"country,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	ASN      string `json:"asn,omitempty"`
	Network  string `json:"network,omitempty"`
	Route    string `json:"route,omitempty"`
	InfoURL  string `json:"info_url,omitempty"` // the ip-info page for the IP
	InfoDown bool   `json:"info_down,omitempty"`
	// Other accounts at the IP, counts only (rule 1: not who they are).
	SameIP     map[string]int `json:"same_ip,omitempty"`
	SameIPMore bool           `json:"same_ip_more,omitempty"`
	SameIPDown bool           `json:"same_ip_down,omitempty"`
	SameIPURL  string         `json:"same_ip_url,omitempty"`
	SameIPSeen bool           `json:"same_ip_seen"` // the app answered
}

// SameIPTotal is the number of other accounts at the IP.
func (o *originView) SameIPTotal() int {
	n := 0
	for _, c := range o.SameIP {
		n += c
	}
	return n
}

// SameIPList is the counts in a fixed order, for the page.
func (o *originView) SameIPList() []statusCount {
	var out []statusCount
	for _, st := range []string{"suspended", "disabled", "silenced", "pending", "active"} {
		if n := o.SameIP[st]; n > 0 {
			out = append(out, statusCount{Status: st, N: n})
		}
	}
	return out
}

type statusCount struct {
	Status string
	N      int
}

// breachView is one provider's answer about the e-mail.
type breachView struct {
	Provider string   `json:"provider"`
	Down     bool     `json:"down,omitempty"` // no answer (limit, outage)
	Count    int      `json:"count"`
	Names    []string `json:"names,omitempty"` // the oldest first, at most breachNamesMax
	Oldest   string   `json:"oldest,omitempty"`
	Newest   string   `json:"newest,omitempty"`
}

// More is how many names were left out.
func (b breachView) More() int { return b.Count - len(b.Names) }

type cached struct {
	v  any
	at time.Time
	ok bool
}

type lookups struct {
	cfg    LookupsConfig
	client *http.Client
	ua     string
	// Base URLs, swapped in tests.
	hibpURL, xonURL string

	mu    sync.Mutex
	cache map[string]cached
}

func newLookups(cfg LookupsConfig, client *http.Client, version string) *lookups {
	return &lookups{cfg: cfg, client: client, ua: "aprova/" + version + " (+https://gitlab.gutocarvalho.net/open/bolha/aprova)",
		hibpURL: "https://haveibeenpwned.com", xonURL: "https://api.xposedornot.com", cache: map[string]cached{}}
}

func (l *lookups) enabled() bool { return l.cfg.IPInfo != "" || len(l.cfg.Breaches) > 0 }

func (l *lookups) get(key string) (any, bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, found := l.cache[key]
	if !found {
		return nil, false, false
	}
	ttl := lookupTTL
	if !c.ok {
		ttl = lookupFailedTTL
	}
	if time.Since(c.at) > ttl {
		delete(l.cache, key)
		return nil, false, false
	}
	return c.v, c.ok, true
}

func (l *lookups) put(key string, v any, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.cache) >= lookupMaxCache {
		// Drop the oldest entries: a day's queue is far smaller than the cap.
		keys := make([]string, 0, len(l.cache))
		for k := range l.cache {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return l.cache[keys[i]].at.Before(l.cache[keys[j]].at) })
		for _, k := range keys[:len(keys)/2] {
			delete(l.cache, k)
		}
	}
	l.cache[key] = cached{v: v, at: time.Now(), ok: ok}
}

// errLookup is all a caller ever sees of a failed lookup: url.Error carries the
// URL, and with it the applicant's e-mail.
var errLookup = errors.New("lookup failed")

// fetch GETs u and decodes JSON into out. found is false on 404; HIBP answers
// 404 for "no breaches".
func (l *lookups) fetch(ctx context.Context, u string, header http.Header, out any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil) // #nosec G704 -- host from the config (https, validated) or a fixed provider; IP and e-mail are escaped into path/query
	if err != nil {
		return false, errLookup
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", l.ua)
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req) // #nosec G704 -- same request as above
	if err != nil {
		return false, errLookup
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode != http.StatusOK:
		return false, errLookup
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, lookupBodyLimit)).Decode(out); err != nil {
		return false, errLookup
	}
	return true, nil
}

// origin looks the IP up. It returns nil when ip is not a public address.
func (l *lookups) origin(ctx context.Context, ip string) *originView {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.IsGlobalUnicast() || a.IsPrivate() {
		return nil
	}
	ip = a.Unmap().String()
	o := &originView{IP: ip}
	if l.cfg.IPInfo == "" {
		return o
	}
	o.InfoURL = l.cfg.IPInfo + "/?ip=" + url.QueryEscape(ip)
	key := "ip " + ip
	v, ok, hit := l.get(key)
	if !hit {
		var info ipInfo
		found, err := l.fetch(ctx, l.cfg.IPInfo+"/api/v1/"+url.PathEscape(ip), nil, &info)
		ok = err == nil && found
		v = info
		l.put(key, v, ok)
	}
	if !ok {
		o.InfoDown = true
		return o
	}
	info := v.(ipInfo)
	o.Hostname, o.Timezone, o.Country = clip(info.Hostname), clip(info.Timezone), clip(info.Country)
	var place []string
	for _, p := range []string{info.City, info.Region, info.Country} {
		if p = clip(p); p != "" {
			place = append(place, p)
		}
	}
	o.Place = strings.Join(place, ", ")
	if info.ASN != nil {
		o.ASN, o.Network, o.Route = clip(info.ASN.ASN), clip(info.ASN.Name), clip(info.ASN.Route)
	}
	return o
}

// clip keeps text from an outside service short. Templates escape it.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 120 {
		return string(r[:120])
	}
	return s
}

// breaches asks every configured provider about the e-mail.
func (l *lookups) breaches(ctx context.Context, email string) []breachView {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") || len(email) > 254 {
		return nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := make([]breachView, len(l.cfg.Breaches))
	for i, p := range l.cfg.Breaches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := l.breach(ctx, p, email)
			mu.Lock()
			out[i] = b
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func (l *lookups) breach(ctx context.Context, provider, email string) breachView {
	key := provider + " " + email
	if v, ok, hit := l.get(key); hit {
		if !ok {
			return breachView{Provider: provider, Down: true}
		}
		return v.(breachView)
	}
	var b breachView
	var err error
	switch provider {
	case providerHIBP:
		b, err = l.hibp(ctx, email)
	case providerXON:
		b, err = l.xon(ctx, email)
	default:
		err = errLookup
	}
	b.Provider = provider
	if err != nil {
		l.put(key, nil, false)
		return breachView{Provider: provider, Down: true}
	}
	l.put(key, b, true)
	return b
}

type dated struct{ name, date string }

func summarize(list []dated) breachView {
	sort.SliceStable(list, func(i, j int) bool { return list[i].date < list[j].date })
	b := breachView{Count: len(list)}
	for _, d := range list {
		if d.date != "" && b.Oldest == "" {
			b.Oldest = d.date
		}
		if d.date != "" {
			b.Newest = d.date
		}
		if len(b.Names) < breachNamesMax {
			b.Names = append(b.Names, clip(d.name))
		}
	}
	return b
}

// year keeps the year of a date like 2013-10-04 or 2013.
func year(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 4 && strings.Trim(s[:4], "0123456789") == "" {
		return s[:4]
	}
	return ""
}

// hibp asks Have I Been Pwned (API v3). 404 means no breach.
func (l *lookups) hibp(ctx context.Context, email string) (breachView, error) {
	if l.cfg.hibpKey == "" {
		return breachView{}, errLookup
	}
	var items []struct {
		Name       string `json:"Name"`
		Title      string `json:"Title"`
		BreachDate string `json:"BreachDate"`
	}
	h := http.Header{"Hibp-Api-Key": {l.cfg.hibpKey}}
	found, err := l.fetch(ctx, l.hibpURL+"/api/v3/breachedaccount/"+url.PathEscape(email)+"?truncateResponse=false", h, &items)
	if err != nil {
		return breachView{}, err
	}
	if !found {
		return breachView{}, nil
	}
	list := make([]dated, 0, len(items))
	for _, it := range items {
		name := it.Title
		if name == "" {
			name = it.Name
		}
		list = append(list, dated{name: name, date: year(it.BreachDate)})
	}
	return summarize(list), nil
}

// xon asks XposedOrNot's breach analytics. It answers 200 with null
// ExposedBreaches (and sometimes 404) when there is none.
func (l *lookups) xon(ctx context.Context, email string) (breachView, error) {
	var out struct {
		ExposedBreaches *struct {
			Details []struct {
				Breach string `json:"breach"`
				Date   string `json:"xposed_date"`
			} `json:"breaches_details"`
		} `json:"ExposedBreaches"`
	}
	found, err := l.fetch(ctx, l.xonURL+"/v1/breach-analytics?email="+url.QueryEscape(email), nil, &out)
	if err != nil {
		return breachView{}, err
	}
	if !found || out.ExposedBreaches == nil {
		return breachView{}, nil
	}
	list := make([]dated, 0, len(out.ExposedBreaches.Details))
	for _, d := range out.ExposedBreaches.Details {
		list = append(list, dated{name: d.Breach, date: year(d.Date)})
	}
	return summarize(list), nil
}

// validate checks the lookups config; called from Config.validate.
func (c *LookupsConfig) validate(getenv func(string) string) error {
	if c.IPInfo != "" {
		c.IPInfo = strings.TrimRight(c.IPInfo, "/")
		if _, ok := httpsURL(c.IPInfo); !ok {
			return errors.New("config: lookups.ip_info must be an https URL")
		}
	}
	seen := map[string]bool{}
	for _, p := range c.Breaches {
		if !slices.Contains(breachProviders, p) {
			return errors.New("config: lookups.breaches takes " + strings.Join(breachProviders, ", "))
		}
		if seen[p] {
			return errors.New("config: lookups.breaches lists " + p + " twice")
		}
		seen[p] = true
	}
	c.hibpKey = getenv("APROVA_HIBP_KEY")
	// Without a key HIBP is left out instead of refusing to start: the key may
	// come later, and nothing else depends on it.
	c.hibpWanted = seen[providerHIBP]
	if c.hibpWanted && c.hibpKey == "" {
		c.Breaches = slices.DeleteFunc(c.Breaches, func(p string) bool { return p == providerHIBP })
	}
	return nil
}

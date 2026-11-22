package core

import (
	"context"
	"crypto/md5" // #nosec G501 -- StopForumSpam takes the MD5 of the e-mail, so the address itself is never sent
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reputation asks spam and abuse databases whether the sign-up's IP (and, for
// StopForumSpam, the e-mail) was reported. Like the other lookups it runs only
// on the page of a pending request and is shown as context, never as a check
// that blocks the quorum: a residential IP is shared, and a report may be about
// someone else on the same network.

// Reputation providers.
const (
	providerSFS   = "sfs"       // StopForumSpam: IP and e-mail hash, no key
	providerAbuse = "abuseipdb" // AbuseIPDB: IP, needs APROVA_ABUSEIPDB_KEY
)

var reputationProviders = []string{providerSFS, providerAbuse}

// abuseMaxAge is how far back AbuseIPDB reports count, in days.
const abuseMaxAge = 90

// repView is one provider's answer.
type repView struct {
	Provider string  `json:"provider"`
	Off      bool    `json:"off,omitempty"`  // listed in the config, but its key is missing
	Down     bool    `json:"down,omitempty"` // no answer (limit, outage)
	IP       *repHit `json:"ip,omitempty"`
	Email    *repHit `json:"email,omitempty"`
	Usage    string  `json:"usage,omitempty"` // AbuseIPDB's usage type, e.g. Data Center/Web Hosting/Transit
	Tor      bool    `json:"tor,omitempty"`   // a Tor exit node
}

// repHit is what a provider knows about one value.
type repHit struct {
	Listed    bool   `json:"listed"`
	Reports   int    `json:"reports,omitempty"`
	Reporters int    `json:"reporters,omitempty"` // distinct reporters, AbuseIPDB only
	Score     int    `json:"score,omitempty"`     // the provider's confidence, 0-100
	Last      string `json:"last,omitempty"`      // the last report, YYYY-MM-DD
}

// Listed reports whether any value is listed.
func (r repView) Listed() bool {
	return (r.IP != nil && r.IP.Listed) || (r.Email != nil && r.Email.Listed)
}

// repRow is one line of the card: the IP first, then the e-mail.
type repRow struct {
	Kind string // ip or email
	repHit
}

// Rows lists what the provider answered about, for the page.
func (r repView) Rows() []repRow {
	var out []repRow
	if r.IP != nil {
		out = append(out, repRow{Kind: "ip", repHit: *r.IP})
	}
	if r.Email != nil {
		out = append(out, repRow{Kind: "email", repHit: *r.Email})
	}
	return out
}

// publicAddr returns ip in canonical form when it is a public address.
func publicAddr(ip string) (string, bool) {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.IsGlobalUnicast() || a.IsPrivate() || a.Zone() != "" {
		return "", false
	}
	return a.Unmap().String(), true
}

// reputation asks every configured provider. ip is dropped unless public;
// email unless it looks like one. It returns nil when there is nothing to ask.
func (l *lookups) reputation(ctx context.Context, ip, email string) []repView {
	ip, _ = publicAddr(ip)
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(email) > 254 {
		email = ""
	}
	if ip == "" && email == "" {
		return nil
	}
	var out []repView
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range l.cfg.Reputation {
		if ip == "" && p == providerAbuse {
			continue // AbuseIPDB knows only IPs
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := l.rep(ctx, p, ip, email)
			mu.Lock()
			out = append(out, r)
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b repView) int {
		return slices.Index(l.cfg.Reputation, a.Provider) - slices.Index(l.cfg.Reputation, b.Provider)
	})
	if l.cfg.abuseWanted && !slices.Contains(l.cfg.Reputation, providerAbuse) && ip != "" {
		out = append(out, repView{Provider: providerAbuse, Off: true})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (l *lookups) rep(ctx context.Context, provider, ip, email string) repView {
	key := provider + " " + ip + " " + email
	if v, ok, hit := l.get(key); hit {
		if !ok {
			return repView{Provider: provider, Down: true}
		}
		return v.(repView)
	}
	var r repView
	var err error
	switch provider {
	case providerSFS:
		r, err = l.sfs(ctx, ip, email)
	case providerAbuse:
		r, err = l.abuseipdb(ctx, ip)
	default:
		err = errLookup
	}
	r.Provider = provider
	if err != nil {
		l.put(key, nil, false)
		return repView{Provider: provider, Down: true}
	}
	l.put(key, r, true)
	return r
}

type sfsHit struct {
	Appears    int     `json:"appears"`
	Frequency  int     `json:"frequency"`
	LastSeen   int64   `json:"lastseen"`
	Confidence float64 `json:"confidence"`
	TorExit    int     `json:"torexit"`
}

func (h *sfsHit) hit() *repHit {
	if h == nil {
		return nil
	}
	r := &repHit{Listed: h.Appears > 0, Reports: h.Frequency, Score: score(h.Confidence)}
	if h.LastSeen > 0 {
		r.Last = time.Unix(h.LastSeen, 0).UTC().Format(time.DateOnly)
	}
	return r
}

// sfs asks StopForumSpam. The e-mail goes as its MD5 (emailhash).
func (l *lookups) sfs(ctx context.Context, ip, email string) (repView, error) {
	q := url.Values{"json": {""}, "unix": {""}}
	if ip != "" {
		q.Set("ip", ip)
	}
	if email != "" {
		sum := md5.Sum([]byte(email)) // #nosec G401 -- the hash StopForumSpam's API takes, not a security control
		q.Set("emailhash", hex.EncodeToString(sum[:]))
	}
	var out struct {
		Success   int     `json:"success"`
		IP        *sfsHit `json:"ip"`
		EmailHash *sfsHit `json:"emailhash"`
	}
	found, err := l.fetch(ctx, l.sfsURL+"/api?"+q.Encode(), nil, &out)
	if err != nil || !found || out.Success != 1 {
		return repView{}, errLookup
	}
	r := repView{IP: out.IP.hit(), Email: out.EmailHash.hit()}
	if out.IP != nil && out.IP.TorExit == 1 {
		r.Tor = true
	}
	return r, nil
}

// abuseipdb asks AbuseIPDB's check endpoint (API v2).
func (l *lookups) abuseipdb(ctx context.Context, ip string) (repView, error) {
	if l.cfg.abuseKey == "" || ip == "" {
		return repView{}, errLookup
	}
	var out struct {
		Data *struct {
			Score     int    `json:"abuseConfidenceScore"`
			Usage     string `json:"usageType"`
			Tor       bool   `json:"isTor"`
			Reports   int    `json:"totalReports"`
			Reporters int    `json:"numDistinctUsers"`
			Last      string `json:"lastReportedAt"`
		} `json:"data"`
	}
	q := url.Values{"ipAddress": {ip}, "maxAgeInDays": {strconv.Itoa(abuseMaxAge)}}
	found, err := l.fetch(ctx, l.abuseURL+"/api/v2/check?"+q.Encode(), http.Header{"Key": {l.cfg.abuseKey}}, &out)
	if err != nil || !found || out.Data == nil {
		return repView{}, errLookup
	}
	d := out.Data
	h := &repHit{Listed: d.Reports > 0, Reports: d.Reports, Reporters: d.Reporters, Score: min(max(d.Score, 0), 100)}
	if len(d.Last) >= 10 {
		h.Last = d.Last[:10]
	}
	return repView{IP: h, Usage: clip(d.Usage), Tor: d.Tor}, nil
}

func score(f float64) int {
	if math.IsNaN(f) || f < 0 {
		return 0
	}
	return int(math.Min(math.Round(f), 100))
}

// validateReputation checks lookups.reputation; called from LookupsConfig.validate.
func (c *LookupsConfig) validateReputation(getenv func(string) string) error {
	seen := map[string]bool{}
	for _, p := range c.Reputation {
		if !slices.Contains(reputationProviders, p) {
			return errors.New("config: lookups.reputation takes " + strings.Join(reputationProviders, ", "))
		}
		if seen[p] {
			return errors.New("config: lookups.reputation lists " + p + " twice")
		}
		seen[p] = true
	}
	c.abuseKey = getenv("APROVA_ABUSEIPDB_KEY")
	c.abuseWanted = seen[providerAbuse]
	if c.abuseWanted && c.abuseKey == "" {
		c.Reputation = slices.DeleteFunc(c.Reputation, func(p string) bool { return p == providerAbuse })
	}
	return nil
}

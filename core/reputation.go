package core

import (
	"context"
	"crypto/md5" // #nosec G501 -- StopForumSpam takes the MD5 of the e-mail, so the address itself is never sent
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
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
	providerCT    = "cleantalk" // CleanTalk Block Lists: IP and e-mail hash, needs APROVA_CLEANTALK_KEY
)

var reputationProviders = []string{providerSFS, providerAbuse, providerCT}

// reputationKeyEnv names the variable holding each paid provider's key.
var reputationKeyEnv = map[string]string{providerAbuse: "APROVA_ABUSEIPDB_KEY", providerCT: "APROVA_CLEANTALK_KEY"}

// abuseMaxAge is how far back AbuseIPDB reports count, in days.
const abuseMaxAge = 90

// repView is one provider's answer.
type repView struct {
	Provider string  `json:"provider"`
	Off      bool    `json:"off,omitempty"`  // listed in the config, but its key is missing
	Down     bool    `json:"down,omitempty"` // no answer (limit, outage)
	IP       *repHit `json:"ip,omitempty"`
	Email    *repHit `json:"email,omitempty"`
	Usage    string  `json:"usage,omitempty"`   // AbuseIPDB's usage type, e.g. Data Center/Web Hosting/Transit
	Tor      bool    `json:"tor,omitempty"`     // a Tor exit node
	Network  string  `json:"network,omitempty"` // CleanTalk's network type: hosting, paid_vpn or public
}

// repHit is what a provider knows about one value.
type repHit struct {
	Listed    bool   `json:"listed"`
	Reports   int    `json:"reports,omitempty"`
	Reporters int    `json:"reporters,omitempty"` // distinct reporters, AbuseIPDB only
	Score     int    `json:"score,omitempty"`     // the provider's confidence, 0-100
	Last      string `json:"last,omitempty"`      // the last report, YYYY-MM-DD
	// Disposable is CleanTalk's mark for a throwaway e-mail service.
	Disposable bool `json:"disposable,omitempty"`
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
	for _, p := range l.cfg.repOff {
		if ip != "" || p != providerAbuse {
			out = append(out, repView{Provider: p, Off: true})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (l *lookups) rep(ctx context.Context, provider, ip, email string) (r repView) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("lookups: %s: %v", provider, p)
			r = repView{Provider: provider, Down: true}
		}
	}()
	key := provider + " " + ip + " " + email
	if v, ok, hit := l.get(key); hit {
		if !ok {
			return repView{Provider: provider, Down: true}
		}
		return v.(repView)
	}
	var err error
	switch provider {
	case providerSFS:
		r, err = l.sfs(ctx, ip, email)
	case providerAbuse:
		r, err = l.abuseipdb(ctx, ip)
	case providerCT:
		r, err = l.cleantalk(ctx, ip, email)
	default:
		err = errLookup
	}
	r.Provider = provider
	if err != nil {
		l.failed(provider, key, err)
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
	if err != nil {
		return repView{}, err
	}
	if !found || out.Success != 1 {
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
	if l.cfg.repKeys[providerAbuse] == "" || ip == "" {
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
	found, err := l.fetch(ctx, l.abuseURL+"/api/v2/check?"+q.Encode(), http.Header{"Key": {l.cfg.repKeys[providerAbuse]}}, &out)
	if err != nil {
		return repView{}, err
	}
	if !found || out.Data == nil {
		return repView{}, errLookup
	}
	d := out.Data
	h := &repHit{Listed: d.Reports > 0, Reports: d.Reports, Reporters: d.Reporters, Score: min(max(d.Score, 0), 100)}
	if len(d.Last) >= 10 {
		h.Last = d.Last[:10]
	}
	return repView{IP: h, Usage: clip(d.Usage), Tor: d.Tor}, nil
}

// ctRecord is one value in CleanTalk's spam_check answer.
type ctRecord struct {
	Appears    int     `json:"appears"`
	Frequency  int     `json:"frequency"`
	SpamRate   float64 `json:"spam_rate"`
	Updated    string  `json:"updated"`
	Network    string  `json:"network_type"`
	Disposable int     `json:"disposable_email"`
}

func (c *ctRecord) hit() *repHit {
	if c == nil {
		return nil
	}
	h := &repHit{Listed: c.Appears > 0, Reports: c.Frequency, Score: score(c.SpamRate * 100), Disposable: c.Disposable == 1}
	if c.Appears > 0 && len(c.Updated) >= 10 {
		h.Last = c.Updated[:10]
	}
	return h
}

// cleantalk asks CleanTalk's Block Lists (spam_check). The e-mail goes as
// email_<SHA-256>, which CleanTalk answers the same as the address.
func (l *lookups) cleantalk(ctx context.Context, ip, email string) (repView, error) {
	key := l.cfg.repKeys[providerCT]
	if key == "" {
		return repView{}, errLookup
	}
	q := url.Values{"method_name": {"spam_check"}, "auth_key": {key}}
	var emailKey string
	if ip != "" {
		q.Set("ip", ip)
	}
	if email != "" {
		sum := sha256.Sum256([]byte(email))
		emailKey = "email_" + hex.EncodeToString(sum[:])
		q.Set("email", emailKey)
	}
	var out struct {
		Data    map[string]*ctRecord `json:"data"`
		ErrorNo int                  `json:"error_no"`
	}
	found, err := l.fetch(ctx, l.cleantalkURL+"/?"+q.Encode(), nil, &out)
	switch {
	case err != nil:
		return repView{}, err
	case out.ErrorNo != 0:
		return repView{}, errRefused
	case !found || out.Data == nil:
		return repView{}, errLookup
	}
	r := repView{}
	if ip != "" {
		r.IP = out.Data[ip].hit()
		if c := out.Data[ip]; c != nil {
			switch c.Network {
			case "tor":
				r.Tor = true
			case "hosting", "paid_vpn", "public":
				r.Network = c.Network
			}
		}
	}
	if emailKey != "" {
		r.Email = out.Data[emailKey].hit()
	}
	return r, nil
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
	// A paid provider without its key is left out instead of refusing to
	// start: the key may come later. The page shows it as off.
	c.repKeys, c.repOff = map[string]string{}, nil
	for p, env := range reputationKeyEnv {
		c.repKeys[p] = getenv(env)
	}
	for _, p := range c.Reputation {
		if _, paid := reputationKeyEnv[p]; paid && c.repKeys[p] == "" {
			c.repOff = append(c.repOff, p)
		}
	}
	c.Reputation = slices.DeleteFunc(c.Reputation, func(p string) bool { return slices.Contains(c.repOff, p) })
	return nil
}

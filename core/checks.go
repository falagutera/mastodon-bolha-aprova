package core

import (
	"context"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

// Checks the core runs on every request, whatever the app.
const (
	checkBotSignal  = "bot_signal"
	checkDisposable = "disposable_email"
	checkReason     = "reason_given"
)

var coreChecks = []string{checkBotSignal, checkDisposable, checkReason}

var (
	randomWord = regexp.MustCompile(`^[A-Za-z]{6,40}$`)
	// many dots before @gmail.com: one inbox under many names
	dottedGmail = regexp.MustCompile(`(?i)^([^@.]+\.){4,}[^@]*@(gmail|googlemail)\.com$`)
	hasLink     = regexp.MustCompile(`(?i)https?://`)
)

// botSignal is the heuristic of the old aprovacoes script: a random-looking
// single word as the reason, a dotted Gmail address, or a link in the reason.
func botSignal(email, reason string) (bool, string) {
	r := strings.TrimSpace(reason)
	if randomWord.MatchString(r) {
		upper, flips := 0, false
		for i, c := range r {
			if unicode.IsUpper(c) {
				upper++
				if i > 0 && unicode.IsLower(rune(r[i-1])) {
					flips = true
				}
			}
		}
		if upper >= 3 && flips {
			return true, "random_reason"
		}
	}
	if dottedGmail.MatchString(email) {
		return true, "dotted_gmail"
	}
	if hasLink.MatchString(r) {
		return true, "link_in_reason"
	}
	return false, ""
}

// checker runs the core checks. MX lookups are cached: a queue has many
// requests from the same few domains.
type checker struct {
	disposable []string
	lookupMX   func(ctx context.Context, domain string) ([]*net.MX, error)

	mu    sync.Mutex
	cache map[string]mxResult
}

type mxResult struct {
	hosts []string
	at    time.Time
}

func newChecker(c ChecksConfig) *checker {
	var d []string
	for _, x := range c.DisposableDomains {
		d = append(d, strings.ToLower(strings.TrimSuffix(strings.TrimSpace(x), ".")))
	}
	return &checker{disposable: d, lookupMX: net.DefaultResolver.LookupMX, cache: map[string]mxResult{}}
}

func (c *checker) mxHosts(ctx context.Context, domain string) []string {
	c.mu.Lock()
	if r, ok := c.cache[domain]; ok && time.Since(r.at) < 10*time.Minute {
		c.mu.Unlock()
		return r.hosts
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var hosts []string
	if mx, err := c.lookupMX(ctx, domain); err == nil {
		for _, m := range mx {
			hosts = append(hosts, strings.ToLower(strings.TrimSuffix(m.Host, ".")))
		}
	}
	c.mu.Lock()
	c.cache[domain] = mxResult{hosts: hosts, at: time.Now()}
	c.mu.Unlock()
	return hosts
}

// listed is true when name is a listed domain or a subdomain of one.
func (c *checker) listed(name string) bool {
	return slices.ContainsFunc(c.disposable, func(d string) bool { return name == d || strings.HasSuffix(name, "."+d) })
}

// run checks one request. noReason (from the module's Caps) drops the reason
// check for apps whose sign-up form has no reason field.
func (c *checker) run(ctx context.Context, r module.Request, noReason bool) []module.Check {
	out := []module.Check{}
	bot, why := botSignal(r.Email, r.Reason)
	out = append(out, module.Check{Name: checkBotSignal, OK: !bot, Detail: why})
	domain := ""
	if _, d, ok := strings.Cut(r.Email, "@"); ok {
		domain = strings.ToLower(strings.TrimSpace(d))
	}
	disposable := false
	if domain != "" && len(c.disposable) > 0 {
		disposable = c.listed(domain)
		if !disposable {
			disposable = slices.ContainsFunc(c.mxHosts(ctx, domain), c.listed)
		}
	}
	out = append(out, module.Check{Name: checkDisposable, OK: !disposable, Detail: domain})
	if !noReason {
		out = append(out, module.Check{Name: checkReason, OK: strings.TrimSpace(r.Reason) != ""})
	}
	return append(out, r.Checks...)
}

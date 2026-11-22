// Package mastodon approves sign-ups on a Mastodon instance with the admin API.
//
// With auth: moderator it acts with the signed-in moderator's token (scopes
// admin:read:accounts admin:write:accounts), so the instance's moderation log
// names the person. With auth: token it uses a service account's token.
package mastodon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gitlab.gutocarvalho.net/open/bolha/aprova/module"
)

func init() { module.Register("mastodon", New) }

var (
	idPattern = regexp.MustCompile(`^[0-9]{1,24}$`)
	nextLink  = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)
)

const (
	responseLimit = 8 << 20
	maxPages      = 10
	sameIPLimit   = 100
)

type mastodon struct {
	base   string
	auth   string
	token  string
	client *http.Client
}

// New builds the module for one instance.
func New(c module.Config) (module.Module, error) {
	if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://127.0.0.1") {
		return nil, errors.New("mastodon: url must be https")
	}
	switch c.Auth {
	case "moderator":
	case "token":
		if c.Token == "" {
			return nil, errors.New("mastodon: auth token needs a token")
		}
	default:
		return nil, fmt.Errorf("mastodon: unknown auth %q", c.Auth)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &mastodon{base: strings.TrimRight(c.URL, "/"), auth: c.Auth, token: c.Token, client: client}, nil
}

func (m *mastodon) Caps() module.Caps {
	// Rejecting deletes the pending account without e-mail; approving lets the
	// instance send its own welcome.
	return module.Caps{ModeratorAuth: true, SilentReject: true, SendsWelcome: true}
}

func (m *mastodon) tokenFor(a module.Actor) (string, error) {
	if m.auth == "token" {
		return m.token, nil
	}
	if a.Token == "" {
		return "", module.ErrUnauthorized
	}
	return a.Token, nil
}

type adminAccount struct {
	ID            string    `json:"id"`
	Username      string    `json:"username"`
	Email         string    `json:"email"`
	CreatedAt     time.Time `json:"created_at"`
	Approved      bool      `json:"approved"`
	InviteRequest string    `json:"invite_request"`
	Domain        *string   `json:"domain"` // null for local accounts
	IP            string    `json:"ip"`
	IPs           []struct {
		IP string `json:"ip"`
	} `json:"ips"`
	Suspended bool `json:"suspended"`
	Disabled  bool `json:"disabled"`
	Silenced  bool `json:"silenced"`
}

// publicIP returns s when it is a public unicast address, else "". It is the
// only way an IP from the instance reaches a URL or a page.
func publicIP(s string) string {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !a.IsGlobalUnicast() || a.IsPrivate() || a.Zone() != "" {
		return ""
	}
	return a.Unmap().String()
}

// signUpIP is the address the instance reports for the account: the latest, or
// the first of the list (it holds the sign-up IP).
func (a adminAccount) signUpIP() string {
	if ip := publicIP(a.IP); ip != "" {
		return ip
	}
	for _, x := range a.IPs {
		if ip := publicIP(x.IP); ip != "" {
			return ip
		}
	}
	return ""
}

func (a adminAccount) status() string {
	switch {
	case a.Suspended:
		return "suspended"
	case a.Disabled:
		return "disabled"
	case a.Silenced:
		return "silenced"
	case !a.Approved:
		return "pending"
	}
	return "active"
}

// pending is a local account not approved yet. Remote accounts have no approval.
func (a adminAccount) pending() bool { return a.Domain == nil && !a.Approved }

func (a adminAccount) request() module.Request {
	return module.Request{ID: a.ID, Username: a.Username, Email: a.Email, Reason: a.InviteRequest, Created: a.CreatedAt, IP: a.signUpIP()}
}

func (m *mastodon) do(ctx context.Context, token, method, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return m.client.Do(req)
}

func status(resp *http.Response) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return module.ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return module.ErrGone
	default:
		return fmt.Errorf("mastodon: status %d", resp.StatusCode)
	}
}

func (m *mastodon) Pending(ctx context.Context, a module.Actor) ([]module.Request, error) {
	token, err := m.tokenFor(a)
	if err != nil {
		return nil, err
	}
	var out []module.Request
	next := m.base + "/api/v1/admin/accounts?local=true&pending=true&limit=200"
	for page := 0; next != "" && page < maxPages; page++ {
		resp, err := m.do(ctx, token, http.MethodGet, next)
		if err != nil {
			return nil, err
		}
		link := resp.Header.Get("Link")
		var items []adminAccount
		err = status(resp)
		if err == nil {
			err = json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&items)
		}
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.pending() && idPattern.MatchString(it.ID) {
				out = append(out, it.request())
			}
		}
		next = ""
		// Follow only links on this same instance.
		if mm := nextLink.FindStringSubmatch(link); mm != nil && strings.HasPrefix(mm[1], m.base+"/") && len(items) > 0 {
			next = mm[1]
		}
	}
	return out, nil
}

func (m *mastodon) Get(ctx context.Context, a module.Actor, id string) (module.Request, bool, error) {
	if !idPattern.MatchString(id) {
		return module.Request{}, false, nil
	}
	token, err := m.tokenFor(a)
	if err != nil {
		return module.Request{}, false, err
	}
	resp, err := m.do(ctx, token, http.MethodGet, m.base+"/api/v1/admin/accounts/"+id)
	if err != nil {
		return module.Request{}, false, err
	}
	defer resp.Body.Close()
	if err := status(resp); errors.Is(err, module.ErrGone) {
		return module.Request{}, false, nil
	} else if err != nil {
		return module.Request{}, false, err
	}
	var it adminAccount
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&it); err != nil {
		return module.Request{}, false, err
	}
	return it.request(), it.pending(), nil
}

func (m *mastodon) act(ctx context.Context, a module.Actor, id, action string) error {
	if !idPattern.MatchString(id) {
		return module.ErrGone
	}
	if _, pending, err := m.Get(ctx, a, id); err != nil {
		return err
	} else if !pending {
		return module.ErrGone
	}
	token, err := m.tokenFor(a)
	if err != nil {
		return err
	}
	resp, err := m.do(ctx, token, http.MethodPost, m.base+"/api/v1/admin/accounts/"+id+"/"+action)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		// The instance answers 403 both for "not allowed" and "not pending anymore".
		if _, pending, err := m.Get(ctx, a, id); err == nil && !pending {
			return module.ErrGone
		}
		return module.ErrUnauthorized
	}
	return status(resp)
}

func (m *mastodon) Approve(ctx context.Context, a module.Actor, id string) error {
	return m.act(ctx, a, id, "approve")
}

func (m *mastodon) Reject(ctx context.Context, a module.Actor, id string) error {
	return m.act(ctx, a, id, "reject")
}

// SameIP counts the other local accounts the instance saw at ip, by status. One
// page only: a crowd of accounts on one IP is the signal, the exact size isn't.
func (m *mastodon) SameIP(ctx context.Context, a module.Actor, id, ip string) (module.IPMatches, error) {
	ip = publicIP(ip)
	if ip == "" || !idPattern.MatchString(id) {
		return module.IPMatches{}, module.ErrGone
	}
	token, err := m.tokenFor(a)
	if err != nil {
		return module.IPMatches{}, err
	}
	resp, err := m.do(ctx, token, http.MethodGet, m.base+"/api/v1/admin/accounts?local=true&limit="+fmt.Sprint(sameIPLimit)+"&ip="+url.QueryEscape(ip))
	if err != nil {
		return module.IPMatches{}, err
	}
	defer resp.Body.Close()
	if err := status(resp); err != nil {
		return module.IPMatches{}, err
	}
	var items []adminAccount
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&items); err != nil {
		return module.IPMatches{}, err
	}
	out := module.IPMatches{Counts: map[string]int{}, More: len(items) >= sameIPLimit, AdminURL: m.base + "/admin/accounts?ip=" + url.QueryEscape(ip)}
	for _, it := range items {
		if it.ID != id && it.Domain == nil {
			out.Counts[it.status()]++
		}
	}
	return out, nil
}

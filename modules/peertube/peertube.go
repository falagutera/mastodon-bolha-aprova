// Package peertube approves sign-up requests on a PeerTube instance that
// requires approval (tested on 8.3.2).
//
// PeerTube's API takes OAuth tokens from a password grant, so token_env holds
// "username:password" of a dedicated user with the Moderator role (it has
// MANAGE_REGISTRATIONS). The module signs in, keeps the token in memory and
// refreshes it; the password and tokens never go to a log or an error.
//
// Reject is silent (preventEmailDelivery); accept lets PeerTube send its own
// welcome. The moderation response PeerTube requires is a fixed internal note.
package peertube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

func init() { module.Register("peertube", New) }

const (
	statePending  = 1
	pageSize      = 100
	maxPages      = 10
	responseLimit = 8 << 20
	approveNote   = "Approved through aprova."
	rejectNote    = "Decided through aprova."
)

var idPattern = regexp.MustCompile(`^[0-9]{1,18}$`)

type peertube struct {
	base     string
	user     string
	password string
	client   *http.Client

	mu           sync.Mutex
	clientID     string
	clientSecret string
	access       string
	refresh      string
	expires      time.Time
}

// New builds the module for one instance.
func New(c module.Config) (module.Module, error) {
	if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://127.0.0.1") {
		return nil, errors.New("peertube: url must be https")
	}
	if c.Auth != "token" {
		return nil, errors.New("peertube: needs auth: token, with token_env holding username:password of a Moderator")
	}
	user, password, ok := strings.Cut(c.Token, ":")
	if !ok || user == "" || password == "" {
		return nil, errors.New("peertube: the token must be username:password of a Moderator")
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &peertube{base: strings.TrimRight(c.URL, "/"), user: user, password: password, client: client}, nil
}

func (p *peertube) Caps() module.Caps {
	return module.Caps{SilentReject: true, SendsWelcome: true}
}

// ---- sign-in

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// token returns a valid access token, signing in or refreshing as needed.
// stale is a token the caller saw refused: it is not reused.
func (p *peertube) token(ctx context.Context, stale string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.access != "" && p.access != stale && time.Now().Before(p.expires) {
		return p.access, nil
	}
	if p.clientID == "" {
		if err := p.fetchClient(ctx); err != nil {
			return "", err
		}
	}
	if p.refresh != "" {
		if err := p.grant(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {p.refresh}}); err == nil {
			return p.access, nil
		}
	}
	if err := p.grant(ctx, url.Values{"grant_type": {"password"}, "username": {p.user}, "password": {p.password}}); err != nil {
		return "", err
	}
	return p.access, nil
}

func (p *peertube) fetchClient(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/api/v1/oauth-clients/local", nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("peertube: oauth client: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peertube: oauth client: status %d", resp.StatusCode)
	}
	var c struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&c); err != nil || c.ClientID == "" {
		return errors.New("peertube: oauth client: bad answer")
	}
	p.clientID, p.clientSecret = c.ClientID, c.ClientSecret
	return nil
}

// grant runs one token grant. Errors carry the status only: the form holds secrets.
func (p *peertube) grant(ctx context.Context, form url.Values) error {
	form.Set("client_id", p.clientID)
	form.Set("client_secret", p.clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/api/v1/users/token", strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("peertube: sign-in request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("peertube: sign-in: %w", ue.Err)
		}
		return errors.New("peertube: sign-in failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			p.access, p.refresh = "", ""
			return module.ErrUnauthorized
		}
		return fmt.Errorf("peertube: sign-in: status %d", resp.StatusCode)
	}
	var t tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&t); err != nil || t.AccessToken == "" {
		return errors.New("peertube: sign-in: bad answer")
	}
	ttl := time.Duration(t.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	p.access, p.refresh = t.AccessToken, t.RefreshToken
	// A minute early, so a token never expires between the check and the call.
	p.expires = time.Now().Add(ttl - time.Minute)
	return nil
}

// do calls the API with the token, signing in again once if it's refused.
func (p *peertube) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	stale := ""
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := p.token(ctx, stale)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, p.base+path, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := p.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			stale = tok
			continue
		}
		return resp, nil
	}
	return nil, module.ErrUnauthorized
}

func status(resp *http.Response) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return module.ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict:
		return module.ErrGone
	default:
		return fmt.Errorf("peertube: status %d", resp.StatusCode)
	}
}

// ---- registrations

type registration struct {
	ID    int64 `json:"id"`
	State struct {
		ID int `json:"id"`
	} `json:"state"`
	RegistrationReason string    `json:"registrationReason"`
	Username           string    `json:"username"`
	Email              string    `json:"email"`
	EmailVerified      *bool     `json:"emailVerified"`
	CreatedAt          time.Time `json:"createdAt"`
}

func (r registration) request() module.Request {
	verified := r.EmailVerified != nil && *r.EmailVerified
	return module.Request{
		ID: strconv.FormatInt(r.ID, 10), Username: r.Username, Email: r.Email, Reason: r.RegistrationReason, Created: r.CreatedAt,
		Checks: []module.Check{{Name: "email_verified", OK: verified}},
	}
}

func (p *peertube) pending(ctx context.Context) ([]registration, error) {
	var out []registration
	for page := 0; page < maxPages; page++ {
		q := url.Values{"stateOneOf": {strconv.Itoa(statePending)}, "sort": {"createdAt"},
			"start": {strconv.Itoa(page * pageSize)}, "count": {strconv.Itoa(pageSize)}}
		resp, err := p.do(ctx, http.MethodGet, "/api/v1/users/registrations?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var list struct {
			Total int            `json:"total"`
			Data  []registration `json:"data"`
		}
		err = status(resp)
		if err == nil {
			err = json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&list)
		}
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, r := range list.Data {
			if r.State.ID == statePending {
				out = append(out, r)
			}
		}
		if len(list.Data) < pageSize || (page+1)*pageSize >= list.Total {
			break
		}
	}
	return out, nil
}

func (p *peertube) Pending(ctx context.Context, _ module.Actor) ([]module.Request, error) {
	list, err := p.pending(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]module.Request, 0, len(list))
	for _, r := range list {
		out = append(out, r.request())
	}
	return out, nil
}

// Get scans the pending list: PeerTube has no way to read one registration.
func (p *peertube) Get(ctx context.Context, _ module.Actor, id string) (module.Request, bool, error) {
	if !idPattern.MatchString(id) {
		return module.Request{}, false, nil
	}
	list, err := p.pending(ctx)
	if err != nil {
		return module.Request{}, false, err
	}
	for _, r := range list {
		if strconv.FormatInt(r.ID, 10) == id {
			return r.request(), true, nil
		}
	}
	return module.Request{}, false, nil
}

func (p *peertube) act(ctx context.Context, id, action string, prevent bool, note string) error {
	if !idPattern.MatchString(id) {
		return module.ErrGone
	}
	if _, pending, err := p.Get(ctx, module.Actor{}, id); err != nil {
		return err
	} else if !pending {
		return module.ErrGone
	}
	body := map[string]any{"moderationResponse": note, "preventEmailDelivery": prevent}
	resp, err := p.do(ctx, http.MethodPost, "/api/v1/users/registrations/"+id+"/"+action, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
	return status(resp)
}

// Approve lets PeerTube send its own acceptance e-mail: it is the welcome.
func (p *peertube) Approve(ctx context.Context, _ module.Actor, id string) error {
	return p.act(ctx, id, "accept", false, approveNote)
}

// Reject sends nothing to the applicant; aprova sends its own message.
func (p *peertube) Reject(ctx context.Context, _ module.Actor, id string) error {
	return p.act(ctx, id, "reject", true, rejectNote)
}

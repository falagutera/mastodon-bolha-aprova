// Package forgejo approves sign-ups on a Forgejo instance with manual
// confirmation of new accounts (REGISTER_MANUAL_CONFIRM): a new account stays
// inactive until an admin activates it.
//
// Approving activates the account; rejecting deletes it, purging what it owns.
// The token is a site admin's (read:admin, write:admin, read:user), so it can do
// anything on the instance. This module only ever calls the paths in the
// package's tests. Requests are known by the account's numeric id; the login
// used in the admin paths comes from Forgejo itself. Before deleting it checks again that the account was
// never activated and never signed in.
package forgejo

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
	"time"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

func init() { module.Register("forgejo", New) }

const (
	responseLimit = 8 << 20
	pageSize      = 50
	maxPages      = 40
	pathAdmin     = "/api/v1/admin/users"
	pathSearch    = "/api/v1/users/search"
)

// idPattern is a request id: the account's numeric id. Never 0: uid=0 means
// "no filter" to the search endpoint.
var idPattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// loginPattern is what a login may look like here: it goes into URL paths.
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$`)

func validLogin(s string) bool {
	return loginPattern.MatchString(s) && !strings.Contains(s, "..")
}

type forgejo struct {
	base   string
	token  string
	client *http.Client
}

// New builds the module for one instance. Only auth: token is possible.
func New(c module.Config) (module.Module, error) {
	if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://127.0.0.1") {
		return nil, errors.New("forgejo: url must be https")
	}
	if c.Auth != "token" || c.Token == "" {
		return nil, errors.New("forgejo: needs auth: token and a site admin token")
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &forgejo{base: strings.TrimRight(c.URL, "/"), token: c.Token, client: client}, nil
}

func (f *forgejo) Caps() module.Caps {
	// Deleting sends nothing, and neither does activating: aprova is the only voice.
	return module.Caps{SilentReject: true, SendsWelcome: false}
}

type user struct {
	ID            int64     `json:"id"`
	Login         string    `json:"login"`
	Email         string    `json:"email"`
	Description   string    `json:"description"` // the profile bio; Forgejo's sign-up asks no reason
	Created       time.Time `json:"created"`
	LastLogin     time.Time `json:"last_login"`
	Active        bool      `json:"active"`
	ProhibitLogin bool      `json:"prohibit_login"`
}

// pending is an account waiting for manual confirmation. A blocked account
// (prohibit_login) is a moderation decision, not a sign-up.
func (u user) pending() bool { return !u.Active && !u.ProhibitLogin }

// used is any sign the account was ever in use. Forgejo reports a never-set
// last login as the Unix epoch.
func (u user) used() bool { return u.LastLogin.Unix() > 0 }

func (u user) request() module.Request {
	return module.Request{ID: strconv.FormatInt(u.ID, 10), Username: u.Login, Email: u.Email, Reason: u.Description, Created: u.Created}
}

func (f *forgejo) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+f.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return f.client.Do(req)
}

func status(resp *http.Response) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return module.ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return module.ErrGone
	default:
		return fmt.Errorf("forgejo: status %d", resp.StatusCode)
	}
}

func (f *forgejo) Pending(ctx context.Context, _ module.Actor) ([]module.Request, error) {
	var out []module.Request
	for page := 1; page <= maxPages; page++ {
		resp, err := f.do(ctx, http.MethodGet, fmt.Sprintf("%s?page=%d&limit=%d&sort=newest", pathAdmin, page, pageSize), nil)
		if err != nil {
			return nil, err
		}
		var list []user
		err = status(resp)
		if err == nil {
			err = json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&list)
		}
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, u := range list {
			if u.pending() && validLogin(u.Login) {
				out = append(out, u.request())
			}
		}
		if len(list) < pageSize {
			break
		}
	}
	return out, nil
}

// get finds an account by its numeric id. With a site admin token the answer
// includes active, prohibit_login and last_login.
func (f *forgejo) get(ctx context.Context, id string) (user, bool, error) {
	resp, err := f.do(ctx, http.MethodGet, pathSearch+"?uid="+id+"&limit=1", nil)
	if err != nil {
		return user{}, false, err
	}
	defer resp.Body.Close()
	if err := status(resp); errors.Is(err, module.ErrGone) {
		return user{}, false, nil
	} else if err != nil {
		return user{}, false, err
	}
	var out struct {
		OK   bool   `json:"ok"`
		Data []user `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&out); err != nil {
		return user{}, false, err
	}
	if len(out.Data) == 0 {
		return user{}, false, nil
	}
	u := out.Data[0]
	if strconv.FormatInt(u.ID, 10) != id || !validLogin(u.Login) {
		return user{}, false, errors.New("forgejo: answered for another account")
	}
	return u, true, nil
}

func (f *forgejo) Get(ctx context.Context, _ module.Actor, id string) (module.Request, bool, error) {
	if !idPattern.MatchString(id) {
		return module.Request{}, false, nil
	}
	u, found, err := f.get(ctx, id)
	if err != nil || !found {
		return module.Request{}, false, err
	}
	return u.request(), u.pending(), nil
}

func (f *forgejo) Approve(ctx context.Context, _ module.Actor, id string) error {
	if !idPattern.MatchString(id) {
		return module.ErrGone
	}
	u, found, err := f.get(ctx, id)
	if err != nil {
		return err
	}
	if !found || !u.pending() {
		return module.ErrGone
	}
	// source_id and login_name must go together or not at all; only active changes.
	resp, err := f.do(ctx, http.MethodPatch, pathAdmin+"/"+url.PathEscape(u.Login), map[string]bool{"active": true})
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
	resp.Body.Close()
	return status(resp)
}

// Reject deletes the account. It re-reads it right before, and refuses unless it
// is still an inactive account that never signed in.
func (f *forgejo) Reject(ctx context.Context, _ module.Actor, id string) error {
	if !idPattern.MatchString(id) {
		return module.ErrGone
	}
	u, found, err := f.get(ctx, id)
	if err != nil {
		return err
	}
	if !found || !u.pending() || u.used() {
		return module.ErrGone
	}
	resp, err := f.do(ctx, http.MethodDelete, pathAdmin+"/"+url.PathEscape(u.Login)+"?purge=true", nil)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
	resp.Body.Close()
	return status(resp)
}

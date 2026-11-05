// Package gitlab approves sign-ups on a GitLab instance that requires admin
// approval for new users.
//
// GitLab has no scope smaller than admin for this, so the token can do anything
// on the instance. This module only ever calls the three paths in allowedPaths;
// a test fails if the code uses any other.
package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

func init() { module.Register("gitlab", New) }

const (
	pendingState  = "blocked_pending_approval"
	responseLimit = 8 << 20
	maxPages      = 20
	pathUsers     = "/api/v4/users"
)

var idPattern = regexp.MustCompile(`^[0-9]{1,18}$`)

type gitlab struct {
	base   string
	token  string
	client *http.Client
}

// New builds the module for one instance. Only auth: token is possible.
func New(c module.Config) (module.Module, error) {
	if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://127.0.0.1") {
		return nil, errors.New("gitlab: url must be https")
	}
	if c.Auth != "token" || c.Token == "" {
		return nil, errors.New("gitlab: needs auth: token and an admin token")
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &gitlab{base: strings.TrimRight(c.URL, "/"), token: c.Token, client: client}, nil
}

func (g *gitlab) Caps() module.Caps {
	// GitLab e-mails the person both when approved and when rejected, and can't
	// be told not to. aprova then skips its own rejection e-mail.
	// The sign-up form asks for no reason (Reason is the profile bio).
	return module.Caps{SilentReject: false, SendsWelcome: true, NoReason: true}
}

type user struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	State     string    `json:"state"`
	Bio       string    `json:"bio"`
	CreatedAt time.Time `json:"created_at"`
}

func (u user) request() module.Request {
	return module.Request{ID: strconv.FormatInt(u.ID, 10), Username: u.Username, Email: u.Email, Reason: u.Bio, Created: u.CreatedAt}
}

func (g *gitlab) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", g.token)
	return g.client.Do(req)
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
		return fmt.Errorf("gitlab: status %d", resp.StatusCode)
	}
}

func (g *gitlab) Pending(ctx context.Context, _ module.Actor) ([]module.Request, error) {
	var out []module.Request
	for page := 1; page <= maxPages; page++ {
		resp, err := g.do(ctx, http.MethodGet, pathUsers+"?per_page=100&page="+strconv.Itoa(page))
		if err != nil {
			return nil, err
		}
		var items []user
		err = status(resp)
		if err == nil {
			err = json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&items)
		}
		next := resp.Header.Get("X-Next-Page")
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, u := range items {
			if u.State == pendingState {
				out = append(out, u.request())
			}
		}
		if next == "" || len(items) == 0 {
			break
		}
	}
	return out, nil
}

func (g *gitlab) Get(ctx context.Context, _ module.Actor, id string) (module.Request, bool, error) {
	if !idPattern.MatchString(id) {
		return module.Request{}, false, nil
	}
	resp, err := g.do(ctx, http.MethodGet, pathUsers+"/"+id)
	if err != nil {
		return module.Request{}, false, err
	}
	defer resp.Body.Close()
	if err := status(resp); errors.Is(err, module.ErrGone) {
		return module.Request{}, false, nil
	} else if err != nil {
		return module.Request{}, false, err
	}
	var u user
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&u); err != nil {
		return module.Request{}, false, err
	}
	return u.request(), u.State == pendingState, nil
}

func (g *gitlab) act(ctx context.Context, a module.Actor, id, action string) error {
	if !idPattern.MatchString(id) {
		return module.ErrGone
	}
	if _, pending, err := g.Get(ctx, a, id); err != nil {
		return err
	} else if !pending {
		return module.ErrGone
	}
	resp, err := g.do(ctx, http.MethodPost, pathUsers+"/"+id+"/"+action)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
	resp.Body.Close()
	return status(resp)
}

func (g *gitlab) Approve(ctx context.Context, a module.Actor, id string) error {
	return g.act(ctx, a, id, "approve")
}

func (g *gitlab) Reject(ctx context.Context, a module.Actor, id string) error {
	return g.act(ctx, a, id, "reject")
}

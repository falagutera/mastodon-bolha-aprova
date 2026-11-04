// Package writefreely approves sign-ups on a WriteFreely instance running the
// sign-up approval patch (open/custom/writefreely, patches/0001-signup-approval):
// with approval_required on, a new account stays pending, can't sign in and has
// its blog hidden until it is approved.
//
// The key is the instance's approval_api_key. It only reaches
// /api/admin/registrations, which lists pending accounts and approves or rejects
// them; WriteFreely itself refuses to reject an account that is no longer
// pending. Neither decision makes WriteFreely send anything: aprova is the only
// voice.
package writefreely

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

func init() { module.Register("writefreely", New) }

const (
	responseLimit = 8 << 20
	pathRegs      = "/api/admin/registrations"
)

// idPattern is a request id: the account's numeric id.
var idPattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

type writefreely struct {
	base   string
	key    string
	client *http.Client
}

// New builds the module for one instance. Only auth: token is possible, with the
// instance's approval_api_key as the token.
func New(c module.Config) (module.Module, error) {
	if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://127.0.0.1") {
		return nil, errors.New("writefreely: url must be https")
	}
	if c.Auth != "token" || c.Token == "" {
		return nil, errors.New("writefreely: needs auth: token and the instance's approval_api_key")
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &writefreely{base: strings.TrimRight(c.URL, "/"), key: c.Token, client: client}, nil
}

func (w *writefreely) Caps() module.Caps {
	return module.Caps{SilentReject: true, SendsWelcome: false}
}

type registration struct {
	ID       int64     `json:"id"`
	Username string    `json:"username"`
	Email    string    `json:"email"`
	Reason   string    `json:"reason"`
	Created  time.Time `json:"created"`
	Pending  *bool     `json:"pending,omitempty"` // only in the single-account answer
}

func (r registration) request() module.Request {
	return module.Request{ID: strconv.FormatInt(r.ID, 10), Username: r.Username, Email: r.Email, Reason: r.Reason, Created: r.Created}
}

// call does one request and decodes WriteFreely's {"code", "data"} envelope.
func (w *writefreely) call(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, w.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.key)
	req.Header.Set("Accept", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return module.ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict:
		// Unknown id, already decided, or (404 on the list) the API isn't enabled.
		return module.ErrGone
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("writefreely: status %d", resp.StatusCode)
	}
	if out == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
		return err
	}
	env := struct {
		Data json.RawMessage `json:"data"`
	}{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, responseLimit)).Decode(&env); err != nil {
		return err
	}
	return json.Unmarshal(env.Data, out)
}

func (w *writefreely) Pending(ctx context.Context, _ module.Actor) ([]module.Request, error) {
	var list []registration
	if err := w.call(ctx, http.MethodGet, pathRegs, &list); err != nil {
		if errors.Is(err, module.ErrGone) {
			return nil, errors.New("writefreely: registrations API not found (patch missing or approval_api_key unset)")
		}
		return nil, err
	}
	out := make([]module.Request, 0, len(list))
	for _, r := range list {
		if r.ID > 0 {
			out = append(out, r.request())
		}
	}
	return out, nil
}

func (w *writefreely) Get(ctx context.Context, _ module.Actor, id string) (module.Request, bool, error) {
	if !idPattern.MatchString(id) {
		return module.Request{}, false, nil
	}
	var r registration
	if err := w.call(ctx, http.MethodGet, pathRegs+"/"+id, &r); err != nil {
		if errors.Is(err, module.ErrGone) {
			return module.Request{}, false, nil
		}
		return module.Request{}, false, err
	}
	if strconv.FormatInt(r.ID, 10) != id {
		return module.Request{}, false, errors.New("writefreely: answered for another account")
	}
	return r.request(), r.Pending != nil && *r.Pending, nil
}

func (w *writefreely) decide(ctx context.Context, id, action string) error {
	if !idPattern.MatchString(id) {
		return module.ErrGone
	}
	return w.call(ctx, http.MethodPost, pathRegs+"/"+id+"/"+action, nil)
}

func (w *writefreely) Approve(ctx context.Context, _ module.Actor, id string) error {
	return w.decide(ctx, id, "approve")
}

// Reject deletes the account. WriteFreely does it only while the account is
// still pending (409 otherwise, which is ErrGone here).
func (w *writefreely) Reject(ctx context.Context, _ module.Actor, id string) error {
	return w.decide(ctx, id, "reject")
}

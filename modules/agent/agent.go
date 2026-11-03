// Package agent approves sign-ups through an agent that runs next to the app
// and speaks the wire contract (see package wire). It is how apps without an
// API come in: aprova-agent runs the app's own code, and this module only signs
// requests to it.
//
//	module: agent
//	url: http://aprova-agent.photos.svc   # plain http only inside a private network
//	auth: token
//	token_env: APROVA_PHOTOS_AGENT_SECRET  # shared with the agent, 32+ characters
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/module"
	"gitlab.gutocarvalho.net/open/aprova/wire"
)

func init() { module.Register("agent", New) }

const responseLimit = 8 << 20

type agent struct {
	base   string
	secret []byte
	client *http.Client

	mu   sync.Mutex
	caps *module.Caps
}

// New builds the module for one agent.
func New(c module.Config) (module.Module, error) {
	if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://") {
		return nil, errors.New("agent: url must be http(s)")
	}
	if c.Auth != "token" {
		return nil, errors.New("agent: auth must be token (the secret shared with the agent)")
	}
	if len(c.Token) < wire.MinSecret {
		return nil, fmt.Errorf("agent: the shared secret needs at least %d characters", wire.MinSecret)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	return &agent{base: strings.TrimRight(c.URL, "/"), secret: []byte(c.Token), client: client}, nil
}

func (m *agent) call(ctx context.Context, method, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, bytes.NewReader(nil))
	if err != nil {
		return 0, err
	}
	wire.Sign(req, m.secret, nil, time.Now())
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, responseLimit)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return resp.StatusCode, module.ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict:
		return resp.StatusCode, module.ErrGone
	case resp.StatusCode/100 != 2:
		var e wire.Error
		json.NewDecoder(body).Decode(&e)
		if e.Error != "" {
			return resp.StatusCode, fmt.Errorf("agent: status %d: %s", resp.StatusCode, e.Error)
		}
		return resp.StatusCode, fmt.Errorf("agent: status %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("agent: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// Caps asks the agent once. Until it answers, it assumes a silent reject: the
// core only reads it after a reject went through the agent, so by then it knows.
func (m *agent) Caps() module.Caps {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.caps != nil {
		return *m.caps
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var c wire.Caps
	if _, err := m.call(ctx, http.MethodGet, "/v1/caps", &c); err != nil {
		return module.Caps{SilentReject: true}
	}
	m.caps = &module.Caps{SilentReject: c.SilentReject, SendsWelcome: c.SendsWelcome}
	return *m.caps
}

func convert(r wire.Request) module.Request {
	return module.Request{ID: r.ID, Username: r.Username, Email: r.Email, Reason: r.Reason, Created: r.Created}
}

func (m *agent) Pending(ctx context.Context, _ module.Actor) ([]module.Request, error) {
	var p wire.Pending
	if _, err := m.call(ctx, http.MethodGet, "/v1/pending", &p); err != nil {
		return nil, err
	}
	out := make([]module.Request, 0, len(p.Requests))
	for _, r := range p.Requests {
		if wire.ID.MatchString(r.ID) {
			out = append(out, convert(r))
		}
	}
	return out, nil
}

func (m *agent) Get(ctx context.Context, _ module.Actor, id string) (module.Request, bool, error) {
	if !wire.ID.MatchString(id) {
		return module.Request{}, false, nil
	}
	var one wire.One
	if _, err := m.call(ctx, http.MethodGet, "/v1/requests/"+id, &one); errors.Is(err, module.ErrGone) {
		return module.Request{}, false, nil
	} else if err != nil {
		return module.Request{}, false, err
	}
	if one.Request.ID != id {
		return module.Request{}, false, errors.New("agent: answered for another request")
	}
	return convert(one.Request), one.Pending, nil
}

func (m *agent) act(ctx context.Context, id, action string) error {
	if !wire.ID.MatchString(id) {
		return module.ErrGone
	}
	_, err := m.call(ctx, http.MethodPost, "/v1/requests/"+id+"/"+action, nil)
	return err
}

func (m *agent) Approve(ctx context.Context, _ module.Actor, id string) error {
	return m.act(ctx, id, "approve")
}

func (m *agent) Reject(ctx context.Context, _ module.Actor, id string) error {
	return m.act(ctx, id, "reject")
}

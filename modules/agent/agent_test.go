package agent

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.gutocarvalho.net/open/bolha/aprova/module"
	"gitlab.gutocarvalho.net/open/bolha/aprova/moduletest"
	"gitlab.gutocarvalho.net/open/bolha/aprova/wire"
)

const secret = "0123456789abcdef0123456789abcdef-test"

// fake is an agent that checks every signature, as aprova-agent does.
type fake struct {
	mu       sync.Mutex
	reqs     map[string]wire.Request
	approved map[string]bool
	rejected map[string]bool
	calls    int
	refused  int
}

func (f *fake) Add(r module.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs[r.ID] = wire.Request{ID: r.ID, Username: r.Username, Email: r.Email, Reason: r.Reason, Created: r.Created}
}
func (f *fake) Remove(id string)        { f.mu.Lock(); delete(f.reqs, id); f.mu.Unlock() }
func (f *fake) Approved(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.approved[id] }
func (f *fake) Rejected(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.rejected[id] }
func (f *fake) Notified(string) bool    { return false }
func (f *fake) Requests() int           { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{reqs: map[string]wire.Request{}, approved: map[string]bool{}, rejected: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/caps", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wire.Caps{SilentReject: true, SendsWelcome: true})
	})
	mux.HandleFunc("GET /v1/pending", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := wire.Pending{Requests: []wire.Request{{ID: "../bad"}}} // dropped by the module
		for _, q := range f.reqs {
			p.Requests = append(p.Requests, q)
		}
		json.NewEncoder(w).Encode(p)
	})
	mux.HandleFunc("GET /v1/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q, ok := f.reqs[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(wire.One{Request: q, Pending: true})
	})
	mux.HandleFunc("POST /v1/requests/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		if _, ok := f.reqs[id]; !ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		delete(f.reqs, id)
		if r.PathValue("action") == "approve" {
			f.approved[id] = true
		} else {
			f.rejected[id] = true
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls++
		f.mu.Unlock()
		if err := wire.Verify(r, []byte(secret), body, time.Now()); err != nil {
			f.mu.Lock()
			f.refused++
			f.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestContract(t *testing.T) {
	moduletest.Run(t, func(t *testing.T) (module.Module, moduletest.Fake, module.Actor) {
		f, srv := newFake(t)
		m, err := New(module.Config{URL: srv.URL, Auth: "token", Token: secret})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if f.refused > 0 {
				t.Errorf("%d requests had a bad signature", f.refused)
			}
		})
		return m, f, module.Actor{}
	})
}

func TestCaps(t *testing.T) {
	_, srv := newFake(t)
	m, _ := New(module.Config{URL: srv.URL, Auth: "token", Token: secret})
	if c := m.Caps(); !c.SilentReject || !c.SendsWelcome || c.ModeratorAuth {
		t.Errorf("caps: %+v", c)
	}
}

func TestWrongSecretIsUnauthorized(t *testing.T) {
	_, srv := newFake(t)
	m, _ := New(module.Config{URL: srv.URL, Auth: "token", Token: strings.Repeat("x", 40)})
	if _, err := m.Pending(t.Context(), module.Actor{}); !errors.Is(err, module.ErrUnauthorized) {
		t.Errorf("pending with the wrong secret: %v", err)
	}
}

func TestConfig(t *testing.T) {
	for _, c := range []module.Config{
		{URL: "http://agent", Auth: "moderator", Token: secret},
		{URL: "http://agent", Auth: "token", Token: "short"},
		{URL: "ftp://agent", Auth: "token", Token: secret},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestSignatureCoversPathAndTime(t *testing.T) {
	s := []byte(secret)
	now := time.Now()
	r := httptest.NewRequest(http.MethodPost, "/v1/requests/1/approve", nil)
	wire.Sign(r, s, nil, now)
	if err := wire.Verify(r, s, nil, now); err != nil {
		t.Fatal(err)
	}
	moved := httptest.NewRequest(http.MethodPost, "/v1/requests/2/approve", nil)
	moved.Header = r.Header.Clone()
	if err := wire.Verify(moved, s, nil, now); !errors.Is(err, wire.ErrBadSig) {
		t.Errorf("signature reused on another id: %v", err)
	}
	other := httptest.NewRequest(http.MethodPost, "/v1/requests/1/reject", nil)
	other.Header = r.Header.Clone()
	if err := wire.Verify(other, s, nil, now); !errors.Is(err, wire.ErrBadSig) {
		t.Errorf("approve signature reused for reject: %v", err)
	}
	if err := wire.Verify(r, s, nil, now.Add(2*wire.MaxSkew)); !errors.Is(err, wire.ErrSkew) {
		t.Errorf("old signature: %v", err)
	}
}

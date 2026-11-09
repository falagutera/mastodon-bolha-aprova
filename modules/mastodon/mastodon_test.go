package mastodon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gitlab.gutocarvalho.net/open/bolha/aprova/module"
	"gitlab.gutocarvalho.net/open/bolha/aprova/moduletest"
)

type fake struct {
	mu       sync.Mutex
	accounts map[string]*adminAccount
	approved map[string]bool
	rejected map[string]bool
	calls    int
	token    string
}

func (f *fake) Add(r module.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[r.ID] = &adminAccount{ID: r.ID, Username: r.Username, Email: r.Email, InviteRequest: r.Reason, CreatedAt: r.Created}
}
func (f *fake) Remove(id string)        { f.mu.Lock(); delete(f.accounts, id); f.mu.Unlock() }
func (f *fake) Approved(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.approved[id] }
func (f *fake) Rejected(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.rejected[id] }
func (f *fake) Notified(string) bool    { return false }
func (f *fake) Requests() int           { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func (f *fake) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pending") != "true" || r.URL.Query().Get("local") != "true" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		list := []*adminAccount{}
		for _, a := range f.accounts {
			if !a.Approved {
				list = append(list, a)
			}
		}
		json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("GET /api/v1/admin/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, ok := f.accounts[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(a)
	})
	mux.HandleFunc("POST /api/v1/admin/accounts/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a, ok := f.accounts[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if a.Approved {
			w.WriteHeader(http.StatusForbidden) // what the instance does for "not pending"
			return
		}
		switch r.PathValue("action") {
		case "approve":
			a.Approved, f.approved[id] = true, true
		case "reject":
			delete(f.accounts, id)
			f.rejected[id] = true
		}
		fmt.Fprint(w, `{}`)
	})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

func newFake(t *testing.T, token string) (*fake, *httptest.Server) {
	f := &fake{accounts: map[string]*adminAccount{}, approved: map[string]bool{}, rejected: map[string]bool{}, token: token}
	srv := f.server()
	t.Cleanup(srv.Close)
	return f, srv
}

func TestContractModerator(t *testing.T) {
	moduletest.Run(t, func(t *testing.T) (module.Module, moduletest.Fake, module.Actor) {
		f, srv := newFake(t, "mod-token")
		m, err := New(module.Config{URL: srv.URL, Auth: "moderator", Client: srv.Client()})
		if err != nil {
			t.Fatal(err)
		}
		return m, f, module.Actor{AccountID: "1", Acct: "ana", Token: "mod-token"}
	})
}

func TestContractServiceToken(t *testing.T) {
	moduletest.Run(t, func(t *testing.T) (module.Module, moduletest.Fake, module.Actor) {
		f, srv := newFake(t, "svc-token")
		m, err := New(module.Config{URL: srv.URL, Auth: "token", Token: "svc-token", Client: srv.Client()})
		if err != nil {
			t.Fatal(err)
		}
		// The moderator's own token is not used with a service token.
		return m, f, module.Actor{AccountID: "1", Acct: "ana", Token: "ignored"}
	})
}

func TestModeratorWithoutTokenIsRefused(t *testing.T) {
	f, srv := newFake(t, "mod-token")
	f.Add(module.Request{ID: "1", Username: "x"})
	m, _ := New(module.Config{URL: srv.URL, Auth: "moderator", Client: srv.Client()})
	if _, err := m.Pending(t.Context(), module.Actor{}); !errors.Is(err, module.ErrUnauthorized) {
		t.Errorf("no token: %v", err)
	}
	if _, err := m.Pending(t.Context(), module.Actor{Token: "wrong"}); !errors.Is(err, module.ErrUnauthorized) {
		t.Errorf("bad token: %v", err)
	}
}

// Remote accounts have no approval: never treat one as pending.
func TestRemoteAccountIsNotPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"7","username":"far","approved":false,"domain":"remote.example"}`)
	}))
	defer srv.Close()
	m, _ := New(module.Config{URL: srv.URL, Auth: "token", Token: "t", Client: srv.Client()})
	if _, pending, err := m.Get(t.Context(), module.Actor{}, "7"); pending || err != nil {
		t.Errorf("remote: %v %v", pending, err)
	}
	if err := m.Approve(t.Context(), module.Actor{}, "7"); !errors.Is(err, module.ErrGone) {
		t.Errorf("approve remote: %v", err)
	}
}

func TestConfig(t *testing.T) {
	for _, c := range []module.Config{
		{URL: "http://example.social", Auth: "moderator"},
		{URL: "https://example.social", Auth: "token"},
		{URL: "https://example.social", Auth: "other"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestPagesStayOnTheInstance(t *testing.T) {
	var other bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other = true; fmt.Fprint(w, `[]`) }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+evil.URL+`/api/v1/admin/accounts?max_id=1>; rel="next"`)
		fmt.Fprint(w, `[{"id":"5","username":"a","approved":false}]`)
	}))
	defer srv.Close()
	m, _ := New(module.Config{URL: srv.URL, Auth: "token", Token: "t", Client: srv.Client()})
	got, err := m.Pending(t.Context(), module.Actor{})
	if err != nil || len(got) != 1 || other {
		t.Errorf("followed a link off the instance: %v %v %v", got, err, other)
	}
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1") {
		t.Skip("test server not on 127.0.0.1")
	}
}

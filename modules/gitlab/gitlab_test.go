package gitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"sync"
	"testing"

	"gitlab.gutocarvalho.net/open/aprova/module"
	"gitlab.gutocarvalho.net/open/aprova/moduletest"
)

type fake struct {
	mu       sync.Mutex
	users    map[string]*user
	approved map[string]bool
	rejected map[string]bool
	notified map[string]bool // rejected through POST /reject, which e-mails the person
	paths    []string
}

func (f *fake) Add(r module.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, _ := strconv.ParseInt(r.ID, 10, 64)
	f.users[r.ID] = &user{ID: id, Username: r.Username, Email: r.Email, Bio: r.Reason, State: pendingState, CreatedAt: r.Created}
}
func (f *fake) Remove(id string)        { f.mu.Lock(); delete(f.users, id); f.mu.Unlock() }
func (f *fake) Approved(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.approved[id] }
func (f *fake) Rejected(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.rejected[id] }

// GitLab's reject e-mails the person; deleting the user doesn't.
func (f *fake) Notified(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.notified[id] }
func (f *fake) Requests() int           { f.mu.Lock(); defer f.mu.Unlock(); return len(f.paths) }

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{users: map[string]*user{}, approved: map[string]bool{}, rejected: map[string]bool{}, notified: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v4/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := f.users[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("hard_delete") != "" {
			w.WriteHeader(http.StatusBadRequest) // must not destroy contributions
			return
		}
		delete(f.users, id)
		f.rejected[id] = true
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v4/users", func(w http.ResponseWriter, r *http.Request) {
		list := []*user{{ID: 999, Username: "active", State: "active"}} // not pending: filtered out
		for _, u := range f.users {
			list = append(list, u)
		}
		json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("GET /api/v4/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, ok := f.users[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(u)
	})
	mux.HandleFunc("POST /api/v4/users/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		u, ok := f.users[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if u.State != pendingState {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.PathValue("action") {
		case "approve":
			u.State, f.approved[id] = "active", true
		case "reject":
			delete(f.users, id)
			f.rejected[id], f.notified[id] = true, true
		}
		fmt.Fprint(w, `{}`)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		if r.Header.Get("PRIVATE-TOKEN") != "admin-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

var allowed = regexp.MustCompile(`^(GET /api/v4/users|GET /api/v4/users/[0-9]+|POST /api/v4/users/[0-9]+/approve|DELETE /api/v4/users/[0-9]+)$`)

func TestContract(t *testing.T) {
	var fakes []*fake
	moduletest.Run(t, func(t *testing.T) (module.Module, moduletest.Fake, module.Actor) {
		f, srv := newFake(t)
		fakes = append(fakes, f)
		m, err := New(module.Config{URL: srv.URL, Auth: "token", Token: "admin-token", Client: srv.Client()})
		if err != nil {
			t.Fatal(err)
		}
		return m, f, module.Actor{AccountID: "1", Acct: "ana"}
	})
	// The admin token can do anything; the module may only ever do these calls.
	for _, f := range fakes {
		for _, p := range f.paths {
			if !allowed.MatchString(p) {
				t.Errorf("called %s", p)
			}
		}
	}
}

func TestSourceUsesOnlyTheUsersPath(t *testing.T) {
	src, err := os.ReadFile("gitlab.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := regexp.MustCompile(`/api/v4/[a-z_/]*`).FindAllString(string(src), -1); len(got) != 1 || got[0] != "/api/v4/users" {
		t.Errorf("API paths in the source: %v", got)
	}
	if m := regexp.MustCompile(`http\.Method(Put|Patch)`).FindString(string(src)); m != "" {
		t.Errorf("uses %s", m)
	}
}

func TestRejectIsSilent(t *testing.T) {
	m, _ := New(module.Config{URL: "https://gitlab.example", Auth: "token", Token: "x"})
	if !m.Caps().SilentReject {
		t.Error("reject deletes the pending user, which e-mails nobody")
	}
}

// Delete would remove an active user too: Reject must check first.
func TestRejectLeavesActiveUsers(t *testing.T) {
	f, srv := newFake(t)
	m, _ := New(module.Config{URL: srv.URL, Auth: "token", Token: "admin-token", Client: srv.Client()})
	f.Add(module.Request{ID: "61", Username: "ana", Email: "ana@example.org"})
	f.users["61"].State = "active"
	if err := m.Reject(t.Context(), module.Actor{}, "61"); !errors.Is(err, module.ErrGone) {
		t.Errorf("reject active: %v", err)
	}
	if _, ok := f.users["61"]; !ok {
		t.Error("deleted an active user")
	}
}

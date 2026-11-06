package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/module"
	"gitlab.gutocarvalho.net/open/aprova/moduletest"
)

const token = "admin-token"

type fake struct {
	mu       sync.Mutex
	users    map[string]*user
	approved map[string]bool
	rejected map[string]bool
	calls    []string
	// flip activates a user on its nth lookup, as another admin would.
	flipID  string
	flipAt  int
	gets    int
	patches []string
}

func (f *fake) Add(r module.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, _ := strconv.ParseInt(r.ID, 10, 64)
	f.users[r.ID] = &user{ID: id, Login: r.Username, Email: r.Email, Description: r.Reason, Created: r.Created, LastLogin: time.Unix(0, 0)}
}

func (f *fake) byLogin(login string) (string, *user) {
	for id, u := range f.users {
		if u.Login == login {
			return id, u
		}
	}
	return "", nil
}
func (f *fake) Remove(id string)        { f.mu.Lock(); delete(f.users, id); f.mu.Unlock() }
func (f *fake) Approved(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.approved[id] }
func (f *fake) Rejected(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.rejected[id] }
func (f *fake) Notified(string) bool    { return false } // Forgejo sends nothing on delete
func (f *fake) Requests() int           { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{users: map[string]*user{}, approved: map[string]bool{}, rejected: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/admin/users", func(w http.ResponseWriter, r *http.Request) {
		list := []*user{{ID: 1, Login: "active", Active: true}, {ID: 2, Login: "blocked", ProhibitLogin: true}}
		if r.URL.Query().Get("page") != "1" {
			list = nil
		}
		if r.URL.Query().Get("page") == "1" {
			for _, u := range f.users {
				list = append(list, u)
			}
		}
		json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("GET /api/v1/users/search", func(w http.ResponseWriter, r *http.Request) {
		uid := r.URL.Query().Get("uid")
		if uid == "" || uid == "0" {
			t.Errorf("search without a uid: %s", r.URL.RawQuery)
		}
		var data []*user
		if u, ok := f.users[uid]; ok {
			f.gets++
			if uid == f.flipID && f.gets == f.flipAt {
				u.Active = true
			}
			data = append(data, u)
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	})
	mux.HandleFunc("PATCH /api/v1/admin/users/{login}", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.patches = append(f.patches, string(raw))
		_, u := f.byLogin(r.PathValue("login"))
		if u == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var in map[string]any
		json.Unmarshal(raw, &in)
		if _, s := in["source_id"]; s != (in["login_name"] != nil) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		if in["active"] == true {
			u.Active, f.approved[strconv.FormatInt(u.ID, 10)] = true, true
		}
		json.NewEncoder(w).Encode(u)
	})
	mux.HandleFunc("DELETE /api/v1/admin/users/{login}", func(w http.ResponseWriter, r *http.Request) {
		login := r.PathValue("login")
		id, u := f.byLogin(login)
		if u == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if u.Active {
			t.Errorf("deleted active user %s", login)
		}
		if r.URL.Query().Get("purge") != "true" {
			t.Errorf("delete without purge")
		}
		delete(f.users, id)
		f.rejected[id] = true
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.EscapedPath())
		if r.Header.Get("Authorization") != "token "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func newModule(t *testing.T, srv *httptest.Server) module.Module {
	m, err := New(module.Config{URL: srv.URL, Auth: "token", Token: token, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The token is a site admin's: the module may only ever make these calls.
var allowed = regexp.MustCompile(`^(GET /api/v1/admin/users|GET /api/v1/users/search|PATCH /api/v1/admin/users/[A-Za-z0-9_.-]+|DELETE /api/v1/admin/users/[A-Za-z0-9_.-]+)$`)

func TestContract(t *testing.T) {
	var fakes []*fake
	moduletest.Run(t, func(t *testing.T) (module.Module, moduletest.Fake, module.Actor) {
		f, srv := newFake(t)
		fakes = append(fakes, f)
		return newModule(t, srv), f, module.Actor{}
	})
	for _, f := range fakes {
		for _, c := range f.calls {
			if !allowed.MatchString(c) {
				t.Errorf("called %s", c)
			}
		}
		for _, p := range f.patches {
			if p != `{"active":true}` {
				t.Errorf("patch body %s", p)
			}
		}
	}
}

func TestSourceUsesOnlyTheUserPaths(t *testing.T) {
	src, err := os.ReadFile("forgejo.go")
	if err != nil {
		t.Fatal(err)
	}
	got := regexp.MustCompile(`/api/v1/[a-z_/]*`).FindAllString(string(src), -1)
	if len(got) != 2 || got[0] != "/api/v1/admin/users" || got[1] != "/api/v1/users/search" {
		t.Errorf("API paths in the source: %v", got)
	}
	if m := regexp.MustCompile(`http\.Method(Put|Post)`).FindString(string(src)); m != "" {
		t.Errorf("uses %s", m)
	}
}

func TestActiveOrBlockedAccountsAreNeverDeleted(t *testing.T) {
	ctx := context.Background()
	f, srv := newFake(t)
	m := newModule(t, srv)

	// Activated by another admin after the core's check: the module's own
	// re-read sees it and refuses.
	f.Add(module.Request{ID: "31", Username: "ana", Email: "ana@example.org"})
	if _, pending, _ := m.Get(ctx, module.Actor{}, "31"); !pending {
		t.Fatal("ana not pending")
	}
	f.flipID, f.flipAt = "31", 2
	if err := m.Reject(ctx, module.Actor{}, "31"); !errors.Is(err, module.ErrGone) || f.Rejected("31") {
		t.Errorf("rejected an account activated meanwhile: %v", err)
	}

	// Inactive but signed in before approval: a sign-in no longer protects a
	// pending sign-up (unapproved login only reaches an info page), so a reject
	// still deletes it — bots included.
	f.Add(module.Request{ID: "32", Username: "bia", Email: "bia@example.org"})
	f.mu.Lock()
	f.users["32"].LastLogin = time.Now()
	f.mu.Unlock()
	if err := m.Reject(ctx, module.Actor{}, "32"); err != nil || !f.Rejected("32") {
		t.Errorf("did not delete a signed-in pending account: %v", err)
	}

	// Blocked by moderation: not a sign-up, neither listed nor touched.
	f.mu.Lock()
	f.users["33"] = &user{ID: 33, Login: "cid", ProhibitLogin: true, LastLogin: time.Unix(0, 0)}
	f.mu.Unlock()
	if err := m.Reject(ctx, module.Actor{}, "33"); !errors.Is(err, module.ErrGone) || f.Rejected("33") {
		t.Errorf("blocked: %v", err)
	}
	list, _ := m.Pending(ctx, module.Actor{})
	for _, r := range list {
		if r.Username == "active" || r.Username == "blocked" || r.Username == "cid" || r.ID == "31" {
			t.Errorf("listed %s", r.Username)
		}
	}
}

func TestBadIDsNeverReachTheServer(t *testing.T) {
	ctx := context.Background()
	f, srv := newFake(t)
	m := newModule(t, srv)
	for _, id := range []string{"0", "00", "-1", "-2", "../x", ".hidden", "a..b", "ana", strings.Repeat("9", 19), "1/approve", "1 ", "1%2f", ""} {
		if _, pending, err := m.Get(ctx, module.Actor{}, id); pending || err != nil {
			t.Errorf("get %q: %v %v", id, pending, err)
		}
		if err := m.Approve(ctx, module.Actor{}, id); !errors.Is(err, module.ErrGone) {
			t.Errorf("approve %q: %v", id, err)
		}
		if err := m.Reject(ctx, module.Actor{}, id); !errors.Is(err, module.ErrGone) {
			t.Errorf("reject %q: %v", id, err)
		}
	}
	if n := f.Requests(); n != 0 {
		t.Errorf("%d calls with bad logins: %v", n, f.calls)
	}
}

func TestWrongTokenIsUnauthorized(t *testing.T) {
	_, srv := newFake(t)
	m, _ := New(module.Config{URL: srv.URL, Auth: "token", Token: "nope", Client: srv.Client()})
	if _, err := m.Pending(context.Background(), module.Actor{}); !errors.Is(err, module.ErrUnauthorized) {
		t.Errorf("pending with a wrong token: %v", err)
	}
}

func TestConfig(t *testing.T) {
	for _, c := range []module.Config{
		{URL: "http://forgejo.example", Auth: "token", Token: "x"},
		{URL: "https://forgejo.example", Auth: "moderator"},
		{URL: "https://forgejo.example", Auth: "token"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	m, _ := New(module.Config{URL: "https://forgejo.example", Auth: "token", Token: "x"})
	if c := m.Caps(); !c.SilentReject || c.SendsWelcome || c.ModeratorAuth {
		t.Errorf("caps: %+v", c)
	}
}

// A login Forgejo returns is checked before it goes into an admin path.
func TestOddLoginFromTheServerIsNotUsed(t *testing.T) {
	ctx := context.Background()
	f, srv := newFake(t)
	m := newModule(t, srv)
	f.mu.Lock()
	f.users["40"] = &user{ID: 40, Login: "../admin", LastLogin: time.Unix(0, 0)}
	f.mu.Unlock()
	if err := m.Reject(ctx, module.Actor{}, "40"); err == nil || errors.Is(err, module.ErrGone) && f.Rejected("40") {
		t.Errorf("acted on an odd login: %v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "DELETE") || strings.HasPrefix(c, "PATCH") {
			t.Errorf("called %s", c)
		}
	}
}

package writefreely

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gitlab.gutocarvalho.net/open/bolha/aprova/module"
	"gitlab.gutocarvalho.net/open/bolha/aprova/moduletest"
)

const key = "approval-key"

// fake behaves like WriteFreely with the sign-up approval patch.
type fake struct {
	mu       sync.Mutex
	pending  map[string]registration
	active   map[string]bool
	approved map[string]bool
	rejected map[string]bool
	calls    []string
}

func (f *fake) Add(r module.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, _ := strconv.ParseInt(r.ID, 10, 64)
	f.pending[r.ID] = registration{ID: id, Username: r.Username, Email: r.Email, Reason: r.Reason, Created: r.Created}
}
func (f *fake) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pending, id)
	f.active[id] = true // approved by an admin in WriteFreely's own panel
}
func (f *fake) Approved(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.approved[id] }
func (f *fake) Rejected(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.rejected[id] }
func (f *fake) Notified(string) bool    { return false } // WriteFreely sends nothing
func (f *fake) Requests() int           { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func write(w http.ResponseWriter, code int, data any) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"code": code, "data": data})
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{pending: map[string]registration{}, active: map[string]bool{}, approved: map[string]bool{}, rejected: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/registrations", func(w http.ResponseWriter, r *http.Request) {
		list := []registration{}
		for _, reg := range f.pending {
			list = append(list, reg)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		write(w, 200, list)
	})
	mux.HandleFunc("GET /api/admin/registrations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		yes, no := true, false
		if reg, ok := f.pending[id]; ok {
			reg.Pending = &yes
			write(w, 200, reg)
			return
		}
		if f.active[id] {
			n, _ := strconv.ParseInt(id, 10, 64)
			write(w, 200, registration{ID: n, Username: "someone", Pending: &no})
			return
		}
		write(w, 404, nil)
	})
	mux.HandleFunc("POST /api/admin/registrations/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id, action := r.PathValue("id"), r.PathValue("action")
		if _, ok := f.pending[id]; !ok {
			if f.active[id] {
				write(w, 409, nil)
			} else {
				write(w, 404, nil)
			}
			return
		}
		delete(f.pending, id)
		switch action {
		case "approve":
			f.active[id], f.approved[id] = true, true
		case "reject":
			f.rejected[id] = true
		default:
			t.Errorf("action %q", action)
		}
		write(w, 200, map[string]any{"id": id, "action": action})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.EscapedPath())
		if r.Header.Get("Authorization") != "Bearer "+key {
			write(w, 401, nil)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func newModule(t *testing.T, srv *httptest.Server) module.Module {
	m, err := New(module.Config{URL: srv.URL, Auth: "token", Token: key, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var allowed = regexp.MustCompile(`^(GET /api/admin/registrations|GET /api/admin/registrations/[1-9][0-9]*|POST /api/admin/registrations/[1-9][0-9]*/(approve|reject))$`)

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
	}
}

func TestSourceUsesOnlyTheRegistrationsAPI(t *testing.T) {
	src, err := os.ReadFile("writefreely.go")
	if err != nil {
		t.Fatal(err)
	}
	got := regexp.MustCompile(`"/api/[a-z_/]*"`).FindAllString(string(src), -1)
	if len(got) != 1 || got[0] != `"/api/admin/registrations"` {
		t.Errorf("API paths in the source: %v", got)
	}
}

func TestDecidedAccountsAreGone(t *testing.T) {
	ctx := context.Background()
	f, srv := newFake(t)
	m := newModule(t, srv)
	f.Add(module.Request{ID: "21", Username: "ana", Email: "ana@example.org", Reason: "poems"})
	f.Remove("21")
	if _, pending, err := m.Get(ctx, module.Actor{}, "21"); pending || err != nil {
		t.Errorf("approved elsewhere still pending: %v %v", pending, err)
	}
	if err := m.Reject(ctx, module.Actor{}, "21"); !errors.Is(err, module.ErrGone) || f.Rejected("21") {
		t.Errorf("rejected an active account: %v", err)
	}
}

func TestBadIDsNeverReachTheServer(t *testing.T) {
	ctx := context.Background()
	f, srv := newFake(t)
	m := newModule(t, srv)
	for _, id := range []string{"0", "00", "-1", "../x", "ana", strings.Repeat("9", 19), "1/approve", "1 ", "1%2f", ""} {
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
		t.Errorf("%d calls with bad ids: %v", n, f.calls)
	}
}

func TestWrongKeyIsUnauthorized(t *testing.T) {
	_, srv := newFake(t)
	m, _ := New(module.Config{URL: srv.URL, Auth: "token", Token: "nope", Client: srv.Client()})
	if _, err := m.Pending(context.Background(), module.Actor{}); !errors.Is(err, module.ErrUnauthorized) {
		t.Errorf("pending with a wrong key: %v", err)
	}
}

func TestMissingAPIIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	m, _ := New(module.Config{URL: srv.URL, Auth: "token", Token: key, Client: srv.Client()})
	if _, err := m.Pending(context.Background(), module.Actor{}); err == nil || errors.Is(err, module.ErrGone) {
		t.Errorf("an instance without the patch must fail loudly: %v", err)
	}
}

func TestConfig(t *testing.T) {
	for _, c := range []module.Config{
		{URL: "http://bolha.blog", Auth: "token", Token: "x"},
		{URL: "https://bolha.blog", Auth: "moderator"},
		{URL: "https://bolha.blog", Auth: "token"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	m, _ := New(module.Config{URL: "https://bolha.blog", Auth: "token", Token: "x"})
	if c := m.Caps(); !c.SilentReject || c.SendsWelcome || c.ModeratorAuth {
		t.Errorf("caps: %+v", c)
	}
}

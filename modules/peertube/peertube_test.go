package peertube

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gitlab.gutocarvalho.net/open/aprova/module"
	"gitlab.gutocarvalho.net/open/aprova/moduletest"
)

const (
	user     = "aprova-mod"
	password = "s3cret-pass"
)

type fake struct {
	mu        sync.Mutex
	regs      map[string]*registration
	approved  map[string]bool
	rejected  map[string]bool
	notified  map[string]bool
	calls     int
	tokens    map[string]bool // valid access tokens
	refreshes map[string]bool
	n         int
	passwords int
	refreshed int
}

func (f *fake) Add(r module.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, _ := strconv.ParseInt(r.ID, 10, 64)
	v := true
	reg := &registration{ID: id, Username: r.Username, Email: r.Email, RegistrationReason: r.Reason, CreatedAt: r.Created, EmailVerified: &v}
	reg.State.ID = statePending
	f.regs[r.ID] = reg
}
func (f *fake) Remove(id string)        { f.mu.Lock(); delete(f.regs, id); f.mu.Unlock() }
func (f *fake) Approved(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.approved[id] }
func (f *fake) Rejected(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.rejected[id] }
func (f *fake) Notified(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.notified[id] }
func (f *fake) Requests() int           { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// expire drops every access token, as a restart or expiry would.
func (f *fake) expire() { f.mu.Lock(); f.tokens = map[string]bool{}; f.mu.Unlock() }

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{regs: map[string]*registration{}, approved: map[string]bool{}, rejected: map[string]bool{}, notified: map[string]bool{},
		tokens: map[string]bool{}, refreshes: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/oauth-clients/local", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"client_id":"cid","client_secret":"csecret"}`)
	})
	mux.HandleFunc("POST /api/v1/users/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("client_id") != "cid" || r.PostForm.Get("client_secret") != "csecret" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "password":
			if r.PostForm.Get("username") != user || r.PostForm.Get("password") != password {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.passwords++
		case "refresh_token":
			if !f.refreshes[r.PostForm.Get("refresh_token")] {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.refreshed++
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.n++
		a, rt := fmt.Sprintf("access-%d", f.n), fmt.Sprintf("refresh-%d", f.n)
		f.tokens[a], f.refreshes[rt] = true, true
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: a, RefreshToken: rt, ExpiresIn: 86400})
	})
	mux.HandleFunc("GET /api/v1/users/registrations", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stateOneOf") != "1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var list []*registration
		for _, reg := range f.regs {
			list = append(list, reg)
		}
		// One that isn't pending: the module must not list it.
		done := &registration{ID: 999, Username: "old"}
		done.State.ID = 3
		list = append(list, done)
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		json.NewEncoder(w).Encode(map[string]any{"total": len(list), "data": list})
	})
	mux.HandleFunc("POST /api/v1/users/registrations/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var in struct {
			ModerationResponse   string `json:"moderationResponse"`
			PreventEmailDelivery *bool  `json:"preventEmailDelivery"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.ModerationResponse) < 2 || in.PreventEmailDelivery == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, ok := f.regs[id]; !ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		delete(f.regs, id)
		switch r.PathValue("action") {
		case "accept":
			f.approved[id] = true
		case "reject":
			f.rejected[id] = true
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !*in.PreventEmailDelivery {
			f.notified[id] = true
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if strings.HasPrefix(r.URL.Path, "/api/v1/users/registrations") {
			tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !f.tokens[tok] {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func newModule(t *testing.T, srv *httptest.Server, token string) module.Module {
	m, err := New(module.Config{URL: srv.URL, Auth: "token", Token: token, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestContract(t *testing.T) {
	moduletest.Run(t, func(t *testing.T) (module.Module, moduletest.Fake, module.Actor) {
		f, srv := newFake(t)
		return newModule(t, srv, user+":"+password), f, module.Actor{}
	})
}

func TestRejectIsSilentAndApproveWelcomes(t *testing.T) {
	f, srv := newFake(t)
	m := newModule(t, srv, user+":"+password)
	f.Add(module.Request{ID: "1", Username: "ana", Email: "ana@example.org"})
	f.Add(module.Request{ID: "2", Username: "bia", Email: "bia@example.org"})
	ctx := t.Context()
	if err := m.Reject(ctx, module.Actor{}, "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Approve(ctx, module.Actor{}, "2"); err != nil {
		t.Fatal(err)
	}
	if f.Notified("1") {
		t.Error("reject let PeerTube e-mail the applicant")
	}
	if !f.Notified("2") {
		t.Error("approve kept PeerTube from sending its welcome")
	}
	if c := m.Caps(); !c.SilentReject || !c.SendsWelcome || c.ModeratorAuth {
		t.Errorf("caps: %+v", c)
	}
}

func TestEmailVerifiedCheck(t *testing.T) {
	f, srv := newFake(t)
	m := newModule(t, srv, user+":"+password)
	f.Add(module.Request{ID: "5", Username: "eva", Email: "eva@example.org"})
	no := false
	f.mu.Lock()
	f.regs["5"].EmailVerified = &no
	f.mu.Unlock()
	r, _, err := m.Get(t.Context(), module.Actor{}, "5")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checks) != 1 || r.Checks[0].Name != "email_verified" || r.Checks[0].OK {
		t.Errorf("checks: %+v", r.Checks)
	}
}

func TestSignsInOnceAndRefreshesAfterA401(t *testing.T) {
	f, srv := newFake(t)
	m := newModule(t, srv, user+":"+password)
	ctx := t.Context()
	for i := 0; i < 3; i++ {
		if _, err := m.Pending(ctx, module.Actor{}); err != nil {
			t.Fatal(err)
		}
	}
	if f.passwords != 1 || f.refreshed != 0 {
		t.Fatalf("sign-ins: password %d refresh %d", f.passwords, f.refreshed)
	}
	f.expire()
	if _, err := m.Pending(ctx, module.Actor{}); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if f.refreshed != 1 || f.passwords != 1 {
		t.Errorf("expected a refresh: password %d refresh %d", f.passwords, f.refreshed)
	}
	// A refresh token that stopped working: back to the password grant.
	f.mu.Lock()
	f.tokens, f.refreshes = map[string]bool{}, map[string]bool{}
	f.mu.Unlock()
	if _, err := m.Pending(ctx, module.Actor{}); err != nil {
		t.Fatalf("after refresh refused: %v", err)
	}
	if f.passwords != 2 {
		t.Errorf("password grants: %d", f.passwords)
	}
}

func TestBadCredentialsStayOutOfErrors(t *testing.T) {
	_, srv := newFake(t)
	m := newModule(t, srv, user+":wrong-"+password)
	_, err := m.Pending(t.Context(), module.Actor{})
	if !errors.Is(err, module.ErrUnauthorized) {
		t.Fatalf("wrong password: %v", err)
	}
	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), user) {
		t.Errorf("credentials in the error: %v", err)
	}
	// Unreachable instance: the error names no secret either.
	srv.Close()
	_, err = newModule(t, srv, user+":"+password).Pending(t.Context(), module.Actor{})
	if err == nil || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "refresh-") || strings.Contains(err.Error(), "access-") {
		t.Errorf("unreachable: %v", err)
	}
}

func TestConfig(t *testing.T) {
	for _, c := range []module.Config{
		{URL: "https://tube.example", Auth: "moderator", Token: "a:b"},
		{URL: "https://tube.example", Auth: "token", Token: "no-colon"},
		{URL: "https://tube.example", Auth: "token", Token: ":pass"},
		{URL: "http://tube.example", Auth: "token", Token: "a:b"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

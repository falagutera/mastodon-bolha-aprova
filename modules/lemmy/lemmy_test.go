package lemmy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/module"
	"gitlab.gutocarvalho.net/open/aprova/moduletest"
)

type app struct {
	appID    int64
	personID int64
	name     string
	email    string
	answer   string
	admin    *int64
	verified bool
	created  time.Time
}

type fake struct {
	mu         sync.Mutex
	apps       map[string]*app // by person id
	next       int64
	approved   map[string]bool
	rejected   map[string]bool
	calls      int
	bodies     []string
	smtpBroken bool
}

func (f *fake) Add(r module.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pid, _ := strconv.ParseInt(r.ID, 10, 64)
	f.next++
	f.apps[r.ID] = &app{appID: 1000 + f.next, personID: pid, name: r.Username, email: r.Email, answer: r.Reason, verified: true, created: r.Created}
}

// Remove: decided elsewhere, as another admin would.
func (f *fake) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.apps[id]; a != nil {
		one := int64(1)
		a.admin = &one
	}
}
func (f *fake) Approved(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.approved[id] }
func (f *fake) Rejected(id string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.rejected[id] }

// Lemmy e-mails the applicant on reject, which the module declares in Caps.
func (f *fake) Notified(id string) bool { return f.Rejected(id) }
func (f *fake) Requests() int           { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func view(a *app) map[string]any {
	ra := map[string]any{"id": a.appID, "local_user_id": a.personID + 5, "answer": a.answer, "published": a.created.UTC().Format("2006-01-02T15:04:05.000000Z")}
	if a.admin != nil {
		ra["admin_id"] = *a.admin
	}
	return map[string]any{
		"registration_application": ra,
		"creator_local_user":       map[string]any{"email": a.email, "email_verified": a.verified},
		"creator":                  map[string]any{"id": a.personID, "name": a.name},
	}
}

func lemmyErr(w http.ResponseWriter, code string) {
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{apps: map[string]*app{}, approved: map[string]bool{}, rejected: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/admin/registration_application/list", func(w http.ResponseWriter, r *http.Request) {
		list := []map[string]any{}
		if r.URL.Query().Get("page") == "1" {
			for _, a := range f.apps {
				if a.admin == nil || r.URL.Query().Get("unread_only") != "true" {
					list = append(list, view(a))
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"registration_applications": list})
	})
	mux.HandleFunc("GET /api/v3/admin/registration_application", func(w http.ResponseWriter, r *http.Request) {
		a, ok := f.apps[r.URL.Query().Get("person_id")]
		if !ok {
			lemmyErr(w, "couldnt_find_registration_application")
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"registration_application": view(a)})
	})
	mux.HandleFunc("PUT /api/v3/admin/registration_application/approve", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.bodies = append(f.bodies, string(raw))
		var in struct {
			ID      int64 `json:"id"`
			Approve bool  `json:"approve"`
		}
		json.Unmarshal(raw, &in)
		for pid, a := range f.apps {
			if a.appID != in.ID {
				continue
			}
			admin := int64(1)
			a.admin = &admin // Lemmy doesn't refuse a second decision
			if in.Approve {
				f.approved[pid] = true
			} else {
				f.rejected[pid] = true
			}
			if f.smtpBroken {
				lemmyErr(w, "email_send_failed")
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"registration_application": view(a)})
			return
		}
		lemmyErr(w, "couldnt_find_registration_application")
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if r.Header.Get("Authorization") != "Bearer admin-jwt" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func setup(t *testing.T) (module.Module, *fake) {
	f, srv := newFake(t)
	m, err := New(module.Config{URL: srv.URL, Auth: "token", Token: "admin-jwt", Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return m, f
}

func TestContract(t *testing.T) {
	moduletest.Run(t, func(t *testing.T) (module.Module, moduletest.Fake, module.Actor) {
		m, f := setup(t)
		return m, f, module.Actor{}
	})
}

func TestNeverSendsADenyReason(t *testing.T) {
	m, f := setup(t)
	f.Add(module.Request{ID: "7", Username: "ana", Email: "ana@example.org"})
	if err := m.Reject(t.Context(), module.Actor{}, "7"); err != nil {
		t.Fatal(err)
	}
	for _, b := range f.bodies {
		if strings.Contains(b, "deny_reason") {
			t.Errorf("sent %s", b)
		}
	}
}

// Lemmy commits the decision, then fails sending its e-mail and answers an error.
func TestDecidedDespiteMailError(t *testing.T) {
	m, f := setup(t)
	f.smtpBroken = true
	f.Add(module.Request{ID: "8", Username: "bia", Email: "bia@example.org"})
	if err := m.Approve(t.Context(), module.Actor{}, "8"); err != nil {
		t.Errorf("approve went through but reported %v", err)
	}
	if !f.Approved("8") {
		t.Error("not approved")
	}
}

func TestFieldsAndChecks(t *testing.T) {
	m, f := setup(t)
	when := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	f.Add(module.Request{ID: "9", Username: "cai", Email: "cai@example.org", Reason: "I run a forum", Created: when})
	f.apps["9"].verified = false
	r, pending, err := m.Get(t.Context(), module.Actor{}, "9")
	if err != nil || !pending {
		t.Fatal(pending, err)
	}
	if r.Username != "cai" || r.Email != "cai@example.org" || r.Reason != "I run a forum" || !r.Created.Equal(when) {
		t.Errorf("fields: %+v", r)
	}
	if len(r.Checks) != 1 || r.Checks[0].Name != "email_verified" || r.Checks[0].OK {
		t.Errorf("checks: %+v", r.Checks)
	}
}

func TestWrongTokenIsUnauthorized(t *testing.T) {
	_, srv := newFake(t)
	m, _ := New(module.Config{URL: srv.URL, Auth: "token", Token: "other", Client: srv.Client()})
	if _, err := m.Pending(t.Context(), module.Actor{}); !errors.Is(err, module.ErrUnauthorized) {
		t.Errorf("pending: %v", err)
	}
}

func TestCapsAndConfig(t *testing.T) {
	m, _ := New(module.Config{URL: "https://forum.example", Auth: "token", Token: "x"})
	if c := m.Caps(); c.SilentReject || !c.SendsWelcome || c.ModeratorAuth {
		t.Errorf("caps: %+v", c)
	}
	for _, c := range []module.Config{
		{URL: "http://forum.example", Auth: "token", Token: "x"},
		{URL: "https://forum.example", Auth: "moderator"},
		{URL: "https://forum.example", Auth: "token"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestTimestamps(t *testing.T) {
	for _, s := range []string{`"2026-11-03T12:00:00.123456Z"`, `"2026-11-03T12:00:00.123456"`, `"2026-11-03T12:00:00Z"`, `"2026-11-03T09:00:00-03:00"`} {
		var ts lemmyTS
		if err := json.Unmarshal([]byte(s), &ts); err != nil || time.Time(ts).IsZero() || time.Time(ts).UTC().Hour() != 12 {
			t.Errorf("%s -> %v %v", s, time.Time(ts), err)
		}
	}
}

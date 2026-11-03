package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/wire"
)

const secret = "0123456789abcdef0123456789abcdef-test"

type fakeRecipe struct {
	mu   sync.Mutex
	open map[string]bool
	ids  []string // ids that reached the recipe
}

func (f *fakeRecipe) seen(id string) { f.mu.Lock(); f.ids = append(f.ids, id); f.mu.Unlock() }
func (f *fakeRecipe) name() string   { return "fake" }
func (f *fakeRecipe) caps() wire.Caps {
	return wire.Caps{SilentReject: true}
}
func (f *fakeRecipe) pending(context.Context) ([]wire.Request, error) {
	return []wire.Request{{ID: "1", Username: "ana"}}, nil
}
func (f *fakeRecipe) get(_ context.Context, id string) (wire.Request, bool, error) {
	f.seen(id)
	f.mu.Lock()
	defer f.mu.Unlock()
	open, ok := f.open[id]
	if !ok {
		return wire.Request{}, false, errGone
	}
	return wire.Request{ID: id}, open, nil
}
func (f *fakeRecipe) decide(id string) error {
	f.seen(id)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open[id] {
		return errGone
	}
	f.open[id] = false
	return nil
}
func (f *fakeRecipe) approve(_ context.Context, id string) error { return f.decide(id) }
func (f *fakeRecipe) reject(_ context.Context, id string) error  { return f.decide(id) }

func do(t *testing.T, h http.Handler, method, path string, sign bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if sign {
		wire.Sign(r, []byte(secret), nil, time.Now())
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServer(t *testing.T) {
	f := &fakeRecipe{open: map[string]bool{"7": true}}
	h := (&server{secret: []byte(secret), recipe: f}).handler()

	if w := do(t, h, "GET", "/healthz", false); w.Code != 200 {
		t.Errorf("healthz: %d", w.Code)
	}
	for _, p := range []string{"/v1/caps", "/v1/pending", "/v1/requests/7"} {
		if w := do(t, h, "GET", p, false); w.Code != http.StatusUnauthorized {
			t.Errorf("unsigned %s: %d", p, w.Code)
		}
	}
	if w := do(t, h, "POST", "/v1/requests/7/approve", false); w.Code != http.StatusUnauthorized || !f.open["7"] {
		t.Errorf("unsigned approve: %d", w.Code)
	}
	w := do(t, h, "GET", "/v1/requests/7", true)
	var one wire.One
	json.Unmarshal(w.Body.Bytes(), &one)
	if w.Code != 200 || !one.Pending || one.Request.ID != "7" {
		t.Errorf("get: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/v1/requests/8", true); w.Code != http.StatusNotFound {
		t.Errorf("unknown: %d", w.Code)
	}
	if w := do(t, h, "POST", "/v1/requests/7/approve", true); w.Code != http.StatusNoContent {
		t.Errorf("approve: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/v1/requests/7/reject", true); w.Code != http.StatusConflict {
		t.Errorf("reject after approve: %d", w.Code)
	}
	if w := do(t, h, "POST", "/v1/requests/7/delete", true); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("unknown action: %d", w.Code)
	}
	before := len(f.ids)
	for _, id := range []string{"a.b", "1%3B2", "%2e%2e", strings.Repeat("9", 65)} {
		do(t, h, "GET", "/v1/requests/"+id, true)
		do(t, h, "POST", "/v1/requests/"+id+"/approve", true)
	}
	if len(f.ids) != before {
		t.Errorf("bad ids reached the recipe: %v", f.ids[before:])
	}
}

// A shell stands in for the app's interpreter.
func TestScript(t *testing.T) {
	body := `case "$1" in
pending) echo "framework noise"; echo '{"requests":[{"id":"5","username":"ana","email":"a@x","reason":"hi","created":"2026-11-03T12:00:00Z"}]}' ;;
get) [ "$2" = 5 ] && echo '{"request":{"id":"5"},"pending":true}' || echo '{"request":null}' ;;
approve) [ "$2" = 5 ] && echo '{"ok":true}' || echo '{"gone":true}' ;;
reject) echo '{"error":"boom"}'; exit 1 ;;
esac`
	t.Setenv("APROVA_AGENT_TMP", t.TempDir())
	p, err := writeScript("fake.sh", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	s := &script{recipeName: "fake", interp: []string{"sh"}, path: p, dir: filepath.Dir(p), timeout: 10 * time.Second}
	ctx := context.Background()
	list, err := s.pending(ctx)
	if err != nil || len(list) != 1 || list[0].Reason != "hi" || list[0].Created.IsZero() {
		t.Fatalf("pending: %+v %v", list, err)
	}
	if _, open, err := s.get(ctx, "5"); err != nil || !open {
		t.Errorf("get 5: %v %v", open, err)
	}
	if _, _, err := s.get(ctx, "6"); err != errGone {
		t.Errorf("get 6: %v", err)
	}
	if err := s.approve(ctx, "5"); err != nil {
		t.Errorf("approve 5: %v", err)
	}
	if err := s.approve(ctx, "6"); err != errGone {
		t.Errorf("approve 6: %v", err)
	}
	if err := s.reject(ctx, "5"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("reject: %v", err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm()&0o222 != 0 {
		t.Errorf("recipe file is writable: %v", fi.Mode())
	}
}

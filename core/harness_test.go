package core

// Test harness: a fake Mastodon sign-in instance (bolha), a second Mastodon
// with a service token (jornalismo), a fake GitLab, a mail catcher and a
// Mattermost catcher, all in process, with a real PostgreSQL.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/module"
	_ "gitlab.gutocarvalho.net/open/aprova/modules/all"
)

type person struct{ id, username, role string }

// Sign-in tokens: the code "x" exchanges to "token-x".
var people = map[string]*person{
	"token-guto":  {"1", "guto", "Fundador"},
	"token-sabia": {"2", "sabia", "Sábios"},
	"token-p1":    {"3", "p1", "Protetores"},
	"token-p2":    {"4", "p2", "Protetores"},
	"token-fig":   {"5", "fig", "Figurinhas"},
}

type acct struct {
	ID            string    `json:"id"`
	Username      string    `json:"username"`
	Email         string    `json:"email"`
	CreatedAt     time.Time `json:"created_at"`
	Approved      bool      `json:"approved"`
	InviteRequest string    `json:"invite_request"`
}

type instance struct {
	mu         sync.Mutex
	accounts   map[string]*acct
	approvedBy map[string]string // account id -> token that approved
	rejectedBy map[string]string
	revoked    []string
	serviceTok string // when set, the admin API only takes this token
	roles      map[string]string
	dms        []url.Values   // direct messages posted, with the token as "token"
	team       map[string]int // username -> role id, for the v2 admin accounts list
	lookups    int
	srv        *httptest.Server
}

func newInstance(t *testing.T, serviceToken string) *instance {
	in := &instance{accounts: map[string]*acct{}, approvedBy: map[string]string{}, rejectedBy: map[string]string{}, serviceTok: serviceToken, roles: map[string]string{}}
	mux := http.NewServeMux()
	bearer := func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":%q}`, "token-"+r.FormValue("code"))
	})
	mux.HandleFunc("POST /oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		in.revoked = append(in.revoked, r.FormValue("token"))
	})
	mux.HandleFunc("POST /api/v1/statuses", func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != "bot-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.ParseForm()
		f := r.PostForm
		f.Set("token", bearer(r))
		f.Set("key", r.Header.Get("Idempotency-Key"))
		in.dms = append(in.dms, f)
		fmt.Fprint(w, `{"id":"1"}`)
	})
	// Admin accounts by role, one per page, to exercise the pagination.
	mux.HandleFunc("GET /api/v2/admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		// The mux wrapper already holds in.mu.
		if bearer(r) != "lookup-token" || r.URL.Query().Get("origin") != "local" || r.URL.Query().Get("status") != "active" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		in.lookups++
		want := r.URL.Query()["role_ids[]"]
		var names []string
		for n, role := range in.team {
			if contains(want, fmt.Sprint(role)) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < len(names)-1 {
			q := r.URL.Query()
			q.Set("page", fmt.Sprint(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v2/admin/accounts?%s>; rel="next"`, in.srv.URL, q.Encode()))
		}
		out := []map[string]any{}
		if page < len(names) {
			out = append(out, map[string]any{"username": names[page], "role": map[string]any{"id": fmt.Sprint(in.team[names[page]])}})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /api/v1/accounts/verify_credentials", func(w http.ResponseWriter, r *http.Request) {
		p, ok := people[bearer(r)]
		if !ok || contains(in.revoked, bearer(r)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		role := p.role
		if v, ok := in.roles[p.id]; ok {
			role = v
		}
		fmt.Fprintf(w, `{"id":%q,"username":%q,"role":{"name":%q}}`, p.id, p.username, role)
	})
	admin := func(w http.ResponseWriter, r *http.Request) bool {
		tok := bearer(r)
		if in.serviceTok != "" {
			if tok != in.serviceTok {
				w.WriteHeader(http.StatusUnauthorized)
				return false
			}
			return true
		}
		p, ok := people[tok]
		if !ok || p.role == "Figurinhas" || contains(in.revoked, tok) {
			w.WriteHeader(http.StatusForbidden)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/v1/admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		list := []*acct{}
		for _, a := range in.accounts {
			if !a.Approved {
				list = append(list, a)
			}
		}
		json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("GET /api/v1/admin/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		a, ok := in.accounts[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(a)
	})
	mux.HandleFunc("POST /api/v1/admin/accounts/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		id := r.PathValue("id")
		a, ok := in.accounts[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if a.Approved {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.PathValue("action") == "approve" {
			a.Approved, in.approvedBy[id] = true, bearer(r)
		} else {
			delete(in.accounts, id)
			in.rejectedBy[id] = bearer(r)
		}
		fmt.Fprint(w, `{}`)
	})
	in.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in.mu.Lock()
		defer in.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(in.srv.Close)
	return in
}

func (in *instance) add(id, username, email, reason string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.accounts[id] = &acct{ID: id, Username: username, Email: email, InviteRequest: reason, CreatedAt: time.Now()}
}

func (in *instance) state(id string) (approvedBy, rejectedBy string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.approvedBy[id], in.rejectedBy[id]
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

type gitlabFake struct {
	mu       sync.Mutex
	users    map[string]map[string]any
	approved map[string]bool
	rejected map[string]bool
	srv      *httptest.Server
}

func newGitlab(t *testing.T) *gitlabFake {
	g := &gitlabFake{users: map[string]map[string]any{}, approved: map[string]bool{}, rejected: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/users", func(w http.ResponseWriter, r *http.Request) {
		list := []map[string]any{}
		for _, u := range g.users {
			list = append(list, u)
		}
		json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("GET /api/v4/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		u, ok := g.users[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(u)
	})
	mux.HandleFunc("DELETE /api/v4/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := g.users[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(g.users, id)
		g.rejected[id] = true
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v4/users/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		u, ok := g.users[id]
		if !ok || u["state"] != "blocked_pending_approval" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.PathValue("action") == "approve" {
			u["state"], g.approved[id] = "active", true
		} else {
			delete(g.users, id)
			g.rejected[id] = true
		}
		fmt.Fprint(w, `{}`)
	})
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.Header.Get("PRIVATE-TOKEN") != "gl-admin" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gitlabFake) add(id, username, email string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	fmt.Sscan(id, &n)
	g.users[id] = map[string]any{"id": n, "username": username, "email": email, "state": "blocked_pending_approval", "bio": "hello there", "created_at": time.Now()}
}

type mailCatcher struct {
	mu   sync.Mutex
	sent []Message
	fail bool
}

func (m *mailCatcher) Send(_ context.Context, msg Message) error {
	if err := msg.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return fmt.Errorf("smtp down")
	}
	m.sent = append(m.sent, msg)
	return nil
}

type hookCatcher struct {
	mu   sync.Mutex
	msgs []string
	srv  *httptest.Server
}

func newHook(t *testing.T) *hookCatcher {
	h := &hookCatcher{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Text string }
		json.NewDecoder(r.Body).Decode(&in)
		h.mu.Lock()
		h.msgs = append(h.msgs, in.Text)
		h.mu.Unlock()
	}))
	t.Cleanup(h.srv.Close)
	return h
}

type world struct {
	t       *testing.T
	bolha   *instance
	jorn    *instance
	gitlab  *gitlabFake
	mail    *mailCatcher
	hook    *hookCatcher
	srv     *Server
	app     *httptest.Server
	options Options
}

const testConfig = `
instance: %[1]s
base_url: https://aprova.example.org
lang: en
modules: [mastodon, gitlab]
services:
  bolha-us:   { module: mastodon, name: bolha.us, url: %[1]s, public_url: https://bolha.example, auth: moderator }
  jornalismo: { module: mastodon, name: jornalismo.social, url: %[2]s, public_url: https://jornalismo.example, auth: token, token_env: T_JORN }
  gitlab-gcn: { module: gitlab, name: gitlab, url: %[3]s, auth: token, token_env: T_GITLAB }
profiles:
  bolha:
    services: [bolha-us, jornalismo]
    approve: [Protetores, Sábios, Fundador]
    bulk_reject: [Sábios, Fundador]
    escalate_to: [Sábios, Fundador]
    review: { quorum: 2, required_checks: [bot_signal, disposable_email] }
    mail: { from: "aprova@bolha.example", reply_to: "cadastro@bolha.example", approve: true }
    mattermost_webhook_env: T_HOOK
    mastodon_dm: { token_env: T_BOT, recipients: ["@p1", "P2"], escalation_recipients: [sabia] }
  gcn:
    services: [gitlab-gcn]
    approve: [Sábios, Fundador]
    bulk_reject: [Sábios, Fundador]
    escalate_to: [Sábios, Fundador]
    mail: { from: "aprova@gcn.example" }
checks:
  disposable_domains: [tinyhost.shop]
`

func testEnv(extra map[string]string) func(string) string {
	base := map[string]string{
		"APROVA_CLIENT_ID": "cid", "APROVA_CLIENT_SECRET": "csecret", "APROVA_ENCRYPTION_KEY": strings.Repeat("k", 32),
		"APROVA_DB_HOST": os.Getenv("APROVA_TEST_DB_HOST"), "APROVA_DB_PORT": os.Getenv("APROVA_TEST_DB_PORT"),
		"APROVA_DB_PASSWORD": "aprova", "APROVA_DB_SSLMODE": "disable",
		"T_JORN": "svc-jorn", "T_GITLAB": "gl-admin", "T_BOT": "bot-token",
	}
	for k, v := range extra {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func newWorld(t *testing.T) *world { t.Helper(); return newWorldWith(t, nil, nil) }

// newWorldWith edits the test config and swaps the clock.
func newWorldWith(t *testing.T, edit func(string) string, now func() time.Time) *world {
	t.Helper()
	if os.Getenv("APROVA_TEST_DB_HOST") == "" {
		t.Skip("APROVA_TEST_DB_HOST not set")
	}
	w := &world{t: t, bolha: newInstance(t, ""), jorn: newInstance(t, "svc-jorn"), gitlab: newGitlab(t), mail: &mailCatcher{}, hook: newHook(t)}
	conf := testConfig
	if edit != nil {
		conf = edit(conf)
	}
	cfg, err := ParseConfig([]byte(fmt.Sprintf(conf, w.bolha.srv.URL, w.jorn.srv.URL, w.gitlab.srv.URL)), testEnv(map[string]string{"T_HOOK": w.hook.srv.URL, "T_LOOKUP": "lookup-token"}))
	if err != nil {
		t.Fatal(err)
	}
	w.options = Options{Mailer: w.mail, Now: now, MX: func(ctx context.Context, domain string) ([]*net.MX, error) {
		if domain == "hidden-disposable.example" {
			return []*net.MX{{Host: "mx.tinyhost.shop."}}, nil
		}
		return nil, nil
	}}
	srv, err := New(context.Background(), cfg, w.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	// Each test starts from an empty database.
	for _, table := range []string{"sessions", "requests", "reviews", "digests", "prefs"} {
		if _, err := srv.db.pool.Exec(context.Background(), "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	w.srv = srv
	w.app = httptest.NewServer(srv.Handler())
	t.Cleanup(w.app.Close)
	return w
}

// uniq gives ids that never repeat across tests: comments and the audit log only grow.
var seq atomic.Int64

func uniq() string { return fmt.Sprint(time.Now().Unix()%1e9*1e5 + seq.Add(1)) }

type browser struct {
	t      *testing.T
	w      *world
	cookie string
	csrf   string
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (w *world) login(code string) *browser {
	w.t.Helper()
	b := &browser{t: w.t, w: w}
	_, loc, _ := b.do(http.MethodGet, "/login", nil)
	u, _ := url.Parse(loc)
	if u.Query().Get("code_challenge_method") != "S256" {
		w.t.Fatalf("login: %s", loc)
	}
	status, _, _ := b.do(http.MethodGet, "/auth/callback?state="+url.QueryEscape(u.Query().Get("state"))+"&code="+code, nil)
	if status != http.StatusFound {
		return b // refused; the caller checks
	}
	_, _, page := b.do(http.MethodGet, "/queue", nil)
	if m := csrfField.FindStringSubmatch(page); m != nil {
		b.csrf = m[1]
	}
	return b
}

func (b *browser) do(method, path string, form url.Values) (int, string, string) {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, b.w.app.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if b.cookie != "" {
		req.Header.Set("Cookie", b.cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, c := range resp.Cookies() {
		if (c.Name == sessionCookie || c.Name == stateCookie) && c.Value != "" {
			b.cookie = c.Name + "=" + c.Value
		}
	}
	return resp.StatusCode, resp.Header.Get("Location"), string(raw)
}

func (b *browser) post(path string, form url.Values) (int, string) {
	b.t.Helper()
	form.Set("csrf", b.csrf)
	status, loc, _ := b.do(http.MethodPost, path, form)
	return status, loc
}

func (b *browser) review(svc, id, verdict, body string) string {
	b.t.Helper()
	_, loc := b.post("/r/"+svc+"/"+id+"/review", url.Values{"verdict": {verdict}, "body": {body}})
	return loc
}

func (w *world) decision(svc, id string) string {
	var d string
	w.srv.db.pool.QueryRow(context.Background(), `SELECT decision FROM requests WHERE service = $1 AND request_id = $2`, svc, id).Scan(&d)
	return d
}

func (w *world) escalated(svc, id string) bool {
	var e bool
	w.srv.db.pool.QueryRow(context.Background(), `SELECT escalated FROM requests WHERE service = $1 AND request_id = $2`, svc, id).Scan(&e)
	return e
}

var _ = module.ErrGone

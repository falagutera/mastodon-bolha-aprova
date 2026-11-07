package core

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/internal/relnotes"
	"gitlab.gutocarvalho.net/open/aprova/module"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

// Version is set by main.
var Version = "dev"

// Releases feed the release notes page; main sets them from CHANGELOG.md.
var Releases []relnotes.Release

type service struct {
	id      string
	cfg     *ServiceConfig
	mod     module.Module
	profile *Profile
}

type Server struct {
	cfg      *Config
	db       *store
	tmpl     *template.Template
	client   *http.Client
	services map[string]*service
	order    []string // service ids, sorted
	tokenBox *box
	stateBox *box
	checks   *checker
	mailer   Mailer
	inbox    Inbox
	bounce   Inbox // the sender mailbox, read for delivery-status notifications
	mm       *mattermost
}

// Options lets tests swap what reaches the outside.
type Options struct {
	Client *http.Client
	Mailer Mailer
	// Inbox replaces the IMAP mailbox, for tests.
	Inbox Inbox
	// Bounce replaces the sender mailbox read for bounces, for tests.
	Bounce Inbox
	MX     func(ctx context.Context, domain string) ([]*net.MX, error)
}

// New builds the server: it opens the database and every enabled service.
func New(ctx context.Context, cfg *Config, opt Options) (*Server, error) {
	client := opt.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	client = routed(client, cfg.routes)
	tokenBox, err := newBox(cfg.EncryptionKey, "session-token")
	if err != nil {
		return nil, err
	}
	stateBox, _ := newBox(cfg.EncryptionKey, "login-state")
	db, err := openStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	tmpl, err := parseTemplates(cfg.loc)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, db: db, tmpl: tmpl, client: client, services: map[string]*service{}, tokenBox: tokenBox, stateBox: stateBox,
		checks: newChecker(cfg.Checks), mailer: opt.Mailer, mm: &mattermost{client: client}}
	if opt.MX != nil {
		s.checks.lookupMX = opt.MX
	}
	s.inbox = opt.Inbox
	if s.inbox == nil && cfg.IMAPHost != "" {
		s.inbox = &imapInbox{host: cfg.IMAPHost, port: cfg.IMAPPort, user: cfg.IMAPUser, pass: cfg.IMAPPass}
	}
	// Bounces return to the sender account (SMTP user), a different mailbox from
	// the reply-to. Read it over IMAPS with the SMTP credentials (one account on
	// the mail host). Skip when it is the same mailbox as the replies: readReplies
	// would mark a bounce seen before readBounces could see it.
	s.bounce = opt.Bounce
	if s.bounce == nil && cfg.IMAPHost != "" && cfg.SMTPUser != "" && cfg.SMTPUser != cfg.IMAPUser {
		s.bounce = &imapInbox{host: cfg.IMAPHost, port: cfg.IMAPPort, user: cfg.SMTPUser, pass: cfg.SMTPPass}
	}
	if s.mailer == nil && cfg.SMTPEnabled() {
		s.mailer = &smtpMailer{host: cfg.SMTPHost, port: cfg.SMTPPort, user: cfg.SMTPUser, pass: cfg.SMTPPass, mode: cfg.SMTPTLS}
	}
	for id, sc := range cfg.Services {
		f, _ := module.Lookup(sc.Module)
		m, err := f(module.Config{URL: sc.URL, Auth: sc.Auth, Token: sc.token, Client: client})
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", id, err)
		}
		if sc.Auth == "moderator" && !m.Caps().ModeratorAuth {
			return nil, fmt.Errorf("service %s: module %s can't use auth moderator", id, sc.Module)
		}
		s.services[id] = &service{id: id, cfg: sc, mod: m, profile: cfg.Profiles[cfg.serviceProfile[id]]}
		s.order = append(s.order, id)
	}
	sort.Strings(s.order)
	return s, nil
}

// Close releases the database.
func (s *Server) Close() { s.db.pool.Close() }

// Run starts the background work: session sweep and digests.
func (s *Server) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweep(ctx)
				s.readReplies(ctx)
				s.readBounces(ctx)
				s.welcomeAll(ctx)
			}
		}
	}()
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Digest(ctx)
			}
		}
	}()
}

func parseTemplates(loc *time.Location) (*template.Template, error) {
	if loc == nil {
		loc = time.UTC
	}
	return template.New("").Funcs(template.FuncMap{
		"when": func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04") },
		"zone": func() string { return loc.String() },
		"failed": func(cs []module.Check) []module.Check {
			return slices.DeleteFunc(slices.Clone(cs), func(c module.Check) bool { return c.OK })
		},
		"passed": func(cs []module.Check) []module.Check {
			return slices.DeleteFunc(slices.Clone(cs), func(c module.Check) bool { return !c.OK })
		},
		"join": func(xs []string) string { return strings.Join(xs, ", ") },
		"tf":   func(m map[string]string, key string, args ...any) string { return fmt.Sprintf(m[key], args...) },
		"t": func(m map[string]string, key string) string {
			if v, ok := m[key]; ok {
				return v
			}
			return key
		},
	}).ParseFS(templateFS, "templates/*.html")
}

func (s *Server) visibleProfiles(role string) []*Profile {
	var out []*Profile
	for _, id := range sortedKeys(s.cfg.Profiles) {
		if p := s.cfg.Profiles[id]; p.canView(role) {
			out = append(out, p)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func actorFor(sess session, svc *service) module.Actor {
	a := module.Actor{AccountID: sess.AccountID, Acct: sess.Acct}
	if svc.cfg.Auth == "moderator" {
		a.Token = sess.Token
	}
	return a
}

// ---- handlers plumbing

type authed func(http.ResponseWriter, *http.Request, session)

func sameSite(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

func (s *Server) page(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok, failed := s.current(w, r)
		if failed {
			return
		}
		if !ok {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		next(w, r, sess)
	}
}

func (s *Server) action(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameSite(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		sess, ok, failed := s.current(w, r)
		if failed {
			return
		}
		if !ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil || subtle.ConstantTimeCompare([]byte(sess.CSRF), []byte(r.PostForm.Get("csrf"))) != 1 {
			s.renderError(w, r, http.StatusForbidden, "expired")
			return
		}
		next(w, r, sess)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.pool.Ping(ctx); err != nil {
			http.Error(w, "database", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
	})
	assets, _ := fs.Sub(assetFS, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("POST /logout", s.action(s.handleLogout))
	mux.HandleFunc("POST /api-token", s.action(s.handleAPIToken))
	mux.HandleFunc("POST /prefs", s.action(s.handlePrefs))
	mux.HandleFunc("GET /queue", s.page(s.handleQueue))
	mux.HandleFunc("GET /audit", s.page(s.handleAudit))
	mux.HandleFunc("GET /release-notes", s.page(s.handleNotes))
	mux.HandleFunc("GET /how-it-works", s.page(s.handleRules))
	mux.HandleFunc("GET /release-notes.json", func(w http.ResponseWriter, r *http.Request) {
		relnotes.JSONHandler("aprova", Version, Releases).ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /r/{service}/{id}", s.page(s.handleRequest))
	for _, a := range actions {
		mux.HandleFunc("POST /r/{service}/{id}/"+a, s.action(s.handleAct(a)))
	}
	mux.HandleFunc("POST /r/{service}/{id}/edit", s.action(s.handleEdit))
	mux.HandleFunc("POST /bulk-reject", s.action(s.handleBulkReject))
	s.apiRoutes(mux)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; font-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---- views

type viewData struct {
	T        map[string]string
	Lang     string
	Version  string
	Domain   string
	Acct     string
	Role     string
	CSRF     string
	Flash    string
	Error    string
	Nav      string
	Modules  []string
	TeamChat string
	Releases template.HTML
	Rules    []ruleView
	Message  string
	Profiles []profileView
	Filter   string
	Req      requestView
	Audit    []auditEntry
	Mail     bool
	Filters  []string
	DM       bool // this moderator is a recipient of the bot's messages
	DMOff    bool
}

var flashes = []string{"prefs", "approved", "rejected", "reviewed", "escalated", "deescalated", "answered", "commented", "asked", "gone", "already", "bulk", "mail_failed", "no_mail", "edited"}

func (s *Server) view(r *http.Request, sess session, nav string) viewData {
	lang := language(r, s.cfg.Lang)
	v := viewData{T: strs(lang), Lang: lang, Version: Version, Domain: hostOf(s.cfg.BaseURL), Acct: sess.Acct, Role: sess.Role,
		CSRF: sess.CSRF, Nav: nav, Modules: s.cfg.Enabled(), TeamChat: s.cfg.TeamChat, Mail: s.mailer != nil, Filters: filters}
	if k := r.URL.Query().Get("done"); slices.Contains(flashes, k) {
		v.Flash = v.T["flash_"+k]
	}
	if k := r.URL.Query().Get("error"); k != "" {
		v.Error = v.T["form_"+k]
	}
	return v
}

func hostOf(u string) string {
	return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
}

func (s *Server) render(w http.ResponseWriter, name string, status int, v viewData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, v); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, key string) {
	v := s.view(r, session{}, "")
	v.Message = v.T["error_"+key]
	s.render(w, "error.html", status, v)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if q := r.URL.Query().Get("lang"); q != "" {
		setLanguage(w, q)
	}
	_, ok, failed := s.current(w, r)
	if failed {
		return
	}
	if ok {
		http.Redirect(w, r, "/queue", http.StatusFound)
		return
	}
	s.render(w, "index.html", http.StatusOK, s.view(r, session{}, ""))
}

// ---- queue

type itemView struct {
	Service       string   `json:"service"`
	ID            string   `json:"id"`
	Username      string   `json:"username"`
	Email         string   `json:"email"`
	Reason        string   `json:"reason"`
	Created       string   `json:"created"`
	Status        string   `json:"status"` // new, reviewing, ready, blocked, awaiting, escalated
	Approvals     int      `json:"approvals"`
	Rejects       int      `json:"rejects"`
	Needed        int      `json:"needed"`
	Stale         bool     `json:"stale"`
	Flags         []string `json:"flags"` // failed checks
	Undeliverable bool     `json:"undeliverable"`
	CanBulk       bool     `json:"can_bulk"`
}

type serviceView struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Color   int        `json:"-"` // palette slot, stable per service
	Error   string     `json:"error"`
	Items   []itemView `json:"items"`
	CanBulk bool       `json:"can_bulk"`
}

type profileView struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Services []serviceView  `json:"services"`
	Counts   map[string]int `json:"counts"`
}

// paletteSize matches the .svc-N classes in aprova.css.
const paletteSize = 6

// color gives each service a slot by its place in the configuration, so it keeps
// its color on every page.
func (s *Server) color(id string) int {
	if i := slices.Index(s.order, id); i >= 0 {
		return i % paletteSize
	}
	return 0
}

var filters = []string{"new", "reviewing", "awaiting", "escalated"}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request, sess session) {
	v := s.view(r, sess, "queue")
	v.Filter = r.URL.Query().Get("f")
	if !slices.Contains(filters, v.Filter) {
		v.Filter = ""
	}
	date := v.T["date_format"]
	for _, p := range s.visibleProfiles(sess.Role) {
		pv := profileView{ID: p.id, Name: p.Name, Counts: map[string]int{}}
		var wg sync.WaitGroup
		svcs := make([]serviceView, len(p.Services))
		for i, sid := range p.Services {
			wg.Add(1)
			go func(i int, svc *service) {
				defer wg.Done()
				svcs[i] = s.serviceQueue(r.Context(), sess, svc, date)
			}(i, s.services[sid])
		}
		wg.Wait()
		for i := range svcs {
			all := svcs[i].Items
			svcs[i].Items = nil
			for _, it := range all {
				pv.Counts[it.Status]++
				if v.Filter == "" || it.Status == v.Filter || v.Filter == "reviewing" && (it.Status == "ready" || it.Status == "blocked") {
					svcs[i].Items = append(svcs[i].Items, it)
				}
			}
			svcs[i].Color = s.color(svcs[i].ID)
			pv.Services = append(pv.Services, svcs[i])
		}
		v.Profiles = append(v.Profiles, pv)
	}
	acct := strings.ToLower(sess.Acct)
	for _, p := range s.visibleProfiles(sess.Role) {
		if d := p.MastodonDM; d != nil && (slices.Contains(d.Recipients, acct) || slices.Contains(d.EscalationRecipients, acct)) {
			v.DM = true
		}
	}
	if v.DM {
		off, err := s.db.dmOff(r.Context(), []string{acct})
		if err != nil {
			log.Printf("prefs: %v", err)
		}
		v.DMOff = off[acct]
	}
	s.render(w, "queue.html", http.StatusOK, v)
}

// handlePrefs turns the bot's direct messages off or on for the signed-in moderator.
func (s *Server) handlePrefs(w http.ResponseWriter, r *http.Request, sess session) {
	if err := s.db.setDMOff(r.Context(), sess.Acct, r.PostForm.Get("dm") == "off"); err != nil {
		log.Printf("prefs: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	http.Redirect(w, r, "/queue?done=prefs", http.StatusSeeOther)
}

func (s *Server) serviceQueue(ctx context.Context, sess session, svc *service, date string) serviceView {
	sv := serviceView{ID: svc.id, Name: svc.cfg.Name, CanBulk: svc.profile.canBulkReject(sess.Role)}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	reqs, err := svc.mod.Pending(ctx, actorFor(sess, svc))
	if err != nil {
		log.Printf("%s pending: %v", svc.id, err)
		sv.Error = "unavailable"
		if errors.Is(err, module.ErrUnauthorized) {
			sv.Error = "unauthorized"
		}
		return sv
	}
	// auth: moderator services without a digest token can only be listed with a
	// moderator's credentials, so new requests there are greeted from here.
	if svc.profile.Mail.Welcome && svc.cfg.digestToken == "" && welcomeRun.TryLock() {
		go func() {
			defer welcomeRun.Unlock()
			s.welcome(context.WithoutCancel(ctx), svc, reqs)
		}()
	}
	ids := make([]string, len(reqs))
	for i, q := range reqs {
		ids[i] = q.ID
	}
	states, err := s.db.states(ctx, svc.id, ids)
	if err != nil {
		log.Printf("%s states: %v", svc.id, err)
		sv.Error = "unavailable"
		return sv
	}
	rounds := map[string]int{}
	for _, id := range ids {
		rounds[id] = 1
		if st, ok := states[id]; ok {
			rounds[id] = st.Round
		}
	}
	counts, err := s.db.reviewCounts(ctx, svc.id, rounds)
	if err != nil {
		log.Printf("%s reviews: %v", svc.id, err)
		sv.Error = "unavailable"
		return sv
	}
	stale := time.Duration(s.cfg.Checks.InfoStaleDays) * 24 * time.Hour
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].Created.Before(reqs[j].Created) })
	for _, q := range reqs {
		st := states[q.ID]
		c := counts[q.ID]
		it := itemView{Service: svc.id, ID: q.ID, Username: q.Username, Email: q.Email, Reason: q.Reason, Created: q.Created.In(s.cfg.loc).Format(date),
			Approvals: c[0], Rejects: c[1], Needed: svc.profile.Review.Quorum}
		it.CanBulk = sv.CanBulk && svc.profile.canDecide(sess.Role, st.Escalated)
		it.Undeliverable = st.Undeliverable
		for _, chk := range s.checks.run(ctx, q, svc.mod.Caps().NoReason) {
			if !chk.OK {
				it.Flags = append(it.Flags, chk.Name)
			}
		}
		switch {
		case st.Escalated:
			it.Status = "escalated"
		case st.AwaitingSince != nil:
			it.Status = "awaiting"
			it.Stale = time.Since(*st.AwaitingSince) > stale
		case c[1] > 0:
			it.Status = "blocked"
		case c[0] >= it.Needed:
			it.Status = "ready"
		case c[0] > 0:
			it.Status = "reviewing"
		default:
			it.Status = "new"
		}
		sv.Items = append(sv.Items, it)
	}
	return sv
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, sess session) {
	var ids []string
	for _, p := range s.visibleProfiles(sess.Role) {
		ids = append(ids, p.id)
	}
	v := s.view(r, sess, "audit")
	list, err := s.db.auditLog(r.Context(), ids, 300)
	if err != nil {
		log.Printf("audit: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	v.Audit = list
	s.render(w, "audit.html", http.StatusOK, v)
}

func (s *Server) handleNotes(w http.ResponseWriter, r *http.Request, sess session) {
	v := s.view(r, sess, "notes")
	v.Releases = relnotes.HTML(Releases)
	s.render(w, "notes.html", http.StatusOK, v)
}

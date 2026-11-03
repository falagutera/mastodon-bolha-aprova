package core

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	sessionCookie = "__Host-aprova"
	stateCookie   = "__Host-aprova-state"
	stateTTL      = 10 * time.Minute
	sessionTTL    = 8 * time.Hour
	roleRecheck   = time.Minute
)

func (s *Server) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	s.setCookie(w, name, "", -time.Second)
}

func (s *Server) scope() string {
	if s.cfg.moderatorScopes() {
		// The moderator's own token approves on the sign-in instance.
		return "profile admin:read:accounts admin:write:accounts"
	}
	return "profile"
}

type loginState struct {
	State    string    `json:"s"`
	Verifier string    `json:"v"`
	Expires  time.Time `json:"e"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	st := loginState{State: randomToken(), Verifier: randomToken(), Expires: time.Now().Add(stateTTL)}
	raw, _ := json.Marshal(st)
	s.setCookie(w, stateCookie, base64.RawURLEncoding.EncodeToString(s.stateBox.seal(raw, []byte("state"))), stateTTL)
	challenge := sha256.Sum256([]byte(st.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {s.cfg.ClientID},
		"redirect_uri":          {s.cfg.BaseURL + "/auth/callback"},
		"scope":                 {s.scope()},
		"state":                 {st.State},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, s.cfg.Instance+"/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "expired")
		return
	}
	s.clearCookie(w, stateCookie)
	var st loginState
	sealed, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err == nil {
		var plain []byte
		if plain, err = s.stateBox.open(sealed, []byte("state")); err == nil {
			err = json.Unmarshal(plain, &st)
		}
	}
	q := r.URL.Query()
	if err != nil || time.Now().After(st.Expires) || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		s.renderError(w, r, http.StatusBadRequest, "expired")
		return
	}
	if q.Get("code") == "" {
		s.renderError(w, r, http.StatusForbidden, "denied")
		return
	}
	token, err := s.exchange(r.Context(), q.Get("code"), st.Verifier)
	if err != nil {
		log.Printf("token exchange: %v", err)
		s.renderError(w, r, http.StatusBadGateway, "instance")
		return
	}
	who, err := s.whoami(r.Context(), token)
	if err != nil {
		log.Printf("verify_credentials: %v", err)
		s.revokeLater(token)
		s.renderError(w, r, http.StatusBadGateway, "instance")
		return
	}
	if len(s.visibleProfiles(who.Role)) == 0 {
		s.revokeLater(token)
		s.renderError(w, r, http.StatusForbidden, "role")
		return
	}
	id := randomToken()
	sess := session{IDHash: hashID(id), AccountID: who.ID, Acct: who.Username, Role: who.Role, RoleChecked: time.Now(),
		Token: token, TokenHash: hashID(token), CSRF: randomToken(), Expires: time.Now().Add(sessionTTL)}
	if err := s.db.createSession(r.Context(), s.tokenBox, sess); err != nil {
		log.Printf("session: %v", err)
		s.revokeLater(token)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	s.setCookie(w, sessionCookie, id, sessionTTL)
	http.Redirect(w, r, "/queue", http.StatusFound)
}

func (s *Server) exchange(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {s.cfg.ClientID}, "client_secret": {s.cfg.ClientSecret},
		"redirect_uri": {s.cfg.BaseURL + "/auth/callback"}, "code_verifier": {verifier}, "scope": {s.scope()},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Instance+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("empty access token")
	}
	return out.AccessToken, nil
}

type whoami struct {
	ID       string
	Username string
	Role     string
}

var errRevoked = errors.New("token revoked")

func (s *Server) whoami(ctx context.Context, token string) (whoami, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.Instance+"/api/v1/accounts/verify_credentials", nil)
	if err != nil {
		return whoami{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		return whoami{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return whoami{}, errRevoked
	}
	if resp.StatusCode != http.StatusOK {
		return whoami{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var out struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     *struct {
			Name string `json:"name"`
		} `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return whoami{}, err
	}
	if out.ID == "" || out.Username == "" {
		return whoami{}, errors.New("empty account")
	}
	w := whoami{ID: out.ID, Username: out.Username}
	if out.Role != nil {
		w.Role = out.Role.Name
	}
	return w, nil
}

func (s *Server) revokeToken(token string) error {
	form := url.Values{"client_id": {s.cfg.ClientID}, "client_secret": {s.cfg.ClientSecret}, "token": {token}}
	resp, err := s.client.PostForm(s.cfg.Instance+"/oauth/revoke", form)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoke: status %d", resp.StatusCode)
	}
	return nil
}

// revokeLater is for a token that never became a session.
func (s *Server) revokeLater(token string) {
	if err := s.revokeToken(token); err != nil {
		log.Printf("revoke: %v", err)
	}
}

// current loads the session and re-reads the role from the instance when it is
// older than a minute: losing a role there takes effect here right away.
func (s *Server) current(w http.ResponseWriter, r *http.Request) (session, bool, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return session{}, false, false
	}
	sess, err := s.db.session(r.Context(), s.tokenBox, hashID(c.Value))
	if errors.Is(err, errNoSession) {
		s.clearCookie(w, sessionCookie)
		return session{}, false, false
	}
	if err != nil {
		log.Printf("session: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return session{}, false, true
	}
	sess, alive, err := s.recheck(r.Context(), sess)
	if err != nil {
		log.Printf("role check: %v", err)
		s.renderError(w, r, http.StatusBadGateway, "instance")
		return session{}, false, true
	}
	if !alive {
		s.clearCookie(w, sessionCookie)
		return session{}, false, false
	}
	return sess, true, false
}

// recheck re-reads the role from the instance when it is older than a minute.
// alive is false, and the session ends, when the token or the role is gone.
func (s *Server) recheck(ctx context.Context, sess session) (session, bool, error) {
	if time.Since(sess.RoleChecked) <= roleRecheck {
		return sess, true, nil
	}
	who, err := s.whoami(ctx, sess.Token)
	switch {
	case errors.Is(err, errRevoked) || err == nil && (who.ID != sess.AccountID || len(s.visibleProfiles(who.Role)) == 0):
		s.db.expireSession(ctx, sess.IDHash)
		return session{}, false, nil
	case err != nil:
		return session{}, false, err
	}
	sess.Role = who.Role
	if err := s.db.updateRole(ctx, sess.IDHash, sess.Role); err != nil {
		log.Printf("session: %v", err)
	}
	return sess, true, nil
}

// handleAPIToken makes a token for the API: a new session of the same account,
// with the same lifetime and checks. The instance's token never leaves aprova.
func (s *Server) handleAPIToken(w http.ResponseWriter, r *http.Request, sess session) {
	id := randomToken()
	tok := session{IDHash: hashID(id), AccountID: sess.AccountID, Acct: sess.Acct, Role: sess.Role, RoleChecked: sess.RoleChecked,
		Token: sess.Token, TokenHash: sess.TokenHash, CSRF: randomToken(), Expires: time.Now().Add(sessionTTL)}
	if err := s.db.createSession(r.Context(), s.tokenBox, tok); err != nil {
		log.Printf("api token: %v", err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	v := s.view(r, sess, "token")
	v.Message = apiTokenPrefix + id
	s.render(w, "token.html", http.StatusOK, v)
}

// API tokens carry a prefix, so a Mastodon token sent by mistake is told apart.
const apiTokenPrefix = "aprova_"

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, sess session) {
	if err := s.db.expireSession(r.Context(), sess.IDHash); err != nil {
		log.Printf("logout: %v", err)
	}
	s.sweep(r.Context())
	s.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) sweep(ctx context.Context) {
	if err := s.db.finishSessions(ctx, s.tokenBox, s.revokeToken); err != nil {
		log.Printf("sweep: %v", err)
	}
	// Questions and handled messages are only needed while answers may come.
	if _, err := s.db.pool.Exec(ctx, `DELETE FROM asked WHERE at < now() - interval '180 days'`); err != nil {
		log.Printf("sweep asked: %v", err)
	}
	if _, err := s.db.pool.Exec(ctx, `DELETE FROM inbound WHERE at < now() - interval '180 days'`); err != nil {
		log.Printf("sweep inbound: %v", err)
	}
}

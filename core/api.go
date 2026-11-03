package core

// The JSON API. The pages and the API share every rule: both end in act and
// buildRequest. API clients authenticate with an aprova token made on the
// "API token" page (Authorization: Bearer aprova_...). It is a session of the
// moderator, with the same lifetime and role checks; the instance's token never
// leaves aprova.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) (session, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, isOurs := strings.CutPrefix(token, apiTokenPrefix)
	if !ok || !isOurs || id == "" || len(id) > 128 {
		// Only aprova's own tokens: a Mastodon token from any other app must not work here.
		apiError(w, http.StatusUnauthorized, "missing_token")
		return session{}, false
	}
	sess, err := s.db.session(r.Context(), s.tokenBox, hashID(id))
	if errors.Is(err, errNoSession) {
		apiError(w, http.StatusUnauthorized, "bad_token")
		return session{}, false
	}
	if err != nil {
		log.Printf("api session: %v", err)
		apiError(w, http.StatusServiceUnavailable, "unavailable")
		return session{}, false
	}
	sess, alive, err := s.recheck(r.Context(), sess)
	if err != nil {
		log.Printf("api role check: %v", err)
		apiError(w, http.StatusBadGateway, "instance")
		return session{}, false
	}
	if !alive {
		apiError(w, http.StatusUnauthorized, "bad_token")
		return session{}, false
	}
	return sess, true
}

func apiError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) api(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Cookies never authenticate the API, so a browser can't be made to call it.
		if _, err := r.Cookie(sessionCookie); err == nil && r.Header.Get("Authorization") == "" {
			apiError(w, http.StatusUnauthorized, "missing_token")
			return
		}
		sess, ok := s.apiSession(w, r)
		if !ok {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		next(w, r, sess)
	}
}

func (s *Server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/me", s.api(func(w http.ResponseWriter, r *http.Request, sess session) {
		var ps []map[string]string
		for _, p := range s.visibleProfiles(sess.Role) {
			ps = append(ps, map[string]string{"id": p.id, "name": p.Name})
		}
		writeJSON(w, http.StatusOK, map[string]any{"acct": sess.Acct, "role": sess.Role, "profiles": ps, "modules": s.cfg.Enabled()})
	}))
	mux.HandleFunc("GET /api/v1/queue", s.api(func(w http.ResponseWriter, r *http.Request, sess session) {
		var out []profileView
		for _, p := range s.visibleProfiles(sess.Role) {
			pv := profileView{ID: p.id, Name: p.Name, Counts: map[string]int{}}
			for _, sid := range p.Services {
				sv := s.serviceQueue(r.Context(), sess, s.services[sid], time.RFC3339)
				for _, it := range sv.Items {
					pv.Counts[it.Status]++
				}
				pv.Services = append(pv.Services, sv)
			}
			out = append(out, pv)
		}
		writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
	}))
	// search finds pending requests by e-mail, for the help desk's link to aprova.
	mux.HandleFunc("GET /api/v1/search", s.api(func(w http.ResponseWriter, r *http.Request, sess session) {
		email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
		if email == "" || !strings.Contains(email, "@") || len(email) > 320 {
			apiError(w, http.StatusBadRequest, "email")
			return
		}
		var found []itemView
		for _, p := range s.visibleProfiles(sess.Role) {
			for _, sid := range p.Services {
				for _, it := range s.serviceQueue(r.Context(), sess, s.services[sid], time.RFC3339).Items {
					if strings.EqualFold(it.Email, email) {
						found = append(found, it)
					}
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"requests": found})
	}))
	mux.HandleFunc("GET /api/v1/requests/{service}/{id}", s.api(func(w http.ResponseWriter, r *http.Request, sess session) {
		svc, ok := s.services[r.PathValue("service")]
		if !ok || !svc.profile.canView(sess.Role) {
			apiError(w, http.StatusNotFound, "not_found")
			return
		}
		rv, err := s.buildRequest(r.Context(), sess, svc, r.PathValue("id"), time.RFC3339)
		if s.apiFailed(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, rv)
	}))
	mux.HandleFunc("POST /api/v1/requests/{service}/{id}/{action}", s.api(func(w http.ResponseWriter, r *http.Request, sess session) {
		svc, ok := s.services[r.PathValue("service")]
		if !ok || !svc.profile.canView(sess.Role) {
			apiError(w, http.StatusNotFound, "not_found")
			return
		}
		action := r.PathValue("action")
		if !slicesContains(actions, action) {
			apiError(w, http.StatusNotFound, "not_found")
			return
		}
		var in struct {
			Verdict string `json:"verdict"`
			Body    string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			apiError(w, http.StatusBadRequest, "bad_json")
			return
		}
		in.Body = strings.TrimSpace(in.Body)
		if utf8.RuneCountInString(in.Body) > maxText || !utf8.ValidString(in.Body) {
			apiError(w, http.StatusBadRequest, "too_long")
			return
		}
		out, err := s.act(r.Context(), sess, svc, r.PathValue("id"), action, in.Body, in.Verdict)
		if s.apiFailed(w, err) {
			return
		}
		if out.formError != "" {
			apiError(w, http.StatusUnprocessableEntity, out.formError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"result": out.done})
	}))
	mux.HandleFunc("GET /api/v1/audit", s.api(func(w http.ResponseWriter, r *http.Request, sess session) {
		var ids []string
		for _, p := range s.visibleProfiles(sess.Role) {
			ids = append(ids, p.id)
		}
		list, err := s.db.auditLog(r.Context(), ids, 300)
		if s.apiFailed(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": list})
	}))
}

var actions = []string{"review", "approve", "reject", "escalate", "deescalate", "answer", "comment"}

func slicesContains(list []string, v string) bool {
	for _, x := range list {
		if subtle.ConstantTimeCompare([]byte(x), []byte(v)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) apiFailed(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errNotFound):
		apiError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, errForbidden):
		apiError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, module.ErrUnauthorized):
		apiError(w, http.StatusForbidden, "unauthorized")
	default:
		log.Printf("api: %v", err)
		apiError(w, http.StatusServiceUnavailable, "unavailable")
	}
	return true
}

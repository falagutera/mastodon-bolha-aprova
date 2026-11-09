// aprova-agent runs next to an app that has no approval API and answers aprova's
// agent module (see package wire). It never takes code or commands from the
// network: each recipe is a fixed program, and the only thing that reaches it
// is a validated request id.
//
//	APROVA_AGENT_RECIPE   pixelfed (see recipes/)
//	APROVA_AGENT_SECRET   shared with aprova, 32+ characters
//	APROVA_AGENT_LISTEN   default :8090
//
// The image is distroless, so to run inside the app's own image an init
// container copies the binary out: aprova-agent -install /agent/aprova-agent.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gitlab.gutocarvalho.net/open/bolha/aprova/internal/healthcheck"
	"gitlab.gutocarvalho.net/open/bolha/aprova/wire"
)

var build = "dev"

func main() {
	install := flag.String("install", "", "copy this binary to the given path and exit")
	probe := flag.Bool("healthcheck", false, "check the local agent and exit")
	flag.Parse()
	if *install != "" {
		if err := copySelf(*install); err != nil {
			log.Fatal(err)
		}
		return
	}
	listen := os.Getenv("APROVA_AGENT_LISTEN")
	if listen == "" {
		listen = ":8090"
	}
	if *probe {
		if err := healthcheck.Probe(listen); err != nil {
			log.Fatal(err)
		}
		return
	}
	secret := os.Getenv("APROVA_AGENT_SECRET")
	if len(secret) < wire.MinSecret {
		log.Fatalf("APROVA_AGENT_SECRET needs at least %d characters", wire.MinSecret)
	}
	rec, err := newRecipe(os.Getenv("APROVA_AGENT_RECIPE"))
	if err != nil {
		log.Fatal(err)
	}
	a := &server{secret: []byte(secret), recipe: rec}
	hs := &http.Server{Addr: listen, Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 120 * time.Second}
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
		<-ch
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		hs.Shutdown(ctx)
	}()
	log.Printf("aprova-agent %s, recipe %s, listening on %s", build, rec.name(), listen)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func copySelf(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(self) // #nosec G304 -- our own executable, as os.Executable names it
	if err != nil {
		return err
	}
	defer in.Close()
	// dst is the operator's -install path. The app's container runs the copy
	// under the app's user, not the one that copied it, so it must be
	// executable by others; it is never writable.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o555) // #nosec G302 G304 -- operator-chosen path; read+exec for the app's user, no write
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// errGone means the request is no longer pending, or never was.
var errGone = errors.New("no longer pending")

// recipe is the app-specific part. Ids are validated before they reach it.
type recipe interface {
	name() string
	caps() wire.Caps
	pending(ctx context.Context) ([]wire.Request, error)
	get(ctx context.Context, id string) (wire.Request, bool, error)
	approve(ctx context.Context, id string) error
	reject(ctx context.Context, id string) error
}

func newRecipe(name string) (recipe, error) {
	switch name {
	case "pixelfed":
		return newPixelfed()
	case "bookwyrm":
		return newBookwyrm()
	case "friendica":
		return newFriendica()
	case "":
		return nil, errors.New("APROVA_AGENT_RECIPE is empty (known: pixelfed, bookwyrm, friendica)")
	default:
		return nil, fmt.Errorf("unknown recipe %q (known: pixelfed, bookwyrm, friendica)", name)
	}
}

type server struct {
	secret []byte
	recipe recipe
	// One decision at a time, so two moderators can't both act on a request
	// between its check and its change.
	act sync.Mutex
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, errGone) {
		writeJSON(w, http.StatusConflict, wire.Error{Error: "no longer pending"})
		return
	}
	// The detail stays in the log; it may hold the app's internals.
	log.Printf("recipe: %v", err)
	writeJSON(w, http.StatusBadGateway, wire.Error{Error: "the app's recipe failed, see the agent's log"})
}

func (a *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /v1/caps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.recipe.caps())
	})
	mux.HandleFunc("GET /v1/pending", func(w http.ResponseWriter, r *http.Request) {
		list, err := a.recipe.pending(r.Context())
		if err != nil {
			a.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, wire.Pending{Requests: list})
	})
	mux.HandleFunc("GET /v1/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !wire.ID.MatchString(id) {
			writeJSON(w, http.StatusNotFound, wire.Error{Error: "unknown"})
			return
		}
		req, pending, err := a.recipe.get(r.Context(), id)
		if errors.Is(err, errGone) {
			writeJSON(w, http.StatusNotFound, wire.Error{Error: "unknown"})
			return
		} else if err != nil {
			a.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, wire.One{Request: req, Pending: pending})
	})
	decide := func(do func(context.Context, string) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if !wire.ID.MatchString(id) {
				writeJSON(w, http.StatusConflict, wire.Error{Error: "no longer pending"})
				return
			}
			a.act.Lock()
			defer a.act.Unlock()
			if err := do(r.Context(), id); err != nil {
				a.fail(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("POST /v1/requests/{id}/approve", decide(a.recipe.approve))
	mux.HandleFunc("POST /v1/requests/{id}/reject", decide(a.recipe.reject))
	return a.signed(mux)
}

// signed lets through only requests aprova signed. /healthz is open: it says nothing.
func (a *server) signed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, wire.Error{Error: "bad body"})
			return
		}
		if err := wire.Verify(r, a.secret, body, time.Now()); err != nil {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			log.Printf("refused %s %s from %s: %v", r.Method, r.URL.Path, host, err)
			writeJSON(w, http.StatusUnauthorized, wire.Error{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

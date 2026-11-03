// Package module is the contract between aprova's core and the apps it approves
// sign-ups for. The core knows nothing about any app: each app is a module that
// registers itself, and an installation turns on the modules it wants.
package module

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Request is one pending sign-up as the app reports it.
type Request struct {
	ID       string
	Username string
	Email    string
	Reason   string // the applicant's own words, if the app asks for them
	Created  time.Time
	// Checks a module adds from what only the app knows. The core adds its own.
	Checks []Check
}

// Check is an automatic signal shown next to the reviews. A failed required check
// keeps a request from being approved by quorum.
type Check struct {
	Name   string `json:"name"` // stable id, used in required_checks
	OK     bool   `json:"ok"`
	Detail string `json:"detail"` // short, shown to moderators
}

// Actor is the moderator acting. Token is set only for modules whose service
// uses the moderator's own sign-in (auth: moderator).
type Actor struct {
	AccountID string
	Acct      string
	Token     string
}

// Caps tells the core what a module can do.
type Caps struct {
	// ModeratorAuth: the module can act with the signed-in moderator's token, so
	// the app's own moderation log names the person.
	ModeratorAuth bool
	// SilentReject: rejecting sends nothing to the applicant, so aprova's own
	// e-mail is the only message. When false, aprova doesn't send its e-mail.
	SilentReject bool
	// SendsWelcome: approving makes the app send its own welcome e-mail.
	SendsWelcome bool
}

// Module is one app. Every method must check its arguments: ids come from URLs.
type Module interface {
	// Pending lists the requests waiting for a decision.
	Pending(ctx context.Context, a Actor) ([]Request, error)
	// Get returns one request. pending is false when it was decided or removed
	// meanwhile, and the request may then be empty.
	Get(ctx context.Context, a Actor, id string) (r Request, pending bool, err error)
	// Approve and Reject act on a pending request, and return ErrGone when it no
	// longer is. Reject must not message the applicant if Caps says SilentReject.
	Approve(ctx context.Context, a Actor, id string) error
	Reject(ctx context.Context, a Actor, id string) error
	Caps() Caps
}

// ErrGone means the request is no longer pending.
var ErrGone = errors.New("request is no longer pending")

// ErrUnauthorized means the credentials were refused by the app.
var ErrUnauthorized = errors.New("refused by the app")

// Config is what an installation gives a module for one service.
type Config struct {
	URL    string
	Auth   string // "moderator" or "token"
	Token  string // for auth: token
	Client *http.Client
}

// Factory builds a module for one service.
type Factory func(Config) (Module, error)

var (
	mu       sync.Mutex
	registry = map[string]Factory{}
)

// Register is called from a module's init. A name is registered once.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("module %q registered twice", name))
	}
	registry[name] = f
}

// Lookup finds a compiled-in module.
func Lookup(name string) (Factory, bool) {
	mu.Lock()
	defer mu.Unlock()
	f, ok := registry[name]
	return f, ok
}

// Names lists the compiled-in modules.
func Names() []string {
	mu.Lock()
	defer mu.Unlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

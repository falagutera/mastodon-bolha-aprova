// Package moduletest is the contract every module must pass. A module's tests run
// it against a fake of the app:
//
//	moduletest.Run(t, func(t *testing.T) (module.Module, moduletest.Fake, module.Actor) { ... })
package moduletest

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitlab.gutocarvalho.net/open/bolha/aprova/module"
)

// Fake is the app's side, as the contract needs to see it.
type Fake interface {
	// Add makes a pending request with these fields.
	Add(r module.Request)
	// Remove decides or deletes a request elsewhere, as another moderator would.
	Remove(id string)
	Approved(id string) bool
	Rejected(id string) bool
	// Notified reports whether the app itself messaged the applicant of id.
	Notified(id string) bool
	// Requests counts the calls the module made to the app.
	Requests() int
}

// Setup returns a fresh module, its fake and an actor allowed to act.
type Setup func(t *testing.T) (module.Module, Fake, module.Actor)

// Run checks the guarantees the core relies on.
func Run(t *testing.T, setup Setup) {
	ctx := context.Background()
	t.Run("lists pending", func(t *testing.T) {
		m, f, a := setup(t)
		f.Add(module.Request{ID: "11", Username: "ana", Email: "ana@example.org", Reason: "hi there", Created: time.Now()})
		f.Add(module.Request{ID: "12", Username: "bia", Email: "bia@example.org"})
		got, err := m.Pending(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("%d pending: %+v", len(got), got)
		}
		byID := map[string]module.Request{}
		for _, r := range got {
			byID[r.ID] = r
		}
		if r := byID["11"]; r.Username != "ana" || r.Email != "ana@example.org" || r.Reason != "hi there" {
			t.Errorf("fields: %+v", r)
		}
	})
	t.Run("get follows the app", func(t *testing.T) {
		m, f, a := setup(t)
		f.Add(module.Request{ID: "21", Username: "ana", Email: "ana@example.org"})
		if r, pending, err := m.Get(ctx, a, "21"); err != nil || !pending || r.Username != "ana" {
			t.Fatalf("pending: %+v %v %v", r, pending, err)
		}
		f.Remove("21")
		if _, pending, err := m.Get(ctx, a, "21"); err != nil || pending {
			t.Errorf("after remove: %v %v", pending, err)
		}
	})
	t.Run("approve once", func(t *testing.T) {
		m, f, a := setup(t)
		f.Add(module.Request{ID: "31", Username: "ana", Email: "ana@example.org"})
		if err := m.Approve(ctx, a, "31"); err != nil {
			t.Fatal(err)
		}
		if !f.Approved("31") {
			t.Error("not approved in the app")
		}
		if err := m.Approve(ctx, a, "31"); !errors.Is(err, module.ErrGone) {
			t.Errorf("second approve: %v", err)
		}
		if err := m.Reject(ctx, a, "31"); !errors.Is(err, module.ErrGone) || f.Rejected("31") {
			t.Errorf("reject after approve: %v", err)
		}
	})
	t.Run("reject", func(t *testing.T) {
		m, f, a := setup(t)
		f.Add(module.Request{ID: "41", Username: "ana", Email: "ana@example.org"})
		if err := m.Reject(ctx, a, "41"); err != nil {
			t.Fatal(err)
		}
		if !f.Rejected("41") {
			t.Error("not rejected in the app")
		}
		if m.Caps().SilentReject && f.Notified("41") {
			t.Error("SilentReject, but the app messaged the applicant")
		}
	})
	t.Run("gone is not acted on", func(t *testing.T) {
		m, f, a := setup(t)
		f.Add(module.Request{ID: "51", Username: "ana", Email: "ana@example.org"})
		f.Remove("51")
		if err := m.Approve(ctx, a, "51"); !errors.Is(err, module.ErrGone) || f.Approved("51") {
			t.Errorf("approve removed: %v", err)
		}
	})
	t.Run("bad ids never reach the app", func(t *testing.T) {
		m, f, a := setup(t)
		before := f.Requests()
		for _, id := range []string{"", "../admin", "1/approve", "1;2", "1?x=y", "%2e%2e", "１２"} {
			if _, pending, err := m.Get(ctx, a, id); pending || err != nil {
				t.Errorf("get %q: %v %v", id, pending, err)
			}
			if err := m.Approve(ctx, a, id); !errors.Is(err, module.ErrGone) {
				t.Errorf("approve %q: %v", id, err)
			}
			if err := m.Reject(ctx, a, id); !errors.Is(err, module.ErrGone) {
				t.Errorf("reject %q: %v", id, err)
			}
		}
		if f.Requests() != before {
			t.Errorf("%d calls made with bad ids", f.Requests()-before)
		}
	})
}

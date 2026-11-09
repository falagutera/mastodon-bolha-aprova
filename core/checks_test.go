package core

import (
	"context"
	"testing"

	"gitlab.gutocarvalho.net/open/bolha/aprova/module"
)

func hasCheck(cs []module.Check, name string) bool {
	for _, c := range cs {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestReasonCheckOnlyWhenTheAppAsksForOne(t *testing.T) {
	c := &checker{}
	q := module.Request{Username: "ana", Email: "ana@example.org"}

	asks := c.run(context.Background(), q, false)
	if !hasCheck(asks, checkReason) {
		t.Fatalf("an app with a reason field must get the reason check: %+v", asks)
	}
	for _, chk := range asks {
		if chk.Name == checkReason && chk.OK {
			t.Fatalf("an empty reason must fail the check: %+v", chk)
		}
	}

	noField := c.run(context.Background(), q, true)
	if hasCheck(noField, checkReason) {
		t.Fatalf("an app without a reason field must not get the reason check: %+v", noField)
	}
	if !hasCheck(noField, checkBotSignal) || !hasCheck(noField, checkDisposable) {
		t.Fatalf("the other core checks still run: %+v", noField)
	}
}

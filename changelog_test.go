package main

import (
	"testing"

	"gitlab.gutocarvalho.net/open/bolha/aprova/internal/relnotes"
)

// Every VERSION needs its CHANGELOG.md entry: the release notes page is built from it.
func TestChangelogHasVersion(t *testing.T) {
	if err := relnotes.Check(changelog, versionFile); err != nil {
		t.Fatal(err)
	}
}

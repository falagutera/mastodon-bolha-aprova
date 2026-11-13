package core

import (
	"os"
	"strings"
	"testing"
)

func exampleEnv() map[string]string {
	return map[string]string{
		"APROVA_CLIENT_ID": "i", "APROVA_CLIENT_SECRET": "s",
		"APROVA_ENCRYPTION_KEY":    strings.Repeat("k", 32),
		"APROVA_DB_HOST":           "db",
		"APROVA_DB_PASSWORD":       "p",
		"APROVA_SISTER_TOKEN":      "t",
		"APROVA_GITLAB_TOKEN":      "g",
		"APROVA_MAIN_DIGEST_TOKEN": "d",
	}
}

func TestExampleConfigLoads(t *testing.T) {
	raw, err := os.ReadFile("../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	env := exampleEnv()
	if _, err := ParseConfig(raw, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
}

// A small valid config, mutated below so each change makes ParseConfig refuse it.
// It is the test's own fixture, independent of config.example.yaml (which the
// example ships with a minimal default and the rest commented out).
const refuseBase = `instance: https://example.social
base_url: https://aprova.example.social
lang: en
modules: [mastodon, gitlab]
services:
  main:
    module: mastodon
    name: example.social
    url: https://example.social
    auth: moderator
  code:
    module: gitlab
    name: code.example
    url: https://code.example
    auth: token
    token_env: APROVA_GITLAB_TOKEN
profiles:
  community:
    name: Community
    services: [main]
    approve: [Moderator]
    escalate_to: [Admin]
    review:
      quorum: 2
      on_conflict: escalate
      required_checks: [bot_signal, disposable_email]
  infra:
    name: Infra
    services: [code]
    approve: [Admin]
    escalate_to: [Admin]
`

func TestConfigRefusesWhatIsUnclear(t *testing.T) {
	env := exampleEnv()
	get := func(k string) string { return env[k] }
	if _, err := ParseConfig([]byte(refuseBase), get); err != nil {
		t.Fatalf("base config should be valid: %v", err)
	}
	for name, change := range map[string][2]string{
		"no modules":          {"modules: [mastodon, gitlab]", "modules: []"},
		"unknown module":      {"modules: [mastodon, gitlab]", "modules: [mastodon, gitlab, nosuchapp]"},
		"service off":         {"modules: [mastodon, gitlab]", "modules: [mastodon]"},
		"moderator elsewhere": {"url: https://code.example\n    auth: token", "url: https://code.example\n    auth: moderator"},
		"two profiles":        {"services: [code]", "services: [code, main]"},
		"unknown check":       {"[bot_signal, disposable_email]", "[bot_signal, typo]"},
		"http service":        {"url: https://code.example", "url: http://code.example"},
		"unknown field":       {"lang: en", "lang: en\nlanguage: en"},
		"quorum":              {"quorum: 2", "quorum: 99"},
	} {
		if !strings.Contains(refuseBase, change[0]) {
			t.Fatalf("%s: base has no %q", name, change[0])
		}
		if _, err := ParseConfig([]byte(strings.Replace(refuseBase, change[0], change[1], 1)), get); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	delete(env, "APROVA_GITLAB_TOKEN")
	if _, err := ParseConfig([]byte(refuseBase), get); err == nil {
		t.Error("missing token accepted")
	}
}

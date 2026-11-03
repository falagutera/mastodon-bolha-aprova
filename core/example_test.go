package core

import (
	"os"
	"strings"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	raw, err := os.ReadFile("../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"APROVA_CLIENT_ID": "i", "APROVA_CLIENT_SECRET": "s", "APROVA_ENCRYPTION_KEY": strings.Repeat("k", 32),
		"APROVA_DB_HOST": "db", "APROVA_DB_PASSWORD": "p", "APROVA_SISTER_TOKEN": "t", "APROVA_GITLAB_TOKEN": "g"}
	if _, err := ParseConfig(raw, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRefusesWhatIsUnclear(t *testing.T) {
	raw, _ := os.ReadFile("../config.example.yaml")
	base := string(raw)
	env := map[string]string{"APROVA_CLIENT_ID": "i", "APROVA_CLIENT_SECRET": "s", "APROVA_ENCRYPTION_KEY": strings.Repeat("k", 32),
		"APROVA_DB_HOST": "db", "APROVA_DB_PASSWORD": "p", "APROVA_SISTER_TOKEN": "t", "APROVA_GITLAB_TOKEN": "g"}
	get := func(k string) string { return env[k] }
	for name, change := range map[string][2]string{
		"no modules":          {"modules: [mastodon, gitlab]", "modules: []"},
		"unknown module":      {"modules: [mastodon, gitlab]", "modules: [mastodon, gitlab, lemmy]"},
		"service off":         {"modules: [mastodon, gitlab]", "modules: [mastodon]"},
		"moderator elsewhere": {"    url: https://sister.example\n    auth: token", "    url: https://sister.example\n    auth: moderator"},
		"two profiles":        {"services: [code]", "services: [code, main]"},
		"unknown check":       {"[bot_signal, disposable_email]", "[bot_signal, typo]"},
		"http service":        {"url: https://code.example", "url: http://code.example"},
		"unknown field":       {"lang: en            # en or pt-BR", "lang: en\nlanguage: en"},
		"quorum":              {"quorum: 2 ", "quorum: 99 "},
	} {
		if !strings.Contains(base, change[0]) {
			t.Fatalf("%s: example has no %q", name, change[0])
		}
		if _, err := ParseConfig([]byte(strings.Replace(base, change[0], change[1], 1)), get); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	delete(env, "APROVA_GITLAB_TOKEN")
	if _, err := ParseConfig(raw, get); err == nil {
		t.Error("missing token accepted")
	}
}

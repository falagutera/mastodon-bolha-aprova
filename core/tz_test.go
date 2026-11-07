package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 00:03 UTC on Nov 7 is still Nov 6 in Brasília: the panel shows local time.
func TestTimesShownInConfiguredZone(t *testing.T) {
	cfg, err := ParseConfig([]byte(fmt.Sprintf(testConfig, "https://a.example", "https://b.example", "https://c.example")), testEnv(map[string]string{"T_HOOK": "https://h.example"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timezone != "America/Sao_Paulo" {
		t.Fatalf("default timezone %q", cfg.Timezone)
	}
	tmpl, err := parseTemplates(cfg.loc)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	at := time.Date(2026, 11, 7, 0, 3, 0, 0, time.UTC)
	x, err := tmpl.New("tz").Parse(`{{when .}} {{zone}}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.Execute(&b, at); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "2026-11-06 21:03 America/Sao_Paulo" {
		t.Errorf("got %q", got)
	}
	if _, err := ParseConfig([]byte(fmt.Sprintf(testConfig, "https://a.example", "https://b.example", "https://c.example")+"timezone: Mars/Base\n"), testEnv(map[string]string{"T_HOOK": "https://h.example"})); err == nil {
		t.Error("bad timezone accepted")
	}
}

package core

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// An aprova API token (aprova_...) and a browser session once shared one hash
// namespace, so the token's bare id, sent as the __Host-aprova cookie, resolved
// to the API session and let the holder mint fresh tokens and outlive logout.
func TestAPITokenCannotBeReplayedAsABrowserSession(t *testing.T) {
	w := newWorld(t)
	tok := w.login("p1").apiToken()
	id := strings.TrimPrefix(tok, apiTokenPrefix)

	// Sanity: the token still authenticates the API as a bearer token.
	if status, _ := api(t, w, tok, http.MethodGet, "/api/v1/queue", nil); status != http.StatusOK {
		t.Fatalf("api token as bearer: %d", status)
	}

	// Neither the bare id nor the whole token may open a UI session: an
	// authenticated /queue is 200, an unauthenticated one redirects (302).
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, v := range []string{id, tok} {
		req, _ := http.NewRequest(http.MethodGet, w.app.URL+"/queue", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: v})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("cookie %q opened a UI session: %d", v, resp.StatusCode)
		}
	}
}

// A NUL byte in an inbound reply used to abort the PostgreSQL write (the store
// runs in simple-protocol mode), so the message was never marked handled and
// was re-read on every sweep. parseInbound now drops NUL from every field.
func TestParseInboundDropsNUL(t *testing.T) {
	raw := "From: a@ex\r\n" +
		"Message-Id: <m\x00id@ex>\r\n" +
		"In-Reply-To: <a\x00b@ex>\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"hel\x00lo\r\n"
	in, err := parseInbound(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if in.MessageID != "mid@ex" {
		t.Errorf("MessageID = %q", in.MessageID)
	}
	if in.Body != "hello" {
		t.Errorf("Body = %q", in.Body)
	}
	if len(in.References) != 1 || in.References[0] != "ab@ex" {
		t.Errorf("References = %v", in.References)
	}
}

// The pages run no script and no inline style. The CSP allows script and fetch only from
// 'self', for the bolha-bar the edge injects, never inline; the templates must hold none.
func TestNoScriptOrInlineStyle(t *testing.T) {
	rec := httptest.NewRecorder()
	securityHeaders(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'; connect-src 'self';") || strings.Count(csp, "script-src") != 1 || strings.Contains(csp, "unsafe-") {
		t.Errorf("CSP %q", csp)
	}
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil || len(pages) == 0 {
		t.Fatal(pages, err)
	}
	bad := regexp.MustCompile(`(?i)<script|<style|\sstyle=|\son[a-z]+=|javascript:`)
	for _, p := range pages {
		raw, _ := fs.ReadFile(templateFS, p)
		if m := bad.Find(raw); m != nil {
			t.Errorf("%s: %q", p, m)
		}
	}
}

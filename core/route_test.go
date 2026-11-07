package core

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInternalRoutes(t *testing.T) {
	var gotHost, gotProto, gotPath string
	in := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotProto, gotPath = r.Host, r.Header.Get("X-Forwarded-Proto"), r.URL.RequestURI()
		io.WriteString(w, "internal")
	}))
	defer in.Close()
	routes, err := parseRoutes(map[string]string{"Bolha.US": in.URL})
	if err != nil {
		t.Fatal(err)
	}
	c := routed(&http.Client{}, routes)
	resp, err := c.Get("https://bolha.us/api/v1/accounts/verify_credentials?x=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "internal" || gotHost != "bolha.us" || gotProto != "https" || gotPath != "/api/v1/accounts/verify_credentials?x=1" {
		t.Errorf("routed: body=%q host=%q proto=%q path=%q", body, gotHost, gotProto, gotPath)
	}
	// Other hosts and plain http are untouched.
	for _, u := range []string{"https://other.example.invalid/", "http://bolha.us:8443/"} {
		req, _ := http.NewRequest("GET", u, nil)
		rt := c.Transport.(*internalRoutes)
		if _, ok := rt.routes[req.URL.Hostname()]; ok && req.URL.Scheme == "https" {
			t.Errorf("%s would be routed", u)
		}
	}
	for _, bad := range []map[string]string{{"bolha.us/x": in.URL}, {"bolha.us": "ftp://x"}, {"bolha.us": "http://h/path"}} {
		if _, err := parseRoutes(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if routed(&http.Client{}, nil).Transport != nil {
		t.Error("no routes should keep the client as is")
	}
}

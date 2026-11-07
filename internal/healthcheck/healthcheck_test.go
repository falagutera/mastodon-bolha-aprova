package healthcheck

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbe(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	for _, listen := range []string{":" + port, "0.0.0.0:" + port, "127.0.0.1:" + port} {
		if err := Probe(listen); err != nil {
			t.Errorf("Probe(%q): %v", listen, err)
		}
	}
	status = http.StatusServiceUnavailable
	if err := Probe(":" + port); err == nil {
		t.Error("Probe accepted a 503")
	}
	for _, bad := range []string{"", "8080", ":0", ":99999", ":http", ":80@example.com"} {
		if err := Probe(bad); err == nil {
			t.Errorf("Probe(%q) accepted", bad)
		}
	}
}

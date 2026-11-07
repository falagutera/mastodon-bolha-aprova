// Package healthcheck is the -healthcheck flag of aprova and aprova-agent: it
// asks the server running in the same container for /healthz, on loopback.
package healthcheck

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Probe asks the server listening on listen (":8080", "0.0.0.0:8080") for
// /healthz on 127.0.0.1. Only the port of listen is used, so the request
// never leaves the container.
func Probe(listen string) error {
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("bad port in %q", listen)
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Path: "/healthz"}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(u.String())
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

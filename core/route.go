package core

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// internalRoutes sends requests for a public host straight to a URL inside the
// cluster (e.g. bolha.us -> http://web.bolha-us.svc.cluster.local:3000), so
// aprova does not go out and back in through the edge proxy and WAF: a burst of
// fake logins can't get the panel's egress IP banned there. The app still sees
// its public Host and X-Forwarded-Proto: https, as it would behind the edge.
type internalRoutes struct {
	routes map[string]*url.URL // public host -> internal base
	next   http.RoundTripper
}

func parseRoutes(m map[string]string) (map[string]*url.URL, error) {
	out := map[string]*url.URL{}
	for host, target := range m {
		h := strings.ToLower(strings.TrimSpace(host))
		if h == "" || strings.ContainsAny(h, "/:") {
			return nil, fmt.Errorf("config: internal_routes: %q must be a bare host name", host)
		}
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("config: internal_routes: %s: %q must be http(s)://host[:port]", host, target)
		}
		out[h] = u
	}
	return out, nil
}

func (r *internalRoutes) RoundTrip(req *http.Request) (*http.Response, error) {
	to, ok := r.routes[strings.ToLower(req.URL.Hostname())]
	if !ok || req.URL.Scheme != "https" || (req.URL.Port() != "" && req.URL.Port() != "443") {
		return r.next.RoundTrip(req)
	}
	out := req.Clone(req.Context())
	u := *req.URL
	u.Scheme, u.Host = to.Scheme, to.Host
	out.URL = &u
	out.Host = req.URL.Hostname()
	out.Header.Set("X-Forwarded-Proto", "https")
	out.Header.Set("X-Forwarded-Host", req.URL.Hostname())
	return r.next.RoundTrip(out)
}

// routed wraps the client's transport with the internal routes, if any.
func routed(c *http.Client, routes map[string]*url.URL) *http.Client {
	if len(routes) == 0 {
		return c
	}
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	cc := *c
	cc.Transport = &internalRoutes{routes: routes, next: next}
	return &cc
}

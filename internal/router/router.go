// Package router authenticates client workbenches and selects the upstream.
//
// This is what makes the gateway "managed" rather than "transparent": the
// workbench presents a key issued by the gateway, the gateway resolves that key
// to a specific upstream and presents ITS OWN credential upstream. The real
// provider key therefore never reaches the workbench.
package router

import (
	"crypto/subtle"
	"fmt"
	"net/url"
	"strings"
)

// Upstream is one resolved upstream target.
type Upstream struct {
	Name    string
	BaseURL string
	APIKey  string

	base *url.URL
}

// Client identifies the calling workbench.
type Client struct {
	Name string
	Key  string
	// Upstream pins this client to one upstream. Empty means "any".
	Upstream string
}

// Router resolves a bearer token to a client and an upstream.
type Router struct {
	clients         []Client
	upstreams       []Upstream
	defaultUpstream string
	authRequired    bool
	dashboardKey    string
}

// New builds a Router. Upstream base URLs are validated once here so a bad
// configuration fails at startup rather than on the first request.
func New(upstreams []Upstream, clients []Client, defaultUpstream, dashboardKey string) (*Router, error) {
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("router: at least one upstream is required")
	}

	prepared := make([]Upstream, 0, len(upstreams))
	byName := make(map[string]bool, len(upstreams))
	for i, u := range upstreams {
		if strings.TrimSpace(u.Name) == "" {
			return nil, fmt.Errorf("router: upstreams[%d] has no name", i)
		}
		if byName[u.Name] {
			return nil, fmt.Errorf("router: duplicate upstream %q", u.Name)
		}
		byName[u.Name] = true

		base, err := normalizeBase(u.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("router: upstream %q: %w", u.Name, err)
		}
		u.base = base
		prepared = append(prepared, u)
	}

	if defaultUpstream == "" {
		defaultUpstream = prepared[0].Name
	}
	if !byName[defaultUpstream] {
		return nil, fmt.Errorf("router: default upstream %q is not configured", defaultUpstream)
	}

	for i, c := range clients {
		if c.Upstream != "" && !byName[c.Upstream] {
			return nil, fmt.Errorf("router: clients[%d] (%s): unknown upstream %q", i, c.Name, c.Upstream)
		}
	}

	return &Router{
		clients:         clients,
		upstreams:       prepared,
		defaultUpstream: defaultUpstream,
		authRequired:    len(clients) > 0,
		dashboardKey:    dashboardKey,
	}, nil
}

// AuthRequired reports whether client keys are enforced.
func (r *Router) AuthRequired() bool { return r.authRequired }

// DefaultUpstreamName exposes the fallback upstream name for startup logging.
func (r *Router) DefaultUpstreamName() string { return r.defaultUpstream }

// UpstreamNames lists configured upstream names, in configuration order.
func (r *Router) UpstreamNames() []string {
	out := make([]string, 0, len(r.upstreams))
	for _, u := range r.upstreams {
		out = append(out, u.Name)
	}
	return out
}

// Authenticate resolves a presented bearer token to a client.
//
// Every configured key is compared in constant time, and the loop does not
// short-circuit on a match, so the time taken does not reveal which key
// matched or how far through the list a wrong key got.
//
// The presented token is NOT trimmed. Whitespace is significant in a secret,
// and trimming would both weaken the comparison and silently accept a
// truncated or padded key. Callers strip the "Bearer " scheme prefix and
// nothing else.
func (r *Router) Authenticate(presented string) (Client, bool) {
	if !r.authRequired {
		// Open mode: a stable synthetic identity so stats still attribute work.
		return Client{Name: "anonymous"}, true
	}

	var (
		found Client
		ok    bool
	)
	for _, c := range r.clients {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(c.Key)) == 1 {
			found = c
			ok = true
		}
	}
	return found, ok
}

// DashboardAuthorized reports whether a dashboard request may proceed.
// An unset dashboard key leaves the dashboard open, which is the documented
// single-user default.
func (r *Router) DashboardAuthorized(presented string) bool {
	if r.dashboardKey == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(presented)), []byte(r.dashboardKey)) == 1
}

// Resolve picks the upstream for a client and builds the absolute target URL.
//
// path is the incoming request path (e.g. /v1/chat/completions). The path is
// appended to the upstream base verbatim: OpenAI-compatible providers differ in
// whether they mount under /v1, /v2, or nothing, and the gateway must not guess.
func (r *Router) Resolve(c Client, path string) (Upstream, string, error) {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	name := c.Upstream
	if name == "" {
		name = r.defaultUpstream
	}
	up, ok := r.byName(name)
	if !ok {
		return Upstream{}, "", fmt.Errorf("upstream %q not found", name)
	}

	out := *up.base
	out.Path = up.base.Path + path
	return up, out.String(), nil
}

func (r *Router) byName(name string) (Upstream, bool) {
	for _, u := range r.upstreams {
		if u.Name == name {
			return u, true
		}
	}
	return Upstream{}, false
}

// normalizeBase validates an upstream base URL and strips trailing slashes so
// joining a path cannot produce a double slash.
func normalizeBase(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("base URL is empty")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("base URL %q has no host", raw)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

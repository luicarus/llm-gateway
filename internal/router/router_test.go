package router

import "testing"

func TestAuthenticateConstantTimeLoop(t *testing.T) {
	r, err := New(
		[]Upstream{{Name: "a", BaseURL: "https://a.example"}},
		[]Client{{Name: "alice", Key: "key-a"}, {Name: "bob", Key: "key-b"}},
		"a", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if !r.AuthRequired() {
		t.Error("AuthRequired() = false, want true when clients are configured")
	}

	for _, c := range []struct{ key, want string }{
		{"key-a", "alice"},
		{"key-b", "bob"},
	} {
		got, ok := r.Authenticate(c.key)
		if !ok || got.Name != c.want {
			t.Errorf("Authenticate(%q) = %q ok=%v, want %q", c.key, got.Name, ok, c.want)
		}
	}

	for _, bad := range []string{"", "key", "key-a ", "KEY-A", "key-c"} {
		if _, ok := r.Authenticate(bad); ok {
			t.Errorf("Authenticate(%q) succeeded, want rejection", bad)
		}
	}
}

func TestOpenModeWhenNoClients(t *testing.T) {
	r, err := New([]Upstream{{Name: "a", BaseURL: "https://a.example"}}, nil, "a", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.AuthRequired() {
		t.Error("AuthRequired() = true, want false with no clients")
	}
	c, ok := r.Authenticate("")
	if !ok || c.Name != "anonymous" {
		t.Errorf("open mode gave %q ok=%v, want anonymous", c.Name, ok)
	}
}

func TestResolvePathJoining(t *testing.T) {
	r, err := New(
		[]Upstream{
			{Name: "root", BaseURL: "https://api.example.com"},
			{Name: "slashed", BaseURL: "https://api.example.com/"},
			{Name: "v1", BaseURL: "https://api.example.com/v1"},
			{Name: "bare", BaseURL: "api.example.com"},
		},
		[]Client{
			{Name: "a", Key: "k1", Upstream: "root"},
			{Name: "b", Key: "k2", Upstream: "slashed"},
			{Name: "c", Key: "k3", Upstream: "v1"},
			{Name: "d", Key: "k4", Upstream: "bare"},
		}, "root", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct{ key, path, want string }{
		{"k1", "/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"k2", "/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		// A base that already carries a path keeps it: providers mount their
		// API under prefixes and the gateway must not rewrite them.
		{"k3", "/v1/chat/completions", "https://api.example.com/v1/v1/chat/completions"},
		{"k4", "/v1/models", "https://api.example.com/v1/models"},
	}
	for _, c := range cases {
		client, _ := r.Authenticate(c.key)
		_, got, err := r.Resolve(client, c.path)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", c.path, err)
		}
		if got != c.want {
			t.Errorf("Resolve(key=%s, path=%q) = %q, want %q", c.key, c.path, got, c.want)
		}
	}
}

// A client with no pin uses the default upstream.
func TestResolveDefaultUpstream(t *testing.T) {
	r, _ := New(
		[]Upstream{
			{Name: "first", BaseURL: "https://first.example"},
			{Name: "second", BaseURL: "https://second.example"},
		},
		[]Client{{Name: "any", Key: "k"}}, "second", "")

	client, _ := r.Authenticate("k")
	up, _, err := r.Resolve(client, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if up.Name != "second" {
		t.Errorf("resolved upstream = %q, want the configured default 'second'", up.Name)
	}
}

func TestNewRejectsBadConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		ups     []Upstream
		clients []Client
		deflt   string
	}{
		{"no upstreams", nil, nil, ""},
		{"unnamed upstream", []Upstream{{BaseURL: "https://a.example"}}, nil, ""},
		{"duplicate upstream", []Upstream{
			{Name: "a", BaseURL: "https://a.example"},
			{Name: "a", BaseURL: "https://b.example"},
		}, nil, ""},
		{"bad scheme", []Upstream{{Name: "a", BaseURL: "ftp://a.example"}}, nil, ""},
		{"empty base", []Upstream{{Name: "a", BaseURL: "  "}}, nil, ""},
		{"unknown default", []Upstream{{Name: "a", BaseURL: "https://a.example"}}, nil, "nope"},
		{"client pins unknown upstream", []Upstream{{Name: "a", BaseURL: "https://a.example"}},
			[]Client{{Name: "x", Key: "k", Upstream: "ghost"}}, "a"},
	}
	for _, c := range cases {
		if _, err := New(c.ups, c.clients, c.deflt, ""); err == nil {
			t.Errorf("%s: New succeeded, want an error", c.name)
		}
	}
}

func TestDashboardAuthorized(t *testing.T) {
	// No dashboard key: everything is allowed (single-user default).
	open, _ := New([]Upstream{{Name: "a", BaseURL: "https://a.example"}}, nil, "a", "")
	if !open.DashboardAuthorized("") {
		t.Error("open dashboard rejected an empty key")
	}

	protected, _ := New([]Upstream{{Name: "a", BaseURL: "https://a.example"}}, nil, "a", "dash-secret")
	if !protected.DashboardAuthorized("dash-secret") {
		t.Error("correct dashboard key rejected")
	}
	for _, bad := range []string{"", "wrong", "dash-secre"} {
		if protected.DashboardAuthorized(bad) {
			t.Errorf("DashboardAuthorized(%q) = true, want false", bad)
		}
	}
}

func TestUpstreamNames(t *testing.T) {
	r, _ := New([]Upstream{
		{Name: "one", BaseURL: "https://one.example"},
		{Name: "two", BaseURL: "https://two.example"},
	}, nil, "one", "")

	got := r.UpstreamNames()
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("UpstreamNames() = %v, want [one two] preserving config order", got)
	}
}

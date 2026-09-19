package router

import (
	"net/url"
	"strings"
	"testing"
)

// The authoritative SSRF check is not "does the string contain a foreign host"
// but "does re-parsing the produced URL still address the configured host".
// Query strings and paths legitimately contain host-like text, so string
// matching alone produces false positives.
func TestResolveNeverEscapesConfiguredHost(t *testing.T) {
	const wantHost = "api.example.com"

	r, err := New(
		[]Upstream{{Name: "u", BaseURL: "https://" + wantHost + "/v1base"}},
		[]Client{{Name: "c", Key: "k"}}, "u", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client, _ := r.Authenticate("k")

	// Each of these is an attempt to make the gateway talk to someone else, or
	// to climb out of the base path.
	cases := []string{
		"//evil.example/steal",
		"/../../../etc/passwd",
		"/..%2f..%2fetc",
		"/;/evil.example",
		"/@evil.example",
		"/\\evil.example",
		"https://evil.example/x",
		"http://evil.example",
		"////evil.example",
		"/%2F%2Fevil.example",
		"/?next=https://evil.example",
	}

	for _, path := range cases {
		_, got, err := r.Resolve(client, path)
		if err != nil {
			continue // refusing outright is a valid defense
		}
		parsed, perr := url.Parse(got)
		if perr != nil {
			t.Errorf("path %q produced an unparseable URL %q: %v", path, got, perr)
			continue
		}
		if parsed.Host != wantHost {
			t.Errorf("SSRF: path %q resolved to host %q, want %q (full: %s)",
				path, parsed.Host, wantHost, got)
		}
		if parsed.Scheme != "https" {
			t.Errorf("path %q changed the scheme to %q", path, parsed.Scheme)
		}
	}
}

// A leading "//" is the specific case where a URL parser can treat the next
// segment as an authority, so it deserves an explicit assertion.
func TestResolveDoubleSlashDoesNotBecomeAuthority(t *testing.T) {
	r, _ := New([]Upstream{{Name: "u", BaseURL: "https://api.example.com"}},
		[]Client{{Name: "c", Key: "k"}}, "u", "")
	client, _ := r.Authenticate("k")

	_, got, err := r.Resolve(client, "//evil.example/steal")
	if err != nil {
		return
	}
	parsed, perr := url.Parse(got)
	if perr != nil {
		t.Fatalf("unparseable: %s", got)
	}
	if parsed.Host != "api.example.com" {
		t.Errorf("double-slash path produced authority %q (full URL: %s)", parsed.Host, got)
	}
	if !strings.HasPrefix(parsed.Path, "/") {
		t.Errorf("path %q did not stay absolute: %s", parsed.Path, got)
	}
}

// The base path must be preserved, not replaced by the incoming path.
func TestResolvePreservesBasePath(t *testing.T) {
	r, _ := New([]Upstream{{Name: "u", BaseURL: "https://api.example.com/v1beta"}},
		[]Client{{Name: "c", Key: "k"}}, "u", "")
	client, _ := r.Authenticate("k")

	_, got, err := r.Resolve(client, "/chat/completions")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasPrefix(got, "https://api.example.com/v1beta/") {
		t.Errorf("base path was lost: %s", got)
	}
}

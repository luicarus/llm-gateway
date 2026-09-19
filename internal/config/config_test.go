package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// A minimal YAML config with a secret pulled from the environment is the
// documented shared-deployment shape.
func TestLoadYAMLWithEnvSecret(t *testing.T) {
	t.Setenv("MY_DS_KEY", "sk-from-env")
	t.Setenv("BOB_KEY", "gw-bob")

	p := write(t, "gateway.yaml", `
listen: ":9000"
default_upstream: deepseek
upstreams:
  - name: deepseek
    base_url: https://api.deepseek.com
    api_key_env: MY_DS_KEY
  - name: openai
    base_url: https://api.openai.com
    api_key: sk-inline
client_keys:
  - name: alice-workbench
    key: gw-alice
    upstream: deepseek
  - name: bob-workbench
    key_env: BOB_KEY
`)

	s, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Listen != ":9000" {
		t.Errorf("Listen = %q, want :9000", s.Listen)
	}
	if len(s.Upstreams) != 2 {
		t.Fatalf("upstreams = %d, want 2", len(s.Upstreams))
	}
	if s.Upstreams[0].APIKey != "sk-from-env" {
		t.Errorf("upstream secret not resolved from env: %q", s.Upstreams[0].APIKey)
	}
	if s.Upstreams[1].APIKey != "sk-inline" {
		t.Errorf("inline secret = %q, want sk-inline", s.Upstreams[1].APIKey)
	}
	// A key entry whose env var is unset must fail loudly rather than silently
	// dropping the workbench.
	if s.Upstreams[0].Name != "deepseek" || s.DefaultUpstream != "deepseek" {
		t.Errorf("default/name resolution wrong: %+v", s)
	}
}

func TestLoadJSON(t *testing.T) {
	p := write(t, "gateway.json", `{
	  "listen": ":9100",
	  "upstreams": [{"name":"u","base_url":"https://u.example","api_key":"k"}],
	  "client_keys": [{"name":"c","key":"ck"}]
	}`)

	s, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Listen != ":9100" || len(s.Upstreams) != 1 || len(s.ClientKeys) != 1 {
		t.Errorf("unexpected parse result: %+v", s)
	}
}

// Environment variables override the file so a container can be reconfigured
// without rebuilding its image.
func TestEnvOverridesFile(t *testing.T) {
	t.Setenv("LLM_GATEWAY_LISTEN", ":7777")
	t.Setenv("LLM_GATEWAY_UPSTREAM", "https://override.example")
	t.Setenv("LLM_GATEWAY_UPSTREAM_API_KEY", "sk-override")

	p := write(t, "gateway.yaml", `
listen: ":9000"
upstreams:
  - name: original
    base_url: https://original.example
`)
	s, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Listen != ":7777" {
		t.Errorf("Listen = %q, want the env override :7777", s.Listen)
	}
	if len(s.Upstreams) != 1 || s.Upstreams[0].BaseURL != "https://override.example" {
		t.Errorf("upstream not overridden: %+v", s.Upstreams)
	}
	if s.Upstreams[0].APIKey != "sk-override" {
		t.Errorf("override upstream key = %q, want sk-override", s.Upstreams[0].APIKey)
	}
}

// The single-upstream shorthand must work with no file at all.
func TestEnvOnlySingleUpstream(t *testing.T) {
	t.Setenv("LLM_GATEWAY_UPSTREAM", "https://api.deepseek.com")
	t.Setenv("LLM_GATEWAY_UPSTREAM_API_KEY", "sk-x")

	s, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Listen != DefaultListen {
		t.Errorf("Listen = %q, want default %q", s.Listen, DefaultListen)
	}
	if len(s.Upstreams) != 1 || s.Upstreams[0].APIKey != "sk-x" {
		t.Errorf("shorthand upstream wrong: %+v", s.Upstreams)
	}
	if s.AuthRequired() {
		t.Error("AuthRequired() = true, want false without client keys")
	}
}

func TestValidationErrors(t *testing.T) {
	t.Setenv("UNSET_KEY_FOR_TEST", "")

	cases := []struct{ name, yaml string }{
		{"no upstreams", `listen: ":8080"`},
		{"upstream without name", `
upstreams: [{base_url: "https://a.example"}]`},
		{"upstream without base_url", `
upstreams: [{name: "a"}]`},
		{"duplicate upstream names", `
upstreams:
  - {name: a, base_url: "https://a.example"}
  - {name: a, base_url: "https://b.example"}`},
		{"unknown default upstream", `
default_upstream: ghost
upstreams: [{name: a, base_url: "https://a.example"}]`},
		{"client key with unset env", `
upstreams: [{name: a, base_url: "https://a.example"}]
client_keys: [{name: c, key_env: UNSET_KEY_FOR_TEST}]`},
		{"client key with neither key nor env", `
upstreams: [{name: a, base_url: "https://a.example"}]
client_keys: [{name: c}]`},
		{"client pins unknown upstream", `
upstreams: [{name: a, base_url: "https://a.example"}]
client_keys: [{name: c, key: k, upstream: ghost}]`},
		{"cors enabled with no origins", `
cors: {enabled: true}
upstreams: [{name: a, base_url: "https://a.example"}]`},
		{"duplicate client key values", `
upstreams: [{name: a, base_url: "https://a.example"}]
client_keys: [{name: c1, key: same}, {name: c2, key: same}]`},
	}

	for _, c := range cases {
		p := write(t, "bad.yaml", c.yaml)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: Load succeeded, want an error", c.name)
		}
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	// An explicitly named config that does not exist must fail rather than
	// silently starting with defaults the operator did not intend.
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("Load(missing) succeeded, want an error")
	}
}

func TestCORSAndDashboardKey(t *testing.T) {
	t.Setenv("DASH_KEY", "dash-secret")

	p := write(t, "gateway.yaml", `
dashboard_key_env: DASH_KEY
cors:
  enabled: true
  allowed_origins: ["https://bench.example", "*"]
upstreams: [{name: a, base_url: "https://a.example"}]
`)
	s, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.DashboardKey != "dash-secret" {
		t.Errorf("DashboardKey = %q, want the env value", s.DashboardKey)
	}
	if !s.CORS.Enabled || len(s.CORS.AllowedOrigins) != 2 {
		t.Errorf("CORS parsed wrong: %+v", s.CORS)
	}
}

func TestCORSOriginsFromEnv(t *testing.T) {
	t.Setenv("LLM_GATEWAY_UPSTREAM", "https://a.example")
	t.Setenv("LLM_GATEWAY_CORS_ORIGINS", "https://one.example, https://two.example")

	s, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !s.CORS.Enabled || len(s.CORS.AllowedOrigins) != 2 {
		t.Errorf("CORS from env wrong: %+v", s.CORS)
	}
}

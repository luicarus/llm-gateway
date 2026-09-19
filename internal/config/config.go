// Package config loads the gateway's configuration.
//
// The gateway is meant to be deployed once and then used by other people's
// workbenches, so configuration must live in a file rather than in flags:
// a deployment needs several upstreams, per-upstream credentials, and the set
// of client keys allowed to use it.
//
// Precedence is: explicit file (--config) > environment > defaults. Environment
// overrides exist so a container can be reconfigured without rebuilding an
// image, which is how most deployments will run this.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultListen is used when nothing else specifies an address.
const DefaultListen = ":8080"

// Upstream is one upstream LLM API the gateway can forward to.
type Upstream struct {
	// Name identifies this upstream in stats and logs. Required, and must be
	// unique: it is the label operators see on the dashboard.
	Name string `json:"name" yaml:"name"`

	// BaseURL is the upstream's API root, e.g. https://api.deepseek.com.
	BaseURL string `json:"base_url" yaml:"base_url"`

	// APIKey is the credential the gateway presents to this upstream. When
	// empty the client's own Authorization header is forwarded instead, which
	// lets a deployment run in transparent mode for this upstream only.
	APIKey string `json:"api_key" yaml:"api_key"`

	// APIKeyEnv names an environment variable holding APIKey. Preferred over
	// inlining the secret, so a config file can be committed safely.
	APIKeyEnv string `json:"api_key_env" yaml:"api_key_env"`
}

// ClientKey is a credential a workbench presents to THIS gateway.
type ClientKey struct {
	// Key is the bearer token the client sends. Never logged.
	Key string `json:"key" yaml:"key"`

	// KeyEnv names an environment variable holding Key.
	KeyEnv string `json:"key_env" yaml:"key_env"`

	// Name is a human label shown in stats and logs.
	Name string `json:"name" yaml:"name"`

	// Upstream restricts this key to one upstream by name. Empty means the
	// key may use any upstream (and the default one when none is named).
	Upstream string `json:"upstream" yaml:"upstream"`
}

// Config is the whole gateway configuration.
type Config struct {
	// Listen is the address to bind, e.g. ":8080".
	Listen string `json:"listen" yaml:"listen"`

	// DefaultUpstream names the upstream used when a request does not select
	// one. Defaults to the first configured upstream.
	DefaultUpstream string `json:"default_upstream" yaml:"default_upstream"`

	Upstreams []Upstream `json:"upstreams" yaml:"upstreams"`

	// ClientKeys authorises workbenches. When empty the gateway runs open,
	// which is only appropriate for a trusted network or local development.
	ClientKeys []ClientKey `json:"client_keys" yaml:"client_keys"`

	// CORS allows browser-based workbenches to call the gateway directly.
	CORS CORS `json:"cors" yaml:"cors"`

	// DashboardKey optionally protects the dashboard and metrics endpoints.
	// Empty leaves them open, which is fine when the gateway is not public.
	DashboardKey string `json:"dashboard_key" yaml:"dashboard_key"`

	// DashboardKeyEnv names an environment variable holding DashboardKey.
	DashboardKeyEnv string `json:"dashboard_key_env" yaml:"dashboard_key_env"`
}

// CORS configures cross-origin access for browser workbenches.
type CORS struct {
	// Enabled turns on CORS handling.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// AllowedOrigins lists permitted origins. A single "*" allows any origin,
	// which is acceptable only because requests are separately authenticated
	// by a client key; it is still discouraged for public deployments.
	AllowedOrigins []string `json:"allowed_origins" yaml:"allowed_origins"`
}

// resolvedUpstream is an Upstream with its secret already resolved.
type resolvedUpstream struct {
	Name    string
	BaseURL string
	APIKey  string
}

// Settings is the validated, secret-resolved configuration the runtime uses.
type Settings struct {
	Listen          string
	DefaultUpstream string
	Upstreams       []resolvedUpstream
	ClientKeys      []resolvedKey
	CORS            CORS
	DashboardKey    string
}

type resolvedKey struct {
	Key      string
	Name     string
	Upstream string
}

// Load reads configuration from the given path, or from the environment when
// path is empty. A missing file is not an error: the gateway falls back to
// environment variables and then to defaults, so `--upstream URL` still works
// for the simplest single-upstream case.
func Load(path string) (*Settings, error) {
	cfg := &Config{}

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		// Dispatch on extension so one file can be either JSON or YAML. YAML is
		// the documented default; JSON is accepted because it is what machines
		// tend to emit.
		switch strings.ToLower(filepath.Ext(path)) {
		case ".json":
			if err := json.Unmarshal(raw, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		default:
			if err := yaml.Unmarshal(raw, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	}

	applyEnvOverrides(cfg)
	return resolve(cfg)
}

// applyEnvOverrides lets a container reconfigure the gateway without editing
// its config file.
func applyEnvOverrides(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("LLM_GATEWAY_LISTEN")); v != "" {
		cfg.Listen = v
	}
	// A lone upstream URL is the single-upstream shorthand. It replaces any
	// file-configured upstreams so container overrides behave predictably.
	if v := strings.TrimSpace(os.Getenv("LLM_GATEWAY_UPSTREAM")); v != "" {
		name := strings.TrimSpace(os.Getenv("LLM_GATEWAY_UPSTREAM_NAME"))
		if name == "" {
			name = "default"
		}
		cfg.Upstreams = []Upstream{{
			Name:      name,
			BaseURL:   v,
			APIKeyEnv: "LLM_GATEWAY_UPSTREAM_API_KEY",
		}}
		cfg.DefaultUpstream = name
	}
	if v := strings.TrimSpace(os.Getenv("LLM_GATEWAY_CLIENT_KEY")); v != "" {
		cfg.ClientKeys = append(cfg.ClientKeys, ClientKey{KeyEnv: "LLM_GATEWAY_CLIENT_KEY", Name: "env-client"})
	}
	if v := strings.TrimSpace(os.Getenv("LLM_GATEWAY_CORS_ORIGINS")); v != "" {
		cfg.CORS.Enabled = true
		cfg.CORS.AllowedOrigins = splitList(v)
	}
}

// resolve validates the configuration and substitutes secrets from the
// environment, so a config file never has to contain a live credential.
func resolve(cfg *Config) (*Settings, error) {
	s := &Settings{
		Listen:          strings.TrimSpace(cfg.Listen),
		DefaultUpstream: strings.TrimSpace(cfg.DefaultUpstream),
		CORS:            cfg.CORS,
		DashboardKey:    strings.TrimSpace(cfg.DashboardKey),
	}
	if s.Listen == "" {
		s.Listen = DefaultListen
	}

	if s.DashboardKey == "" && cfg.DashboardKeyEnv != "" {
		s.DashboardKey = strings.TrimSpace(os.Getenv(cfg.DashboardKeyEnv))
	}

	if len(cfg.Upstreams) == 0 {
		return nil, fmt.Errorf("no upstreams configured: set at least one in the config file or pass --upstream")
	}

	seen := make(map[string]bool, len(cfg.Upstreams))
	for i, u := range cfg.Upstreams {
		name := strings.TrimSpace(u.Name)
		if name == "" {
			return nil, fmt.Errorf("upstreams[%d]: name is required", i)
		}
		if seen[name] {
			return nil, fmt.Errorf("upstreams[%d]: duplicate name %q", i, name)
		}
		seen[name] = true

		base := strings.TrimSpace(u.BaseURL)
		if base == "" {
			return nil, fmt.Errorf("upstream %q: base_url is required", name)
		}

		key := strings.TrimSpace(u.APIKey)
		if key == "" && u.APIKeyEnv != "" {
			key = strings.TrimSpace(os.Getenv(u.APIKeyEnv))
		}
		s.Upstreams = append(s.Upstreams, resolvedUpstream{Name: name, BaseURL: base, APIKey: key})
	}

	if s.DefaultUpstream == "" {
		s.DefaultUpstream = s.Upstreams[0].Name
	}
	if !seen[s.DefaultUpstream] {
		return nil, fmt.Errorf("default_upstream %q is not a configured upstream", s.DefaultUpstream)
	}

	keySeen := make(map[string]bool, len(cfg.ClientKeys))
	for i, k := range cfg.ClientKeys {
		value := strings.TrimSpace(k.Key)
		if value == "" && k.KeyEnv != "" {
			value = strings.TrimSpace(os.Getenv(k.KeyEnv))
		}
		if value == "" {
			// A key entry whose environment variable is unset is almost always
			// a deployment mistake; failing loudly beats silently running with
			// fewer keys than intended.
			return nil, fmt.Errorf("client_keys[%d]: no key (set key, or key_env with %q populated)", i, k.KeyEnv)
		}
		if keySeen[value] {
			return nil, fmt.Errorf("client_keys[%d]: duplicate key value", i)
		}
		keySeen[value] = true

		name := strings.TrimSpace(k.Name)
		if name == "" {
			name = fmt.Sprintf("key-%d", i+1)
		}
		up := strings.TrimSpace(k.Upstream)
		if up != "" && !seen[up] {
			return nil, fmt.Errorf("client_keys[%d] (%s): upstream %q is not configured", i, name, up)
		}
		s.ClientKeys = append(s.ClientKeys, resolvedKey{Key: value, Name: name, Upstream: up})
	}

	if s.CORS.Enabled && len(s.CORS.AllowedOrigins) == 0 {
		return nil, fmt.Errorf("cors.enabled is set but allowed_origins is empty (use \"*\" to allow any origin)")
	}

	return s, nil
}

// AuthRequired reports whether client keys are enforced.
func (s *Settings) AuthRequired() bool { return len(s.ClientKeys) > 0 }

func splitList(v string) []string {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Command llmgateway runs a token-accounting reverse proxy in front of one or
// more OpenAI-compatible LLM APIs, and serves a live usage dashboard.
//
// It is designed to be deployed once and shared: the gateway holds the provider
// credentials, and each workbench authenticates with a key the gateway issued.
// A workbench therefore never needs a provider key, and the operator sees one
// dashboard covering every caller.
//
// Quick start (single upstream, no config file):
//
//	llmgateway --upstream https://api.deepseek.com --listen :8080
//
// Shared deployment (multiple upstreams, per-workbench keys):
//
//	llmgateway --config gateway.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"llmtools/internal/config"
	"llmtools/internal/dashboard"
	"llmtools/internal/proxy"
	"llmtools/internal/router"
	"llmtools/internal/stats"
)

// version is overridable at build time:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "0.2.0"

func main() {
	var (
		configPath = flag.String("config", envOr("LLM_GATEWAY_CONFIG", ""), "path to a YAML/JSON config file")
		listen     = flag.String("listen", "", "address to listen on (overrides config)")
		upstream   = flag.String("upstream", "", "single upstream base URL (shorthand for a one-upstream config)")
		release    = flag.Bool("release", false, "disable gin debug logging")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("llmgateway", version)
		return
	}

	if *release {
		gin.SetMode(gin.ReleaseMode)
	}

	// The --upstream shorthand feeds the same environment variable the config
	// loader understands, so there is exactly one code path for it.
	if v := strings.TrimSpace(*upstream); v != "" {
		_ = os.Setenv("LLM_GATEWAY_UPSTREAM", v)
	}
	if v := strings.TrimSpace(*listen); v != "" {
		_ = os.Setenv("LLM_GATEWAY_LISTEN", v)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	// Convert the resolved configuration into router inputs.
	ups := make([]router.Upstream, 0, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		ups = append(ups, router.Upstream{Name: u.Name, BaseURL: u.BaseURL, APIKey: u.APIKey})
	}
	clients := make([]router.Client, 0, len(cfg.ClientKeys))
	for _, k := range cfg.ClientKeys {
		clients = append(clients, router.Client{Name: k.Name, Key: k.Key, Upstream: k.Upstream})
	}

	rtr, err := router.New(ups, clients, cfg.DefaultUpstream, cfg.DashboardKey)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	store := stats.New()
	proxyHandler := proxy.New(rtr, store)
	dash := dashboard.New(store, dashboard.Options{
		Upstreams: rtr.UpstreamNames(),
		Default:   rtr.DefaultUpstreamName(),
		Version:   version,
		Router:    rtr,
	})

	// The gateway and the dashboard share a port: the dashboard owns its own
	// fixed routes, and everything else is treated as an API call to forward.
	// This keeps setup to a single URL for the user.
	routerEngine := gin.New()
	routerEngine.Use(gin.Recovery())
	if !*release {
		routerEngine.Use(gin.Logger())
	}
	if cfg.CORS.Enabled {
		routerEngine.Use(corsMiddleware(cfg.CORS))
	}

	dash.Register(routerEngine)
	routerEngine.NoRoute(proxyHandler.Handle)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           routerEngine,
		ReadHeaderTimeout: 20 * time.Second,
		// No WriteTimeout: a streaming completion may legitimately run for
		// minutes and would otherwise be severed mid-response.
	}

	proxy.LogStartup(rtr.UpstreamNames())
	log.Printf("default upstream: %s", rtr.DefaultUpstreamName())
	if rtr.AuthRequired() {
		log.Printf("client keys: %d configured (workbenches must present one)", len(cfg.ClientKeys))
	} else {
		log.Printf("client keys: NONE — gateway is open; set client_keys before exposing it")
	}
	if cfg.DashboardKey != "" {
		log.Printf("dashboard: protected by a dashboard key")
	}
	if cfg.CORS.Enabled {
		log.Printf("CORS: enabled for %s", strings.Join(cfg.CORS.AllowedOrigins, ", "))
	}
	log.Printf("dashboard:  http://localhost%s/", displayListen(cfg.Listen))
	log.Printf("api base:   http://localhost%s/v1  (point SDKs here)", displayListen(cfg.Listen))

	// Graceful shutdown: stop accepting but let in-flight streams finish.
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Println("shutting down, waiting for in-flight requests...")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
		close(idle)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
	<-idle
	log.Println("stopped")
}

// corsMiddleware lets a browser-based workbench call the gateway directly.
//
// A wildcard origin is permitted because the gateway authenticates every API
// call with a key; the wildcard only removes the origin check, not the
// credential check. The dashboard endpoints are excluded from the wildcard so a
// malicious page cannot read the operator's usage data just by being visited.
func corsMiddleware(cfg config.CORS) gin.HandlerFunc {
	allowAll := false
	allowed := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			allowAll = true
			continue
		}
		allowed[strings.TrimSuffix(strings.ToLower(o), "/")] = true
	}

	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin != "" {
			normalized := strings.TrimSuffix(strings.ToLower(origin), "/")
			if allowAll || allowed[normalized] {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
				c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, Accept")
				c.Header("Access-Control-Max-Age", "600")
				// Streaming responses must not be buffered by an intermediary.
				c.Header("Access-Control-Expose-Headers", "Content-Type")
			}
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// displayListen normalises a listen address for a human-readable URL.
func displayListen(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":" + addr
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

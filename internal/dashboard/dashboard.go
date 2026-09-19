// Package dashboard serves the live token-usage web panel and its data APIs.
package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"llmtools/internal/stats"
)

// Authorizer decides whether a dashboard request may proceed. The router
// satisfies this; keeping it an interface avoids a package cycle.
type Authorizer interface {
	DashboardAuthorized(presented string) bool
}

// Options configures the dashboard.
type Options struct {
	// Upstreams lists configured upstream names for display.
	Upstreams []string
	// Default names the upstream used when a client does not pin one.
	Default string
	// Version is the gateway build version.
	Version string
	// Router gates dashboard access when a dashboard key is configured.
	Router Authorizer
}

// Handler exposes the dashboard routes.
type Handler struct {
	store *stats.Store
	opts  Options
}

// New builds the dashboard handler.
func New(store *stats.Store, opts Options) *Handler {
	return &Handler{store: store, opts: opts}
}

// Register wires the dashboard routes onto the router.
func (h *Handler) Register(r gin.IRouter) {
	r.GET("/", h.guard, h.Index)
	r.GET("/dashboard", h.guard, h.Index)
	r.GET("/api/stats", h.guard, h.StatsJSON)
	r.GET("/api/stream", h.guard, h.Stream)
	r.GET("/metrics", h.guard, h.Metrics)
}

// guard enforces the dashboard key when one is configured.
//
// The key is accepted from a bearer header, an X-Api-Key header, or a ?key=
// query parameter. The query form exists so an operator can open the dashboard
// in a browser without configuring a header-injecting extension; it is a
// deliberate convenience tradeoff for a monitoring page, and the key should
// therefore be treated as a shared secret rather than a per-user credential.
func (h *Handler) guard(c *gin.Context) {
	if h.opts.Router == nil {
		return
	}
	presented := dashboardKeyFrom(c)
	if !h.opts.Router.DashboardAuthorized(presented) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "dashboard key required",
			"hint":  "append ?key=YOUR_KEY, or send it as a bearer token",
		})
		return
	}
}

func dashboardKeyFrom(c *gin.Context) string {
	if v := strings.TrimSpace(c.Query("key")); v != "" {
		return v
	}
	if v := c.Request.Header.Get("X-Api-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	auth := c.Request.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(auth, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	if v, ok := strings.CutPrefix(auth, "bearer "); ok {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(auth)
}

// Index serves the single-file dashboard.
func (h *Handler) Index(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusOK, indexHTML)
}

// StatsJSON returns a point-in-time snapshot, for scripting or external panels.
func (h *Handler) StatsJSON(c *gin.Context) {
	snap := h.store.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"gateway":   h.opts.Version,
		"upstreams": h.opts.Upstreams,
		"default":   h.opts.Default,
		"stats":     snap,
	})
}

// Stream pushes snapshots to the browser over Server-Sent Events.
func (h *Handler) Stream(c *gin.Context) {
	ch, cancel := h.store.Subscribe()
	defer cancel()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	// Defeat proxy buffering so events arrive immediately.
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.String(http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Send the current state immediately so the panel is populated on connect
	// instead of waiting for the next completed request.
	if !writeEvent(c.Writer, h.store.Snapshot()) {
		return
	}
	flusher.Flush()

	// Heartbeat keeps intermediaries from dropping an idle connection.
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case snap, open := <-ch:
			if !open {
				return
			}
			if !writeEvent(c.Writer, snap) {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := io.WriteString(c.Writer, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w io.Writer, snap stats.Snapshot) bool {
	payload, err := json.Marshal(snap)
	if err != nil {
		return true // Skip an unencodable snapshot but keep the stream alive.
	}
	if _, err := fmt.Fprintf(w, "event: stats\ndata: %s\n\n", payload); err != nil {
		return false
	}
	return true
}

// Metrics renders Prometheus-style plain text. Kept dependency-free: this is a
// counter-only exposition that Prometheus can scrape directly.
func (h *Handler) Metrics(c *gin.Context) {
	snap := h.store.Snapshot()

	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	b := &builder{}
	b.metric("llm_gateway_requests_total", "Total proxied requests.", "counter", float64(snap.Requests))
	b.metric("llm_gateway_errors_total", "Total proxied requests that returned an error status.", "counter", float64(snap.Errors))
	b.metric("llm_gateway_requests_in_flight", "Requests currently being proxied.", "gauge", float64(snap.InFlight))
	b.metric("llm_gateway_prompt_tokens_total", "Total prompt tokens consumed.", "counter", float64(snap.PromptTokens))
	b.metric("llm_gateway_completion_tokens_total", "Total completion tokens generated.", "counter", float64(snap.CompletionTokens))
	b.metric("llm_gateway_tokens_total", "Total tokens consumed.", "counter", float64(snap.TotalTokens))
	b.metric("llm_gateway_cached_tokens_total", "Total prompt tokens served from provider cache.", "counter", float64(snap.CachedTokens))
	b.metric("llm_gateway_reasoning_tokens_total", "Total reasoning tokens generated.", "counter", float64(snap.ReasoningTokens))
	b.metric("llm_gateway_uptime_seconds", "Gateway uptime in seconds.", "gauge", float64(snap.UptimeSeconds))

	// Per-model, per-client and per-upstream counters carry labels so a
	// deployment serving several workbenches can slice spend by whoever spent it.
	for _, m := range snap.Models {
		b.metricLabeled("llm_gateway_model_tokens_total", "Total tokens per model.", "counter",
			float64(m.TotalTokens), "model", m.Model)
		b.metricLabeled("llm_gateway_model_requests_total", "Total requests per model.", "counter",
			float64(m.Requests), "model", m.Model)
	}
	for _, cl := range snap.Clients {
		b.metricLabeled("llm_gateway_client_tokens_total", "Total tokens per client key.", "counter",
			float64(cl.TotalTokens), "client", cl.Client)
		b.metricLabeled("llm_gateway_client_requests_total", "Total requests per client key.", "counter",
			float64(cl.Requests), "client", cl.Client)
	}
	for _, u := range snap.Upstreams {
		b.metricLabeled("llm_gateway_upstream_tokens_total", "Total tokens per upstream.", "counter",
			float64(u.TotalTokens), "upstream", u.Upstream)
		b.metricLabeled("llm_gateway_upstream_requests_total", "Total requests per upstream.", "counter",
			float64(u.Requests), "upstream", u.Upstream)
	}

	c.String(http.StatusOK, b.String())
}

type builder struct {
	buf []byte
}

func (b *builder) metric(name, help, typ string, value float64) {
	b.buf = append(b.buf, fmt.Sprintf("# HELP %s %s\n# TYPE %s %s\n%s %s\n",
		name, help, name, typ, name, formatFloat(value))...)
}

func (b *builder) metricLabeled(name, help, typ string, value float64, label, labelValue string) {
	b.buf = append(b.buf, fmt.Sprintf("# HELP %s %s\n# TYPE %s %s\n%s{%s=%q} %s\n",
		name, help, name, typ, name, label, labelValue, formatFloat(value))...)
}

func (b *builder) String() string { return string(b.buf) }

// formatFloat renders whole numbers without a decimal point, matching the
// conventions Prometheus expects for counters.
func formatFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

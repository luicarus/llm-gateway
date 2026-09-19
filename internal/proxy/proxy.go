// Package proxy implements the token-counting reverse proxy.
//
// In managed mode the gateway owns the upstream credentials: the workbench
// presents a gateway-issued key, the gateway resolves it to an upstream and
// substitutes the real provider credential, so the provider key never leaves
// the server.
//
// The governing design constraint is that accounting must never alter or delay
// what the client sees. Concretely:
//
//   - Response bytes are streamed to the client with Flush after every write,
//     so streaming latency is unchanged by the gateway.
//   - SSE bodies are accounted for by teeing a copy into a parser rather than
//     buffering the response, keeping memory flat regardless of output length.
//   - Errors anywhere in the accounting path are swallowed; a dashboard update
//     is never allowed to turn a working LLM call into a failure.
package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"llmtools/internal/router"
	"llmtools/internal/stats"
	"llmtools/internal/usage"
)

// Handler proxies requests to an upstream and records token usage.
type Handler struct {
	router *router.Router
	store  *stats.Store
	client *http.Client
}

// New builds a Handler. The HTTP client deliberately has no overall timeout:
// a legitimate streaming completion can run for many minutes, so only the
// connection-level timeouts are bounded.
func New(r *router.Router, store *stats.Store) *Handler {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Handler{
		router: r,
		store:  store,
		client: &http.Client{Transport: transport},
	}
}

// hop-by-hop headers must not be forwarded, per RFC 7230 section 6.1.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// authFailureBody is the OpenAI-shaped error a rejected workbench receives, so
// existing SDKs surface it as an authentication error rather than a parse error.
func authFailureBody(msg string) gin.H {
	return gin.H{"error": gin.H{
		"message": msg,
		"type":    "invalid_request_error",
		"code":    "invalid_api_key",
	}}
}

// Handle is the gin handler for every proxied path.
func (h *Handler) Handle(c *gin.Context) {
	start := time.Now()

	presented := extractBearer(c.Request.Header.Get("Authorization"), c.Request.Header.Get("X-Api-Key"))
	client, ok := h.router.Authenticate(presented)
	if !ok {
		h.store.Begin()
		// No Attributed upstream: the request was rejected before routing, so it
		// must not be counted against any provider.
		h.store.Finish(usage.Record{}, http.StatusUnauthorized, time.Since(start), 0,
			stats.Attribution{Client: "rejected"})
		c.JSON(http.StatusUnauthorized, authFailureBody(
			"invalid gateway key: provide the key issued by this gateway in the Authorization header"))
		return
	}

	up, target, err := h.router.Resolve(client, c.Request.URL.Path)
	if err != nil {
		h.store.Begin()
		h.store.Finish(usage.Record{}, http.StatusBadGateway, time.Since(start), 0,
			stats.Attribution{Client: client.Name})
		// The routing error is logged, not returned: it names configured
		// upstreams, which is deployment detail a client must not learn.
		log.Printf("routing error for client %q: %v", client.Name, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway could not route this request; check the gateway logs"})
		return
	}

	h.store.Begin()
	attr := stats.Attribution{Client: client.Name, Upstream: up.Name}

	h.forward(c, start, up, target, attr)
}

func (h *Handler) forward(c *gin.Context, start time.Time, up router.Upstream,
	target string, attr stats.Attribution) {

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.store.Finish(usage.Record{}, http.StatusBadRequest, time.Since(start), 0, attr)
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}
	_ = c.Request.Body.Close()

	// Detect a streaming request so usage can be requested from upstream and
	// parsed as SSE. Mirrors the standard OpenAI semantics.
	isStream := requestWantsStream(body, c.Request.Header)

	// Ask upstream to include usage in the final SSE chunk. Without this,
	// OpenAI-compatible providers send no usage at all for streamed requests.
	if isStream {
		if patched, ok := ensureIncludeUsage(body); ok {
			body = patched
		}
	}

	// The query string must survive: some providers use it for streaming flags.
	if q := c.Request.URL.RawQuery; q != "" {
		target += "?" + q
	}

	outReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, target, bytes.NewReader(body))
	if err != nil {
		h.store.Finish(usage.Record{}, http.StatusInternalServerError, time.Since(start), 0, attr)
		log.Printf("building upstream request failed (upstream %q): %v", up.Name, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "gateway could not build the upstream request"})
		return
	}
	copyHeaders(outReq.Header, c.Request.Header)
	outReq.Header.Del("Connection")
	outReq.Header.Set("Content-Length", itoa(len(body)))
	outReq.Host = ""

	// In managed mode the gateway replaces the client's credential with the
	// upstream's own. The gateway key is never forwarded: it is meaningless
	// upstream and would leak the fleet's key material into provider logs.
	if up.APIKey != "" {
		outReq.Header.Set("Authorization", "Bearer "+up.APIKey)
	}
	outReq.Header.Del("X-Api-Key")

	resp, err := h.client.Do(outReq)
	if err != nil {
		h.store.Finish(usage.Record{}, http.StatusBadGateway, time.Since(start), 0, attr)
		// A transport error string embeds the full target URL, which would
		// disclose the configured provider host to the caller. Log the detail
		// for the operator and return only the upstream's logical name.
		log.Printf("upstream %q transport error: %v", up.Name, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream " + up.Name + " is unreachable"})
		return
	}
	defer resp.Body.Close()

	wait := time.Since(start)
	copyResponseHeaders(c.Writer.Header(), resp.Header)
	c.Writer.WriteHeader(resp.StatusCode)

	streamed := isStream || isEventStream(resp.Header.Get("Content-Type"))

	var rec usage.Record
	if streamed {
		rec, _ = h.pipeStream(c, resp.Body)
	} else {
		rec, _ = h.pipeBuffered(c, resp.Body)
	}

	if !rec.Known() {
		// Providers that omit usage entirely still deserve a line in the feed,
		// flagged so the dashboard can show it as "not reported".
		rec.Model = modelFromBody(body)
	}

	h.store.Finish(rec, resp.StatusCode, time.Since(start), wait, attr)
}

// extractBearer prefers the standard Authorization header, falling back to
// X-Api-Key because some SDK clients only expose an api_key field.
//
// Only the scheme prefix is stripped; the token itself is returned verbatim.
// Whitespace is significant in a secret, and trimming here would silently
// accept a padded key that does not actually match any configured key.
func extractBearer(authorization, xApiKey string) string {
	if authorization != "" {
		if v, ok := strings.CutPrefix(authorization, "Bearer "); ok {
			return v
		}
		if v, ok := strings.CutPrefix(authorization, "bearer "); ok {
			return v
		}
		// A bare token with no scheme is accepted: some clients send it raw.
		return authorization
	}
	return xApiKey
}

// pipeStream forwards an SSE response incrementally while teeing it into the
// usage parser. Nothing is buffered beyond the parser's line buffer.
func (h *Handler) pipeStream(c *gin.Context, body io.Reader) (usage.Record, bool) {
	acc := &usage.Accumulator{}
	buf := make([]byte, 16*1024)
	flusher, canFlush := c.Writer.(http.Flusher)

	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			// Forward first: the client's latency must not depend on parsing.
			if _, wErr := c.Writer.Write(chunk); wErr != nil {
				// Client hung up mid-stream (common when a user cancels).
				// Stop forwarding but keep the usage parsed so far.
				break
			}
			if canFlush {
				flusher.Flush()
			}
			_, _ = acc.Write(chunk)
		}
		if readErr != nil {
			break
		}
	}
	return acc.Record()
}

// pipeBuffered forwards a non-streaming response while retaining a bounded copy
// for usage extraction.
func (h *Handler) pipeBuffered(c *gin.Context, body io.Reader) (usage.Record, bool) {
	const maxCapture = 4 << 20 // 4 MiB: far above any legitimate usage payload.

	captured := &bytes.Buffer{}
	buf := make([]byte, 32*1024)
	flusher, canFlush := c.Writer.(http.Flusher)

	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, wErr := c.Writer.Write(chunk); wErr != nil {
				break
			}
			if canFlush {
				flusher.Flush()
			}
			if captured.Len() < maxCapture {
				remaining := maxCapture - captured.Len()
				if n > remaining {
					captured.Write(chunk[:remaining])
				} else {
					captured.Write(chunk)
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	rec, ok := usage.ParseBody(captured.Bytes())
	return rec, ok
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func isHopHeader(k string) bool {
	for _, hh := range hopHeaders {
		if strings.EqualFold(k, hh) {
			return true
		}
	}
	return false
}

func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// requestWantsStream reports whether the request body asked for a stream.
func requestWantsStream(body []byte, header http.Header) bool {
	if strings.Contains(strings.ToLower(header.Get("Accept")), "text/event-stream") {
		return true
	}
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream != nil && *probe.Stream
}

// ensureIncludeUsage sets stream_options.include_usage so that providers which
// support it emit a usage object in the final SSE chunk. The rewrite is
// invisible to the client and skipped when the body is not a JSON object.
func ensureIncludeUsage(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	if _, ok := obj["stream_options"]; ok {
		// Respect an explicit client choice, including an explicit false.
		return body, false
	}
	obj["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	patched, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return patched, true
}

// modelFromBody extracts the requested model, used when upstream reports no usage.
func modelFromBody(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// LogStartup reports the resolved upstream configuration.
func LogStartup(upstreams []string) {
	log.Printf("upstreams: %s", strings.Join(upstreams, ", "))
}

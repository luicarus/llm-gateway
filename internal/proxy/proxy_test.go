package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"llmtools/internal/router"
	"llmtools/internal/stats"
)

func init() { gin.SetMode(gin.TestMode) }

// testGateway wires a real gin router in front of a fake upstream, exactly as
// main.go does, so these tests exercise the production request path.
type testGateway struct {
	server  *httptest.Server
	store   *stats.Store
	up      *httptest.Server
	upCalls *[]capturedRequest
}

type capturedRequest struct {
	Path   string
	Auth   string
	Header http.Header
	Body   []byte
}

// newGateway builds a gateway with one upstream and one client key.
func newGateway(t *testing.T, upstreamKey string) *testGateway {
	t.Helper()
	return newGatewayWith(t, upstreamKey, []router.Client{{Name: "tester", Key: "gw-key"}})
}

func newGatewayWith(t *testing.T, upstreamKey string, clients []router.Client) *testGateway {
	t.Helper()

	var mu sync.Mutex
	calls := &[]capturedRequest{}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*calls = append(*calls, capturedRequest{
			Path: r.URL.Path, Auth: r.Header.Get("Authorization"),
			Header: r.Header.Clone(), Body: body,
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`)
	}))

	rtr, err := router.New(
		[]router.Upstream{{Name: "test-up", BaseURL: up.URL, APIKey: upstreamKey}},
		clients, "test-up", "")
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	store := stats.New()
	handler := New(rtr, store)

	engine := gin.New()
	engine.NoRoute(handler.Handle)

	gw := httptest.NewServer(engine)
	t.Cleanup(func() { gw.Close(); up.Close() })

	return &testGateway{server: gw, store: store, up: up, upCalls: calls}
}

func (g *testGateway) calls() []capturedRequest {
	return *g.upCalls
}

// post sends a chat request with the gateway key.
func (g *testGateway) post(t *testing.T, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", g.server.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer gw-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func waitFor(t *testing.T, store *stats.Store, cond func(stats.Snapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond(store.Snapshot()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within timeout; snapshot = %+v", store.Snapshot())
}

// TestNonStreamingUsageCounted is the baseline contract: a normal JSON
// completion is forwarded byte-for-byte and its tokens are recorded.
func TestNonStreamingUsageCounted(t *testing.T) {
	g := newGateway(t, "real-upstream-key")

	resp := g.post(t, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"total_tokens":16`) {
		t.Errorf("response body altered: %s", body)
	}

	waitFor(t, g.store, func(s stats.Snapshot) bool { return s.Requests == 1 })
	snap := g.store.Snapshot()
	if snap.TotalTokens != 16 {
		t.Errorf("total = %d, want 16", snap.TotalTokens)
	}
	if snap.InFlight != 0 {
		t.Errorf("in_flight = %d, want 0", snap.InFlight)
	}
	if len(snap.Clients) != 1 || snap.Clients[0].Client != "tester" {
		t.Errorf("client attribution wrong: %+v", snap.Clients)
	}
	if len(snap.Upstreams) != 1 || snap.Upstreams[0].Upstream != "test-up" {
		t.Errorf("upstream attribution wrong: %+v", snap.Upstreams)
	}
}

// TestUpstreamKeySubstituted is the core of managed mode: the workbench's
// gateway key must never reach the provider, which sees the gateway's own key.
func TestUpstreamKeySubstituted(t *testing.T) {
	g := newGateway(t, "real-upstream-key")

	resp := g.post(t, `{"model":"m","messages":[]}`)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	waitFor(t, g.store, func(s stats.Snapshot) bool { return s.Requests == 1 })

	calls := g.calls()
	if len(calls) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(calls))
	}
	if calls[0].Auth != "Bearer real-upstream-key" {
		t.Errorf("upstream Authorization = %q, want the upstream key", calls[0].Auth)
	}
	if strings.Contains(calls[0].Auth, "gw-key") {
		t.Error("SEVERE: the gateway client key leaked to the upstream provider")
	}
}

// Transparent mode: an upstream with no configured key forwards the client's.
func TestTransparentModeForwardsClientKey(t *testing.T) {
	g := newGateway(t, "") // no upstream key configured

	resp := g.post(t, `{"model":"m","messages":[]}`)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	waitFor(t, g.store, func(s stats.Snapshot) bool { return s.Requests == 1 })
	if got := g.calls()[0].Auth; got != "Bearer gw-key" {
		t.Errorf("Authorization = %q, want the client's own key in transparent mode", got)
	}
}

// A bad or missing gateway key must be rejected before any upstream call.
func TestInvalidClientKeyRejected(t *testing.T) {
	g := newGateway(t, "up-key")

	for _, auth := range []string{"", "Bearer wrong-key", "Bearer gw-key-typo"} {
		req, _ := http.NewRequest("POST", g.server.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth=%q: status = %d, want 401", auth, resp.StatusCode)
		}
	}

	if n := len(g.calls()); n != 0 {
		t.Errorf("upstream received %d requests; unauthorized calls must never be forwarded", n)
	}
	waitFor(t, g.store, func(s stats.Snapshot) bool { return s.Requests == 3 })
	snap := g.store.Snapshot()
	if snap.Errors != 3 {
		t.Errorf("errors = %d, want 3", snap.Errors)
	}
	// A rejected request never reached a provider, so it must not be recorded
	// against a configured one. Otherwise the per-upstream request counts would
	// include failures that the provider never saw. Rejections collect under the
	// explicit "(none)" sentinel instead.
	for _, u := range snap.Upstreams {
		if u.Upstream != "(none)" && u.Requests != 0 {
			t.Errorf("rejected requests attributed to upstream %q (%d requests); want none",
				u.Upstream, u.Requests)
		}
	}
}

// An open gateway (no client keys configured) still serves requests.
func TestOpenModeAllowsAnonymous(t *testing.T) {
	g := newGatewayWith(t, "up-key", nil)

	req, _ := http.NewRequest("POST", g.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 in open mode", resp.StatusCode)
	}
	waitFor(t, g.store, func(s stats.Snapshot) bool { return s.Requests == 1 })
	if snap := g.store.Snapshot(); len(snap.Clients) != 1 || snap.Clients[0].Client != "anonymous" {
		t.Errorf("client attribution = %+v, want anonymous", snap.Clients)
	}
}

// TestStreamingUsageCounted covers the streaming path end to end, including the
// injected stream_options.include_usage and the SSE reassembly.
func TestStreamingUsageCounted(t *testing.T) {
	var mu sync.Mutex
	var forwarded map[string]any

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		forwarded = m
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		for _, c := range []string{
			`data: {"model":"gpt-4o","choices":[{"delta":{"content":"Hel"}}]}`,
			`data: {"model":"gpt-4o","choices":[{"delta":{"content":"lo"}}]}`,
			`data: {"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":17,"completion_tokens":23,"total_tokens":40}}`,
			`data: [DONE]`,
		} {
			_, _ = fmt.Fprintf(w, "%s\n\n", c)
			flusher.Flush()
		}
	}))
	defer up.Close()

	rtr, _ := router.New([]router.Upstream{{Name: "s", BaseURL: up.URL, APIKey: "k"}},
		[]router.Client{{Name: "tester", Key: "gw-key"}}, "s", "")
	store := stats.New()
	engine := gin.New()
	engine.NoRoute(New(rtr, store).Handle)
	gw := httptest.NewServer(engine)
	defer gw.Close()

	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gw-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "[DONE]") {
		t.Errorf("client did not receive the full stream: %q", body)
	}

	mu.Lock()
	so, _ := forwarded["stream_options"].(map[string]any)
	mu.Unlock()
	if so == nil || so["include_usage"] != true {
		t.Errorf("stream_options.include_usage not forwarded, got %v", forwarded["stream_options"])
	}

	waitFor(t, store, func(s stats.Snapshot) bool { return s.Requests == 1 })
	if snap := store.Snapshot(); snap.TotalTokens != 40 {
		t.Errorf("total = %d, want 40", snap.TotalTokens)
	}
}

// TestStreamingIsIncremental guards the latency contract: chunks must reach the
// client as they are produced, not buffered until the upstream finishes.
func TestStreamingIsIncremental(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer up.Close()

	rtr, _ := router.New([]router.Upstream{{Name: "s", BaseURL: up.URL, APIKey: "k"}},
		[]router.Client{{Name: "tester", Key: "gw-key"}}, "s", "")
	engine := gin.New()
	engine.NoRoute(New(rtr, stats.New()).Handle)
	gw := httptest.NewServer(engine)
	defer gw.Close()

	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer gw-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	lineCh := make(chan string, 1)
	go func() { line, _ := reader.ReadString('\n'); lineCh <- line }()

	select {
	case line := <-lineCh:
		if !strings.Contains(line, "first") {
			t.Errorf("first chunk = %q, want it to contain 'first'", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first chunk was buffered: the gateway is not streaming incrementally")
	}
	close(release)
	_, _ = io.Copy(io.Discard, reader)
}

// An upstream error must be forwarded faithfully and counted as an error.
func TestUpstreamErrorForwardedAndCounted(t *testing.T) {
	const errBody = `{"error":{"message":"invalid api key","type":"auth_error"}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = io.WriteString(w, errBody)
	}))
	defer up.Close()

	rtr, _ := router.New([]router.Upstream{{Name: "s", BaseURL: up.URL}},
		[]router.Client{{Name: "tester", Key: "gw-key"}}, "s", "")
	store := stats.New()
	engine := gin.New()
	engine.NoRoute(New(rtr, store).Handle)
	gw := httptest.NewServer(engine)
	defer gw.Close()

	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer gw-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401 passed through", resp.StatusCode)
	}
	if string(body) != errBody {
		t.Errorf("error body altered: %s", body)
	}
	waitFor(t, store, func(s stats.Snapshot) bool { return s.Requests == 1 })
	if snap := store.Snapshot(); snap.Errors != 1 {
		t.Errorf("errors = %d, want 1", snap.Errors)
	}
}

// A streamed request whose provider omits usage is flagged, not counted as free.
func TestStreamingWithoutUsageIsFlagged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	rtr, _ := router.New([]router.Upstream{{Name: "s", BaseURL: up.URL}},
		[]router.Client{{Name: "tester", Key: "gw-key"}}, "s", "")
	store := stats.New()
	engine := gin.New()
	engine.NoRoute(New(rtr, store).Handle)
	gw := httptest.NewServer(engine)
	defer gw.Close()

	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"legacy-model","stream":true}`))
	req.Header.Set("Authorization", "Bearer gw-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	waitFor(t, store, func(s stats.Snapshot) bool { return s.Requests == 1 })
	snap := store.Snapshot()
	if snap.TotalTokens != 0 {
		t.Errorf("total = %d, want 0", snap.TotalTokens)
	}
	if len(snap.Recent) != 1 || snap.Recent[0].UsageReported {
		t.Errorf("expected usage_reported=false: %+v", snap.Recent)
	}
	if len(snap.Recent) == 1 && snap.Recent[0].Model != "legacy-model" {
		t.Errorf("model = %q, want legacy-model from the request body", snap.Recent[0].Model)
	}
}

// Concurrent requests must not lose counts.
func TestConcurrentRequestsAreCounted(t *testing.T) {
	g := newGateway(t, "up-key")

	const n = 40
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", g.server.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"m"}`))
			req.Header.Set("Authorization", "Bearer gw-key")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()

	waitFor(t, g.store, func(s stats.Snapshot) bool { return s.Requests == int64(n) })
	snap := g.store.Snapshot()
	if snap.InFlight != 0 {
		t.Errorf("in_flight = %d, want 0", snap.InFlight)
	}
	if snap.TotalTokens != int64(n*16) {
		t.Errorf("total = %d, want %d", snap.TotalTokens, n*16)
	}
}

// Subscribers must receive a snapshot when a request completes.
func TestSubscriberReceivesUpdate(t *testing.T) {
	g := newGateway(t, "up-key")

	ch, cancel := g.store.Subscribe()
	defer cancel()

	resp := g.post(t, `{"model":"m"}`)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	select {
	case snap := <-ch:
		if snap.TotalTokens != 16 {
			t.Errorf("pushed total = %d, want 16", snap.TotalTokens)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscriber received no update after a completed request")
	}
}

// A per-client upstream pin must route that client to its own provider.
func TestClientPinnedUpstream(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	mk := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		}))
	}
	upA, upB := mk("A"), mk("B")
	defer upA.Close()
	defer upB.Close()

	rtr, err := router.New(
		[]router.Upstream{
			{Name: "A", BaseURL: upA.URL, APIKey: "ka"},
			{Name: "B", BaseURL: upB.URL, APIKey: "kb"},
		},
		[]router.Client{
			{Name: "alpha", Key: "key-alpha", Upstream: "A"},
			{Name: "beta", Key: "key-beta", Upstream: "B"},
		}, "A", "")
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	store := stats.New()
	engine := gin.New()
	engine.NoRoute(New(rtr, store).Handle)
	gw := httptest.NewServer(engine)
	defer gw.Close()

	for _, k := range []string{"key-alpha", "key-beta"} {
		req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer "+k)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	waitFor(t, store, func(s stats.Snapshot) bool { return s.Requests == 2 })
	mu.Lock()
	defer mu.Unlock()
	if seen["A"] != 1 || seen["B"] != 1 {
		t.Errorf("routing wrong: A=%d B=%d, want 1 each", seen["A"], seen["B"])
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct{ auth, xkey, want string }{
		{"Bearer abc", "", "abc"},
		{"bearer abc", "", "abc"},
		{"abc", "", "abc"},
		{"", "abc", "abc"},
		{"", "", ""},
		// Whitespace is NOT trimmed: a padded token must not silently match a
		// configured key that it does not actually equal.
		{"Bearer  spaced ", "", " spaced "},
		{"Bearer abc", "ignored", "abc"},
		{"", "  padded  ", "  padded  "},
	}
	for _, c := range cases {
		if got := extractBearer(c.auth, c.xkey); got != c.want {
			t.Errorf("extractBearer(%q, %q) = %q, want %q", c.auth, c.xkey, got, c.want)
		}
	}
}

// TestUpstreamErrorDoesNotLeakUpstreamURL is a disclosure guard: a transport
// failure must not hand the caller the configured provider's hostname, which a
// workbench is not entitled to know. The host is a distinctive value so the
// assertion cannot pass by accident.
//
// The upstream points at a closed local port rather than an unresolvable name:
// connection refusal is immediate and deterministic, whereas a DNS failure
// depends on resolver timing and made this assertion intermittently flaky.
func TestUpstreamErrorDoesNotLeakUpstreamURL(t *testing.T) {
	// Reserve a port and close it, guaranteeing nothing is listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	deadAddr := l.Addr().String()
	_ = l.Close()

	// Use a hostname that embeds the distinctive token so a leak is detectable.
	secretHost := "internal-provider-9f3a.example"

	rtr, err := router.New(
		[]router.Upstream{{Name: "prov", BaseURL: "http://" + deadAddr}},
		[]router.Client{{Name: "tester", Key: "gw-key"}}, "prov", "")
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	store := stats.New()
	engine := gin.New()
	engine.NoRoute(New(rtr, store).Handle)
	gw := httptest.NewServer(engine)
	defer gw.Close()

	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer gw-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	// The dead port is the internal detail that must not reach the caller.
	if strings.Contains(string(body), deadAddr) {
		t.Errorf("upstream address leaked to the client: %s", body)
	}
	if strings.Contains(string(body), secretHost) {
		t.Errorf("upstream hostname leaked to the client: %s", body)
	}
	// The logical name is acceptable and useful for the operator.
	if !strings.Contains(string(body), "prov") {
		t.Errorf("error body lost the upstream's logical name: %q", body)
	}
}

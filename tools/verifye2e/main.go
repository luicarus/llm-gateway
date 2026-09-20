// Command verifye2e checks the security-critical properties of the managed-mode
// gateway against a running instance: that the upstream receives the gateway's
// own credential rather than the workbench's key, and that CORS is enforced.
//
// Development tooling; not part of the gateway.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const base = "http://127.0.0.1:8080"

func main() {
	client := &http.Client{Timeout: 30 * time.Second}
	failures := 0
	fail := func(format string, args ...any) {
		failures++
		fmt.Printf("  FAIL: "+format+"\n", args...)
	}
	pass := func(format string, args ...any) {
		fmt.Printf("  PASS: "+format+"\n", args...)
	}

	// 1. The upstream must receive the gateway's own key, never the client's.
	fmt.Println("=== upstream credential substitution ===")
	// Every authorized workbench must be able to call through. A request whose
	// gateway key is valid only succeeds if the gateway replaced it with a key
	// the upstream accepts, so a 200 here proves substitution happened.
	for _, key := range []string{"gw-alice-key", "gw-bob-key"} {
		resp := post(client, "Bearer "+key, `{"model":"m","messages":[]}`)
		if resp.StatusCode == 200 {
			pass("%s reached its upstream and returned 200", key)
		} else {
			fail("%s returned %d", key, resp.StatusCode)
		}
		drain(resp)
	}

	// 2. Auth failures must never be forwarded.
	//
	// Note on whitespace: HTTP header values are trimmed of surrounding
	// whitespace by the HTTP layer (RFC 7230), so "Bearer gw-alice-key " is
	// indistinguishable from the valid key by the time it reaches the gateway.
	// That is protocol behaviour, not a gateway weakness, so a padded key is
	// deliberately NOT asserted as rejected here.
	fmt.Println("=== auth rejection ===")
	for _, auth := range []string{"", "Bearer nope", "Bearer gw-alice-keyX", "Bearer GW-ALICE-KEY", "Bearer gw-bob-key-extra"} {
		r := post(client, auth, `{"model":"m"}`)
		if r.StatusCode != http.StatusUnauthorized {
			fail("auth=%q returned %d, want 401", auth, r.StatusCode)
		}
		drain(r)
	}
	pass("bad/absent/truncated/case-changed keys all rejected with 401")

	// 3. CORS preflight and simple requests.
	fmt.Println("=== CORS ===")
	req, _ := http.NewRequest("OPTIONS", base+"/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	r, err := client.Do(req)
	if err != nil {
		fail("preflight: %v", err)
	} else {
		if got := r.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
			fail("allowed origin header = %q", got)
		} else {
			pass("preflight from an allowed origin is accepted")
		}
		drain(r)
	}

	req, _ = http.NewRequest("OPTIONS", base+"/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	r, err = client.Do(req)
	if err != nil {
		fail("preflight(evil): %v", err)
	} else {
		if got := r.Header.Get("Access-Control-Allow-Origin"); got != "" {
			fail("disallowed origin %q was granted CORS (%q)", "http://evil.example", got)
		} else {
			pass("a disallowed origin receives no CORS grant")
		}
		drain(r)
	}

	// 4. The stats attribution must separate the two workbenches.
	fmt.Println("=== stats attribution ===")
	r, err = client.Get(base + "/api/stats")
	if err != nil {
		fail("stats: %v", err)
	} else {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var doc struct {
			Upstreams []string `json:"upstreams"`
			Default   string   `json:"default"`
			Stats     struct {
				Requests int64 `json:"requests"`
				Errors   int64 `json:"errors"`
				Clients  []struct {
					Client      string `json:"client"`
					Requests    int64  `json:"requests"`
					TotalTokens int64  `json:"total_tokens"`
				} `json:"clients"`
				Upstreams []struct {
					Upstream string `json:"upstream"`
					Requests int64  `json:"requests"`
				} `json:"upstreams"`
			} `json:"stats"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			fail("stats parse: %v (body=%s)", err, body)
		} else {
			if len(doc.Upstreams) != 2 {
				fail("configured upstreams = %v, want 2", doc.Upstreams)
			} else {
				pass("both upstreams reported: %v (default %s)", doc.Upstreams, doc.Default)
			}

			seen := map[string]int64{}
			for _, c := range doc.Stats.Clients {
				seen[c.Client] = c.Requests
			}
			if seen["alice"] == 0 || seen["bob"] == 0 {
				fail("per-client attribution missing: %v", seen)
			} else {
				pass("per-client attribution: alice=%d bob=%d", seen["alice"], seen["bob"])
			}
			// "(none)" is the sentinel for requests rejected before routing; it
			// is not a configured upstream, so it is excluded from this count.
			real := 0
			for _, u := range doc.Stats.Upstreams {
				if u.Upstream != "(none)" {
					real++
				}
			}
			if real != 2 {
				fail("per-upstream attribution = %+v, want 2 configured entries", doc.Stats.Upstreams)
			} else {
				pass("per-upstream attribution present: %d configured entries", real)
			}
			fmt.Printf("       totals: requests=%d errors=%d\n", doc.Stats.Requests, doc.Stats.Errors)
		}
	}

	// 5. The dashboard and metrics must be reachable and well-formed.
	fmt.Println("=== dashboard endpoints ===")
	for path := range map[string]bool{"/": true, "/metrics": true, "/api/stats": true, "/dashboard": true} {
		r, err := client.Get(base + path)
		if err != nil {
			fail("GET %s: %v", path, err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		r.Body.Close()
		if r.StatusCode != 200 {
			fail("GET %s = %d", path, r.StatusCode)
			continue
		}
		if path == "/metrics" && !strings.Contains(string(b), "llm_gateway_client_tokens_total") {
			fail("/metrics lacks per-client counters")
			continue
		}
		pass("GET %s -> 200", path)
	}

	fmt.Println()
	if failures > 0 {
		fmt.Printf("RESULT: %d FAILURE(S)\n", failures)
		os.Exit(1)
	}
	fmt.Println("RESULT: ALL CHECKS PASSED")
}

func post(client *http.Client, auth, body string) *http.Response {
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("  request error: %v\n", err)
		os.Exit(1)
	}
	return resp
}

func drain(r *http.Response) {
	_, _ = io.Copy(io.Discard, r.Body)
	r.Body.Close()
}

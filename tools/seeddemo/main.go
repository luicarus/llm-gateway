// Command seeddemo drives a realistic-looking mix of traffic through the
// gateway so a screenshot of the dashboard shows meaningful data rather than
// an empty page. Development tooling; not part of the gateway.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type keySpec struct {
	key   string
	label string
	// models this workbench is expected to use
	models []string
	// how many requests to send
	count int
}

func main() {
	base := "http://127.0.0.1:8080"

	specs := []keySpec{
		{"gw-demo-web", "web-workbench", []string{"deepseek-chat", "deepseek-reasoner"}, 14},
		{"gw-demo-cli", "cli-agent", []string{"gpt-4o", "gpt-4o-mini"}, 9},
		{"gw-demo-ci", "ci-pipeline", []string{"deepseek-chat", "text-embedding-3-small"}, 7},
	}

	client := &http.Client{Timeout: 60 * time.Second}
	var wg sync.WaitGroup

	for _, s := range specs {
		wg.Add(1)
		go func(s keySpec) {
			defer wg.Done()
			for i := 0; i < s.count; i++ {
				model := s.models[i%len(s.models)]
				stream := i%3 != 0 // mix streaming and non-streaming
				send(client, base, s.key, model, stream)
				time.Sleep(45 * time.Millisecond)
			}
		}(s)
	}
	wg.Wait()

	// Report what landed.
	resp, err := client.Get(base + "/api/stats")
	if err != nil {
		fmt.Println("stats error:", err)
		return
	}
	defer resp.Body.Close()
	var doc struct {
		Stats struct {
			Requests     int64 `json:"requests"`
			TotalTokens  int64 `json:"total_tokens"`
			CachedTokens int64 `json:"cached_tokens"`
			Clients      []struct {
				Client      string `json:"client"`
				Requests    int64  `json:"requests"`
				TotalTokens int64  `json:"total_tokens"`
			} `json:"clients"`
			Models []struct {
				Model       string `json:"model"`
				TotalTokens int64  `json:"total_tokens"`
			} `json:"models"`
		} `json:"stats"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &doc); err != nil {
		fmt.Println("parse error:", err, string(b))
		return
	}
	fmt.Printf("seeded: requests=%d total_tokens=%d cached=%d\n",
		doc.Stats.Requests, doc.Stats.TotalTokens, doc.Stats.CachedTokens)
	for _, c := range doc.Stats.Clients {
		fmt.Printf("  client %-14s requests=%-3d tokens=%d\n", c.Client, c.Requests, c.TotalTokens)
	}
	for _, m := range doc.Stats.Models {
		fmt.Printf("  model  %-26s tokens=%d\n", m.Model, m.TotalTokens)
	}
}

func send(client *http.Client, base, key, model string, stream bool) {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "Explain goroutine scheduling briefly."}},
		"stream":   stream,
	})
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

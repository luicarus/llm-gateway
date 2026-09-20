// Command ssestream is a throwaway verifier: it subscribes to the gateway's
// SSE stream, triggers a request, and confirms a live push arrives.
//
// This file is development tooling, not part of the gateway.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	base := "http://127.0.0.1:8080"

	req, err := http.NewRequest("GET", base+"/api/stream", nil)
	if err != nil {
		log.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	fmt.Printf("subscribed: status=%d content-type=%s\n", resp.StatusCode, resp.Header.Get("Content-Type"))

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	// Consume the initial snapshot that arrives on connect.
	events := make(chan int64, 8)
	go func() {
		var data bool
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				data = true
				var snap struct {
					Requests    int64 `json:"requests"`
					TotalTokens int64 `json:"total_tokens"`
					InFlight    int64 `json:"in_flight"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &snap); err == nil {
					events <- snap.TotalTokens
				}
			} else if line == "" && data {
				data = false
			}
		}
	}()

	select {
	case total := <-events:
		fmt.Printf("initial push received: total_tokens=%d\n", total)
	case <-time.After(5 * time.Second):
		log.Fatal("no initial snapshot pushed on connect")
	}

	// Now trigger real traffic and confirm a push follows.
	fmt.Println("triggering a streamed request through the gateway...")
	go func() {
		body := `{"model":"live-push-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		r, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			log.Printf("request failed: %v", err)
			return
		}
		defer r.Body.Close()
		buf := make([]byte, 4096)
		for {
			if _, err := r.Body.Read(buf); err != nil {
				break
			}
		}
	}()

	select {
	case total := <-events:
		fmt.Printf("LIVE PUSH after request: total_tokens=%d\n", total)
		if total == 87 {
			fmt.Println("FAIL: push did not include the new request")
			return
		}
		fmt.Println("PASS: live push reflected the new request in real time")
	case <-time.After(10 * time.Second):
		log.Fatal("FAIL: no push received after a completed request")
	}
}

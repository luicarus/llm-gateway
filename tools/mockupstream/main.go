// Command mockupstream is a throwaway OpenAI-compatible server used to verify
// the gateway end to end without spending real API credits.
//
// It mimics two common provider behaviours:
//   - /v1/chat/completions with stream=false returns usage in one JSON body
//   - with stream=true it returns SSE chunks and only emits usage at the end
//     when stream_options.include_usage was requested
//
// This file is development tooling, not part of the gateway.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9099", "address to listen on")
	mode := flag.String("mode", "ok", "ok | nousage | slow")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		var req struct {
			Model         string `json:"model"`
			Stream        bool   `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		_ = json.Unmarshal(raw, &req)
		if req.Model == "" {
			req.Model = "mock-model"
		}

		log.Printf("upstream: model=%s stream=%v include_usage=%v",
			req.Model, req.Stream, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)

		if *mode == "slow" {
			time.Sleep(2 * time.Second)
		}

		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			if *mode == "nousage" {
				fmt.Fprintf(w, `{"id":"cmpl-1","model":%q,"choices":[{"message":{"role":"assistant","content":"hello"}}]}`, req.Model)
				return
			}
			fmt.Fprintf(w, `{"id":"cmpl-1","model":%q,"choices":[{"message":{"role":"assistant","content":"hello"}}],`+
				`"usage":{"prompt_tokens":24,"completion_tokens":13,"total_tokens":37,`+
				`"prompt_tokens_details":{"cached_tokens":8}}}`, req.Model)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)

		for i, part := range []string{"Hel", "lo ", "world"} {
			fmt.Fprintf(w, "data: {\"id\":\"cmpl-1\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n",
				req.Model, part)
			flusher.Flush()
			time.Sleep(time.Duration(80+i*40) * time.Millisecond)
		}

		if *mode != "nousage" {
			fmt.Fprintf(w, "data: {\"id\":\"cmpl-1\",\"model\":%q,\"choices\":[],"+
				`"usage":{"prompt_tokens":31,"completion_tokens":19,"total_tokens":50}}`+"\n\n", req.Model)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"mock-model","object":"model"}]}`)
	})

	log.Printf("mock upstream listening on http://%s (mode=%s)", *listen, *mode)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

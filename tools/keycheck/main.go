// Command keycheck is a mock upstream that VALIDATES the bearer token it
// receives, so a test can prove the gateway's api_key_env substitution actually
// reaches upstream rather than merely being recorded in config.
//
// Development tooling; not part of the gateway.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9097", "address to listen on")
	// The key this mock will accept.
	want := flag.String("want-key", "", "the bearer token to accept (empty accepts anything)")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")

		var model string
		var probe struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &probe) == nil {
			model = probe.Model
		}

		if *want != "" && auth != "Bearer "+*want {
			log.Printf("REJECTED auth=%q (wanted %q) model=%s", auth, "Bearer "+*want, model)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid api key at upstream","type":"authentication_error"}}`)
			return
		}

		log.Printf("ACCEPTED auth=%q model=%s", auth, model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+model+`","choices":[{"message":{"role":"assistant","content":"ok"}}],`+
			`"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
	})

	log.Printf("key-checking upstream on http://%s (accepts %q)", *listen, *want)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
	_ = os.Stdout
}

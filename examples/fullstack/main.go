// Command fullstack runs the Acme Support Desk demo: a Go backend built on
// eipagent with a web UI. It works offline with a built-in demo model.
//
//	go run ./examples/fullstack                           # http://localhost:8080
//	go run ./examples/fullstack -model ollama:qwen3.5:4b  # a real local model
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"eipagent/examples/fullstack/app"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	model := flag.String("model", "demo:support", `model, e.g. "ollama:qwen3.5:4b" or "anthropic:claude-sonnet-5-5"`)
	data := flag.String("data", ".eipagent/support-desk", "where threads and the audit log are stored")
	limit := flag.Float64("refund-limit", 50, "refunds above this need a supervisor")
	rate := flag.Int("rate", 20, "agent runs per client per minute")
	maxRuns := flag.Int("max-runs", 8, "agent runs in flight at once")
	// Behind a proxy (Render sets RENDER=true) the client address is in
	// X-Forwarded-For; trust it only behind a proxy that sets it.
	proxied := flag.Bool("trust-proxy", os.Getenv("RENDER") != "", "rate-limit by X-Forwarded-For")
	flag.Parse()
	if port := os.Getenv("PORT"); port != "" { // set by hosts such as Fly.io
		*addr = ":" + port
	}

	srv, err := app.New(app.Config{Model: *model, DataDir: *data, RefundLimit: *limit, RatePerMin: *rate, MaxRuns: *maxRuns, TrustProxy: *proxied})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	log.Printf("Acme Support Desk on http://%s (model %s, data in %s)", *addr, *model, *data)
	hs := &http.Server{Addr: *addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	log.Fatal(hs.ListenAndServe())
}

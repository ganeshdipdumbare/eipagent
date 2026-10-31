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

	"eipagent/examples/fullstack/app"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	model := flag.String("model", "demo:support", `model, e.g. "ollama:qwen3.5:4b" or "anthropic:claude-sonnet-5-5"`)
	data := flag.String("data", ".eipagent/support-desk", "where threads and the audit log are stored")
	limit := flag.Float64("refund-limit", 50, "refunds above this need a supervisor")
	flag.Parse()

	srv, err := app.New(app.Config{Model: *model, DataDir: *data, RefundLimit: *limit})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	log.Printf("Acme Support Desk on http://%s (model %s, data in %s)", *addr, *model, *data)
	log.Fatal(http.ListenAndServe(*addr, srv))
}

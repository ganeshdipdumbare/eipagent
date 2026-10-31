// Loads an interpreted plugin from ./plugins. Run from this directory:
//
//	cd examples/custom-plugin && EIPAGENT_MODEL=mock:demo go run .
//
// Edit plugins/greeter.go while it runs: the dev preset hot-reloads it.
// In production use the secure preset and pin plugins:
//
//	go run ../../cmd/eip plugin pin -plugins plugins -lock eip.lock
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/ganeshdipdumbare/eipagent"
)

func main() {
	model := os.Getenv("EIPAGENT_MODEL")
	if model == "" {
		model = "mock:demo"
	}
	a, err := eipagent.New(
		eipagent.WithModel(model),
		eipagent.WithPreset("dev"), // no pins needed, hot reload on
		eipagent.WithPluginDir("plugins"),
		eipagent.WithPlugin("greeter", map[string]any{"greeting": "Hi", "shout": true}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
	for _, s := range a.Status() {
		fmt.Println(s.String())
	}
	res, err := a.Generate(context.Background(), "Please greet Ada.")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Text)
}

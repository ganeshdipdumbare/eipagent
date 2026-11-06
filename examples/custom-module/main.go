// A module in its own Go package: import it, register it with WithModule,
// and configure it with New(...).
//
//	go run ./examples/custom-module
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/examples/custom-module/weather"
	"github.com/ganeshdipdumbare/kiln/modules/model_mock"
)

func main() {
	a, err := kiln.New(kiln.Config{
		Model: "mock:demo",
		Modules: []kiln.Module{
			model_mock.New(map[string]any{"script": []any{
				map[string]any{"tool": "weather", "args": map[string]any{"city": "Oslo"}},
				map[string]any{"text": "It is 18 degrees celsius in Oslo."},
			}}),
			weather.New(map[string]any{"unit": "celsius"}),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	res, err := a.Generate(context.Background(), "What is the weather in Oslo?")
	if err != nil {
		log.Fatal(err)
	}
	for _, step := range res.Steps {
		for _, result := range step.ToolResults {
			fmt.Println(result.Content)
		}
	}
	fmt.Println(res.Text)
}

// Quickstart: a typed tool, streaming, and structured output.
//
//	OPENAI_API_KEY=... go run ./examples/quickstart
//	KILN_MODEL=mock:demo go run ./examples/quickstart   # offline
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/ganeshdipdumbare/kiln"
)

type WeatherIn struct {
	City string `json:"city" desc:"city name"`
}

type Trip struct {
	City  string   `json:"city"`
	Packs []string `json:"packing_list"`
}

func main() {
	ctx := context.Background()
	model := os.Getenv("KILN_MODEL")
	if model == "" {
		model = "openai:gpt-4.1-mini"
	}
	weather := kiln.Tool("weather", "Current weather for a city", func(ctx context.Context, in WeatherIn) (string, error) {
		return "18°C and light rain in " + in.City, nil
	})
	agent, err := kiln.New(
		kiln.WithModel(model),
		kiln.WithInstructions("You are a concise travel assistant."),
		kiln.WithTools(weather),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	// 1. Generate
	res, err := agent.Generate(ctx, "What's the weather in Lisbon?")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Text)

	// 2. Stream
	for delta := range agent.Stream(ctx, "Give me one tip for Lisbon.").Text() {
		fmt.Print(delta)
	}
	fmt.Println()

	// 3. Structured output
	trip, err := kiln.GenerateObject[Trip](ctx, agent, "Plan what to pack for Lisbon this week.")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%+v\n", trip)
}

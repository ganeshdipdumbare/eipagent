// Quickstart: a typed tool, streaming, and structured output.
//
//	OPENAI_API_KEY=... go run ./examples/quickstart
//	EIPAGENT_MODEL=mock:demo go run ./examples/quickstart   # offline
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/ganeshdipdumbare/eipagent"
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
	model := os.Getenv("EIPAGENT_MODEL")
	if model == "" {
		model = "openai:gpt-4.1-mini"
	}
	weather := eipagent.Tool("weather", "Current weather for a city", func(ctx context.Context, in WeatherIn) (string, error) {
		return "18°C and light rain in " + in.City, nil
	})
	agent, err := eipagent.New(
		eipagent.WithModel(model),
		eipagent.WithInstructions("You are a concise travel assistant."),
		eipagent.WithTools(weather),
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
	trip, err := eipagent.GenerateObject[Trip](ctx, agent, "Plan what to pack for Lisbon this week.")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%+v\n", trip)
}

// The ADK Go parity example: the same things an ADK app does, with only
// the kiln package and the offline mock model.
//
//	go run ./examples/adk-parity
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ganeshdipdumbare/kiln"
)

type WeatherIn struct {
	City string `json:"city" desc:"city name"`
}

type Trip struct {
	City string   `json:"city"`
	Pack []string `json:"packing_list"`
}

func main() {
	ctx := context.Background()

	// 1. A typed tool, like functiontool.New.
	weather := kiln.NewTool("weather", "Current weather for a city",
		func(ctx context.Context, in WeatherIn) (string, error) {
			return "18°C and light rain in " + in.City, nil
		})

	// 2. One agent value, like llmagent.New + runner.New.
	agent, err := kiln.New(kiln.Config{
		Model:        "mock:demo",
		Instructions: "You are a concise travel assistant.",
		Tools:        []kiln.Tool{weather},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	res, err := agent.Generate(ctx, "What's the weather in Lisbon?")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("generate:", res.Text)

	// 3. A conversation, without wiring a session service.
	th := agent.Thread("trip-1")
	if _, err := th.Generate(ctx, "My name is Ada."); err != nil {
		log.Fatal(err)
	}
	if _, err := th.Generate(ctx, "What's my name?"); err != nil {
		log.Fatal(err)
	}
	msgs, err := th.Messages(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("thread messages:", len(msgs))

	// 4. Streaming.
	for range agent.Stream(ctx, "One tip for Lisbon.").Text() {
	}
	fmt.Println("stream: ok")

	// 5. Structured output.
	trip, err := kiln.GenerateObject[Trip](ctx, agent, "Plan what to pack for Lisbon.")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("object:", trip.City)

	// 6. A sub-agent, like an ADK sub-agent: the parent calls it as a tool.
	expert, err := kiln.New(kiln.Config{Model: "mock:demo", Instructions: "You are a Lisbon expert."})
	if err != nil {
		log.Fatal(err)
	}
	defer expert.Close()
	parent, err := kiln.New(kiln.Config{
		Model: "mock:demo",
		Tools: []kiln.Tool{expert.AsTool("lisbon_expert", "Answers questions about Lisbon")},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer parent.Close()
	if _, err := parent.Generate(ctx, "Ask the expert for one tip."); err != nil {
		log.Fatal(err)
	}
	fmt.Println("subagent: ok")
}

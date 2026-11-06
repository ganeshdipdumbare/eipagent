// Run kiln fully locally with Ollama: no API keys, nothing leaves
// your machine.
//
//	ollama pull lfm3                        # small and fast; any tool-capable model works
//	go run ./examples/ollama -demo text     # plain text, the simplest start
//	go run ./examples/ollama                # every demo
//	go run ./examples/ollama -demo modules  # add tools with modules compiled into the program
//	go run ./examples/ollama -demo tools    # one demo: text | stream | tools | modules | object | thread | approve | shell | vision
//	go run ./examples/ollama -model qwen3.5:4b
//	go run ./examples/ollama -demo vision -model qwen3-vl:4b -image photo.jpg
//
// Set OLLAMA_HOST (e.g. http://gpu-box:11434) to use a remote Ollama.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/modules/hook_log"
	"github.com/ganeshdipdumbare/kiln/modules/model_openai"
	"github.com/ganeshdipdumbare/kiln/modules/sandbox_local"
	"github.com/ganeshdipdumbare/kiln/modules/tool_fs"
	"github.com/ganeshdipdumbare/kiln/modules/tool_shell"
	"github.com/ganeshdipdumbare/kiln/modules/tool_time"
	"github.com/ganeshdipdumbare/kiln/modules/workspace_sandbox"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var (
	model   = flag.String("model", "lfm3:latest", "Ollama model (must support tool calling for the tools demos)")
	demo    = flag.String("demo", "all", "text | stream | tools | modules | object | thread | approve | shell | vision | all")
	image   = flag.String("image", "", "image file for -demo vision")
	verbose = flag.Bool("v", false, "log every model and tool call")
	ctx     = context.Background()
)

func main() {
	flag.Parse()
	demos := []struct {
		name string
		run  func()
	}{
		{"text", text}, {"stream", stream}, {"tools", tools}, {"modules", modules}, {"object", object}, {"thread", thread},
		{"approve", approve}, {"shell", shell}, {"vision", vision},
	}
	for _, d := range demos {
		if *demo != "all" && *demo != d.name {
			continue
		}
		if d.name == "vision" && *image == "" {
			if *demo == "vision" {
				log.Fatal("-demo vision needs -image path/to/file.jpg (and a vision model like qwen3-vl:4b)")
			}
			continue
		}
		fmt.Printf("\n━━━ %s ━━━\n", d.name)
		t := time.Now()
		d.run()
		fmt.Printf("(%s)\n", time.Since(t).Round(100*time.Millisecond))
	}
}

// newAgent starts an agent on the local Ollama model.
func newAgent(mods ...kiln.Module) *kiln.Agent {
	return newAgentWith(kiln.Config{}, mods...)
}

func newAgentWith(cfg kiln.Config, mods ...kiln.Module) *kiln.Agent {
	cfg.Model = "ollama:" + *model
	cfg.Instructions = "You are a helpful assistant running locally. Be brief. Use tools when they help."
	if cfg.MaxSteps == 0 {
		cfg.MaxSteps = 8
	}
	cfg.Logger = func(scope, msg string) {
		if *verbose {
			fmt.Printf("  · [%s] %s\n", scope, msg)
		}
	}
	cfg.Modules = append(cfg.Modules, mods...)
	if *verbose {
		cfg.Modules = append(cfg.Modules, hook_log.New())
	}
	if host := os.Getenv("OLLAMA_HOST"); host != "" {
		if !strings.HasPrefix(host, "http") {
			host = "http://" + host
		}
		m := model_openai.New(map[string]any{"name": "ollama", "base_url": strings.TrimRight(host, "/") + "/v1", "api_key": ""})
		m.As = "llm_ollama"
		cfg.Modules = append(cfg.Modules, m)
	}
	a, err := kiln.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	return a
}

func check(err error) {
	if err != nil {
		log.Fatalf("error: %v\n(is Ollama running, and is %q pulled? try: ollama pull %s)", err, *model, *model)
	}
}

// ---- text: one prompt in, one answer out ----

func text() {
	a := newAgent()
	defer a.Close()
	res, err := a.Generate(ctx, "In three short bullet points, why run an AI model locally?")
	check(err)
	fmt.Println(res.Text)
	fmt.Printf("  %d input + %d output tokens\n", res.Usage.InputTokens, res.Usage.OutputTokens)
}

// ---- tools: the model calls Go functions ----

type CityIn struct {
	City string `json:"city" desc:"city name, e.g. Pune"`
}

func tools() {
	weather := kiln.NewTool("get_weather", "Current weather for a city", func(ctx context.Context, in CityIn) (map[string]any, error) {
		// A real tool would call an API; this one is deterministic.
		return map[string]any{"city": in.City, "temp_c": 27, "sky": "clear"}, nil
	})
	convert := kiln.NewTool("celsius_to_fahrenheit", "Convert °C to °F", func(ctx context.Context, in struct {
		Celsius float64 `json:"celsius"`
	}) (string, error) {
		return fmt.Sprintf("%.1f°F", in.Celsius*9/5+32), nil
	})
	a := newAgentWith(kiln.Config{Tools: []kiln.Tool{weather, convert}}, tool_time.New())
	defer a.Close()

	res, err := a.Generate(ctx, "What's the weather in Pune right now, in Fahrenheit? Also, what time is it?")
	check(err)
	for _, c := range res.ToolCalls() {
		fmt.Printf("  → %s %v\n", c.Name, c.Args)
	}
	fmt.Println(res.Text)
	fmt.Printf("  %d steps, %d tokens\n", len(res.Steps), res.Usage.TotalTokens)
}

// ---- stream: print tokens as they arrive ----

func stream() {
	a := newAgent()
	defer a.Close()
	s := a.Stream(ctx, "Write a four-line poem about running AI on a laptop.")
	for e := range s.Events() {
		switch e.Type {
		case "text":
			fmt.Print(e.Text)
		case "reasoning":
			if *verbose {
				fmt.Print("\x1b[2m" + e.Text + "\x1b[0m") // dim: the model's thinking
			}
		}
	}
	fmt.Println()
	_, err := s.Result()
	check(err)
}

// ---- modules: application code compiled into the program ----

// officeModule is an application module: Go code compiled into this program
// that registers a tool when the agent starts. Nothing is loaded at run
// time; to change the hours, build the agent with a different module.
func officeModule(hours string) engine.Module {
	return engine.Module{Name: "office", Apply: func(p *sdk.Scope) error {
		p.AddTool(sdk.Tool{
			Name:        "office_hours",
			Description: "Returns the office opening hours. Always use this for questions about office hours.",
			Schema:      sdk.Object(map[string]any{}),
			Run: func(c context.Context, args map[string]any) (string, error) {
				return hours, nil
			},
		})
		return nil
	}}
}

func modules() {
	office := func(hours string) []kiln.Module {
		return []kiln.Module{officeModule(hours), tool_time.New()}
	}
	steps := []struct {
		label, question string
		mods            []kiln.Module
	}{
		{"1) No modules: no tools", "What is the exact current time? If you have no tool for it, say so.", nil},
		{"2) Add the standard tool_time module", "What is the exact current time? Use your tool.", []kiln.Module{tool_time.New()}},
		{"3) Add the office module compiled into the program", "When is the office open?", office("Mon-Fri 9:00-17:00")},
		{"4) Ship new hours: build the agent with the updated module", "When is the office open?", office("Mon-Sat 8:00-20:00 (new winter hours)")},
	}
	for _, s := range steps {
		a := newAgent(s.mods...)
		fmt.Printf("\n%s\n   tools: %v\n", s.label, toolNames(a))
		ask(a, s.question)
		a.Close()
	}
}

func ask(a *kiln.Agent, q string) {
	res, err := a.Generate(ctx, q, kiln.Run{MaxSteps: 4})
	check(err)
	var used []string
	for _, c := range res.ToolCalls() {
		used = append(used, c.Name)
	}
	fmt.Printf("  you: %s\n  ai:  %s\n  tools used: %v\n", q, strings.TrimSpace(res.Text), used)
}

func toolNames(a *kiln.Agent) []string {
	var names []string
	for _, t := range a.Engine().Root().Snapshot().Tools {
		names = append(names, t.Name)
	}
	return names
}

// ---- object: typed structured output ----

type Recipe struct {
	Name        string   `json:"name"`
	Minutes     int      `json:"minutes" desc:"total cooking time"`
	Ingredients []string `json:"ingredients"`
	Vegetarian  bool     `json:"vegetarian"`
}

func object() {
	a := newAgent()
	defer a.Close()
	r, err := kiln.GenerateObject[Recipe](ctx, a, "A quick vegetarian Indian breakfast recipe.")
	check(err)
	fmt.Printf("%s — %d min, vegetarian=%v\n  %s\n", r.Name, r.Minutes, r.Vegetarian, strings.Join(r.Ingredients, ", "))
}

// ---- thread: memory across turns, persisted to disk ----

func thread() {
	dir := filepath.Join(os.TempDir(), "kiln-ollama-threads")
	a := newAgent(kiln.Persistence("file", dir)...)
	defer a.Close()
	th := a.Thread(fmt.Sprintf("demo-%d", time.Now().Unix()))
	for _, q := range []string{"Hi! My name is Ganesh and I live in Pune.", "Where do I live, and what's my name?"} {
		fmt.Println("you:", q)
		res, err := th.Generate(ctx, q)
		check(err)
		fmt.Println("ai: ", res.Text)
	}
	fmt.Println("  thread saved under", filepath.Join(dir, "threads"))
}

// ---- approve: human in the loop, answered at the terminal ----

func approve() {
	sendEmail := kiln.NewTool("send_email", "Send an email", func(ctx context.Context, in struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}) (string, error) {
		return "sent to " + in.To, nil // a real tool would send it
	})
	in := bufio.NewReader(os.Stdin)
	approver := kiln.Approver(func(ctx context.Context, it kiln.Interrupt) (*kiln.Decision, error) {
		fmt.Printf("  ⚠ the agent wants to call %s %v\n  approve? [y/N] ", it.ToolCall.Name, it.ToolCall.Args)
		ans, _ := in.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(ans)) == "y" {
			d := kiln.Approve(it.ID)
			return &d, nil
		}
		d := kiln.Reject(it.ID, "the user declined")
		return &d, nil
	})
	a := newAgentWith(kiln.Config{Tools: []kiln.Tool{sendEmail}}, append(kiln.Approvals("send_email"), approver...)...)
	defer a.Close()
	res, err := a.Generate(ctx, "Email team@example.com with subject 'Standup' saying I'll be 10 minutes late.")
	check(err)
	fmt.Println(res.Text)
}

// ---- shell: the model runs commands in a throwaway workspace ----

func shell() {
	root := filepath.Join(os.TempDir(), "kiln-ollama-boxes")
	a := newAgent(
		sandbox_local.New(map[string]any{"root": root}), // swap for sandbox_docker to isolate
		workspace_sandbox.New(map[string]any{"provider": "local"}),
		tool_shell.New(), tool_fs.New())
	defer a.Close() // deletes the workspace
	res, err := a.Generate(ctx, "Create a file hello.py that prints the first 10 squares, run it with python3, and tell me the output.")
	check(err)
	for _, c := range res.ToolCalls() {
		fmt.Printf("  → %s %v\n", c.Name, c.Args)
	}
	fmt.Println(res.Text)
	for _, ch := range res.Changes {
		fmt.Print(ch.Diff())
	}
}

// ---- vision: describe an image (needs a vision model) ----

func vision() {
	data, err := os.ReadFile(*image)
	check(err)
	mt := "image/jpeg"
	if strings.HasSuffix(strings.ToLower(*image), ".png") {
		mt = "image/png"
	}
	a := newAgent()
	defer a.Close()
	res, err := a.Generate(ctx, "Describe this image in two sentences.", kiln.Run{Images: []kiln.Image{{MediaType: mt, Data: data}}})
	check(err)
	fmt.Println(res.Text)
}

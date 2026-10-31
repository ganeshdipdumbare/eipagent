// Run eipagent fully locally with Ollama: no API keys, nothing leaves
// your machine.
//
//	ollama pull lfm3                        # small and fast; any tool-capable model works
//	go run ./examples/ollama -demo text     # plain text, the simplest start
//	go run ./examples/ollama                # every demo
//	go run ./examples/ollama -demo plugins  # add, update and remove plugins while the agent runs
//	go run ./examples/ollama -demo tools    # one demo: text | stream | tools | plugins | object | thread | approve | shell | vision
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

	"eipagent"
)

var (
	model   = flag.String("model", "lfm3:latest", "Ollama model (must support tool calling for the tools demos)")
	demo    = flag.String("demo", "all", "text | stream | tools | plugins | object | thread | approve | shell | vision | all")
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
		{"text", text}, {"stream", stream}, {"tools", tools}, {"plugins", plugins}, {"object", object}, {"thread", thread},
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
func newAgent(opts ...eipagent.Option) *eipagent.Agent {
	base := []eipagent.Option{
		eipagent.WithModel("ollama:" + *model),
		eipagent.WithInstructions("You are a helpful assistant running locally. Be brief. Use tools when they help."),
		eipagent.WithMaxSteps(8),
		eipagent.WithLogger(func(scope, msg string) {
			if *verbose {
				fmt.Printf("  · [%s] %s\n", scope, msg)
			}
		}),
	}
	if *verbose {
		base = append(base, eipagent.WithPlugins("mw_logger"))
	}
	if host := os.Getenv("OLLAMA_HOST"); host != "" {
		if !strings.HasPrefix(host, "http") {
			host = "http://" + host
		}
		base = append(base, eipagent.WithPluginSpec(eipagent.PluginSpec{Use: "llm_openai", As: "llm_ollama",
			Config: map[string]any{"name": "ollama", "base_url": strings.TrimRight(host, "/") + "/v1", "api_key": ""}}))
	}
	a, err := eipagent.New(append(base, opts...)...)
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
	weather := eipagent.Tool("get_weather", "Current weather for a city", func(ctx context.Context, in CityIn) (map[string]any, error) {
		// A real tool would call an API; this one is deterministic.
		return map[string]any{"city": in.City, "temp_c": 27, "sky": "clear"}, nil
	})
	convert := eipagent.Tool("celsius_to_fahrenheit", "Convert °C to °F", func(ctx context.Context, in struct {
		Celsius float64 `json:"celsius"`
	}) (string, error) {
		return fmt.Sprintf("%.1f°F", in.Celsius*9/5+32), nil
	})
	a := newAgent(eipagent.WithTools(weather, convert), eipagent.WithPlugins("tool_time"))
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

// ---- plugins: add, update and remove capabilities while the agent runs ----

// A brand-new plugin, written to disk while the agent is running. It is
// interpreted Go: one file, one Apply function, nothing to compile.
const officePlugin = `//go:build ignore

package plugin

import (
	"context"

	"eipagent/sdk"
)

func Apply(ctx *sdk.Context) error {
	ctx.AddTool(sdk.Tool{
		Name:        "office_hours",
		Description: "Returns the office opening hours. Always use this for questions about office hours.",
		Schema:      sdk.Object(map[string]any{}),
		Run: func(c context.Context, args map[string]any) (string, error) {
			return "HOURS", nil
		},
	})
	return nil
}
`

func plugins() {
	dir, err := os.MkdirTemp("", "eipagent-plugins-")
	check(err)
	defer os.RemoveAll(dir)
	a := newAgent(
		eipagent.WithPreset("dev"),    // dev: unpinned plugin files allowed (secure needs eip.lock pins)
		eipagent.WithHotReload(false), // we reload explicitly below; in dev it would also reload on save
		eipagent.WithPluginDir(dir),
	)
	defer a.Close()
	eng := a.Engine()
	ask := func(q string) {
		res, err := a.Generate(ctx, q, eipagent.MaxSteps(4))
		check(err)
		var used []string
		for _, c := range res.ToolCalls() {
			used = append(used, c.Name)
		}
		fmt.Printf("  you: %s\n  ai:  %s\n  tools used: %v\n", q, strings.TrimSpace(res.Text), used)
	}
	tools := func() string {
		var names []string
		for _, t := range eng.Root().Snapshot().Tools {
			names = append(names, t.Name)
		}
		return fmt.Sprint(names)
	}

	fmt.Println("1) No tools yet:", tools())
	ask("What is the exact current time? If you have no tool for it, say so.")

	fmt.Println("\n2) Load the built-in tool_time plugin at runtime")
	check(eng.Load(eipagent.PluginSpec{Use: "tool_time"}))
	fmt.Println("   tools:", tools())
	ask("What is the exact current time? Use your tool.")

	fmt.Println("\n3) Write a new plugin file and load it")
	path := filepath.Join(dir, "office.go")
	check(os.WriteFile(path, []byte(strings.Replace(officePlugin, "HOURS", "Mon-Fri 9:00-17:00", 1)), 0o644))
	check(eng.Load(eipagent.PluginSpec{Use: "office"}))
	fmt.Println("   tools:", tools())
	ask("When is the office open?")

	fmt.Println("\n4) Edit the plugin's code and reload it (no restart)")
	check(os.WriteFile(path, []byte(strings.Replace(officePlugin, "HOURS", "Mon-Sat 8:00-20:00 (new winter hours)", 1)), 0o644))
	check(eng.Reload("office"))
	ask("When is the office open?")

	fmt.Println("\n5) A broken edit is refused; the running version stays")
	check(os.WriteFile(path, []byte("package plugin\nfunc Apply( {"), 0o644))
	if err := eng.Reload("office"); err != nil {
		fmt.Println("   reload refused:", strings.SplitN(err.Error(), "\n", 2)[0])
	}
	fmt.Println("   tools:", tools())

	fmt.Println("\n6) Unload both plugins")
	check(eng.Unload("office"))
	check(eng.Unload("tool_time"))
	fmt.Println("   tools:", tools())
	for _, st := range a.Status() {
		fmt.Println("  ", st.String())
	}
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
	r, err := eipagent.GenerateObject[Recipe](ctx, a, "A quick vegetarian Indian breakfast recipe.")
	check(err)
	fmt.Printf("%s — %d min, vegetarian=%v\n  %s\n", r.Name, r.Minutes, r.Vegetarian, strings.Join(r.Ingredients, ", "))
}

// ---- thread: memory across turns, persisted to disk ----

func thread() {
	dir := filepath.Join(os.TempDir(), "eipagent-ollama-threads")
	a := newAgent(eipagent.WithPlugin("checkpoint_file", map[string]any{"dir": dir}))
	defer a.Close()
	th := a.Thread(fmt.Sprintf("demo-%d", time.Now().Unix()))
	for _, q := range []string{"Hi! My name is Ganesh and I live in Pune.", "Where do I live, and what's my name?"} {
		fmt.Println("you:", q)
		res, err := th.Generate(ctx, q)
		check(err)
		fmt.Println("ai: ", res.Text)
	}
	fmt.Println("  thread saved under", dir)
}

// ---- approve: human in the loop, answered at the terminal ----

func approve() {
	sendEmail := eipagent.Tool("send_email", "Send an email", func(ctx context.Context, in struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}) (string, error) {
		return "sent to " + in.To, nil // a real tool would send it
	})
	in := bufio.NewReader(os.Stdin)
	a := newAgent(
		eipagent.WithTools(sendEmail),
		eipagent.WithPlugin("mw_hitl", map[string]any{"interrupt_on": map[string]any{"send_email": true}}),
		eipagent.WithApprover(func(ctx context.Context, it eipagent.Interrupt) (*eipagent.Decision, error) {
			fmt.Printf("  ⚠ the agent wants to call %s %v\n  approve? [y/N] ", it.ToolCall.Name, it.ToolCall.Args)
			ans, _ := in.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(ans)) == "y" {
				d := eipagent.Approve(it.ID)
				return &d, nil
			}
			d := eipagent.Reject(it.ID, "the user declined")
			return &d, nil
		}),
	)
	defer a.Close()
	res, err := a.Generate(ctx, "Email team@example.com with subject 'Standup' saying I'll be 10 minutes late.")
	check(err)
	fmt.Println(res.Text)
}

// ---- shell: the model runs commands in a throwaway workspace ----

func shell() {
	root := filepath.Join(os.TempDir(), "eipagent-ollama-boxes")
	a := newAgent(
		eipagent.WithPlugin("sandbox_local", map[string]any{"root": root}), // swap for sandbox_docker to isolate
		eipagent.WithPlugin("exec_sandbox", map[string]any{"provider": "local"}),
		eipagent.WithPlugins("tool_shell", "tool_fs"),
	)
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
	res, err := a.Generate(ctx, "Describe this image in two sentences.", eipagent.WithImages(eipagent.Image{MediaType: mt, Data: data}))
	check(err)
	fmt.Println(res.Text)
}

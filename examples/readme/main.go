// Every code example in README.md, compiled and run offline with the mock
// model so the docs can't drift from the API:
//
//	go run ./examples/readme
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var ctx = context.Background()

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// mock scripts the offline model so each example is deterministic.
func mock(steps ...map[string]any) kiln.Option {
	script := make([]any, len(steps))
	for i, s := range steps {
		script[i] = s
	}
	return kiln.WithModuleConfig("model_mock", map[string]any{"script": script})
}

func main() {
	dir, err := os.MkdirTemp("", "kiln-readme-")
	check(err)
	defer os.RemoveAll(dir)
	for _, ex := range []struct {
		name string
		fn   func(dir string)
	}{
		{"generate with a tool", generateWithTool},
		{"streaming", streaming},
		{"structured output", structured},
		{"threads", threads},
		{"human in the loop", humanInTheLoop},
		{"middleware", middleware},
		{"custom middleware", customMiddleware},
		{"modules fixed at start", modulesAtStart},
		{"subagents", subagents},
		{"sandboxed tools", sandboxedTools},
		{"lifecycle hooks", lifecycleHooks},
	} {
		fmt.Printf("== %s\n", ex.name)
		ex.fn(dir)
	}
}

// ---- 1. Generate with a typed tool ----

type WeatherIn struct {
	City string `json:"city" desc:"city name"`
}

func generateWithTool(string) {
	weather := kiln.Tool("weather", "Current weather for a city",
		func(ctx context.Context, in WeatherIn) (string, error) {
			return "18°C and light rain in " + in.City, nil
		})

	agent, err := kiln.New(
		kiln.WithModel("mock:demo"), // e.g. "anthropic:claude-sonnet-5-5"
		kiln.WithInstructions("You are a concise travel assistant."),
		kiln.WithTools(weather),
		mock(map[string]any{"tool": "weather", "args": map[string]any{"city": "Lisbon"}},
			map[string]any{"text": "Bring an umbrella: 18°C and rain."}),
	)
	check(err)
	defer agent.Close()

	res, err := agent.Generate(ctx, "What's the weather in Lisbon?")
	check(err)
	fmt.Println(res.Text)                         // final answer
	fmt.Println(len(res.Steps), res.FinishReason) // 2 stop
	fmt.Println(res.ToolCalls()[0].Name)          // weather
	fmt.Println(res.Usage.TotalTokens > 0)        // token accounting
}

// ---- 2. Streaming ----

func streaming(string) {
	agent, err := kiln.New(kiln.WithModel("mock:demo"), mock(map[string]any{"text": "Rain on red roofs"}))
	check(err)
	defer agent.Close()

	// Text only:
	s := agent.Stream(ctx, "A haiku about Lisbon")
	for delta := range s.Text() {
		fmt.Print(delta)
	}
	fmt.Println()
	_, err = s.Result()
	check(err)

	// Everything (text, tool calls and results, steps, interrupts, finish):
	s = agent.Stream(ctx, "again")
	for e := range s.Events() {
		switch e.Type {
		case "tool_call":
			fmt.Println("calling", e.ToolCall.Name)
		case "finish":
			fmt.Println("done:", e.Data["reason"])
		}
	}
	_, err = s.Result()
	check(err)
}

// ---- 3. Structured output ----

type Trip struct {
	City    string   `json:"city"`
	Packing []string `json:"packing" desc:"things to pack"`
	Budget  int      `json:"budget_eur,omitempty"`
}

func structured(string) {
	agent, err := kiln.New(kiln.WithModel("mock:demo"))
	check(err)
	defer agent.Close()

	trip, err := kiln.GenerateObject[Trip](ctx, agent, "Plan what to pack for Lisbon.")
	check(err)
	fmt.Printf("%+v\n", trip)
}

// ---- 4. Threads (durable conversations) ----

func threads(dir string) {
	agent, err := kiln.New(
		kiln.WithModel("mock:demo"),
		kiln.WithPersistence("file", dir), // threads_file + memory_file; or "postgres"
	)
	check(err)
	defer agent.Close()

	th := agent.Thread("customer-42")
	_, err = th.Generate(ctx, "My name is Ada.")
	check(err)
	_, err = th.Generate(ctx, "What's my name?") // sees the earlier turn
	check(err)
	msgs, err := th.Messages(ctx)
	check(err)
	fmt.Println(len(msgs), "messages stored")
}

// ---- 5. Human in the loop ----

func humanInTheLoop(dir string) {
	refund := kiln.Tool("refund", "Refund an order", func(ctx context.Context, in struct {
		Order string `json:"order"`
	}) (string, error) {
		return "refunded " + in.Order, nil
	})
	agent, err := kiln.New(
		kiln.WithModel("mock:demo"),
		kiln.WithTools(refund),
		kiln.WithModuleConfig("hook_approval", map[string]any{"interrupt_on": map[string]any{"refund": true}}),
		mock(map[string]any{"tool": "refund", "args": map[string]any{"order": "A-17"}},
			map[string]any{"text": "Refund done."}),
	)
	check(err)
	defer agent.Close()

	th := agent.Thread("ticket-981")
	res, err := th.Generate(ctx, "Refund order A-17")
	check(err)
	for res.Interrupted() { // paused before the tool ran; state is saved
		in := res.Interrupts[0]
		fmt.Println("approve?", in.Message)
		res, err = th.Resume(ctx, kiln.Approve(in.ID)) // or Reject / Edit / Respond
		check(err)
	}
	fmt.Println(res.Text)
}

// ---- 6. Middleware from the standard modules ----

func middleware(string) {
	agent, err := kiln.New(
		kiln.WithModel("mock:demo"),
		kiln.WithModuleConfig("hook_retry", map[string]any{"max_retries": 3}),
		kiln.WithModuleConfig("hook_fallback", map[string]any{"models": []any{"mock:backup"}}),
		kiln.WithModuleConfig("hook_limits", map[string]any{"model_calls_run": 20, "tool_calls_run": 50}),
		kiln.WithModuleConfig("hook_pii", map[string]any{"types": []any{"email", "credit_card"}}),
		kiln.WithModuleConfig("hook_summarize", map[string]any{"trigger_messages": 40, "keep_messages": 10}),
	)
	check(err)
	defer agent.Close()
	res, err := agent.Generate(ctx, "my email is ada@example.com")
	check(err)
	fmt.Println(res.Text) // the model saw [REDACTED_EMAIL]
}

// ---- 7. Your own middleware (a compiled application module) ----

func customMiddleware(string) {
	timing := kiln.Module{Name: "timing", Apply: func(p *sdk.Module) error {
		p.WrapModel(func(c context.Context, req *kiln.ModelRequest, next kiln.ModelHandler) (*kiln.ModelResponse, error) {
			resp, err := next(c, req) // call the model (and inner middleware)
			if err == nil {
				p.Logf("%s step %d used %d tokens", req.Model, req.Step, resp.Usage.TotalTokens)
			}
			return resp, err
		})
		p.WrapTool(func(c context.Context, req *kiln.ToolRequest, next kiln.ToolHandler) (*kiln.ToolResult, error) {
			if req.Call.Name == "drop_database" {
				return &kiln.ToolResult{Content: "not allowed", IsError: true}, nil
			}
			return next(c, req)
		})
		return nil
	}}
	agent, err := kiln.New(kiln.WithModel("mock:demo"), kiln.WithModule(timing),
		kiln.WithLogger(func(scope, msg string) { fmt.Printf("[%s] %s\n", scope, msg) }))
	check(err)
	defer agent.Close()
	_, err = agent.Generate(ctx, "hi")
	check(err)
}

// ---- 8. Modules are fixed when New runs ----

// There is no runtime load, unload or reload, no module directory and no
// lock file: the module set is composed once. A requirement nobody provides
// is filled by a default module, or New refuses it by name. Application
// code is compiled in with WithModule.
func modulesAtStart(string) {
	// Without defaults, nothing provides "memory": New refuses tool_memory.
	_, err := kiln.New(kiln.WithModel("mock:demo"), kiln.WithoutDefaults(),
		kiln.WithModules("tool_memory"))
	fmt.Println("refused without memory:", err)

	// With defaults, New adds memory_inmem for tool_memory.
	greeter := kiln.Module{Name: "greeter", Apply: func(p *sdk.Module) error {
		p.AddTool(kiln.Tool("greet", "Say hello", func(ctx context.Context, in struct{}) (string, error) {
			return "hello", nil
		}))
		return nil
	}}
	agent, err := kiln.New(
		kiln.WithModel("mock:demo"),
		kiln.WithModules("tool_memory"),
		kiln.WithModule(greeter),
	)
	check(err)
	defer agent.Close()
	for _, s := range agent.Status() {
		fmt.Println(s.String())
	}
}

// ---- 9. Subagents ----

func subagents(string) {
	agent, err := kiln.New(
		kiln.WithModel("mock:demo"),
		kiln.WithModules("tool_time"),
		kiln.WithModuleConfig("tool_subagents", map[string]any{"agents": []any{
			map[string]any{"name": "researcher", "description": "Finds facts",
				"instructions": "Research carefully.", "tools": []any{"current_time"}},
			map[string]any{"name": "billing", "description": "Invoices and refunds",
				"instructions": "You are billing support.", "mode": "handoff"},
		}}),
		mock(map[string]any{"tool": "researcher", "args": map[string]any{"task": "What time is it?"}},
			map[string]any{"tool": "current_time"}, // the researcher's own steps
			map[string]any{"text": "It is noon."},
			map[string]any{"text": "The researcher says it is noon."}),
	)
	check(err)
	defer agent.Close()
	res, err := agent.Generate(ctx, "Ask the researcher for the time")
	check(err)
	fmt.Println(res.Text)

	// A whole agent (own modules, model, policy) can be a tool too:
	expert, err := kiln.New(kiln.WithModel("mock:demo"))
	check(err)
	defer expert.Close()
	_ = kiln.WithTools(expert.AsTool("legal_expert", "Answers legal questions"))
}

// ---- 10. Shell and file tools in a sandbox ----

func sandboxedTools(dir string) {
	agent, err := kiln.New(
		kiln.WithModel("mock:demo"),
		kiln.WithModuleConfig("sandbox_local", map[string]any{"root": dir + "/boxes"}),  // or sandbox_docker
		kiln.WithModuleConfig("workspace_sandbox", map[string]any{"provider": "local"}), // one box per thread
		kiln.WithModules("tool_shell", "tool_fs"),
		mock(map[string]any{"tool": "write_file", "args": map[string]any{"path": "notes.md", "content": "hello\n"}},
			map[string]any{"tool": "bash", "args": map[string]any{"command": "wc -l notes.md"}},
			map[string]any{"text": "Wrote notes.md."}),
	)
	check(err)
	defer agent.Close() // destroys the sandboxes
	res, err := agent.Generate(ctx, "Write notes.md")
	check(err)
	fmt.Println(res.Steps[1].ToolResults[0].Content)
	for _, ch := range res.Changes { // review what the agent changed
		fmt.Print(ch.Diff())
	}
}

// ---- 11. Lifecycle hooks ----

func lifecycleHooks(string) {
	observer := kiln.Module{Name: "observer", Apply: func(p *sdk.Module) error {
		p.OnRunStart(func(c context.Context, st *sdk.RunState) error {
			fmt.Println("run start, user:", st.User)
			return nil // returning an error refuses the run
		})
		p.BeforeStep(func(c context.Context, sc *sdk.StepContext) error {
			sc.Request.System += "\nToday is a holiday." // rewrite the next model request
			return nil
		})
		p.AfterModel(func(c context.Context, sc *sdk.StepContext, resp *sdk.ModelResponse) error {
			if len(resp.Message.ToolCalls) > 3 {
				sc.Jump = "end" // stop the run here
			}
			return nil
		})
		p.OnEvent(func(e sdk.Event) {
			if e.Type == "error" {
				fmt.Println("run failed:", e.Text)
			}
		})
		p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
			fmt.Println("run end:", res.FinishReason) // stop | max_steps | interrupted | tool:x | error
			return nil
		})
		p.Effect(func() func() {
			fmt.Println("observer loaded")
			return func() { fmt.Println("observer unloaded") } // on Close
		})
		return nil
	}}
	boom := kiln.Tool("boom", "fails hard", func(ctx context.Context, in struct{}) (string, error) {
		return "", sdk.Fatal(errors.New("disk on fire")) // sdk.Fatal aborts the run; plain errors go to the model
	})
	agent, err := kiln.New(kiln.WithModel("mock:demo"), kiln.WithUser("ada"),
		kiln.WithModule(observer), kiln.WithTools(boom),
		mock(map[string]any{"text": "hello"}, map[string]any{"tool": "boom"}))
	check(err)
	_, err = agent.Generate(ctx, "hi")
	check(err)
	_, err = agent.Generate(ctx, "break it")
	fmt.Println("error returned:", err != nil)
	agent.Close()
}

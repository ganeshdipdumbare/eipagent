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

	"eipagent"
	"eipagent/sdk"
)

var ctx = context.Background()

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// mock scripts the offline model so each example is deterministic.
func mock(steps ...map[string]any) eipagent.Option {
	script := make([]any, len(steps))
	for i, s := range steps {
		script[i] = s
	}
	return eipagent.WithPlugin("llm_mock", map[string]any{"script": script})
}

func main() {
	dir, err := os.MkdirTemp("", "eip-readme-")
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
		{"runtime plugin management", runtimePlugins},
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
	weather := eipagent.Tool("weather", "Current weather for a city",
		func(ctx context.Context, in WeatherIn) (string, error) {
			return "18°C and light rain in " + in.City, nil
		})

	agent, err := eipagent.New(
		eipagent.WithModel("mock:demo"), // e.g. "anthropic:claude-sonnet-5-5"
		eipagent.WithInstructions("You are a concise travel assistant."),
		eipagent.WithTools(weather),
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
	agent, err := eipagent.New(eipagent.WithModel("mock:demo"), mock(map[string]any{"text": "Rain on red roofs"}))
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
	agent, err := eipagent.New(eipagent.WithModel("mock:demo"))
	check(err)
	defer agent.Close()

	trip, err := eipagent.GenerateObject[Trip](ctx, agent, "Plan what to pack for Lisbon.")
	check(err)
	fmt.Printf("%+v\n", trip)
}

// ---- 4. Threads (durable conversations) ----

func threads(dir string) {
	agent, err := eipagent.New(
		eipagent.WithModel("mock:demo"),
		eipagent.WithPlugin("checkpoint_file", map[string]any{"dir": dir + "/threads"}), // or checkpoint_sql
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
	refund := eipagent.Tool("refund", "Refund an order", func(ctx context.Context, in struct {
		Order string `json:"order"`
	}) (string, error) {
		return "refunded " + in.Order, nil
	})
	agent, err := eipagent.New(
		eipagent.WithModel("mock:demo"),
		eipagent.WithTools(refund),
		eipagent.WithPlugin("mw_hitl", map[string]any{"interrupt_on": map[string]any{"refund": true}}),
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
		res, err = th.Resume(ctx, eipagent.Approve(in.ID)) // or Reject / Edit / Respond
		check(err)
	}
	fmt.Println(res.Text)
}

// ---- 6. Middleware from the standard plugins ----

func middleware(string) {
	agent, err := eipagent.New(
		eipagent.WithModel("mock:demo"),
		eipagent.WithPlugin("mw_retry", map[string]any{"max_retries": 3}),
		eipagent.WithPlugin("mw_fallback", map[string]any{"models": []any{"mock:backup"}}),
		eipagent.WithPlugin("mw_limits", map[string]any{"model_calls_run": 20, "tool_calls_run": 50}),
		eipagent.WithPlugin("mw_pii", map[string]any{"types": []any{"email", "credit_card"}}),
		eipagent.WithPlugin("mw_summarize", map[string]any{"trigger_messages": 40, "keep_messages": 10}),
	)
	check(err)
	defer agent.Close()
	res, err := agent.Generate(ctx, "my email is ada@example.com")
	check(err)
	fmt.Println(res.Text) // the model saw [REDACTED_EMAIL]
}

// ---- 7. Your own middleware (a compiled plugin) ----

func customMiddleware(string) {
	timing := eipagent.Plugin{Name: "timing", Apply: func(ctx *eipagent.PluginContext) error {
		ctx.WrapModel(func(c context.Context, req *eipagent.ModelRequest, next eipagent.ModelHandler) (*eipagent.ModelResponse, error) {
			resp, err := next(c, req) // call the model (and inner middleware)
			if err == nil {
				ctx.Logf("%s step %d used %d tokens", req.Model, req.Step, resp.Usage.TotalTokens)
			}
			return resp, err
		})
		ctx.WrapTool(func(c context.Context, req *eipagent.ToolRequest, next eipagent.ToolHandler) (*eipagent.ToolResult, error) {
			if req.Call.Name == "drop_database" {
				return &eipagent.ToolResult{Content: "not allowed", IsError: true}, nil
			}
			return next(c, req)
		})
		return nil
	}}
	agent, err := eipagent.New(eipagent.WithModel("mock:demo"), eipagent.WithCompiledPlugin(timing),
		eipagent.WithLogger(func(scope, msg string) { fmt.Printf("[%s] %s\n", scope, msg) }))
	check(err)
	defer agent.Close()
	_, err = agent.Generate(ctx, "hi")
	check(err)
}

// ---- 8. Add, update and remove plugins at runtime ----

func runtimePlugins(string) {
	agent, err := eipagent.New(eipagent.WithModel("mock:demo"))
	check(err)
	defer agent.Close()
	eng := agent.Engine()

	check(eng.Load(eipagent.PluginSpec{Use: "memory_store"})) // needs a "store": waits
	fmt.Println(eng.State("memory_store"))                    // PENDING
	check(eng.Load(eipagent.PluginSpec{Use: "store_inmem"}))
	fmt.Println(eng.State("memory_store")) // ACTIVE: its dependency arrived

	check(eng.Load(eipagent.PluginSpec{Use: "mw_pii", Config: map[string]any{"strategy": "mask"}}))
	check(eng.Load(eipagent.PluginSpec{Use: "mw_pii", Config: map[string]any{"strategy": "hash"}})) // update config
	check(eng.Reload("mw_pii"))                                                                     // update code from source
	check(eng.Unload("store_inmem"))
	fmt.Println(eng.State("memory_store")) // PENDING again; its tools are gone

	for _, s := range agent.Status() {
		fmt.Println(s.String())
	}
}

// ---- 9. Subagents ----

func subagents(string) {
	agent, err := eipagent.New(
		eipagent.WithModel("mock:demo"),
		eipagent.WithPlugins("tool_time"),
		eipagent.WithPlugin("agents", map[string]any{"agents": []any{
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

	// A whole agent (own plugins, model, policy) can be a tool too:
	expert, err := eipagent.New(eipagent.WithModel("mock:demo"))
	check(err)
	defer expert.Close()
	_ = eipagent.WithTools(expert.AsTool("legal_expert", "Answers legal questions"))
}

// ---- 10. Shell and file tools in a sandbox ----

func sandboxedTools(dir string) {
	agent, err := eipagent.New(
		eipagent.WithModel("mock:demo"),
		eipagent.WithPlugin("sandbox_local", map[string]any{"root": dir + "/boxes"}), // or sandbox_docker
		eipagent.WithPlugin("exec_sandbox", map[string]any{"provider": "local"}),     // one box per thread
		eipagent.WithPlugins("tool_shell", "tool_fs"),
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
	observer := eipagent.Plugin{Name: "observer", Apply: func(ctx *eipagent.PluginContext) error {
		ctx.OnAgentStart(func(c context.Context, st *sdk.RunState) error {
			fmt.Println("run start, user:", st.User)
			return nil // returning an error refuses the run
		})
		ctx.OnStepPrepare(func(c context.Context, sc *sdk.StepContext) error {
			sc.Request.System += "\nToday is a holiday." // rewrite the next model request
			return nil
		})
		ctx.OnModelAfter(func(c context.Context, sc *sdk.StepContext, resp *sdk.ModelResponse) error {
			if len(resp.Message.ToolCalls) > 3 {
				sc.Jump = "end" // stop the run here
			}
			return nil
		})
		ctx.OnEvent(func(e sdk.Event) {
			if e.Type == "error" {
				fmt.Println("run failed:", e.Text)
			}
		})
		ctx.OnAgentEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
			fmt.Println("run end:", res.FinishReason) // stop | max_steps | interrupted | tool:x | error
			return nil
		})
		ctx.Effect(func() func() {
			fmt.Println("observer loaded")
			return func() { fmt.Println("observer unloaded") } // on Unload / Close / reload
		})
		return nil
	}}
	boom := eipagent.Tool("boom", "fails hard", func(ctx context.Context, in struct{}) (string, error) {
		return "", sdk.Fatal(errors.New("disk on fire")) // sdk.Fatal aborts the run; plain errors go to the model
	})
	agent, err := eipagent.New(eipagent.WithModel("mock:demo"), eipagent.WithUser("ada"),
		eipagent.WithCompiledPlugin(observer), eipagent.WithTools(boom),
		mock(map[string]any{"text": "hello"}, map[string]any{"tool": "boom"}))
	check(err)
	_, err = agent.Generate(ctx, "hi")
	check(err)
	_, err = agent.Generate(ctx, "break it")
	fmt.Println("error returned:", err != nil)
	agent.Close()
}

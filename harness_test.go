package kiln_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/evals"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// scripted is a compiled provider whose replies come from fn; it records
// every request it gets.
type scripted struct {
	mu   sync.Mutex
	reqs []*sdk.ModelRequest
	fn   func(req *sdk.ModelRequest, n int) (*sdk.ModelResponse, error)
}

func (s *scripted) plugin(name string) kiln.Module {
	return kiln.Module{Name: "scripted_" + name, Provides: []string{"model/" + name}, Apply: func(p *sdk.Scope) error {
		return p.Provide("model/"+name, &sdk.Provider{Name: name, Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			cp := *req
			cp.Messages = append([]sdk.Message(nil), req.Messages...)
			cp.Tools = append([]sdk.Tool(nil), req.Tools...)
			s.mu.Lock()
			s.reqs = append(s.reqs, &cp)
			n := len(s.reqs)
			s.mu.Unlock()
			return s.fn(&cp, n)
		}})
	}}
}

func (s *scripted) last() *sdk.ModelRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}

func call(name string, args map[string]any) *sdk.ModelResponse {
	return &sdk.ModelResponse{Message: sdk.Message{Role: "assistant", ToolCalls: []sdk.ToolCall{{Name: name, Args: args}}}, FinishReason: "tool_calls"}
}

func say(text string) *sdk.ModelResponse {
	return &sdk.ModelResponse{Message: sdk.Message{Role: "assistant", Content: text}, FinishReason: "stop"}
}

func toolResults(ms []sdk.Message) []string {
	var out []string
	for _, m := range ms {
		if m.Role == sdk.RoleTool {
			out = append(out, m.Content)
		}
	}
	return out
}

func newScripted(t *testing.T, s *scripted, cfgs ...kiln.Config) *kiln.Agent {
	t.Helper()
	a, err := startAgent(append([]kiln.Config{withModel("s:m"), withModule(s.plugin("s"))}, cfgs...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestCacheUsage(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "ak")
	srv := fakeServer(t, func(*http.Request, map[string]any) {}, func(int, map[string]any) ([]string, string) {
		return nil, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":900,"cache_creation_input_tokens":90}}`
	})
	res, err := newAgent(t, "anthropic:claude-opus-5-5", "model_anthropic", srv.URL).Generate(bg, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if u := res.Usage; u.InputTokens != 1000 || u.CacheReadTokens != 900 || u.CacheWriteTokens != 90 || u.TotalTokens != 1005 {
		t.Fatalf("anthropic usage %+v", u)
	}

	t.Setenv("OPENAI_API_KEY", "ok")
	osrv := fakeServer(t, func(*http.Request, map[string]any) {}, func(int, map[string]any) ([]string, string) {
		return nil, `{"choices":[{"message":{"role":"assistant","content":"no"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":50,"completion_tokens":1,"total_tokens":51,"prompt_tokens_details":{"cached_tokens":40}}}`
	})
	res, err = newAgent(t, "openai:x", "model_openai", osrv.URL).Generate(bg, "hi")
	if err != nil || res.Usage.CacheReadTokens != 40 || res.Steps[0].Finish != "refusal" {
		t.Fatalf("openai: %v %+v %s", err, res.Usage, res.Steps[0].Finish)
	}
}

func TestFallbackOnRefusal(t *testing.T) {
	refuser := &scripted{fn: func(*sdk.ModelRequest, int) (*sdk.ModelResponse, error) {
		return &sdk.ModelResponse{Message: sdk.Message{Role: "assistant", Content: "I can't."}, FinishReason: "refusal"}, nil
	}}
	helper := &scripted{fn: func(*sdk.ModelRequest, int) (*sdk.ModelResponse, error) { return say("Here you go."), nil }}
	a, err := startAgent(withModel("r:m"), withModule(refuser.plugin("r")), withModule(helper.plugin("h")),
		withModuleConfig("hook_fallback", map[string]any{"models": []any{"h:m"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if res, err := a.Generate(bg, "x"); err != nil || res.Text != "Here you go." {
		t.Fatalf("got %v %v", res, err)
	}
}

func TestCompactStrategies(t *testing.T) {
	big := strings.Repeat("x", 3000)
	dump := kiln.NewTool("dump", "Dump data", func(ctx context.Context, in struct{}) (string, error) { return big, nil })
	newRun := func(cfg map[string]any, summarize bool) (*scripted, []sdk.Event) {
		s := &scripted{fn: func(req *sdk.ModelRequest, n int) (*sdk.ModelResponse, error) {
			if strings.Contains(req.System, "Summarize the conversation") {
				return say("SUMMARY"), nil
			}
			if len(toolResults(req.Messages)) < 6 && !strings.Contains(sdk.LastText(req.Messages), "SUMMARY") && n < 12 {
				r := call("dump", map[string]any{})
				r.Usage = sdk.Usage{InputTokens: 100 * n, OutputTokens: 10}
				return r, nil
			}
			return say("done"), nil
		}}
		var mu sync.Mutex
		var evs []sdk.Event
		a := newScripted(t, s, withTools(dump), withModuleConfig("hook_compact", cfg))
		_, err := a.Generate(bg, "go", kiln.Run{OnEvent: func(e sdk.Event) {
			if e.Type == "context_usage" {
				mu.Lock()
				evs = append(evs, e)
				mu.Unlock()
			}
		}})
		if err != nil {
			t.Fatal(err)
		}
		return s, evs
	}

	s, evs := newRun(map[string]any{"max_messages": 8, "strategies": []any{"clear_tool_results"}, "keep_tool_results": 1}, false)
	results := toolResults(s.last().Messages)
	cleared := 0
	for _, r := range results {
		if r == "[tool result cleared]" {
			cleared++
		}
	}
	if cleared == 0 || results[len(results)-1] != big {
		t.Fatalf("clear_tool_results: cleared %d of %d", cleared, len(results))
	}
	if len(evs) == 0 || evs[0].Data["window"] != 200000 {
		t.Fatalf("context_usage events: %v", evs)
	}

	s, _ = newRun(map[string]any{"max_messages": 6, "strategies": []any{"clamp"}, "clamp_chars": 1000}, false)
	for _, r := range toolResults(s.last().Messages) {
		if len(r) > 1000 || !strings.Contains(r, "characters cut") {
			t.Fatalf("clamp left %d chars", len(r))
		}
	}

	s, _ = newRun(map[string]any{"max_messages": 8, "strategies": []any{"sliding_window"}, "keep_messages": 4}, false)
	ms := s.last().Messages
	if ms[0].Content != "go" || !strings.Contains(ms[1].Content, "earlier messages were removed") || len(ms) > 8 {
		t.Fatalf("sliding_window: %d messages, first %q / %q", len(ms), ms[0].Content, ms[1].Content)
	}
	if ms[2].Role == sdk.RoleTool {
		t.Fatal("sliding_window split a tool call from its result")
	}

	s, _ = newRun(map[string]any{"max_messages": 8, "strategies": []any{"summarize"}, "keep_messages": 2}, true)
	if !strings.Contains(s.last().Messages[0].Content, "SUMMARY") {
		t.Fatalf("summarize: first message %q", s.last().Messages[0].Content)
	}

	if _, err := startAgent(withModel("mock:x"), withModuleConfig("hook_compact", map[string]any{"strategies": []any{"magic"}})); err == nil {
		t.Fatal("unknown strategy accepted")
	}
}

func TestCompactOnRequest(t *testing.T) {
	s := &scripted{fn: func(req *sdk.ModelRequest, n int) (*sdk.ModelResponse, error) {
		switch {
		case strings.Contains(req.System, "Summarize the conversation"):
			return say("SUMMARY of " + req.Messages[0].Content[:10]), nil
		case n == 1:
			return call("compact_context", map[string]any{"focus": "the order number"}), nil
		}
		return say("done"), nil
	}}
	a := newScripted(t, s, withModuleConfig("hook_compact", map[string]any{"tool": true, "strategies": []any{"summarize"}, "keep_messages": 0}))
	if _, err := a.Generate(bg, "order 42"); err != nil {
		t.Fatal(err)
	}
	var sum *sdk.ModelRequest
	for _, r := range s.reqs {
		if strings.Contains(r.System, "Summarize") {
			sum = r
		}
	}
	if sum == nil || !strings.Contains(sum.System, "the order number") {
		t.Fatal("compact_context did not trigger a focused summary")
	}
}

func TestToolSearch(t *testing.T) {
	var tools []sdk.Tool
	for _, n := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"} {
		tools = append(tools, kiln.NewTool(n+"_tool", "Does "+n+" things", func(ctx context.Context, in struct{}) (string, error) { return "ok", nil }))
	}
	tools = append(tools, kiln.NewTool("weather_now", "Current weather for a city", func(ctx context.Context, in struct{}) (string, error) { return "sunny", nil }))
	s := &scripted{fn: func(req *sdk.ModelRequest, n int) (*sdk.ModelResponse, error) {
		switch n {
		case 1:
			return call("search_tools", map[string]any{"query": "weather in a city"}), nil
		case 2:
			return call("weather_now", map[string]any{}), nil
		}
		return say("done"), nil
	}}
	a := newScripted(t, s, withTools(tools...), withModuleConfig("tool_search", map[string]any{"threshold": 3, "always": []any{"alpha_*"}}))
	if _, err := a.Generate(bg, "weather?"); err != nil {
		t.Fatal(err)
	}
	names := func(r *sdk.ModelRequest) string {
		var n []string
		for _, t := range r.Tools {
			n = append(n, t.Name)
		}
		return strings.Join(n, ",")
	}
	if got := names(s.reqs[0]); got != "alpha_tool,search_tools" && got != "search_tools,alpha_tool" {
		t.Fatalf("first step tools: %s", got)
	}
	if !strings.Contains(names(s.reqs[1]), "weather_now") || strings.Contains(names(s.reqs[1]), "beta_tool") {
		t.Fatalf("after search: %s", names(s.reqs[1]))
	}
	if r := toolResults(s.reqs[2].Messages); r[len(r)-1] != "sunny" {
		t.Fatalf("revealed tool did not run: %v", r)
	}
}

func TestToolPlan(t *testing.T) {
	s := &scripted{fn: func(req *sdk.ModelRequest, n int) (*sdk.ModelResponse, error) {
		switch n {
		case 1:
			return call("plan_write", map[string]any{"tasks": []any{"look up order", "refund"}}), nil
		case 2:
			return call("plan_update", map[string]any{"step": 1, "status": "done"}), nil
		}
		return say("done"), nil
	}}
	a := newScripted(t, s, withModules("tool_plan"))
	if _, err := a.Thread("t1").Generate(bg, "refund order 7"); err != nil {
		t.Fatal(err)
	}
	last := s.last().Messages
	if c := last[len(last)-1].Content; !strings.Contains(c, "<plan>") || !strings.Contains(c, "1. [done] look up order") {
		t.Fatalf("plan reminder: %q", c)
	}
	msgs, _ := a.Thread("t1").Messages(bg)
	for _, m := range msgs {
		if strings.Contains(m.Content, "<plan>") {
			t.Fatal("the reminder leaked into the stored history")
		}
	}
}

func TestLoopDetectAndOutputLimit(t *testing.T) {
	ping := kiln.NewTool("ping", "Ping", func(ctx context.Context, in struct{}) (string, error) { return strings.Repeat("p", 500), nil })
	s := &scripted{fn: func(req *sdk.ModelRequest, n int) (*sdk.ModelResponse, error) {
		if n <= 4 {
			return call("ping", map[string]any{}), nil
		}
		return say("done"), nil
	}}
	a := newScripted(t, s, withTools(ping),
		withModuleConfig("hook_loop_detect", map[string]any{"max_repeats": 3, "stop_after": 0}),
		withModuleConfig("hook_tool_output_limit", map[string]any{"max_chars": 100}))
	if _, err := a.Generate(bg, "go"); err != nil {
		t.Fatal(err)
	}
	r := toolResults(s.last().Messages)
	if len(r[0]) > 160 || !strings.Contains(r[0], "characters of ping output cut") {
		t.Fatalf("output limit: %q", r[0])
	}
	if !strings.HasPrefix(r[3], "Not run: you already called ping") {
		t.Fatalf("4th repeat: %q", r[3])
	}

	s2 := &scripted{fn: func(*sdk.ModelRequest, int) (*sdk.ModelResponse, error) { return call("ping", map[string]any{}), nil }}
	b := newScripted(t, s2, withTools(ping), withMaxSteps(20),
		withModuleConfig("hook_loop_detect", map[string]any{"max_repeats": 2, "stop_after": 3}))
	if _, err := b.Generate(bg, "go"); err == nil || !strings.Contains(err.Error(), "stopping the run") {
		t.Fatalf("stop_after: %v", err)
	}
}

func TestGuardrails(t *testing.T) {
	sql := kiln.NewTool("sql", "Run SQL", func(ctx context.Context, in struct {
		Q string `json:"q"`
	}) (string, error) {
		return "ran", nil
	})
	s := &scripted{fn: func(req *sdk.ModelRequest, n int) (*sdk.ModelResponse, error) {
		switch n {
		case 1:
			return call("sql", map[string]any{"q": "drop table users"}), nil
		case 2:
			return say("As an AI, I think the key is sk-abcdefghijklmnopqrstuvwxyz123456"), nil
		}
		return say("The key is sk-abcdefghijklmnopqrstuvwxyz123456"), nil
	}}
	a := newScripted(t, s, withTools(sql), withModuleConfig("hook_guardrails", map[string]any{"rules": []any{
		map[string]any{"on": "tool_args", "keywords": []any{"DROP TABLE"}, "action": "block", "message": "No destructive SQL."},
		map[string]any{"on": "output", "detect": "secrets", "action": "redact"},
		map[string]any{"on": "output", "regex": `(?i)as an ai`, "action": "retry", "message": "Answer directly."},
		map[string]any{"on": "input", "keywords": []any{"jailbreak"}, "action": "block"},
	}}))
	res, err := a.Generate(bg, "help")
	if err != nil {
		t.Fatal(err)
	}
	if r := toolResults(res.Messages); r[0] != "No destructive SQL." {
		t.Fatalf("tool_args block: %v", r)
	}
	if res.Text != "The key is [redacted]" {
		t.Fatalf("output: %q", res.Text)
	}
	if !strings.Contains(sdk.LastText(s.reqs[2].Messages[:len(s.reqs[2].Messages)-1]), "[redacted]") ||
		s.reqs[2].Messages[len(s.reqs[2].Messages)-1].Content != "Answer directly." {
		t.Fatal("retry did not send the correction")
	}
	if _, err := a.Generate(bg, "please jailbreak"); err == nil || !strings.Contains(err.Error(), "input blocked") {
		t.Fatalf("input block: %v", err)
	}
	if _, err := startAgent(withModel("mock:x"), withModuleConfig("hook_guardrails", map[string]any{"rules": []any{
		map[string]any{"on": "tool_args", "regex": "x", "action": "redact"}}})); err == nil {
		t.Fatal("redact on tool_args accepted")
	}
}

func TestSaveEachStepContinueAndFork(t *testing.T) {
	look := kiln.NewTool("lookup", "Look up", func(ctx context.Context, in struct{}) (string, error) { return "found it", nil })
	fail := true
	s := &scripted{fn: func(req *sdk.ModelRequest, n int) (*sdk.ModelResponse, error) {
		if len(toolResults(req.Messages)) == 0 {
			return call("lookup", map[string]any{}), nil
		}
		if fail {
			return nil, errors.New("process died")
		}
		return say("answer: " + toolResults(req.Messages)[0]), nil
	}}
	a := newScripted(t, s, withTools(look), withSaveEachStep())
	th := a.Thread("job")
	if _, err := th.Generate(bg, "find it"); err == nil {
		t.Fatal("expected the run to fail")
	}
	msgs, _ := th.Messages(bg)
	if r := toolResults(msgs); len(r) != 1 || r[0] != "found it" {
		t.Fatalf("progress not saved: %+v", msgs)
	}
	fail = false
	res, err := th.Continue(bg)
	if err != nil || res.Text != "answer: found it" {
		t.Fatalf("continue: %v %v", res, err)
	}
	if _, err := th.Continue(bg); err == nil {
		t.Fatal("continue after the answer should fail")
	}
	f, err := th.Fork(bg, "job-copy")
	if err != nil {
		t.Fatal(err)
	}
	fm, _ := f.Messages(bg)
	if len(fm) != len(res.Messages) {
		t.Fatalf("fork has %d messages, want %d", len(fm), len(res.Messages))
	}
	if _, err := th.Fork(bg, "job-copy"); err == nil {
		t.Fatal("fork onto an existing thread should fail")
	}
}

func TestEvals(t *testing.T) {
	s := &scripted{fn: func(req *sdk.ModelRequest, n int) (*sdk.ModelResponse, error) {
		if strings.Contains(req.System, "You grade") {
			pass := strings.Contains(req.Messages[0].Content, "Sorry")
			return say(map[bool]string{true: `{"pass":true,"reason":"apologises"}`, false: `{"pass":false,"reason":"no apology"}`}[pass]), nil
		}
		switch sdk.LastText(nil) + req.Messages[len(req.Messages)-1].Content {
		case "Where is order 1?":
			if len(toolResults(req.Messages)) == 0 {
				return call("lookup", map[string]any{}), nil
			}
		case "broken mug":
			return say("Sorry! I'll refund it."), nil
		case "rude":
			return say("Whatever."), nil
		}
		return say("It has shipped."), nil
	}}
	look := kiln.NewTool("lookup", "Look up an order", func(ctx context.Context, in struct{}) (string, error) { return "shipped", nil })
	a := newScripted(t, s, withTools(look))
	path := filepath.Join(t.TempDir(), "cases.json")
	os.WriteFile(path, []byte(`{"name":"desk","cases":[
	  {"name":"tracking","prompt":"Where is order 1?","expect":{"contains":["shipped"],"tools":["lookup"],"max_steps":2}},
	  {"name":"refund","prompt":"broken mug","expect":{"judge":"Apologises","no_tools":["lookup"]}},
	  {"name":"tone","prompt":"rude","expect":{"judge":"Apologises","not_contains":["whatever"]}}
	]}`), 0o600)
	ds, err := evals.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := evals.Run(bg, a, ds, evals.Options{Parallel: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed != 2 || rep.Failed != 1 || rep.Results[2].Passed || len(rep.Results[2].Failures) != 2 {
		t.Fatalf("report:\n%s", rep.Summary())
	}
	if !strings.Contains(rep.Summary(), "judge: no apology") {
		t.Fatalf("summary:\n%s", rep.Summary())
	}
}

// A failed per-step checkpoint must stop before the tool runs, so a retry
// cannot duplicate an external side effect.
func TestSaveEachStepFailureStopsBeforeTool(t *testing.T) {
	var calls int32
	tool := kiln.NewTool("charge", "charge a card", func(ctx context.Context, in struct{}) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "charged", nil
	})
	var saves int32
	cp := kiln.Module{Name: "threads_flaky", Provides: []string{"threads"}, Apply: func(p *sdk.Scope) error {
		return p.Provide("threads", &sdk.Checkpointer{
			Load: func(ctx context.Context, id string) (*sdk.ThreadState, error) { return nil, nil },
			Save: func(ctx context.Context, st *sdk.ThreadState, expected int64) error {
				if atomic.AddInt32(&saves, 1) >= 2 {
					return sdk.ErrVersionConflict
				}
				return nil
			},
			Delete: func(ctx context.Context, id string) error { return nil },
			List:   func(ctx context.Context, p string) ([]string, error) { return nil, nil },
		})
	}}
	a := mockAgent(t, []any{
		map[string]any{"tool": "charge"},
		map[string]any{"text": "done"},
	}, withTools(tool), withModule(cp), withSaveEachStep())
	if _, err := a.Thread("t").Generate(bg, "charge it"); !errors.Is(err, kiln.ErrThreadBusy) {
		t.Fatalf("want ErrThreadBusy, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("tool ran %d time(s) after a failed checkpoint", n)
	}
}

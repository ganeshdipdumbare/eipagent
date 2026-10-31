package eipagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"eipagent"
	"eipagent/sdk"
)

var bg = context.Background()

func mockAgent(t *testing.T, script []any, opts ...eipagent.Option) *eipagent.Agent {
	t.Helper()
	base := []eipagent.Option{
		eipagent.WithModel("mock:test"),
		eipagent.WithPlugin("llm_mock", map[string]any{"script": script}),
		eipagent.WithLogger(func(scope, msg string) { t.Logf("[%s] %s", scope, msg) }),
	}
	a, err := eipagent.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

type weatherIn struct {
	City string `json:"city" desc:"city name"`
	Unit string `json:"unit,omitempty" enum:"c,f"`
}

func TestGenerateWithTypedTool(t *testing.T) {
	var got weatherIn
	weather := eipagent.Tool("weather", "Get weather", func(ctx context.Context, in weatherIn) (map[string]any, error) {
		got = in
		return map[string]any{"city": in.City, "temp": 21}, nil
	})
	a := mockAgent(t, []any{
		map[string]any{"tool": "weather", "args": map[string]any{"city": "Paris"}},
		map[string]any{"text": "It is 21 degrees in Paris."},
	}, eipagent.WithTools(weather))

	res, err := a.Generate(bg, "Weather in Paris?")
	if err != nil {
		t.Fatal(err)
	}
	if got.City != "Paris" || res.Text != "It is 21 degrees in Paris." || len(res.Steps) != 2 {
		t.Fatalf("got %+v / %+v", got, res)
	}
	if calls := res.ToolCalls(); len(calls) != 1 || calls[0].Name != "weather" {
		t.Fatalf("tool calls: %+v", calls)
	}
	tr := res.Steps[0].ToolResults
	if len(tr) != 1 || !strings.Contains(tr[0].Content, `"temp":21`) {
		t.Fatalf("tool result: %+v", tr)
	}
}

func TestStream(t *testing.T) {
	a := mockAgent(t, []any{map[string]any{"text": "one two three"}})
	s := a.Stream(bg, "count")
	var sb strings.Builder
	for d := range s.Text() {
		sb.WriteString(d)
	}
	res, err := s.Result()
	if err != nil {
		t.Fatal(err)
	}
	if sb.String() != "one two three" || res.Text != "one two three" {
		t.Fatalf("streamed %q, result %q", sb.String(), res.Text)
	}
}

func TestStreamEarlyBreak(t *testing.T) {
	a := mockAgent(t, []any{map[string]any{"text": "a b c d e f g"}})
	s := a.Stream(bg, "x")
	for range s.Events() {
		break
	}
	if res, err := s.Result(); err != nil || res.Text == "" {
		t.Fatalf("run should complete after an early break: %v %v", res, err)
	}
}

type recipe struct {
	Name        string   `json:"name"`
	Ingredients []string `json:"ingredients"`
}

func TestGenerateObjectWithRepair(t *testing.T) {
	a := mockAgent(t, []any{
		map[string]any{"text": "Sure! here you go"},
		map[string]any{"json": map[string]any{"name": "Pancakes", "ingredients": []any{"flour", "milk"}}},
	})
	r, err := eipagent.GenerateObject[recipe](bg, a, "A recipe")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "Pancakes" || len(r.Ingredients) != 2 {
		t.Fatalf("got %+v", r)
	}
	n, err := eipagent.GenerateObject[[]int](bg, mockAgent(t, []any{map[string]any{"json": map[string]any{"value": []any{1, 2, 3}}}}), "numbers")
	if err != nil || len(n) != 3 {
		t.Fatalf("wrapped non-object: %v %v", n, err)
	}
}

func TestThreadPersistsHistory(t *testing.T) {
	a := mockAgent(t, nil)
	th := a.Thread("t1")
	if _, err := th.Generate(bg, "hello"); err != nil {
		t.Fatal(err)
	}
	res, err := th.Generate(bg, "again")
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := th.Messages(bg)
	if len(msgs) != 4 || res.Thread != "t1" {
		t.Fatalf("expected 4 stored messages, got %d", len(msgs))
	}
}

func TestMaxSteps(t *testing.T) {
	loopTool := eipagent.Tool("again", "loops", func(ctx context.Context, in struct{}) (string, error) { return "ok", nil })
	var script []any
	for i := 0; i < 10; i++ {
		script = append(script, map[string]any{"tool": "again"})
	}
	a := mockAgent(t, script, eipagent.WithTools(loopTool))
	res, err := a.Generate(bg, "go", eipagent.MaxSteps(3))
	if err != nil || res.FinishReason != "max_steps" || len(res.Steps) != 3 {
		t.Fatalf("got %v %+v", err, res)
	}
	res, err = a.Generate(bg, "go", eipagent.StopOnTool("again"))
	if err != nil || res.FinishReason != "tool:again" || len(res.Steps) != 1 {
		t.Fatalf("stop on tool: %v %+v", err, res)
	}
}

func TestNoModel(t *testing.T) {
	t.Setenv("EIPAGENT_MODEL", "")
	if _, err := eipagent.New(); err == nil {
		t.Fatal("New without a model should fail clearly")
	}
}

func TestCompiledPluginAndMiddleware(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	p := eipagent.Plugin{Name: "spy", Apply: func(ctx *eipagent.PluginContext) error {
		ctx.WrapModel(func(c context.Context, req *eipagent.ModelRequest, next eipagent.ModelHandler) (*eipagent.ModelResponse, error) {
			mu.Lock()
			seen = append(seen, req.Model)
			mu.Unlock()
			return next(c, req)
		})
		return nil
	}}
	a := mockAgent(t, nil, eipagent.WithCompiledPlugin(p))
	if _, err := a.Generate(bg, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "mock:test" {
		t.Fatalf("middleware saw %v", seen)
	}
}

// ---- OpenAI-compatible adapter against a fake server ----

func fakeOpenAI(t *testing.T, handle func(body map[string]any) (status int, reply string, sse []string)) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, "bad key", 401)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		status, reply, sse := handle(body)
		if sse != nil {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range sse {
				fmt.Fprintf(w, "data: %s\n\n", ev)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func openaiAgent(t *testing.T, url string, opts ...eipagent.Option) *eipagent.Agent {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	a, err := eipagent.New(append([]eipagent.Option{
		eipagent.WithModel("openai:gpt-test"),
		eipagent.WithPluginSpec(eipagent.PluginSpec{Use: "llm_openai", As: "llm_openai", Config: map[string]any{"base_url": url}}),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestOpenAIToolRoundTrip(t *testing.T) {
	calls := 0
	srv := fakeOpenAI(t, func(body map[string]any) (int, string, []string) {
		calls++
		msgs := body["messages"].([]any)
		if calls == 1 {
			if body["model"] != "gpt-test" || len(body["tools"].([]any)) != 1 {
				t.Errorf("bad first request: %v", body)
			}
			return 200, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Oslo\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, nil
		}
		last := msgs[len(msgs)-1].(map[string]any)
		if last["role"] != "tool" || last["tool_call_id"] != "c1" {
			t.Errorf("tool result not sent back: %v", last)
		}
		return 200, `{"choices":[{"message":{"content":"Cold in Oslo."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":4,"total_tokens":24}}`, nil
	})
	weather := eipagent.Tool("weather", "w", func(ctx context.Context, in weatherIn) (string, error) { return "-3C in " + in.City, nil })
	a := openaiAgent(t, srv.URL, eipagent.WithTools(weather))
	res, err := a.Generate(bg, "Weather in Oslo?")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Cold in Oslo." || res.Usage.TotalTokens != 39 {
		t.Fatalf("got %+v", res)
	}
}

func TestOpenAIStreaming(t *testing.T) {
	srv := fakeOpenAI(t, func(body map[string]any) (int, string, []string) {
		if body["stream"] != true {
			t.Errorf("expected stream=true")
		}
		return 0, "", []string{
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		}
	})
	a := openaiAgent(t, srv.URL)
	s := a.Stream(bg, "hi")
	var parts []string
	for d := range s.Text() {
		parts = append(parts, d)
	}
	res, err := s.Result()
	if err != nil || res.Text != "Hello" || strings.Join(parts, "|") != "Hel|lo" || res.Usage.TotalTokens != 5 {
		t.Fatalf("got %v %+v %v", err, res, parts)
	}
}

func TestOpenAIStructuredAndErrors(t *testing.T) {
	srv := fakeOpenAI(t, func(body map[string]any) (int, string, []string) {
		rf, _ := body["response_format"].(map[string]any)
		if rf["type"] != "json_schema" {
			return 500, `{"error":"want json_schema"}`, nil
		}
		return 200, `{"choices":[{"message":{"content":"{\"name\":\"Soup\",\"ingredients\":[\"water\"]}"},"finish_reason":"stop"}]}`, nil
	})
	a := openaiAgent(t, srv.URL)
	r, err := eipagent.GenerateObject[recipe](bg, a, "soup")
	if err != nil || r.Name != "Soup" {
		t.Fatalf("got %+v %v", r, err)
	}
	// Plugins see the environment as it was when they loaded.
	t.Setenv("OPENAI_API_KEY", "wrong")
	b, err := eipagent.New(eipagent.WithModel("openai:gpt-test"),
		eipagent.WithPluginSpec(eipagent.PluginSpec{Use: "llm_openai", As: "llm_openai", Config: map[string]any{"base_url": srv.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, err = b.Generate(bg, "hi")
	var pe *eipagent.ProviderError
	if !errors.As(err, &pe) || pe.Status != 401 || pe.Retryable {
		t.Fatalf("expected a non-retryable 401 ProviderError, got %v", err)
	}
}

func TestRunLifecycleAlwaysCloses(t *testing.T) {
	var log []string
	var mu sync.Mutex
	rec := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }
	spy := eipagent.Plugin{Name: "lifecycle_spy", Apply: func(ctx *eipagent.PluginContext) error {
		ctx.OnAgentStart(func(c context.Context, st *sdk.RunState) error { rec("start"); return nil })
		ctx.OnAgentEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error { rec("end:" + res.FinishReason); return nil })
		ctx.OnEvent(func(e sdk.Event) {
			if e.Type == "error" || e.Type == "finish" {
				rec(e.Type)
			}
		})
		return nil
	}}
	failing := eipagent.Tool("boom", "b", func(ctx context.Context, in struct{}) (string, error) { return "", sdk.Fatal(errors.New("disk on fire")) })
	a := mockAgent(t, []any{map[string]any{"text": "fine"}, map[string]any{"tool": "boom"}}, eipagent.WithCompiledPlugin(spy), eipagent.WithTools(failing))
	if _, err := a.Generate(bg, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Generate(bg, "two"); err == nil {
		t.Fatal("fatal tool error should fail the run")
	}
	if got := strings.Join(log, ","); got != "start,end:stop,finish,start,error,end:error" {
		t.Fatalf("lifecycle: %s", got)
	}
}

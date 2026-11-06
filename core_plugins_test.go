package kiln_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// searcher is a compiled provider that calls web_search once, then
// answers with the tool's result.
func searcher(query string) kiln.Module {
	return kiln.Module{Name: "searcher", Provides: []string{"model/searcher"}, Apply: func(ctx *sdk.Scope) error {
		return ctx.Provide("model/searcher", &sdk.Provider{Name: "searcher", Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			last := req.Messages[len(req.Messages)-1]
			if last.Role == sdk.RoleTool {
				return &sdk.ModelResponse{Message: sdk.Message{Role: "assistant", Content: last.Content}}, nil
			}
			call := sdk.ToolCall{ID: "c1", Name: "web_search", Args: map[string]any{"query": query}}
			return &sdk.ModelResponse{Message: sdk.Message{Role: "assistant", ToolCalls: []sdk.ToolCall{call}}, FinishReason: "tool_calls"}, nil
		}})
	}}
}

// capture is a compiled provider that records each request and answers
// with the replies in turn (the last one repeats).
func capture(name string, got *[]*sdk.ModelRequest, replies ...string) kiln.Module {
	return kiln.Module{Name: "capture_" + name, Provides: []string{"model/" + name}, Apply: func(ctx *sdk.Scope) error {
		return ctx.Provide("model/"+name, &sdk.Provider{Name: name, Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			cp := *req
			cp.Messages = append([]sdk.Message(nil), req.Messages...)
			*got = append(*got, &cp)
			r := replies[len(replies)-1]
			if len(*got) <= len(replies) {
				r = replies[len(*got)-1]
			}
			return &sdk.ModelResponse{Message: sdk.Message{Role: "assistant", Content: r}}, nil
		}})
	}}
}

func TestThinkingAndCacheHooks(t *testing.T) {
	var got []*sdk.ModelRequest
	a, err := startAgent(withModel("cap:m"), withModule(capture("cap", &got, "ok")),
		withModuleConfig("hook_thinking", map[string]any{"effort": "high"}),
		withModuleConfig("hook_cache", map[string]any{"ttl": "1h"}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Generate(bg, "hi"); err != nil {
		t.Fatal(err)
	}
	th, _ := got[0].Extra["kiln.thinking"].(map[string]any)
	cc, _ := got[0].Extra["kiln.cache"].(map[string]any)
	if th["effort"] != "high" || cc["ttl"] != "1h" {
		t.Fatalf("extra not set: %v", got[0].Extra)
	}

	if _, err := startAgent(withModel("cap:m"), withModule(capture("cap", &got, "ok")),
		withModuleConfig("hook_thinking", map[string]any{"effort": "extreme"})); err == nil {
		t.Fatal("bad effort accepted")
	}
}

func TestThinkingAndCacheOnAnthropic(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "ak")
	var body map[string]any
	srv := fakeServer(t, func(r *http.Request, b map[string]any) { body = b }, func(n int, _ map[string]any) ([]string, string) {
		return nil, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	})
	a := newAgent(t, "anthropic:claude-opus-5-5", "model_anthropic", srv.URL,
		withInstructions("Be brief."),
		withModuleConfig("hook_thinking", map[string]any{"effort": "max", "display": "summarized"}),
		withModuleConfig("hook_cache", nil))
	if _, err := a.Generate(bg, "hi"); err != nil {
		t.Fatal(err)
	}
	th, _ := body["thinking"].(map[string]any)
	oc, _ := body["output_config"].(map[string]any)
	if th["type"] != "adaptive" || th["display"] != "summarized" || oc["effort"] != "max" {
		t.Errorf("thinking: %v %v", body["thinking"], body["output_config"])
	}
	sys, _ := body["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["cache_control"] == nil || body["cache_control"] == nil {
		t.Errorf("cache breakpoints: system=%v top=%v", body["system"], body["cache_control"])
	}
	for k := range body {
		if strings.HasPrefix(k, "kiln.") {
			t.Errorf("internal key %s leaked into the request", k)
		}
	}
}

func TestThinkingOnOpenAIAndGemini(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ok")
	t.Setenv("GEMINI_API_KEY", "gk")
	var ob, gb map[string]any
	osrv := fakeServer(t, func(r *http.Request, b map[string]any) { ob = b }, func(int, map[string]any) ([]string, string) {
		return nil, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	})
	gsrv := fakeServer(t, func(r *http.Request, b map[string]any) { gb = b }, func(int, map[string]any) ([]string, string) {
		return nil, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
	})
	thinking := withModuleConfig("hook_thinking", map[string]any{"effort": "low"})
	if _, err := newAgent(t, "openai:o9", "model_openai", osrv.URL, thinking, withModuleConfig("hook_cache", nil)).Thread("t1").Generate(bg, "hi"); err != nil {
		t.Fatal(err)
	}
	if ob["reasoning_effort"] != "low" || ob["prompt_cache_key"] != "t1" {
		t.Errorf("openai: %v", ob)
	}
	if _, err := newAgent(t, "gemini:g", "model_gemini", gsrv.URL, thinking).Generate(bg, "hi"); err != nil {
		t.Fatal(err)
	}
	gen, _ := gb["generationConfig"].(map[string]any)
	tc, _ := gen["thinkingConfig"].(map[string]any)
	if tc["thinkingLevel"] != "low" || tc["includeThoughts"] != true {
		t.Errorf("gemini: %v", gb)
	}
}

func TestOutputValidate(t *testing.T) {
	type City struct {
		Name       string `json:"name"`
		Population int    `json:"population"`
	}
	var got []*sdk.ModelRequest
	a, err := startAgent(withModel("cap:m"),
		withModule(capture("cap", &got, `{"name":"Rome"}`, `{"name":"Rome","population":2800000}`)),
		withModuleConfig("hook_output_validate", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c, err := kiln.GenerateObject[City](bg, a, "Largest city in Italy?")
	if err != nil || c.Population != 2800000 || len(got) != 2 {
		t.Fatalf("got %+v err=%v calls=%d", c, err, len(got))
	}
	last := got[1].Messages[len(got[1].Messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content, `missing required property "population"`) {
		t.Fatalf("correction not sent: %+v", last)
	}

	got = nil
	b, err := startAgent(withModel("cap:m"), withModule(capture("cap", &got, `not json`)),
		withModuleConfig("hook_output_validate", map[string]any{"max_retries": 1}))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := kiln.GenerateObject[City](bg, b, "x"); err == nil || !strings.Contains(err.Error(), "after 1 retries") {
		t.Fatalf("want failure after retries, got %v (calls %d)", err, len(got))
	}
}

func TestWebSearch(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		io.WriteString(w, `{"results":[{"title":"Go","url":"https://go.dev","content":"The Go language"},{"title":"Tour","url":"https://go.dev/tour","content":"A tour"}]}`)
	}))
	defer srv.Close()
	t.Setenv("TAVILY_API_KEY", "tk")
	a, err := startAgent(withModel("searcher:x"), withModule(searcher("golang")),
		withModuleConfig("tool_websearch", map[string]any{"backend": "tavily", "base_url": srv.URL, "max_results": 1}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	res, err := a.Generate(bg, "search")
	if err != nil {
		t.Fatal(err)
	}
	out := res.Text
	if !strings.Contains(out, "1. Go\n   https://go.dev\n   The Go language") || strings.Contains(out, "Tour") {
		t.Fatalf("out=%q", out)
	}
	if auth.Load() != "Bearer tk" {
		t.Fatalf("auth header %v", auth.Load())
	}

	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<div><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa&amp;rut=x">Example &amp; Co</a>
<a class="result__snippet" href="x">An <b>example</b> page</a></div>`)
	}))
	defer ddg.Close()
	b, err := startAgent(withModel("searcher:x"), withModule(searcher("example")),
		withModuleConfig("tool_websearch", map[string]any{"base_url": ddg.URL}))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	res, err = b.Generate(bg, "search")
	if err != nil || res.Text != "1. Example & Co\n   https://example.com/a\n   An example page" {
		t.Fatalf("ddg out=%q err=%v", res.Text, err)
	}
}

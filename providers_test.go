package eipagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"eipagent"
)

func fakeServer(t *testing.T, check func(r *http.Request, body map[string]any), reply func(n int, body map[string]any) (sse []string, js string)) *httptest.Server {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		check(r, body)
		n++
		sse, js := reply(n, body)
		if sse != nil {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range sse {
				fmt.Fprintf(w, "data: %s\n\n", ev)
			}
			return
		}
		io.WriteString(w, js)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAgent(t *testing.T, model, use, url string, opts ...eipagent.Option) *eipagent.Agent {
	t.Helper()
	prov, _, _ := strings.Cut(model, ":")
	a, err := eipagent.New(append([]eipagent.Option{
		eipagent.WithModel(model),
		eipagent.WithPluginSpec(eipagent.PluginSpec{Use: use, As: "llm_" + prov, Config: map[string]any{"name": prov, "base_url": url}}),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestAnthropic(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "ak")
	srv := fakeServer(t, func(r *http.Request, body map[string]any) {
		if r.Header.Get("x-api-key") != "ak" || r.URL.Path != "/v1/messages" || body["max_tokens"] == nil {
			t.Errorf("bad request %s %v", r.URL.Path, r.Header)
		}
	}, func(n int, body map[string]any) ([]string, string) {
		switch n {
		case 1: // tool call
			return nil, `{"content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"tu1","name":"weather","input":{"city":"Rome"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`
		case 2:
			msgs := body["messages"].([]any)
			last := msgs[len(msgs)-1].(map[string]any)
			blk := last["content"].([]any)[0].(map[string]any)
			if last["role"] != "user" || blk["type"] != "tool_result" || blk["tool_use_id"] != "tu1" {
				t.Errorf("tool_result not sent: %v", last)
			}
			return nil, `{"content":[{"type":"text","text":"Warm."}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":1}}`
		case 3: // structured via schema tool
			if body["tool_choice"].(map[string]any)["name"] != "final_recipe" {
				t.Errorf("schema tool not forced: %v", body["tool_choice"])
			}
			return nil, `{"content":[{"type":"tool_use","id":"x","name":"final_recipe","input":{"name":"Pasta","ingredients":["egg"]}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
		default: // streaming
			return []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":4}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Ci"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ao"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
				`{"type":"message_stop"}`,
			}, ""
		}
	})
	weather := eipagent.Tool("weather", "w", func(ctx context.Context, in weatherIn) (string, error) { return "25C", nil })
	a := newAgent(t, "anthropic:claude-test", "llm_anthropic", srv.URL, eipagent.WithTools(weather))
	res, err := a.Generate(bg, "Rome?")
	if err != nil || res.Text != "Warm." || res.Usage.TotalTokens != 18 {
		t.Fatalf("%v %+v", err, res)
	}
	b := newAgent(t, "anthropic:claude-test", "llm_anthropic", srv.URL)
	// reuse server counter: next call is n=3
	r, err := eipagent.GenerateObject[recipe](bg, b, "pasta")
	if err != nil || r.Name != "Pasta" {
		t.Fatalf("%v %+v", err, r)
	}
	s := b.Stream(bg, "hi")
	res, err = s.Result()
	if err != nil || res.Text != "Ciao" || res.Usage.TotalTokens != 6 {
		t.Fatalf("stream: %v %+v", err, res)
	}
}

func TestGemini(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	srv := fakeServer(t, func(r *http.Request, body map[string]any) {
		if r.Header.Get("x-goog-api-key") != "gk" || !strings.HasPrefix(r.URL.Path, "/models/gem-test:") {
			t.Errorf("bad request %s", r.URL.Path)
		}
	}, func(n int, body map[string]any) ([]string, string) {
		switch n {
		case 1:
			return nil, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"weather","args":{"city":"Tokyo"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`
		case 2:
			contents := body["contents"].([]any)
			last := contents[len(contents)-1].(map[string]any)
			fr := last["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
			if fr["name"] != "weather" {
				t.Errorf("functionResponse missing: %v", last)
			}
			return nil, `{"candidates":[{"content":{"parts":[{"text":"Rainy."}]},"finishReason":"STOP"}]}`
		case 3:
			gen := body["generationConfig"].(map[string]any)
			if gen["responseMimeType"] != "application/json" {
				t.Errorf("no json mode: %v", gen)
			}
			return nil, `{"candidates":[{"content":{"parts":[{"text":"{\"name\":\"Ramen\",\"ingredients\":[\"noodles\"]}"}]}}]}`
		default:
			return []string{
				`{"candidates":[{"content":{"parts":[{"text":"Kon"}]}}]}`,
				`{"candidates":[{"content":{"parts":[{"text":"nichiwa"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`,
			}, ""
		}
	})
	weather := eipagent.Tool("weather", "w", func(ctx context.Context, in weatherIn) (string, error) { return "rain", nil })
	a := newAgent(t, "gemini:gem-test", "llm_gemini", srv.URL, eipagent.WithTools(weather))
	res, err := a.Generate(bg, "Tokyo?")
	if err != nil || res.Text != "Rainy." {
		t.Fatalf("%v %+v", err, res)
	}
	r, err := eipagent.GenerateObject[recipe](bg, a, "ramen")
	if err != nil || r.Name != "Ramen" {
		t.Fatalf("%v %+v", err, r)
	}
	var sb strings.Builder
	s := a.Stream(bg, "hi")
	for d := range s.Text() {
		sb.WriteString(d)
	}
	if res, err := s.Result(); err != nil || sb.String() != "Konnichiwa" || res.Usage.TotalTokens != 3 {
		t.Fatalf("stream: %v %q", err, sb.String())
	}
}

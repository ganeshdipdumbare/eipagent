//go:build ignore

package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"eipagent/sdk"
)

// A deterministic fake LLM for tests and offline demos, provided as
// "llm/mock" (model string "mock:anything").
//
// Config:
//
//	script: [{text: "..."} | {tool: "name", args: {...}} | {json: {...}}]
//	        replies used in order, one per model call; after the script
//	        runs out the heuristics below apply.
//	name:   service suffix (default "mock")
//
// Heuristics: replies to a tool result by echoing it; "time" calls
// current_time; "remember X" calls remember; "recall" calls recall.
func Apply(ctx *sdk.Context) error {
	script := ctx.ConfigList("script")
	var mu sync.Mutex
	next := 0
	name := ctx.ConfigString("name", "mock")

	reply := func(req *sdk.ModelRequest) sdk.Message {
		mu.Lock()
		var step map[string]any
		if next < len(script) {
			step = script[next]
			next++
		}
		mu.Unlock()
		if step != nil {
			if t, ok := step["tool"].(string); ok {
				args, _ := step["args"].(map[string]any)
				return sdk.Message{Role: "assistant", ToolCalls: []sdk.ToolCall{{Name: t, Args: args}}}
			}
			if j, ok := step["json"]; ok {
				b, _ := json.Marshal(j)
				return sdk.Message{Role: "assistant", Content: string(b)}
			}
			t, _ := step["text"].(string)
			return sdk.Message{Role: "assistant", Content: t}
		}
		has := func(n string) bool {
			for _, t := range req.Tools {
				if t.Name == n {
					return true
				}
			}
			return false
		}
		call := func(n string, args map[string]any) sdk.Message {
			return sdk.Message{Role: "assistant", ToolCalls: []sdk.ToolCall{{Name: n, Args: args}}}
		}
		if len(req.Messages) == 0 {
			return sdk.Message{Role: "assistant", Content: "Hello."}
		}
		if req.ResponseSchema != nil && req.Messages[len(req.Messages)-1].Role != sdk.RoleTool {
			b, _ := json.Marshal(example(req.ResponseSchema))
			return sdk.Message{Role: "assistant", Content: string(b)}
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == sdk.RoleTool {
			return sdk.Message{Role: "assistant", Content: last.Content}
		}
		text := strings.ToLower(last.Content)
		switch {
		case strings.Contains(text, "time") && has("current_time"):
			return call("current_time", nil)
		case strings.HasPrefix(text, "remember ") && has("remember"):
			return call("remember", map[string]any{"text": last.Content[len("remember "):]})
		case strings.Contains(text, "recall") && has("recall"):
			return call("recall", map[string]any{})
		}
		var names []string
		for _, t := range req.Tools {
			names = append(names, t.Name)
		}
		return sdk.Message{Role: "assistant", Content: "echo: " + last.Content + " (tools: " + strings.Join(names, ", ") + ")"}
	}

	return ctx.Provide("llm/"+name, &sdk.Provider{
		Name:         name,
		Capabilities: map[string]bool{"streaming": true, "tools": true, "structured_output": true},
		Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			if err := c.Err(); err != nil {
				return nil, err
			}
			msg := reply(req)
			if req.Stream != nil && msg.Content != "" {
				words := strings.SplitAfter(msg.Content, " ")
				for _, w := range words {
					req.Stream(sdk.Chunk{Type: "text", Text: w})
				}
			}
			finish := "stop"
			if len(msg.ToolCalls) > 0 {
				finish = "tool_calls"
			}
			in := 0
			for _, m := range req.Messages {
				in += len(strings.Fields(m.Content))
			}
			out := len(strings.Fields(msg.Content))
			return &sdk.ModelResponse{Message: msg, FinishReason: finish, Usage: sdk.Usage{InputTokens: in, OutputTokens: out, TotalTokens: in + out}}, nil
		},
	})
}

// example builds a placeholder value matching a JSON Schema.
func example(s map[string]any) map[string]any {
	out := map[string]any{}
	props, _ := s["properties"].(map[string]any)
	for k, v := range props {
		p, _ := v.(map[string]any)
		t, _ := p["type"].(string)
		if t == "string" {
			out[k] = "example"
		} else if t == "integer" || t == "number" {
			out[k] = 1.0
		} else if t == "boolean" {
			out[k] = true
		} else if t == "array" {
			out[k] = []any{}
		} else if t == "object" {
			out[k] = example(p)
		}
	}
	return out
}

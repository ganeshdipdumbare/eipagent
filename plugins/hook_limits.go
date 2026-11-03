//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"sync"

	"eipagent/sdk"
)

// Caps model calls, tool calls and tokens per run and per thread.
//
// Config:
//
//	model_calls_run, model_calls_thread, tool_calls_run, tool_calls_thread,
//	tokens_run, tokens_thread: 0 = unlimited
//	tools:          {tool name: max calls per run}
//	exit_behavior:  "end" (default: finish politely) | "error" (fail the run)
//
// Example:
//
//	eipagent.WithPlugin("hook_limits", map[string]any{
//		"model_calls_run": 20,
//		"tokens_thread":   500000,
//		"tools":           map[string]any{"web_search": 5},
//	})
func Apply(p *sdk.Plugin) error {
	lim := map[string]int{}
	for _, k := range []string{"model_calls_run", "model_calls_thread", "tool_calls_run", "tool_calls_thread", "tokens_run", "tokens_thread"} {
		lim[k] = p.ConfigInt(k, 0)
	}
	perTool := p.ConfigMap("tools")
	exit := p.ConfigString("exit_behavior", "end")

	type counts struct {
		model, tools, tokens int
		perTool              map[string]int
	}
	var mu sync.Mutex
	runs := map[*sdk.RunState]*counts{}
	get := func(st *sdk.RunState) *counts {
		mu.Lock()
		defer mu.Unlock()
		c := runs[st]
		if c == nil {
			c = &counts{perTool: map[string]int{}}
			runs[st] = c
		}
		return c
	}
	thread := func(st *sdk.RunState, key string) int {
		if st.Values == nil {
			return 0
		}
		n, _ := st.Values["limits."+key].(float64)
		if i, ok := st.Values["limits."+key].(int); ok {
			n = float64(i)
		}
		return int(n)
	}
	over := func(limit, n int) bool { return limit > 0 && n >= limit }

	p.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		if req.Run == nil {
			return next(c, req)
		}
		cn := get(req.Run)
		mu.Lock()
		hit := ""
		switch {
		case over(lim["model_calls_run"], cn.model):
			hit = "model calls per run"
		case over(lim["model_calls_thread"], thread(req.Run, "model")+cn.model):
			hit = "model calls per thread"
		case over(lim["tokens_run"], cn.tokens):
			hit = "tokens per run"
		case over(lim["tokens_thread"], thread(req.Run, "tokens")+cn.tokens):
			hit = "tokens per thread"
		}
		if hit == "" {
			cn.model++
		}
		mu.Unlock()
		if hit != "" {
			if exit == "error" {
				return nil, fmt.Errorf("limit reached: %s", hit)
			}
			return &sdk.ModelResponse{Message: sdk.Message{Role: sdk.RoleAssistant, Content: "Stopped: limit reached (" + hit + ")."}, FinishReason: "length"}, nil
		}
		resp, err := next(c, req)
		if err == nil {
			mu.Lock()
			cn.tokens += resp.Usage.TotalTokens
			mu.Unlock()
		}
		return resp, err
	})

	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		if req.Run == nil {
			return next(c, req)
		}
		cn := get(req.Run)
		mu.Lock()
		hit := ""
		max := 0
		switch v := perTool[req.Call.Name].(type) {
		case float64:
			max = int(v)
		case int:
			max = v
		}
		switch {
		case over(lim["tool_calls_run"], cn.tools):
			hit = "tool calls per run"
		case over(lim["tool_calls_thread"], thread(req.Run, "tools")+cn.tools):
			hit = "tool calls per thread"
		case over(max, cn.perTool[req.Call.Name]):
			hit = req.Call.Name + " calls per run"
		}
		if hit == "" {
			cn.tools++
			cn.perTool[req.Call.Name]++
		}
		mu.Unlock()
		if hit != "" {
			if exit == "error" {
				return nil, sdk.Fatal(fmt.Errorf("limit reached: %s", hit))
			}
			return &sdk.ToolResult{Content: "limit reached: " + hit + "; do not call this tool again", IsError: true}, nil
		}
		return next(c, req)
	})

	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		cn := runs[st]
		delete(runs, st)
		mu.Unlock()
		if cn != nil && st.Values != nil {
			st.Values["limits.model"] = thread(st, "model") + cn.model
			st.Values["limits.tools"] = thread(st, "tools") + cn.tools
			st.Values["limits.tokens"] = thread(st, "tokens") + cn.tokens
		}
		return nil
	})
	return nil
}

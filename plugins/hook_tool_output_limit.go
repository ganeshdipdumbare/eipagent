//go:build ignore

package plugin

import (
	"context"
	"fmt"

	"eipagent/sdk"
)

// Caps how much of a tool's result reaches the model. A longer result
// keeps its beginning and end, with a note of how much was cut, so one
// huge file or log can't fill the context window.
//
// Config: max_chars (default 20000), tools (per-tool limits, e.g.
// {"bash": 5000}; 0 = no limit for that tool).
//
// Example:
//
//	eipagent.WithPlugin("hook_tool_output_limit", map[string]any{
//		"max_chars": 10000,
//		"tools":     map[string]any{"read_file": 40000},
//	})
func Apply(p *sdk.Plugin) error {
	maxChars := p.ConfigInt("max_chars", 20000)
	perTool := p.ConfigMap("tools")

	limitOf := func(name string) int {
		switch v := perTool[name].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return maxChars
	}
	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		r, err := next(c, req)
		if err != nil || r == nil || r.Interrupt != nil {
			return r, err
		}
		limit := limitOf(req.Call.Name)
		if limit <= 0 || len(r.Content) <= limit {
			return r, nil
		}
		keep := limit / 2
		cut := len(r.Content) - 2*keep
		out := *r
		out.Content = r.Content[:keep] + fmt.Sprintf("\n…[%d characters of %s output cut]…\n", cut, req.Call.Name) + r.Content[len(r.Content)-keep:]
		return &out, nil
	})
	return nil
}

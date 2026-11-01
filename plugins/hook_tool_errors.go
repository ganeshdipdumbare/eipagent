//go:build ignore

package plugin

import (
	"context"
	"strings"

	"eipagent/sdk"
)

// Controls what happens when a tool fails.
//
// Config:
//
//	on_error: "message" (default: tell the model) | "raise" (abort the run)
//	template: message template; {tool} and {error} are replaced
//	tools:    only these tools ([] = all)
func Apply(p *sdk.Plugin) error {
	mode := p.ConfigString("on_error", "message")
	tmpl := p.ConfigString("template", "Tool {tool} failed: {error}. Check the arguments and try again, or use another approach.")
	only := p.ConfigStrings("tools")
	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		r, err := next(c, req)
		if err == nil {
			return r, nil
		}
		match := len(only) == 0
		for _, n := range only {
			if n == req.Call.Name {
				match = true
			}
		}
		if !match {
			return r, err
		}
		if mode == "raise" {
			return nil, sdk.Fatal(err)
		}
		msg := strings.ReplaceAll(strings.ReplaceAll(tmpl, "{tool}", req.Call.Name), "{error}", err.Error())
		return &sdk.ToolResult{Content: msg, IsError: true}, nil
	})
	return nil
}

package hook_tool_errors

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"strings"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Controls what happens when a tool fails.
//
// Config:
//
//	on_error: "message" (default: tell the model) | "raise" (abort the run)
//	template: message template; {tool} and {error} are replaced
//	tools:    only these tools ([] = all)
//
// Example:
//
//	kiln.WithModuleConfig("hook_tool_errors", map[string]any{
//		"on_error": "message",
//		"template": "{tool} failed: {error}. Try another way.",
//	})
func Apply(p *sdk.Module) error {
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

// Module is the compiled form of "hook_tool_errors". modules/std registers it.
var Module = engine.Module{Name: "hook_tool_errors", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

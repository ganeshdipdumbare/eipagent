//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"time"

	"eipagent/sdk"
)

// hello_hook wraps every model and tool call: it times them and blocks
// tools listed in config.
//
// Config: block (tool names to refuse).
func Apply(p *sdk.Plugin) error {
	blocked := map[string]bool{}
	for _, t := range p.ConfigStrings("block") {
		blocked[t] = true
	}
	p.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		t := time.Now()
		resp, err := next(c, req)
		p.Logf("model %s took %s", req.Model, time.Since(t).Round(time.Millisecond))
		return resp, err
	})
	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		if blocked[req.Call.Name] {
			return &sdk.ToolResult{Content: fmt.Sprintf("tool %s is blocked", req.Call.Name), IsError: true}, nil
		}
		return next(c, req)
	})
	return nil
}

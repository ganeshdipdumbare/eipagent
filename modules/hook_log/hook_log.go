package hook_log

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Logs every model and tool call with timing. Config: args (log tool
// arguments, default true), results (log tool results, default false).
//
// Example:
//
//	kiln.WithModuleConfig("hook_log", map[string]any{"results": true})
func Apply(p *sdk.Module) error {
	logArgs := p.ConfigBool("args", true)
	logResults := p.ConfigBool("results", false)
	p.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		t := time.Now()
		resp, err := next(c, req)
		if err != nil {
			p.Logf("model %s step %d failed after %s: %v", req.Model, req.Step, time.Since(t).Round(time.Millisecond), err)
			return nil, err
		}
		p.Logf("model %s step %d: %d tool calls, %d tokens, %s", req.Model, req.Step, len(resp.Message.ToolCalls), resp.Usage.TotalTokens, time.Since(t).Round(time.Millisecond))
		return resp, nil
	})
	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		t := time.Now()
		if logArgs {
			p.Logf("-> %s %v", req.Call.Name, req.Call.Args)
		} else {
			p.Logf("-> %s", req.Call.Name)
		}
		r, err := next(c, req)
		switch {
		case err != nil:
			p.Logf("<- %s error: %v (%s)", req.Call.Name, err, time.Since(t).Round(time.Millisecond))
		case logResults && r != nil:
			p.Logf("<- %s %q (%s)", req.Call.Name, r.Content, time.Since(t).Round(time.Millisecond))
		default:
			p.Logf("<- %s (%s)", req.Call.Name, time.Since(t).Round(time.Millisecond))
		}
		return r, err
	})
	return nil
}

// Module is the compiled form of "hook_log". modules/std registers it.
var Module = engine.Module{Name: "hook_log", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

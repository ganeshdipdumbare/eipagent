//go:build ignore

package plugin

import (
	"context"
	"time"

	"eipagent/sdk"
)

// Logs every model and tool call with timing. Config: args (log tool
// arguments, default true), results (log tool results, default false).
func Apply(ctx *sdk.Context) error {
	logArgs := ctx.ConfigBool("args", true)
	logResults := ctx.ConfigBool("results", false)
	ctx.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		t := time.Now()
		resp, err := next(c, req)
		if err != nil {
			ctx.Logf("model %s step %d failed after %s: %v", req.Model, req.Step, time.Since(t).Round(time.Millisecond), err)
			return nil, err
		}
		ctx.Logf("model %s step %d: %d tool calls, %d tokens, %s", req.Model, req.Step, len(resp.Message.ToolCalls), resp.Usage.TotalTokens, time.Since(t).Round(time.Millisecond))
		return resp, nil
	})
	ctx.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		t := time.Now()
		if logArgs {
			ctx.Logf("-> %s %v", req.Call.Name, req.Call.Args)
		} else {
			ctx.Logf("-> %s", req.Call.Name)
		}
		r, err := next(c, req)
		switch {
		case err != nil:
			ctx.Logf("<- %s error: %v (%s)", req.Call.Name, err, time.Since(t).Round(time.Millisecond))
		case logResults && r != nil:
			ctx.Logf("<- %s %q (%s)", req.Call.Name, r.Content, time.Since(t).Round(time.Millisecond))
		default:
			ctx.Logf("<- %s (%s)", req.Call.Name, time.Since(t).Round(time.Millisecond))
		}
		return r, err
	})
	return nil
}

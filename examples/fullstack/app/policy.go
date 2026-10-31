package app

import (
	"context"
	"fmt"

	"eipagent"
	"eipagent/sdk"
)

// RefundPolicy is a business rule written as a plugin: agents may refund
// up to limit on their own; anything above pauses the run until a
// supervisor approves, edits or rejects it. The pause is durable: it is
// stored with the thread and survives a server restart.
func RefundPolicy(limit float64) eipagent.Plugin {
	return eipagent.Plugin{Name: "refund_policy", Apply: func(ctx *eipagent.PluginContext) error {
		ctx.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
			if req.Call.Name != "issue_refund" || req.Decision != nil {
				return next(c, req) // other tools, or a supervisor already decided
			}
			amount, _ := req.Call.Args["amount"].(float64)
			if amount <= limit {
				return next(c, req)
			}
			call := req.Call
			return &sdk.ToolResult{Interrupt: &sdk.Interrupt{
				ID:       call.ID,
				Kind:     "refund_approval",
				ToolCall: &call,
				Message:  fmt.Sprintf("Refund of $%.2f is above the $%.0f agent limit.", amount, limit),
				Allowed:  []string{"approve", "edit", "reject"},
				Payload:  map[string]any{"amount": amount, "limit": limit},
			}}, nil
		})
		return nil
	}}
}

// Inspector shows what the model actually received after every other
// middleware ran (for example, card numbers already masked by mw_pii).
// It runs innermost and reports through the run's event stream.
func Inspector() eipagent.Plugin {
	return eipagent.Plugin{Name: "inspector", Apply: func(ctx *eipagent.PluginContext) error {
		ctx.SetPriority(-100)
		ctx.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
			if req.Run != nil && req.Run.Emit != nil {
				last := ""
				for i := len(req.Messages) - 1; i >= 0; i-- {
					if req.Messages[i].Role == sdk.RoleUser {
						last = req.Messages[i].Content
						break
					}
				}
				req.Run.Emit(sdk.Event{Type: "model_call", Step: req.Step, Text: last,
					Data: map[string]any{"model": req.Model, "messages": len(req.Messages), "tools": len(req.Tools)}})
			}
			return next(c, req)
		})
		return nil
	}}
}

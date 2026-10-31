//go:build ignore

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"eipagent/sdk"
)

// Auto approval review: a reviewer model judges risky tool calls before
// they run. It approves, denies, or escalates to a human (mw_hitl).
//
// Config:
//
//	model:   reviewer model, e.g. "anthropic:claude-haiku-5-5" (required)
//	tools:   tools to review ([] = every tool not marked readonly)
//	policy:  extra guidance for the reviewer
//	on_error: escalate (default) | deny
func Apply(ctx *sdk.Context) error {
	if ctx.Priority() == 0 {
		ctx.SetPriority(0)
	}
	model := ctx.ConfigString("model", "")
	if model == "" {
		return fmt.Errorf("mw_auto_review: config model is required")
	}
	only := ctx.ConfigStrings("tools")
	guidance := ctx.ConfigString("policy", "Deny destructive, irreversible, exfiltrating or out-of-scope actions. Escalate when unsure.")
	onErr := ctx.ConfigString("on_error", "escalate")

	needs := func(req *sdk.ToolRequest) bool {
		if len(only) > 0 {
			for _, n := range only {
				if n == req.Call.Name {
					return true
				}
			}
			return false
		}
		if req.Tool == nil {
			return false
		}
		ro, _ := req.Tool.Meta["readonly"].(bool)
		return !ro
	}

	review := func(c context.Context, req *sdk.ToolRequest) (string, string, error) {
		prov, id := sdk.SplitModel(model)
		p, ok := ctx.Service("llm/" + prov).(*sdk.Provider)
		if !ok {
			return "", "", fmt.Errorf("no provider llm/%s", prov)
		}
		var recent strings.Builder
		if req.Run != nil {
			msgs := req.Run.Messages
			if len(msgs) > 6 {
				msgs = msgs[len(msgs)-6:]
			}
			for _, m := range msgs {
				if m.Content != "" {
					fmt.Fprintf(&recent, "%s: %s\n", m.Role, m.Content)
				}
			}
		}
		desc := ""
		if req.Tool != nil {
			desc = req.Tool.Description
		}
		sys := "You review an AI agent's tool calls for safety. " + guidance +
			"\nReply with only JSON: {\"decision\": \"approve\"|\"deny\"|\"escalate\", \"reason\": \"...\"}"
		user := fmt.Sprintf("User: %s\nRecent conversation:\n%s\nTool: %s (%s)\nArguments: %s", req.User, recent.String(), req.Call.Name, desc, sdk.ToJSON(req.Call.Args))
		resp, err := p.Generate(c, &sdk.ModelRequest{Model: model, Provider: prov, ModelID: id, System: sys,
			Messages: []sdk.Message{{Role: sdk.RoleUser, Content: user}}, ResponseName: "review",
			ResponseSchema: sdk.Object(map[string]any{"decision": sdk.Enum("", "approve", "deny", "escalate"), "reason": sdk.Str("")}, "decision", "reason")})
		if err != nil {
			return "", "", err
		}
		text := resp.Message.Content
		if i := strings.Index(text, "{"); i >= 0 {
			text = text[i:]
		}
		var out struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(strings.TrimRight(strings.TrimSpace(text), "`")), &out); err != nil {
			return "", "", fmt.Errorf("unreadable review: %v", err)
		}
		return out.Decision, out.Reason, nil
	}

	ctx.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		if req.Decision != nil || !needs(req) {
			return next(c, req)
		}
		decision, reason, err := review(c, req)
		if err != nil {
			ctx.Logf("review of %s failed: %v", req.Call.Name, err)
			decision, reason = onErr, "reviewer unavailable"
		}
		ctx.Logf("review %s: %s (%s)", req.Call.Name, decision, reason)
		switch decision {
		case "approve":
			r2 := *req
			r2.Decision = &sdk.Decision{InterruptID: req.Call.ID, Type: "approve", Message: reason, By: "auto_review:" + model}
			return next(c, &r2)
		case "deny":
			return &sdk.ToolResult{Content: "denied by reviewer: " + reason, IsError: true}, nil
		}
		return next(c, req) // escalate: let mw_hitl (or the tool) decide
	})
	return nil
}

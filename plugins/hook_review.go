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
// they run. It approves, denies, or escalates to a human (hook_approval).
//
// Config:
//
//	model:   reviewer model, e.g. "anthropic:claude-haiku-5-5" (required)
//	tools:   tools to review ([] = every tool not marked readonly)
//	policy:  extra guidance for the reviewer
//	on_error: escalate (default) | deny
//
// Example:
//
//	eipagent.WithPlugin("hook_review", map[string]any{
//		"model":  "anthropic:claude-haiku-5-5",
//		"tools":  []any{"bash", "write_file"},
//		"policy": "Deny anything that deletes data or touches production.",
//	})
func Apply(p *sdk.Plugin) error {
	if p.Priority() == 0 {
		p.SetPriority(0)
	}
	model := p.ConfigString("model", "")
	if model == "" {
		return fmt.Errorf("hook_review: config model is required")
	}
	only := p.ConfigStrings("tools")
	guidance := p.ConfigString("policy", "Deny destructive, irreversible, exfiltrating or out-of-scope actions. Escalate when unsure.")
	onErr := p.ConfigString("on_error", "escalate")

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
		mp, ok := p.Service("model/" + prov).(*sdk.Provider)
		if !ok {
			return "", "", fmt.Errorf("no provider model/%s", prov)
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
		resp, err := mp.Generate(c, &sdk.ModelRequest{Model: model, Provider: prov, ModelID: id, System: sys,
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

	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		if req.Decision != nil || !needs(req) {
			return next(c, req)
		}
		decision, reason, err := review(c, req)
		if err != nil {
			p.Logf("review of %s failed: %v", req.Call.Name, err)
			decision, reason = onErr, "reviewer unavailable"
		}
		p.Logf("review %s: %s (%s)", req.Call.Name, decision, reason)
		switch decision {
		case "approve":
			r2 := *req
			r2.Decision = &sdk.Decision{InterruptID: req.Call.ID, Type: "approve", Message: reason, By: "auto_review:" + model}
			return next(c, &r2)
		case "deny":
			return &sdk.ToolResult{Content: "denied by reviewer: " + reason, IsError: true}, nil
		}
		return next(c, req) // escalate: let hook_approval (or the tool) decide
	})
	return nil
}

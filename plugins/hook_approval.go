//go:build ignore

package plugin

import (
	"context"
	"fmt"

	"eipagent/sdk"
)

// Human in the loop: pauses the run before chosen tool calls until a
// person approves, edits, rejects or answers them. Resume with
// Thread.Resume / Agent.Resume, or load an "approver" service to decide
// synchronously.
//
// Config:
//
//	interrupt_on:  {tool: true | {allowed: [approve, edit, reject, respond], description: "..."}}
//	               "*" matches every tool
//	non_readonly:  true = also every tool not marked Meta.readonly
//	risk:          ["high"] = also tools whose Meta.risk is listed
func Apply(p *sdk.Plugin) error {
	if p.Priority() == 0 {
		p.SetPriority(-10) // inner: runs after policy, limits, reviewers
	}
	rules := p.ConfigMap("interrupt_on")
	nonRO := p.ConfigBool("non_readonly", false)
	risks := p.ConfigStrings("risk")
	defAllowed := []string{"approve", "edit", "reject", "respond"}

	rule := func(t *sdk.Tool, name string) (bool, []string, string) {
		r, ok := rules[name]
		if !ok {
			r, ok = rules["*"]
		}
		if ok {
			switch v := r.(type) {
			case bool:
				return v, defAllowed, ""
			case map[string]any:
				allowed := defAllowed
				if raw, ok := v["allowed"].([]any); ok {
					allowed = nil
					for _, a := range raw {
						if s, ok := a.(string); ok {
							allowed = append(allowed, s)
						}
					}
				}
				desc, _ := v["description"].(string)
				return true, allowed, desc
			}
		}
		if t != nil {
			if ro, _ := t.Meta["readonly"].(bool); nonRO && !ro {
				return true, defAllowed, ""
			}
			if risk, _ := t.Meta["risk"].(string); risk != "" {
				for _, r := range risks {
					if r == risk {
						return true, defAllowed, ""
					}
				}
			}
		}
		return false, nil, ""
	}

	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		need, allowed, desc := rule(req.Tool, req.Call.Name)
		if !need {
			return next(c, req)
		}
		if d := req.Decision; d != nil {
			ok := false
			for _, a := range allowed {
				if a == d.Type {
					ok = true
				}
			}
			if !ok {
				return &sdk.ToolResult{Content: fmt.Sprintf("decision %q is not allowed for %s", d.Type, req.Call.Name), IsError: true}, nil
			}
			if d.Type == "approve" || d.Type == "edit" {
				return next(c, req)
			}
		}
		if desc == "" {
			desc = fmt.Sprintf("Approve call to %s with %s?", req.Call.Name, sdk.ToJSON(req.Call.Args))
		}
		call := req.Call
		return &sdk.ToolResult{Interrupt: &sdk.Interrupt{ID: call.ID, Kind: "approval", ToolCall: &call, Message: desc, Allowed: allowed}}, nil
	})
	return nil
}

//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"strings"

	"eipagent/sdk"
)

// Multi-agent: subagents exposed as tools, and handoffs that switch
// which agent is talking.
//
// Config:
//
//	agents: [{
//	  name: "researcher", description: "Finds facts",
//	  instructions: "...", model: "openai:gpt-4.1-mini",   // model default: caller's
//	  tools: ["web_fetch", "recall"],                      // parent tools it may use
//	  max_steps: 10,
//	  mode: "tool" (default: call as a tool, get its answer) | "handoff" (take over the conversation)
//	}]
//
// Policy and audit hooks follow subagents (they run in the same snapshot).
//
// Example:
//
//	eipagent.WithPlugin("tool_subagents", map[string]any{"agents": []any{
//		map[string]any{"name": "researcher", "description": "Finds facts on the web",
//			"instructions": "Cite your sources.", "tools": []any{"web_search", "http_fetch"}, "max_steps": 8},
//		map[string]any{"name": "billing", "description": "Handles refunds", "mode": "handoff"},
//	}})
var Requires = []string{"loop"}

func Apply(p *sdk.Plugin) error {
	agents := p.ConfigList("agents")
	if len(agents) == 0 {
		return fmt.Errorf("agents: config agents is empty")
	}
	byName := map[string]map[string]any{}
	for _, a := range agents {
		name := ""
		if x, ok := a["name"].(string); ok {
			name = x
		}
		if name == "" {
			return fmt.Errorf("agents: every agent needs a name")
		}
		byName[name] = a
		mode := ""
		if x, ok := a["mode"].(string); ok {
			mode = x
		}
		if mode == "handoff" {
			p.AddTool(handoffTool(name, a))
		} else {
			p.AddTool(subagentTool(p, name, a))
		}
	}
	// Handoffs: once a handoff happened, the active agent's instructions,
	// model and tools apply to every following step of the thread.
	p.BeforeStep(func(c context.Context, sc *sdk.StepContext) error {
		active, _ := sc.Run.Values["active_agent"].(string)
		a := byName[active]
		if a == nil {
			return nil
		}
		if ins, _ := a["instructions"].(string); ins != "" {
			sc.Request.System = ins
		}
		if m, _ := a["model"].(string); m != "" {
			sc.Request.Model = m
		}
		if allowed := strList(a["tools"]); len(allowed) > 0 {
			sc.Request.Tools = pick(sc.Request.Tools, append(allowed, "transfer_back"))
		}
		return nil
	})
	p.AddTool(sdk.Tool{
		Name:        "transfer_back",
		Description: "Hand the conversation back to the main agent",
		Schema:      sdk.Object(map[string]any{}),
		Run: func(c context.Context, args map[string]any) (string, error) {
			return "Transferred back to the main agent.", nil
		},
	})
	p.AfterModel(func(c context.Context, sc *sdk.StepContext, resp *sdk.ModelResponse) error {
		for _, tc := range resp.Message.ToolCalls {
			switch {
			case strings.HasPrefix(tc.Name, "transfer_to_"):
				sc.Run.Values["active_agent"] = strings.TrimPrefix(tc.Name, "transfer_to_")
			case tc.Name == "transfer_back":
				delete(sc.Run.Values, "active_agent")
			}
		}
		return nil
	})
	return nil
}

func strList(v any) []string {
	raw, _ := v.([]any)
	var out []string
	for _, x := range raw {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func pick(tools []sdk.Tool, names []string) []sdk.Tool {
	var out []sdk.Tool
	for _, t := range tools {
		for _, n := range names {
			if t.Name == n {
				out = append(out, t)
			}
		}
	}
	return out
}

func handoffTool(name string, a map[string]any) sdk.Tool {
	desc, _ := a["description"].(string)
	return sdk.Tool{
		Name:        "transfer_to_" + name,
		Description: "Hand the conversation to " + name + ": " + desc,
		Schema:      sdk.Object(map[string]any{"reason": sdk.Str("why")}),
		Run: func(c context.Context, args map[string]any) (string, error) {
			return "Transferred to " + name + ". Continue as " + name + ".", nil
		},
	}
}

func subagentTool(p *sdk.Plugin, name string, a map[string]any) sdk.Tool {
	desc, _ := a["description"].(string)
	instructions, _ := a["instructions"].(string)
	model, _ := a["model"].(string)
	allowed := strList(a["tools"])
	maxSteps := 10
	if v, ok := a["max_steps"].(float64); ok {
		maxSteps = int(v)
	} else if v, ok := a["max_steps"].(int); ok {
		maxSteps = v
	}
	return sdk.Tool{
		Name:        name,
		Description: desc + " (delegates to the " + name + " agent; give it a complete, self-contained task)",
		Schema:      sdk.Object(map[string]any{"task": sdk.Str("the task, with all context it needs")}, "task"),
		Meta:        map[string]any{"subagent": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			snap := p.Snapshot()
			loop, ok := snap.Services["loop"].(*sdk.Loop)
			if !ok {
				return "", fmt.Errorf("no loop service")
			}
			ri := sdk.RunInfoFrom(c)
			m := model
			if m == "" {
				m = ri.Model
			}
			if m == "" {
				return "", fmt.Errorf("agent %s has no model", name)
			}
			sub := snap.WithTools(pick(snap.Tools, allowed), nil)
			st := &sdk.RunState{
				Thread: ri.Thread + "/" + name, User: ri.User, Roles: ri.Roles, Model: m, System: instructions,
				MaxSteps: maxSteps, Values: map[string]any{},
				Messages: []sdk.Message{{Role: sdk.RoleUser, Content: sdk.ArgString(args, "task")}},
			}
			res, err := loop.Run(c, sub, st)
			if err != nil {
				return "", err
			}
			if len(res.Interrupts) > 0 {
				return "", fmt.Errorf("agent %s needs approval for %s, which subagents can't ask for; run it as the main agent", name, res.Interrupts[0].ToolCall.Name)
			}
			return res.Text, nil
		},
	}
}

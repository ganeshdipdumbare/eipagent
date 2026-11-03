//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"

	"eipagent/sdk"
)

// Enforces the admin policy's rules on every model and tool call. Load it
// as a locked plugin (policy.json "locked": ["policy"], "require":
// [{"use": "policy"}]) so it runs outermost and can't be removed; the
// engine passes policy.json "rules" in as config.rules.
//
// rules:
//
//	mode:            read-only | workspace-write (default) | full-access
//	allow_tools:     glob patterns; [] = all
//	deny_tools:      glob patterns (win over allow)
//	allow_models:    glob patterns over "provider:model"; [] = all
//	require_sandbox: true = shell/fs tools must run in an isolated exec world
//	max_tokens_run:  token budget per run
//	roles:           {role: {allow_tools: [...], mode: "..."}} overrides by user role
//
// Example:
//
//	// policy.json: {"locked": ["policy"], "require": [{"use": "policy"}],
//	//               "rules": {"mode": "read-only", "deny_tools": ["bash"]}}
//	eipagent.WithPolicyFile("policy.json")
func Apply(p *sdk.Plugin) error {
	p.SetPriority(sdk.PriorityReserved) // ignored unless the plugin is locked
	if !p.Trusted() {
		p.Logf("warning: policy is not locked; other plugins may run before it")
	}
	rules := p.ConfigMap("rules")
	if rules == nil {
		rules = p.Config
	}
	list := func(m map[string]any, k string) []string {
		raw, _ := m[k].([]any)
		var out []string
		for _, x := range raw {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	match := func(pats []string, s string) bool {
		for _, pat := range pats {
			if ok, _ := path.Match(pat, s); ok {
				return true
			}
		}
		return false
	}
	str := func(m map[string]any, k, def string) string {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	roleRules := func(roles []string) map[string]any {
		all, _ := rules["roles"].(map[string]any)
		for _, r := range roles {
			if m, ok := all[r].(map[string]any); ok {
				return m
			}
		}
		return nil
	}
	allowModels := list(rules, "allow_models")
	maxTokens := 0
	if v, ok := rules["max_tokens_run"].(float64); ok {
		maxTokens = int(v)
	}
	requireSandbox, _ := rules["require_sandbox"].(bool)

	var mu sync.Mutex
	used := map[*sdk.RunState]int{}
	p.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		if len(allowModels) > 0 && !match(allowModels, req.Model) {
			return nil, fmt.Errorf("policy: model %s is not allowed", req.Model)
		}
		mu.Lock()
		spent := used[req.Run]
		mu.Unlock()
		if maxTokens > 0 && req.Run != nil && spent >= maxTokens {
			return nil, fmt.Errorf("policy: token budget of %d per run exhausted", maxTokens)
		}
		resp, err := next(c, req)
		if err == nil && req.Run != nil {
			mu.Lock()
			used[req.Run] += resp.Usage.TotalTokens
			mu.Unlock()
		}
		return resp, err
	})
	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		delete(used, st)
		mu.Unlock()
		return nil
	})

	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		name := req.Call.Name
		var roles []string
		if req.Run != nil {
			roles = req.Run.Roles
		}
		eff := rules
		if rr := roleRules(roles); rr != nil {
			eff = rr
		}
		deny := func(why string) (*sdk.ToolResult, error) {
			p.Logf("denied %s for %s: %s", name, req.User, why)
			return &sdk.ToolResult{Content: "denied by policy: " + why, IsError: true}, nil
		}
		if match(list(rules, "deny_tools"), name) || match(list(eff, "deny_tools"), name) {
			return deny("tool is denied")
		}
		if allow := list(eff, "allow_tools"); len(allow) > 0 && !match(allow, name) {
			return deny("tool is not in the allow list")
		}
		readonly := false
		sandbox := ""
		if req.Tool != nil {
			readonly, _ = req.Tool.Meta["readonly"].(bool)
			sandbox, _ = req.Tool.Meta["sandbox"].(string)
		}
		switch str(eff, "mode", str(rules, "mode", "workspace-write")) {
		case "read-only":
			if !readonly {
				return deny("read-only mode allows only read-only tools")
			}
		case "workspace-write":
			if sandbox == "" && req.Tool != nil && req.Tool.Meta["risk"] == "high" && !strings.HasPrefix(sandbox, "sandbox:") {
				// high-risk tools outside any exec world (e.g. custom shell tools) need full-access
				return deny("high-risk tool needs full-access mode")
			}
		}
		if requireSandbox && sandbox != "" && !strings.HasPrefix(sandbox, "sandbox:") {
			return deny("shell and file tools must run in a sandbox (load workspace_sandbox)")
		}
		return next(c, req)
	})
	return nil
}

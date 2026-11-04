//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"eipagent/sdk"
)

// Checks what goes in and out of the model against rules, and blocks,
// redacts or retries. Each rule looks at one place:
//
//	input:       the user's message, before the run starts
//	output:      the model's final answer
//	tool_args:   a tool call's arguments, before it runs
//	tool_result: a tool's result, before the model sees it
//
// and matches a regex, any of a list of keywords (case-insensitive), or a
// built-in detector ("secrets": API keys, tokens and private keys). The
// action is:
//
//	block:  input fails the run; output and tool_result are replaced by
//	        message; tool_args returns message to the model instead of running
//	redact: replace each match with [redacted] (input, output, tool_result)
//	retry:  output only: ask the model again with message, up to
//	        max_retries times, then block
//
// Config: rules (required), max_retries (default 1).
//
// Example:
//
//	eipagent.WithPlugin("hook_guardrails", map[string]any{"rules": []any{
//		map[string]any{"on": "output", "detect": "secrets", "action": "redact"},
//		map[string]any{"on": "tool_args", "keywords": []any{"DROP TABLE"}, "action": "block",
//			"message": "Destructive SQL is not allowed."},
//		map[string]any{"on": "output", "regex": `(?i)as an ai`, "action": "retry",
//			"message": "Answer directly, without talking about being an AI."},
//	}})
func Apply(p *sdk.Plugin) error {
	maxRetries := p.ConfigInt("max_retries", 1)

	type rule struct {
		on, action, message string
		re                  *regexp.Regexp
	}
	detectors := map[string]string{
		"secrets": `sk-[A-Za-z0-9_-]{20,}|sk-ant-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{36,}|xox[abpr]-[A-Za-z0-9-]{10,}|AIza[0-9A-Za-z_-]{35}|-----BEGIN [A-Z ]*PRIVATE KEY-----`,
	}
	var rules []rule
	for i, m := range p.ConfigList("rules") {
		// Check ok rather than relying on the zero value: in the plugin
		// interpreter a failed `x, _ := v.(T)` in a loop body keeps the
		// previous iteration's value.
		s := func(k string) string {
			if v, ok := m[k]; ok {
				if str, ok := v.(string); ok {
					return str
				}
			}
			return ""
		}
		r := rule{on: s("on"), action: s("action"), message: s("message")}
		switch r.on {
		case "input", "output", "tool_args", "tool_result":
		default:
			return fmt.Errorf("hook_guardrails: rule %d: on must be input, output, tool_args or tool_result", i+1)
		}
		ok := r.action == "block" || (r.action == "redact" && r.on != "tool_args") || (r.action == "retry" && r.on == "output")
		if !ok {
			return fmt.Errorf("hook_guardrails: rule %d: action %q is not allowed on %s", i+1, r.action, r.on)
		}
		pattern := s("regex")
		var kws []any
		if v, ok := m["keywords"].([]any); ok {
			kws = v
		}
		if len(kws) > 0 {
			var q []string
			for _, k := range kws {
				q = append(q, regexp.QuoteMeta(fmt.Sprint(k)))
			}
			pattern = `(?i)` + strings.Join(q, "|")
		}
		if d := s("detect"); d != "" {
			if detectors[d] == "" {
				return fmt.Errorf("hook_guardrails: rule %d: unknown detector %q", i+1, d)
			}
			pattern = detectors[d]
		}
		if pattern == "" {
			return fmt.Errorf("hook_guardrails: rule %d needs regex, keywords or detect", i+1)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("hook_guardrails: rule %d: %w", i+1, err)
		}
		r.re = re
		if r.message == "" {
			r.message = "This was blocked by a guardrail."
		}
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		return fmt.Errorf("hook_guardrails: config rules is empty")
	}
	// check applies the rules for one place to text. It returns the text
	// to use, and the rule that blocked or asked for a retry, if any.
	check := func(on, text string) (string, *rule) {
		for i := range rules {
			r := &rules[i]
			if r.on != on || !r.re.MatchString(text) {
				continue
			}
			if r.action == "redact" {
				text = r.re.ReplaceAllString(text, "[redacted]")
				continue
			}
			return text, r
		}
		return text, nil
	}

	var mu sync.Mutex
	tries := map[string]int{}
	fixes := map[string]string{}

	p.OnRunStart(func(c context.Context, st *sdk.RunState) error {
		for i := len(st.Messages) - 1; i >= 0; i-- {
			if st.Messages[i].Role != sdk.RoleUser {
				continue
			}
			text, hit := check("input", st.Messages[i].Content)
			if hit != nil {
				return fmt.Errorf("hook_guardrails: input blocked: %s", hit.message)
			}
			st.Messages[i].Content = text
			break
		}
		return nil
	})

	p.BeforeStep(func(c context.Context, sc *sdk.StepContext) error {
		mu.Lock()
		fix := fixes[sc.Run.ID]
		delete(fixes, sc.Run.ID)
		mu.Unlock()
		if fix != "" {
			sc.Run.Messages = append(sc.Run.Messages, sdk.Message{Role: sdk.RoleUser, Content: fix})
			sc.Request.Messages = sc.Run.Messages
		}
		return nil
	})

	p.AfterModel(func(c context.Context, sc *sdk.StepContext, resp *sdk.ModelResponse) error {
		if resp.Message.Content == "" {
			return nil
		}
		text, hit := check("output", resp.Message.Content)
		resp.Message.Content = text
		if hit == nil {
			return nil
		}
		if hit.action == "retry" && len(resp.Message.ToolCalls) == 0 {
			mu.Lock()
			n := tries[sc.Run.ID]
			if n < maxRetries {
				tries[sc.Run.ID] = n + 1
				fixes[sc.Run.ID] = hit.message
			}
			mu.Unlock()
			if n < maxRetries {
				p.Logf("output failed a guardrail; retry %d/%d", n+1, maxRetries)
				sc.Jump = "model"
				return nil
			}
		}
		p.Logf("output blocked")
		resp.Message.Content = hit.message
		return nil
	})

	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		if _, hit := check("tool_args", sdk.ToJSON(req.Call.Args)); hit != nil {
			p.Logf("tool call %s blocked", req.Call.Name)
			return &sdk.ToolResult{Content: hit.message, IsError: true}, nil
		}
		r, err := next(c, req)
		if err != nil || r == nil || r.Interrupt != nil {
			return r, err
		}
		text, hit := check("tool_result", r.Content)
		out := *r
		out.Content = text
		if hit != nil {
			p.Logf("result of %s blocked", req.Call.Name)
			out.Content = hit.message
		}
		return &out, nil
	})

	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		delete(tries, st.ID)
		delete(fixes, st.ID)
		mu.Unlock()
		return nil
	})
	return nil
}

//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"strings"

	"eipagent/sdk"
)

// Summarizes old history when it grows too long, keeping recent
// messages verbatim. Tool calls and their results are never split.
//
// Config:
//
//	trigger_messages: summarize when history exceeds this (default 40)
//	trigger_tokens:   ...or this many estimated tokens (chars/4; 0 = off)
//	keep_messages:    recent messages kept verbatim (default 10)
//	model:            model for the summary (default: the run's model)
//	prompt:           summary instruction
func Apply(ctx *sdk.Context) error {
	trigMsgs := ctx.ConfigInt("trigger_messages", 40)
	trigTokens := ctx.ConfigInt("trigger_tokens", 0)
	keep := ctx.ConfigInt("keep_messages", 10)
	model := ctx.ConfigString("model", "")
	prompt := ctx.ConfigString("prompt", "Summarize the conversation so far for your own future reference. Keep facts, decisions, open tasks, names, numbers and tool results that still matter. Be concise.")

	tokens := func(ms []sdk.Message) int {
		n := 0
		for _, m := range ms {
			n += len(m.Content) / 4
			for _, tc := range m.ToolCalls {
				n += len(sdk.ToJSON(tc.Args)) / 4
			}
		}
		return n
	}

	ctx.OnStepPrepare(func(c context.Context, sc *sdk.StepContext) error {
		msgs := sc.Run.Messages
		if len(msgs) <= keep+1 {
			return nil
		}
		if len(msgs) <= trigMsgs && (trigTokens == 0 || tokens(msgs) <= trigTokens) {
			return nil
		}
		cut := len(msgs) - keep
		// Never start the kept part with a tool result (its call would be lost).
		for cut > 0 && msgs[cut].Role == sdk.RoleTool {
			cut--
		}
		if cut <= 0 {
			return nil
		}
		var b strings.Builder
		for _, m := range msgs[:cut] {
			switch {
			case m.Role == sdk.RoleTool:
				fmt.Fprintf(&b, "[tool %s result] %s\n", m.ToolName, m.Content)
			case len(m.ToolCalls) > 0:
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "[%s called %s %s]\n", m.Role, tc.Name, sdk.ToJSON(tc.Args))
				}
				if m.Content != "" {
					fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
				}
			default:
				fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
			}
		}
		m := model
		if m == "" {
			m = sc.Request.Model
		}
		prov, id := sdk.SplitModel(m)
		p, ok := ctx.Service("llm/" + prov).(*sdk.Provider)
		if !ok {
			return fmt.Errorf("mw_summarize: no provider llm/%s", prov)
		}
		resp, err := p.Generate(c, &sdk.ModelRequest{Model: m, Provider: prov, ModelID: id, System: prompt,
			Messages: []sdk.Message{{Role: sdk.RoleUser, Content: b.String()}}})
		if err != nil {
			return fmt.Errorf("mw_summarize: %w", err)
		}
		summary := sdk.Message{Role: sdk.RoleUser, Content: "Summary of the earlier conversation:\n" + resp.Message.Content}
		kept := append([]sdk.Message{summary}, msgs[cut:]...)
		ctx.Logf("summarized %d messages into 1 (kept %d)", cut, len(msgs)-cut)
		sc.Run.Messages = kept
		sc.Request.Messages = kept
		return nil
	})
	return nil
}

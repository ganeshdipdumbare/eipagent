package hook_compact

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Keeps a run inside the model's context window. Before each step it
// estimates the request's size and, past the trigger, applies the
// strategies in order, cheapest first, until the estimate is under the
// target. Edits stay in the run's history.
//
// The estimate starts from the input and output tokens the provider
// reported for the last step and adds about one token per four
// characters for what came after (the ratio is calibrated from that
// report). It never splits a tool call from its result.
//
// Strategies (in the default order):
//
//	clamp:              cut any message longer than clamp_chars to its head and tail
//	clear_tool_results: blank all but the last keep_tool_results tool results
//	sliding_window:     drop the oldest messages, keeping the first user message
//	summarize:          replace older messages with a summary (one model call)
//
// Each step emits a "context_usage" event: tokens, window, fraction and
// the strategies applied.
//
// Config:
//
//	context_window:    the model's window in tokens (default: a guess from the
//	                   model name, else fallback_context_window, 200000)
//	max_fraction:      compact when the estimate passes this share of the
//	                   window (default 0.8)
//	max_tokens:        ...or this many tokens (0 = off)
//	max_messages:      ...or this many messages (0 = off)
//	target_fraction:   compact down to this share of the window (default 0.5)
//	strategies:        default [clamp, clear_tool_results, sliding_window, summarize]
//	clamp_chars:       default 20000 (keeping 2000 at each end)
//	keep_tool_results: default 3
//	keep_messages:     recent messages sliding_window and summarize keep (default 20)
//	summary_model:     model for summarize (default: the run's model)
//	tool:              give the model a compact_context tool (default false)
//
// Example:
//
//	kiln.WithModuleConfig("hook_compact", map[string]any{
//		"max_fraction":    0.8,
//		"target_fraction": 0.5,
//		"strategies":      []any{"clear_tool_results", "summarize"},
//	})
func Apply(p *sdk.Module) error {
	window := p.ConfigInt("context_window", 0)
	fallbackWindow := p.ConfigInt("fallback_context_window", 200000)
	maxFraction := p.ConfigFloat("max_fraction", 0.8)
	maxTokens := p.ConfigInt("max_tokens", 0)
	maxMessages := p.ConfigInt("max_messages", 0)
	targetFraction := p.ConfigFloat("target_fraction", 0.5)
	strategies := p.ConfigStrings("strategies")
	if len(strategies) == 0 {
		strategies = []string{"clamp", "clear_tool_results", "sliding_window", "summarize"}
	}
	for _, s := range strategies {
		switch s {
		case "clamp", "clear_tool_results", "sliding_window", "summarize":
		default:
			return fmt.Errorf("hook_compact: unknown strategy %q", s)
		}
	}
	clampChars := p.ConfigInt("clamp_chars", 20000)
	keepResults := p.ConfigInt("keep_tool_results", 3)
	keepMessages := p.ConfigInt("keep_messages", 20)
	summaryModel := p.ConfigString("summary_model", "")
	withTool := p.ConfigBool("tool", false)

	// Per run: the last provider report, and compaction asked for by the model.
	type anchor struct {
		tokens, msgs int
		ratio        float64 // tokens per character
	}
	var mu sync.Mutex
	anchors := map[string]anchor{}
	forced := map[string]string{} // run ID -> focus

	windowOf := func(model string) int {
		if window > 0 {
			return window
		}
		_, id := sdk.SplitModel(model)
		switch {
		case strings.Contains(id, "claude-haiku-4-5") || strings.Contains(id, "claude-3"):
			return 200000
		case strings.HasPrefix(id, "claude-"):
			return 1000000
		case strings.HasPrefix(id, "gemini-"):
			return 1048576
		case strings.HasPrefix(id, "gpt-4.1"):
			return 1047576
		}
		return fallbackWindow
	}
	chars := func(ms []sdk.Message) int {
		n := 0
		for _, m := range ms {
			n += len(m.Content) + len(m.Reasoning) + 16
			for _, tc := range m.ToolCalls {
				n += len(tc.Name) + len(sdk.ToJSON(tc.Args))
			}
			n += 3000 * len(m.Images) // images: a rough flat cost
		}
		return n
	}
	fixedChars := func(req *sdk.ModelRequest) int {
		n := len(req.System)
		for _, t := range req.Tools {
			n += len(t.Name) + len(t.Description) + len(sdk.ToJSON(t.Schema))
		}
		return n
	}
	estimate := func(runID string, req *sdk.ModelRequest, ms []sdk.Message, edited bool) int {
		mu.Lock()
		a, ok := anchors[runID]
		mu.Unlock()
		ratio := 0.25
		if ok && a.ratio > 0 {
			ratio = a.ratio
		}
		if ok && !edited && a.msgs <= len(ms) {
			return a.tokens + int(float64(chars(ms[a.msgs:]))*ratio)
		}
		return int(float64(fixedChars(req)+chars(ms)) * ratio)
	}
	// safeCut moves a cut back so the kept part never starts with a tool
	// result whose call would be dropped.
	safeCut := func(ms []sdk.Message, cut int) int {
		for cut > 0 && cut < len(ms) && ms[cut].Role == sdk.RoleTool {
			cut--
		}
		return cut
	}
	firstUser := func(ms []sdk.Message) int {
		for i, m := range ms {
			if m.Role == sdk.RoleUser {
				return i
			}
		}
		return -1
	}

	summarize := func(c context.Context, req *sdk.ModelRequest, ms []sdk.Message, focus string) ([]sdk.Message, error) {
		cut := safeCut(ms, len(ms)-keepMessages)
		if cut <= 1 {
			return ms, nil
		}
		var b strings.Builder
		for _, m := range ms[:cut] {
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
		model := summaryModel
		if model == "" {
			model = req.Model
		}
		prov, id := sdk.SplitModel(model)
		mp, ok := p.Service("model/" + prov).(*sdk.Provider)
		if !ok {
			return ms, fmt.Errorf("hook_compact: no provider model/%s for summarize", prov)
		}
		prompt := "Summarize the conversation so far for your own future reference. Keep facts, decisions, open tasks, names, numbers and tool results that still matter. Be concise."
		if focus != "" {
			prompt += " Focus on: " + focus
		}
		base := func(c context.Context, r *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			return mp.Generate(c, r)
		}
		resp, err := p.Snapshot().ModelChain(base)(c, &sdk.ModelRequest{Model: model, Provider: prov, ModelID: id, System: prompt,
			Messages: []sdk.Message{{Role: sdk.RoleUser, Content: sdk.Redact(b.String())}},
			Run:      req.Run, Thread: req.Thread, User: req.User, Step: req.Step})
		if err != nil {
			return ms, fmt.Errorf("hook_compact: summarize: %w", err)
		}
		summary := sdk.Message{Role: sdk.RoleUser, Content: "Summary of the earlier conversation:\n" + resp.Message.Content}
		return append([]sdk.Message{summary}, ms[cut:]...), nil
	}

	apply := func(c context.Context, req *sdk.ModelRequest, ms []sdk.Message, strategy, focus string) ([]sdk.Message, bool, error) {
		out := append([]sdk.Message(nil), ms...)
		changed := false
		switch strategy {
		case "clamp":
			keep := clampChars / 10
			for i, m := range out {
				if len(m.Content) > clampChars {
					cut := len(m.Content) - 2*keep
					out[i].Content = m.Content[:keep] + fmt.Sprintf("\n…[%d characters cut]…\n", cut) + m.Content[len(m.Content)-keep:]
					changed = true
				}
			}
		case "clear_tool_results":
			seen := 0
			for i := len(out) - 1; i >= 0; i-- {
				if out[i].Role != sdk.RoleTool {
					continue
				}
				seen++
				if seen > keepResults && out[i].Content != "[tool result cleared]" {
					out[i].Content = "[tool result cleared]"
					changed = true
				}
			}
		case "sliding_window":
			cut := safeCut(out, len(out)-keepMessages)
			first := firstUser(out)
			if cut > first+1 {
				kept := append([]sdk.Message{}, out[:first+1]...)
				kept = append(kept, sdk.Message{Role: sdk.RoleUser, Content: fmt.Sprintf("[%d earlier messages were removed to save space]", cut-first-1)})
				out = append(kept, out[cut:]...)
				changed = true
			}
		case "summarize":
			s, err := summarize(c, req, out, focus)
			if err != nil {
				return ms, false, err
			}
			changed = len(s) != len(out)
			out = s
		}
		return out, changed, nil
	}

	p.BeforeStep(func(c context.Context, sc *sdk.StepContext) error {
		runID := sc.Run.ID
		mu.Lock()
		focus, force := forced[runID]
		delete(forced, runID)
		mu.Unlock()

		win := windowOf(sc.Request.Model)
		ms := sc.Run.Messages
		tokens := estimate(runID, sc.Request, ms, false)
		over := force ||
			(maxFraction > 0 && float64(tokens) > maxFraction*float64(win)) ||
			(maxTokens > 0 && tokens > maxTokens) ||
			(maxMessages > 0 && len(ms) > maxMessages)
		var applied []string
		if over {
			target := int(targetFraction * float64(win))
			if maxTokens > 0 && maxTokens < target {
				target = maxTokens * 3 / 4
			}
			for _, s := range strategies {
				out, changed, err := apply(c, sc.Request, ms, s, focus)
				if err != nil {
					return err
				}
				if changed {
					ms = out
					applied = append(applied, s)
					tokens = estimate(runID, sc.Request, ms, true)
				}
				if tokens <= target && (maxMessages == 0 || len(ms) <= maxMessages) {
					break
				}
			}
			if len(applied) > 0 {
				p.Logf("compacted with %s: now about %d tokens (%d messages)", strings.Join(applied, ", "), tokens, len(ms))
				sc.Run.Messages = ms
				sc.Request.Messages = ms
				mu.Lock()
				delete(anchors, runID) // the history changed; re-anchor on the next report
				mu.Unlock()
			}
		}
		if sc.Run.Emit != nil {
			sc.Run.Emit(sdk.Event{Type: "context_usage", Step: sc.Step, Data: map[string]any{
				"tokens": tokens, "window": win, "fraction": float64(tokens) / float64(win), "compacted": applied,
			}})
		}
		return nil
	})

	// Anchor the estimate on what the provider reported for this step.
	p.AfterModel(func(c context.Context, sc *sdk.StepContext, resp *sdk.ModelResponse) error {
		u := resp.Usage
		if u.InputTokens == 0 {
			return nil
		}
		total := u.InputTokens + u.OutputTokens
		n := fixedChars(sc.Request) + chars(sc.Request.Messages) + chars([]sdk.Message{resp.Message})
		a := anchor{tokens: total, msgs: len(sc.Run.Messages) + 1} // the loop appends this reply next
		if n > 0 {
			a.ratio = float64(total) / float64(n)
		}
		mu.Lock()
		anchors[sc.Run.ID] = a
		mu.Unlock()
		return nil
	})

	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		delete(anchors, st.ID)
		delete(forced, st.ID)
		mu.Unlock()
		return nil
	})

	if withTool {
		p.AddTool(sdk.Tool{
			Name:        "compact_context",
			Description: "Compact the conversation before your next step when it has grown long or old details no longer matter. Optionally say what to focus on keeping.",
			Schema:      sdk.Object(map[string]any{"focus": sdk.Str("what the summary should keep (optional)")}),
			Run: func(c context.Context, args map[string]any) (string, error) {
				return "The conversation will be compacted before your next step.", nil
			},
		})
		// The tool itself can't see its run; this records the request per run.
		p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
			if req.Call.Name == "compact_context" && req.Run != nil {
				mu.Lock()
				forced[req.Run.ID] = sdk.ArgString(req.Call.Args, "focus")
				mu.Unlock()
			}
			return next(c, req)
		})
	}
	return nil
}

// Module is the compiled form of "hook_compact". modules/std registers it.
var Module = engine.Module{Name: "hook_compact", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

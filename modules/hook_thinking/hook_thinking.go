package hook_thinking

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"fmt"
	"path"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Turns on model reasoning (thinking) with one provider-neutral setting.
// It sets ModelRequest.Extra["kiln.thinking"]; each model module maps it:
//
//	anthropic: thinking {type: adaptive} + output_config.effort
//	           (budget_tokens > 0: {type: enabled, budget_tokens}, older models)
//	openai:    reasoning_effort
//	gemini:    generationConfig.thinkingConfig (thinkingLevel / thinkingBudget)
//
// Config:
//
//	effort:        low | medium | high | xhigh | max ("" = provider default)
//	budget_tokens: fixed thinking budget, for models that need one (0 = off)
//	display:       anthropic only: summarized | omitted
//	models:        only for these models, globs like "anthropic:*" (default all)
//
// A run can override the effort with RunState.Values["thinking_effort"].
//
// Example:
//
//	kiln.WithModuleConfig("hook_thinking", map[string]any{"effort": "high", "models": []any{"anthropic:*", "openai:*"}})
//	// Shortcut: kiln.WithThinking("high")
func Apply(p *sdk.Module) error {
	effort := p.ConfigString("effort", "")
	budget := p.ConfigInt("budget_tokens", 0)
	display := p.ConfigString("display", "")
	models := p.ConfigStrings("models")
	switch effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("hook_thinking: effort %q is not one of low, medium, high, xhigh, max", effort)
	}

	p.BeforeStep(func(c context.Context, sc *sdk.StepContext) error {
		if len(models) > 0 {
			match := false
			for _, m := range models {
				if ok, _ := path.Match(m, sc.Request.Model); ok {
					match = true
				}
			}
			if !match {
				return nil
			}
		}
		t := map[string]any{}
		e := effort
		if v, ok := sc.Run.Values["thinking_effort"].(string); ok && v != "" {
			e = v
		}
		if e != "" {
			t["effort"] = e
		}
		if budget > 0 {
			t["budget_tokens"] = budget
		}
		if display != "" {
			t["display"] = display
		}
		if sc.Request.Extra == nil {
			sc.Request.Extra = map[string]any{}
		}
		sc.Request.Extra["kiln.thinking"] = t
		return nil
	})
	return nil
}

// Module is the compiled form of "hook_thinking". modules/std registers it.
var Module = engine.Module{Name: "hook_thinking", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

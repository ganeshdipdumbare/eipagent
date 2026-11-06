package hook_guard

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"github.com/ganeshdipdumbare/kiln/modules/hook_guardrails"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Vetoes tool calls whose arguments contain a blocked word. This is a
// thin preset over hook_guardrails (tool_args + keywords + block).
// Config: words (default ["password"]).
//
// Example:
//
//	hook_guard.New(map[string]any{"words": []any{"password", "api_key"}})
//	// Prefer hook_guardrails.New when you need more rules.
func Apply(p *sdk.Scope) error {
	words := p.ConfigStrings("words")
	if len(words) == 0 {
		words = []string{"password"}
	}
	kws := make([]any, len(words))
	for i, w := range words {
		kws[i] = w
	}
	prev := p.Config
	p.Config = map[string]any{"rules": []any{map[string]any{
		"on": "tool_args", "keywords": kws, "action": "block",
		"message": "refusing: arguments mention a blocked word",
	}}}
	err := hook_guardrails.Apply(p)
	p.Config = prev
	return err
}

// Module is the compiled form of "hook_guard". modules/std registers it.
var Module = engine.Module{Name: "hook_guard", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

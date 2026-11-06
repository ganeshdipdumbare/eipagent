package hook_stats

import "github.com/ganeshdipdumbare/kiln/engine"

import "github.com/ganeshdipdumbare/kiln/sdk"

// Counts tool calls; prints a summary when unloaded (Effect cleanup).
//
// Example:
//
//	kiln.Config{Modules: []kiln.Module{hook_stats.New()}} // prints tool-call counts when the agent closes
func Apply(p *sdk.Scope) error {
	calls := 0
	p.OnEvent(func(e sdk.Event) {
		if e.Type == "tool_result" {
			calls++
		}
	})
	p.Effect(func() func() {
		return func() { p.Logf("session summary: %d tool calls", calls) }
	})
	return nil
}

// Module is the compiled form of "hook_stats". modules/std registers it.
var Module = engine.Module{Name: "hook_stats", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

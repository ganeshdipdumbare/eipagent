package hook_guard

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"strings"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Vetoes tool calls whose arguments contain a blocked word.
// Config: words (default ["password"]).
//
// Example:
//
//	hook_guard.New(map[string]any{"words": []any{"password", "api_key"}})
func Apply(p *sdk.Scope) error {
	words := p.ConfigStrings("words")
	if len(words) == 0 {
		words = []string{"password"}
	}
	p.On(sdk.HookToolBefore, func(e *sdk.HookEvent) {
		s := sdk.ToJSON(e.Args)
		for _, w := range words {
			if strings.Contains(strings.ToLower(s), strings.ToLower(w)) {
				e.Veto = "refusing: arguments mention " + w
				return
			}
		}
	})
	return nil
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

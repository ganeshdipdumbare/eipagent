//go:build ignore

package plugin

import (
	"strings"

	"eipagent/sdk"
)

// Vetoes tool calls whose arguments contain a blocked word.
// Config: words (default ["password"]).
//
// Example:
//
//	eipagent.WithPlugin("hook_guard", map[string]any{"words": []any{"password", "api_key"}})
func Apply(p *sdk.Plugin) error {
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

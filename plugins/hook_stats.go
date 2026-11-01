//go:build ignore

package plugin

import "eipagent/sdk"

// Counts tool calls; prints a summary when unloaded (Effect cleanup).
func Apply(p *sdk.Plugin) error {
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

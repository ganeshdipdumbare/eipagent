//go:build ignore

package plugin

import "eipagent/sdk"

// Counts tool calls; prints a summary when unloaded (Effect cleanup).
func Apply(ctx *sdk.Context) error {
	calls := 0
	ctx.OnEvent(func(e sdk.Event) {
		if e.Type == "tool_result" {
			calls++
		}
	})
	ctx.Effect(func() func() {
		return func() { ctx.Logf("session summary: %d tool calls", calls) }
	})
	return nil
}

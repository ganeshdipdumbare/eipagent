//go:build ignore

package plugin

import (
	"strings"

	"eipagent/sdk"
)

// Vetoes tool calls whose arguments contain a blocked word.
// Config: words (default ["password"]).
func Apply(ctx *sdk.Context) error {
	words := ctx.ConfigStrings("words")
	if len(words) == 0 {
		words = []string{"password"}
	}
	ctx.On(sdk.HookToolBefore, func(e *sdk.HookEvent) {
		for _, v := range e.Args {
			s, ok := v.(string)
			if !ok {
				continue
			}
			for _, w := range words {
				if strings.Contains(strings.ToLower(s), w) {
					e.Veto = "refusing: arguments mention " + w
					return
				}
			}
		}
	})
	return nil
}

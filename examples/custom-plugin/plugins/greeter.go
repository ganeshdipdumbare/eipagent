//go:build ignore

package plugin

import (
	"context"
	"strings"

	"eipagent/sdk"
)

// A plugin is one interpreted Go file with an Apply function. Everything
// it registers is undone when it unloads, and it hot-reloads on save in
// the dev preset. It declares the services it needs (Inject) and the
// capabilities it needs (Permissions); this one needs neither.
func Apply(ctx *sdk.Context) error {
	greeting := ctx.ConfigString("greeting", "Hello")

	// A tool.
	ctx.AddTool(sdk.Tool{
		Name:        "greet",
		Description: "Greet someone by name",
		Schema:      sdk.Object(map[string]any{"name": sdk.Str("who")}, "name"),
		Meta:        map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			return greeting + ", " + sdk.ArgString(args, "name") + "!", nil
		},
	})

	// A skill (system prompt fragment).
	ctx.AddSkill(sdk.Skill{Name: "manners", Prompt: "Always greet people with the greet tool first."})

	// Middleware: shout every tool result.
	ctx.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		r, err := next(c, req)
		if err == nil && r != nil && ctx.ConfigBool("shout", false) {
			r.Content = strings.ToUpper(r.Content)
		}
		return r, err
	})
	return nil
}

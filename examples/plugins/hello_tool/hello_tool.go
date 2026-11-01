//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"strings"

	"eipagent/sdk"
)

// hello_tool adds a tool the model can call.
//
// Config: greeting ("Hello").
func Apply(p *sdk.Plugin) error {
	greeting := p.ConfigString("greeting", "Hello")
	p.AddTool(sdk.Tool{
		Name:        "hello_tool",
		Description: "Greets someone by name.",
		Schema:      sdk.Object(map[string]any{"name": sdk.Str("who to greet")}, "name"),
		Meta:        map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			name := strings.TrimSpace(sdk.ArgString(args, "name"))
			if name == "" {
				return "", fmt.Errorf("name is required")
			}
			return greeting + ", " + name + "!", nil
		},
	})
	return nil
}

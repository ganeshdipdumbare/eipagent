//go:build ignore

package plugin

import (
	"context"
	"time"

	"eipagent/sdk"
)

// Returns the current time. Config: layout (Go time layout).
func Apply(ctx *sdk.Context) error {
	layout := ctx.ConfigString("layout", time.RFC1123)
	ctx.AddTool(sdk.Tool{
		Name:        "current_time",
		Description: "Returns the current date and time",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Meta:        map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			return time.Now().Format(layout), nil
		},
	})
	return nil
}

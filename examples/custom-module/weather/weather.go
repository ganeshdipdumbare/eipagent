// Package weather is a custom module in its own Go package. Import it and
// pass weather.Module to kiln.WithModule.
package weather

import (
	"context"
	"fmt"
	"strings"

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Module = engine.Module{
	Name: "weather",
	Apply: func(p *sdk.Module) error {
		unit := p.ConfigString("unit", "celsius")
		p.AddTool(sdk.Tool{
			Name:        "weather",
			Description: "Current weather for a city.",
			Schema:      sdk.Object(map[string]any{"city": sdk.Str("city name")}, "city"),
			Meta:        map[string]any{"readonly": true},
			Run: func(_ context.Context, args map[string]any) (string, error) {
				city := strings.TrimSpace(sdk.ArgString(args, "city"))
				if city == "" {
					return "", fmt.Errorf("city is required")
				}
				return fmt.Sprintf("18 degrees %s in %s", unit, city), nil
			},
		})
		return nil
	},
}

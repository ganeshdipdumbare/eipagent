// Package weather is a custom module in its own Go package. Import it and
// put weather.New(nil) in kiln.Config.Modules.
package weather

import (
	"context"
	"fmt"
	"strings"

	"github.com/ganeshdipdumbare/kiln"
)

var Module = kiln.Module{
	Name: "weather",
	Apply: func(p *kiln.Scope) error {
		unit := p.ConfigString("unit", "celsius")
		p.AddTool(kiln.Tool{
			Name:        "weather",
			Description: "Current weather for a city.",
			Schema:      kiln.Object(map[string]any{"city": kiln.Str("city name")}, "city"),
			Meta:        map[string]any{"readonly": true},
			Run: func(_ context.Context, args map[string]any) (string, error) {
				city := strings.TrimSpace(kiln.ArgString(args, "city"))
				if city == "" {
					return "", fmt.Errorf("city is required")
				}
				return fmt.Sprintf("18 degrees %s in %s", unit, city), nil
			},
		})
		return nil
	},
}

// New returns the module. Keys in cfg are the module's configuration.
func New(cfg ...map[string]any) kiln.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

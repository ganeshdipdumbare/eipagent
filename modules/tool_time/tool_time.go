package tool_time

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Returns the current time. Config: layout (Go time layout).
//
// Example:
//
//	tool_time.New(map[string]any{"layout": "2006-01-02 15:04 MST"})
func Apply(p *sdk.Scope) error {
	layout := p.ConfigString("layout", time.RFC1123)
	p.AddTool(sdk.Tool{
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

// Module is the compiled form of "tool_time". modules/std registers it.
var Module = engine.Module{Name: "tool_time", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

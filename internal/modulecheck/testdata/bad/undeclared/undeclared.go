package undeclared

import (
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Module = engine.Module{Name: "undeclared", Provides: []string{"a"}, Apply: Apply}

func Apply(p *sdk.Module) error { return p.Provide("b", 1) }

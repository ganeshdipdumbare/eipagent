package initfunc

import (
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Module = engine.Module{Name: "initfunc", Apply: Apply}

func init() {}

func Apply(p *sdk.Scope) error { return nil }

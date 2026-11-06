package wrongname

import (
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Module = engine.Module{Name: "other", Apply: Apply}

func Apply(p *sdk.Scope) error { return nil }

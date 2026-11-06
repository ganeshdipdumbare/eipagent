package alpha

import (
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Module = engine.Module{Name: "alpha", Provides: []string{"alpha.svc"}, Apply: Apply}

func Apply(p *sdk.Scope) error { return p.Provide("alpha.svc", 1) }

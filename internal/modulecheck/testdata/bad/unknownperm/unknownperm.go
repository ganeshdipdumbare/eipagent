package unknownperm

import (
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Module = engine.Module{Name: "unknownperm", Permissions: []string{"root"}, Apply: Apply}

func Apply(p *sdk.Scope) error { return nil }

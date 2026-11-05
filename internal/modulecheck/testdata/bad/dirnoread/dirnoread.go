package dirnoread

import (
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Permissions = []string{"fs:write"}

var Module = engine.Module{Name: "dirnoread", Permissions: Permissions, Apply: Apply}

func Apply(p *sdk.Module) error {
	_, err := p.Dir("/tmp/x", false)
	return err
}

package beta

import (
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Permissions = []string{"net"}

var Module = engine.Module{Name: "beta", Provides: []string{"model/*"}, Permissions: Permissions, Apply: Apply}

func Apply(p *sdk.Module) error {
	if _, err := p.HTTP(sdk.HTTPOptions{AllowHosts: []string{"api.example.com"}}); err != nil {
		return err
	}
	return p.Provide("model/x", 1)
}

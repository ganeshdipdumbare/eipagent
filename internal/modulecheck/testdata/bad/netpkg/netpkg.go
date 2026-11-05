package netpkg

import (
	"net/http"

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Module = engine.Module{Name: "netpkg", Apply: Apply}

func Apply(p *sdk.Module) error {
	_, err := http.Get("https://example.com")
	return err
}

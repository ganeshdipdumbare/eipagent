package osexec

import (
	"os/exec"

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Module = engine.Module{Name: "osexec", Apply: Apply}

func Apply(p *sdk.Scope) error { return exec.ErrNotFound }

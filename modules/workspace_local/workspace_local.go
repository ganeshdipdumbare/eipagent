package workspace_local

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Shell and file tools act on the host, in one workspace directory
// (via sandbox/local). Provided as "workspace". Use workspace_sandbox instead to
// move every shell/fs tool into isolated sandboxes.
//
// Load sandbox_local with config {workdir: "."} to work in a project.
//
// Example:
//
//	sandbox_local.New(map[string]any{"workdir": "."})
//	kiln.Config{Modules: []kiln.Module{workspace_local.New(), tool_shell.New(), tool_fs.New()}}
var Requires = []string{"sandbox/local"}

func Apply(p *sdk.Scope) error {
	prov := p.Service("sandbox/local").(*sdk.SandboxProvider)
	box, err := prov.Create(context.Background(), sdk.SandboxSpec{Network: sdk.NetworkPolicy{Mode: "full"}})
	if err != nil {
		return err
	}
	p.OnClose(func() { box.Close(context.Background()) })
	return p.Provide("workspace", &sdk.ExecWorld{
		Name: "local",
		For:  func(c context.Context) (*sdk.Sandbox, error) { return box, nil },
	})
}

// Module is the compiled form of "workspace_local". modules/std registers it.
var Module = engine.Module{Name: "workspace_local", Requires: Requires, Provides: []string{"workspace"}, Permissions: nil, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

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
//	kiln.WithModuleConfig("sandbox_local", map[string]any{"workdir": "."})
//	kiln.WithModules("workspace_local", "tool_shell", "tool_fs")
var Requires = []string{"sandbox/local"}

func Apply(p *sdk.Module) error {
	prov := p.Service("sandbox/local").(*sdk.SandboxProvider)
	box, err := prov.Create(context.Background(), sdk.SandboxSpec{Network: sdk.NetworkPolicy{Mode: "full"}})
	if err != nil {
		return err
	}
	p.OnUnload(func() { box.Close(context.Background()) })
	return p.Provide("workspace", &sdk.ExecWorld{
		Name: "local",
		For:  func(c context.Context) (*sdk.Sandbox, error) { return box, nil },
	})
}

// Module is the compiled form of "workspace_local". modules/std registers it.
var Module = engine.Module{Name: "workspace_local", Requires: Requires, Provides: []string{"workspace"}, Permissions: nil, Apply: Apply}

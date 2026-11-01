//go:build ignore

package plugin

import (
	"context"

	"eipagent/sdk"
)

// Shell and file tools act on the host, in one workspace directory
// (via sandbox/local). Provided as "workspace". Use workspace_sandbox instead to
// move every shell/fs tool into isolated sandboxes.
//
// Load sandbox_local with config {workdir: "."} to work in a project.
var Requires = []string{"sandbox/local"}

func Apply(p *sdk.Plugin) error {
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

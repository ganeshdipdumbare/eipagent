// A custom sandbox provider. Any execution backend (Firecracker,
// microsandbox, E2B, Kubernetes pods, a remote VM) plugs in by providing
// "sandbox/<name>"; workspace_sandbox and tool_sandbox then use it unchanged.
//
// This one is a compiled application module (Config.Modules) that wraps an
// existing sandbox; a real provider would call its backend's API in
// Create/Exec/ReadFile/WriteFile/Close.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/modules/model_mock"
	"github.com/ganeshdipdumbare/kiln/modules/sandbox_local"
	"github.com/ganeshdipdumbare/kiln/modules/tool_shell"
	"github.com/ganeshdipdumbare/kiln/modules/workspace_sandbox"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

func main() {
	provider := kiln.Module{
		Name:     "sandbox_audited",
		Requires: []string{"sandbox/local"},
		Provides: []string{"sandbox/audited"},
		Apply: func(p *sdk.Scope) error {
			inner := p.Service("sandbox/local").(*sdk.SandboxProvider)
			return p.Provide("sandbox/audited", &sdk.SandboxProvider{
				Name:      "audited",
				Isolation: inner.Isolation,
				Create: func(c context.Context, spec sdk.SandboxSpec) (*sdk.Sandbox, error) {
					box, err := inner.Create(c, spec)
					if err != nil {
						return nil, err
					}
					exec := box.Exec
					box.Provider = "audited"
					box.Exec = func(c context.Context, req sdk.ExecRequest) (*sdk.ExecResult, error) {
						p.Logf("exec in %s: %s%s", box.ID, req.Shell, req.Cmd)
						return exec(c, req)
					}
					return box, nil
				},
			})
		},
	}
	a, err := kiln.New(kiln.Config{
		Model: "mock:demo",
		Modules: []kiln.Module{
			model_mock.New(map[string]any{"script": []any{
				map[string]any{"tool": "bash", "args": map[string]any{"command": "echo hello from the sandbox"}},
				map[string]any{"text": "done"},
			}}),
			sandbox_local.New(),
			provider,
			workspace_sandbox.New(map[string]any{"provider": "audited"}),
			tool_shell.New(),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
	res, err := a.Generate(context.Background(), "run it")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Steps[0].ToolResults[0].Content)
}

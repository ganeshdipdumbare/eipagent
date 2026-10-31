// A custom sandbox provider. Any execution backend (Firecracker,
// microsandbox, E2B, Kubernetes pods, a remote VM) plugs in by providing
// "sandbox/<name>"; exec_sandbox and tool_sandbox then use it unchanged.
//
// This one is compiled Go and wraps an existing sandbox; a real provider
// would call its backend's API in Create/Exec/ReadFile/WriteFile/Close.
// The same struct can be built from an interpreted plugin file too.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ganeshdipdumbare/eipagent"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

func main() {
	provider := eipagent.Plugin{
		Name:   "sandbox_audited",
		Inject: []string{"sandbox/local"},
		Apply: func(ctx *sdk.Context) error {
			inner := ctx.Service("sandbox/local").(*sdk.SandboxProvider)
			return ctx.Provide("sandbox/audited", &sdk.SandboxProvider{
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
						ctx.Logf("exec in %s: %s%s", box.ID, req.Shell, req.Cmd)
						return exec(c, req)
					}
					return box, nil
				},
			})
		},
	}
	a, err := eipagent.New(
		eipagent.WithModel("mock:demo"),
		eipagent.WithPlugin("llm_mock", map[string]any{"script": []any{
			map[string]any{"tool": "bash", "args": map[string]any{"command": "echo hello from the sandbox"}},
			map[string]any{"text": "done"},
		}}),
		eipagent.WithPlugins("sandbox_local"),
		eipagent.WithCompiledPlugin(provider),
		eipagent.WithPlugin("exec_sandbox", map[string]any{"provider": "audited"}),
		eipagent.WithPlugins("tool_shell"),
	)
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

package eipagent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/eipagent"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// A compiled sandbox provider must stay on the host when WithRunIn starts
// the agent in a box. The box has no compiled plugin source to load.
func TestRunInCompiledProvider(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "eip")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/eip").CombinedOutput(); err != nil {
		t.Fatalf("build eip: %v %s", err, out)
	}
	root := t.TempDir()
	provider := eipagent.Plugin{
		Name:     "sandbox_audited",
		Requires: []string{"sandbox/local"},
		Apply: func(p *sdk.Plugin) error {
			inner := p.Service("sandbox/local").(*sdk.SandboxProvider)
			return p.Provide("sandbox/audited", &sdk.SandboxProvider{
				Name:      "audited",
				Isolation: inner.Isolation,
				Create: func(c context.Context, spec sdk.SandboxSpec) (*sdk.Sandbox, error) {
					box, err := inner.Create(c, spec)
					if err != nil {
						return nil, err
					}
					box.Provider = "audited"
					// Plugins get a sandbox without Upload/Download, so
					// move files through the box filesystem instead.
					box.Upload = func(c context.Context, hostPath, boxPath string) error {
						data, err := os.ReadFile(hostPath)
						if err != nil {
							return err
						}
						if err := box.WriteFile(c, boxPath, data); err != nil {
							return err
						}
						_, err = box.Exec(c, sdk.ExecRequest{Cmd: "chmod", Args: []string{"+x", boxPath}})
						return err
					}
					box.Download = func(c context.Context, boxPath, hostPath string) error {
						data, err := box.ReadFile(c, boxPath)
						if err != nil {
							return err
						}
						return os.WriteFile(hostPath, data, 0o644)
					}
					return box, nil
				},
			})
		},
	}
	a, err := eipagent.New(
		eipagent.WithModel("mock:m"),
		eipagent.WithPlugin("model_mock", map[string]any{"script": []any{
			map[string]any{"tool": "bash", "args": map[string]any{"command": "pwd; echo compiled-ok"}},
			map[string]any{"text": "done"},
		}}),
		eipagent.WithCompiledPlugin(provider),
		eipagent.WithPlugins("tool_shell"),
		eipagent.WithRunIn(eipagent.RunIn{
			Provider:    "audited",
			Binary:      bin,
			HostPlugins: []eipagent.PluginSpec{{Use: "sandbox_local", Config: map[string]any{"root": root}}},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	res, err := a.Generate(bg, "go")
	if err != nil {
		t.Fatal(err)
	}
	out := res.Steps[0].ToolResults[0].Content
	if !strings.Contains(out, "compiled-ok") || !strings.Contains(out, root) {
		t.Fatalf("compiled provider with RunIn: %q", out)
	}
}

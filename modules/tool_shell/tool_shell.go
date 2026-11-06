package tool_shell

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"fmt"
	"strings"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// A bash tool that runs in the "workspace" world (host workspace or sandbox).
//
// Config: timeout_seconds (120), max_output (30000 chars).
//
// Example:
//
//	tool_shell.New(map[string]any{"timeout_seconds": 60, "max_output": 10000})
//	kiln.Sandbox("docker") // run it in a container instead of on the host
var Requires = []string{"workspace"}

func Apply(p *sdk.Scope) error {
	world := p.Service("workspace").(*sdk.ExecWorld)
	timeout := p.ConfigInt("timeout_seconds", 120)
	max := p.ConfigInt("max_output", 30000)
	clip := func(s string) string {
		if len(s) > max {
			return s[:max/2] + fmt.Sprintf("\n... [%d chars omitted] ...\n", len(s)-max) + s[len(s)-max/2:]
		}
		return s
	}
	p.AddTool(sdk.Tool{
		Name:        "bash",
		Description: "Run a shell command in the workspace (" + world.Name + "). Returns stdout, stderr and the exit code.",
		Schema: sdk.Object(map[string]any{
			"command":         sdk.Str("the command line"),
			"timeout_seconds": sdk.Int("optional timeout"),
		}, "command"),
		Meta: map[string]any{"risk": "high", "sandbox": world.Name},
		Run: func(c context.Context, args map[string]any) (string, error) {
			box, err := world.For(c)
			if err != nil {
				return "", err
			}
			res, err := box.Exec(c, sdk.ExecRequest{Shell: sdk.ArgString(args, "command"), TimeoutSeconds: sdk.ArgInt(args, "timeout_seconds", timeout)})
			if err != nil {
				return "", err
			}
			var b strings.Builder
			if res.Stdout != "" {
				b.WriteString(clip(res.Stdout))
			}
			if res.Stderr != "" {
				b.WriteString("\n[stderr]\n" + clip(res.Stderr))
			}
			fmt.Fprintf(&b, "\n[exit code %d]", res.ExitCode)
			return strings.TrimSpace(b.String()), nil
		},
	})
	return nil
}

// Module is the compiled form of "tool_shell". modules/std registers it.
var Module = engine.Module{Name: "tool_shell", Requires: Requires, Provides: nil, Permissions: nil, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

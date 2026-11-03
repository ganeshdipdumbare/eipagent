//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"strings"

	"eipagent/sdk"
)

// A bash tool that runs in the "workspace" world (host workspace or sandbox).
//
// Config: timeout_seconds (120), max_output (30000 chars).
//
// Example:
//
//	eipagent.WithPlugin("tool_shell", map[string]any{"timeout_seconds": 60, "max_output": 10000})
//	eipagent.WithSandbox("docker") // run it in a container instead of on the host
var Requires = []string{"workspace"}

func Apply(p *sdk.Plugin) error {
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

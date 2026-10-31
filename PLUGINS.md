# Writing plugins

A plugin is a single Go file, interpreted by [Yaegi](https://github.com/traefik/yaegi):

```go
//go:build ignore

package plugin

import (
	"context"

	"eipagent/sdk"
)

var Inject = []string{"store"}       // services required before Apply runs ("?x" optional, "llm/*" any)
var Permissions = []string{"net"}    // capabilities: net, exec, fs:read, fs:write, env, db

func Apply(ctx *sdk.Context) error {
	store := ctx.Service("store").(*sdk.Store)
	ctx.AddTool(sdk.Tool{Name: "...", Schema: sdk.Object(...), Run: func(c context.Context, args map[string]any) (string, error) { ... }})
	ctx.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) { return next(c, req) })
	ctx.Effect(func() func() { /* start */ return func() { /* cleanup on unload */ } })
	return ctx.Provide("my_service", &MyService{...})
}
```

Lifecycle: `PENDING` (waiting on `Inject`) → `LOADING` → `ACTIVE`, or `FAILED` (anything it registered is rolled back). When a dependency disappears, its dependents go back to `PENDING`. When it returns, they start again.

## What you can register

| API | Purpose |
|---|---|
| `Provide(name, svc)` / `Service(name)` | Services. Names are unique, so unload a plugin to swap it |
| `AddTool`, `AddSkill` | Tools for the model, and system-prompt fragments |
| `WrapModel`, `WrapTool` | Onion middleware (`wrap_model_call` / `wrap_tool_call`) |
| `OnStepPrepare`, `OnModelAfter` | Rewrite the next request; inspect the reply or `Jump` |
| `OnAgentStart`, `OnAgentEnd`, `OnEvent` | Run lifecycle, and observing streamed events |
| `Effect`, `OnDispose` | Goroutines, connections, processes, all cleaned up on unload |
| `Secret("secret://NAME")` | Secrets through the `secrets` service (grants are per plugin) |
| `SetPriority(n)` | Higher runs outer/first. The reserved band is only for locked plugins |

Well-known services: `loop`, `llm/<name>`, `checkpointer`, `store`, `secrets`, `audit`, `approver`, `agent`, `exec`, `sandbox/<name>`, `db`.

## Permissions

The engine parses each plugin's imports before running it. An import the plugin's granted permissions don't cover is refused with a clear error, and `unsafe`, `syscall`, `os.Exit`, `os.Setenv` and similar are never available. The admin policy's `grants` can only narrow what a plugin declares. This is defense in depth. For untrusted plugin code, run the agent in a sandbox (`WithRunIn`).

## Yaegi pitfalls (found while building the standard plugins)

- **Don't pass maps through `any` parameters or struct fields that you then `json.Marshal`.** An interpreted `any` holds a wrapper, and the value marshals as `{}`. Type parameters as `map[string]any`, and build outgoing JSON from maps.
- **Variables declared in a loop body are shared between closures.** Build per-item closures in a helper function (`ctx.AddTool(makeTool(name))`).
- **Don't assign untyped `nil` in a parallel assignment** (`a, b = nil, nil`). Use two statements.
- **No `min` / `max` builtins**, and avoid `switch` on `any` values. Assert to a concrete type first.
- Plugins see the environment as it was when they loaded (only with the `env` permission). Prefer `ctx.Secret`.
- Plugin source is Go 1.22-era syntax and stdlib. Host code can use everything in Go 1.27.

## Compiled plugins

The same lifecycle applies to native Go: `eipagent.WithCompiledPlugin(eipagent.Plugin{Name, Inject, Apply})`. Use compiled plugins for code that needs third-party packages, or for hot paths.

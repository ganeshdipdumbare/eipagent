# Writing modules

The short version. A module is a compiled Go package that declares what it
requires, what it provides and which capabilities it uses. `kiln.New`
validates the whole composition before anything runs.

## Start from an option, write a module when you need one

Most setups are options (`WithModel`, `WithApprovals`, `WithPersistence`,
`WithSandbox`, `WithAudit`, `WithTracing`, `WithMCP`), and plumbing resolves
itself. Write a module when you need behaviour the standard set does not have.

## A module is one package

```go
package weather

import (
	"context"

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

var Module = engine.Module{
	Name:        "weather",
	Requires:    []string{"?secrets"},          // services needed before Apply ("?x" optional, "model/*" any one)
	Provides:    nil,                            // services Apply will register ("x/*" = prefix)
	Permissions: []string{"net"},                // net, exec, fs:read, fs:write, env, env:secrets, db
	Secrets:     []string{"WEATHER_API_KEY"},    // the only secrets it may resolve
	Apply: func(p *sdk.Module) error {
		hc, err := p.HTTP(sdk.HTTPOptions{AllowHosts: []string{"api.openweathermap.org"}})
		if err != nil {
			return err
		}
		key, err := p.Secret("secret://WEATHER_API_KEY")
		if err != nil {
			return err
		}
		p.AddTool(sdk.Tool{
			Name:   "weather",
			Schema: sdk.Object(map[string]any{"city": sdk.Str("city")}, "city"),
			Run: func(c context.Context, args map[string]any) (string, error) {
				u := "https://api.openweathermap.org/data/2.5/weather?appid=" + key + "&q=" + sdk.ArgString(args, "city")
				req, _ := http.NewRequestWithContext(c, "GET", u, nil)
				resp, err := hc.Do(req)
				// ... read and return
			},
		})
		return nil
	},
}
```

Import it and pass it:

```go
agent, err := kiln.New(
	kiln.WithModel("openai:gpt-4.1"),
	kiln.WithModule(weather.Module),
)
```

A module is not one "kind": the same `Apply` can add tools, wrap calls and
provide a service.

The package can live in another directory in your app, or in another Go
module. Fetch it with `go get` (or point at a local checkout with `replace`);
there is no module registry, lock file or runtime loader. Publish it by
tagging the Go module.

`WithModule` registers and enables the descriptor. Configure that row with
`WithModuleConfig(name, cfg)`, or use `WithModuleSpec` when you need an
instance name (`As`) or priority. `WithModules(names...)` only enables modules
already in the compiled registry; it never imports a package.

If you already have a Go library, write a small `engine.Module` adapter:
`Apply` exposes the library's functions as `sdk.Tool`s (or a service), and any
network, file, exec or database access goes through the broker. See the
[Custom modules](https://kilnai-docs.vercel.app/#custom-modules) docs page and
[`examples/custom-module`](examples/custom-module).

Lifecycle: `Apply` runs once, in dependency order, after every required
service exists. Anything it registered is undone on `Close`. There is no
unload or reload; rebuild and redeploy.

## By goal

| You want… | Call |
|---|---|
| the model to be able to do something | `p.AddTool` |
| something to happen around every model or tool call | `p.WrapModel`, `p.WrapTool`, `p.BeforeStep`, `p.AfterModel`, `p.OnRunStart`, `p.OnRunEnd`, `p.OnEvent` |
| connect a model API | `p.Provide("model/<name>", &sdk.Provider{...})` — declare `model/*` in `Provides` |
| save conversations / long-term memory | `p.Provide("threads", &sdk.Checkpointer{...})` / `p.Provide("memory", &sdk.Store{...})` |
| run code somewhere isolated | `p.Provide("sandbox/<name>", &sdk.SandboxProvider{...})` — declare `sandbox/*` |
| start runs from outside (schedule, webhook, queue) | `p.Effect` + `p.Service("agent")` |
| export audit records or traces | `p.Provide("audit/sink/<name>", &sdk.AuditSink{...})`, or hooks |
| resolve `secret://NAME` from another store | `p.Provide("secrets", &sdk.Secrets{...})` |

Other calls: `p.Secret("secret://NAME")` (a declared secret), `p.Config*`
(config with defaults), `p.SetPriority(n)` (higher wraps further out; the
reserved band is for locked modules), `p.Logf` (redacted logging),
`p.AddSkill` (a system-prompt fragment), `p.OnUnload` (cleanup).

Services: `loop`, `model/<name>`, `threads`, `memory`, `secrets`, `audit`,
`approver`, `agent`, `workspace`, `sandbox/<name>`, `db`.

## Capabilities and the broker

Compile-time inclusion is not a sandbox: a module linked into your binary
runs with the process's privileges. The SDK makes the *intended* boundary
explicit and checks it, at build time and at the API boundary.

The only supported way to reach the outside world is the module's context:

| Capability | Call | What it enforces |
|---|---|---|
| `net` | `p.HTTP(sdk.HTTPOptions{AllowHosts, AllowPrivate, AllowInsecure, Timeout})` | host allow-list, no redirects, private/reserved addresses and cloud metadata refused, plain http to remote hosts refused |
| `net` | `p.Listen(addr, allowAny)` | loopback unless `allowAny` |
| `fs:read` | `p.Dir(root, false)` | `os.Root`: `..` and symlinks cannot escape; read-only |
| `fs:write` | `p.Dir(root, true)` | the same, plus `WriteFile`, `MkdirAll`, `Remove`, `Rename` |
| `exec` | `p.Exec("prog", ...)` then `Run` / `Start` | named programs only, no shell, secret-looking env refused, explicit secrets only |
| `env` | `p.Env(name)` | credential-looking names refused |
| `env:secrets` | `p.Env(name)` | also secret-looking names |
| `db` | `p.DB()` | the connection the `db_sql` provider opened; no `database/sql` in the module |

Every call fails closed with the missing capability name. The admin policy's
`grants` can only narrow what a module declares.

Modules also cannot create host directories. A file-backed module is given a
directory that already exists; the application (or `WithPersistence` /
`WithAudit`) creates it.

## The build-time check

```bash
kiln module check modules     # exit 1 on any error
```

| Rule | Error when |
|---|---|
| `forbidden-import` | the module imports `os`, `os/exec`, `net`, `syscall`, `database/sql` (except the `db_sql` provider), `unsafe`, `io/ioutil`, `embed`, … or calls a bypassing selector such as `http.Get` or `net.Dialer` |
| `missing-permission` | a broker call needs a capability the module did not declare |
| `unknown-permission` | `Permissions` contains a name that is not a capability |
| `undeclared-provide` | `Provide("x")` is not covered by `Provides` |
| `descriptor` | no `var Module = engine.Module{...}`, no `Apply`, or `Name` does not match the directory |
| `init-func` | the package has an `init` function |

Warnings (not failures) are reported when a value is not a literal and cannot
be checked (for example `Provide("model/" + name)`).

`kiln module manifest modules` prints the same information as JSON: per module,
`Requires`, `Provides`, `Permissions`, `Secrets` and the capabilities the code
actually uses (`BrokerCalls`). Keep it next to `go version -m` output to answer
"which modules are in this build, and what can each reach?".

The analyzer is a CI gate, not a runtime control. A module that skips it is
not contained by it.

## Secrets

Resolve secrets when you use them (`p.Secret("secret://NAME")` inside the
request path), not once in `Apply`: providers cache and refresh, so rotation
reaches running modules. Every value resolved through the `secrets` service is
redacted from logs, traces and audit records. A module may resolve only the
names it declares in `Secrets`, or that the application writes as `secret://`
in that module's config.

To write your own secret provider (KMS, a cloud secret manager), provide
`"secrets"` as an `sdk.Secrets{Get: func(ctx, caller, name)}`: the core always
passes the real calling module as `caller`, so you can enforce per-module
grants. `secrets_env` and `secrets_vault` are the standard examples.

## Compiled in, not linked at runtime

There is no `Load`, `Reload`, `Unload`, hot reload, plugin store, signed
package or lock file. To change behaviour: edit the module, `go build`,
redeploy. To ship a module to another team, ship the Go package (or publish
the module path) and let them import it.

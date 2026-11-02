# Writing plugins

The short version. The [docs' Plugins section](https://eipagent-docs.vercel.app/#plugins) has a page per kind, each covering what it is, when to use it, every built-in with its config, how to write your own and the gotchas.

## Start from an option, write a plugin when you need one

Most setups are options (`WithModel`, `WithApprovals`, `WithPersistence`, `WithSandbox`, `WithAudit`, `WithTracing`, `WithMCP`), and plumbing loads itself. The goal-first table [What to use when](https://eipagent-docs.vercel.app/#plugins-choose) maps every goal to its option and plugins.

## A plugin is one function

A plugin is a single Go file, interpreted by [Yaegi](https://github.com/traefik/yaegi), with `func Apply(p *sdk.Plugin) error`. Inside it, call what you need. A plugin is not one "kind": the same plugin can add tools, wrap calls and provide a service.

```go
//go:build ignore

package plugin

import (
	"context"

	"eipagent/sdk"
)

var Requires = []string{"memory"}  // services needed before Apply runs ("?x" optional, "model/*" any one)
var Permissions = []string{"net"}  // capabilities: net, exec, fs:read, fs:write, env, env:secrets, db

func Apply(p *sdk.Plugin) error {
	mem := p.Service("memory").(*sdk.Store)
	p.AddTool(sdk.Tool{Name: "...", Schema: sdk.Object(...), Run: func(c context.Context, args map[string]any) (string, error) { ... }})
	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) { return next(c, req) })
	p.Effect(func() func() { /* start */ return func() { /* stop on unload */ } })
	return p.Provide("my_service", &MyService{...})
}
```

Lifecycle: `PENDING` (waiting on `Requires`) → `LOADING` → `ACTIVE`, or `FAILED` (anything it registered is rolled back). When a dependency disappears, its dependents go back to `PENDING`, and they start again when it returns.

`eip plugin init NAME` asks what the plugin should do and writes a working one. `eip plugin validate` lints it and says what it does, e.g. `weather: adds 1 tool (weather) · requires ?secrets · permissions net`.

## By goal

| You want… | Call | Kind page |
|---|---|---|
| the model to be able to do something | `AddTool` | [Tools](https://eipagent-docs.vercel.app/#kind-tools) |
| something to happen around every model or tool call | `WrapModel`, `WrapTool`, `BeforeStep`, `AfterModel`, `OnRunStart`, `OnRunEnd`, `OnEvent` | [Hooks](https://eipagent-docs.vercel.app/#kind-hooks) |
| connect a model API | `Provide("model/<name>", &sdk.Provider{...})` | [Model providers](https://eipagent-docs.vercel.app/#kind-models) |
| save conversations / long-term memory | `Provide("threads", &sdk.Checkpointer{...})` / `Provide("memory", &sdk.Store{...})` | [Threads and memory](https://eipagent-docs.vercel.app/#kind-threads-memory) |
| run code somewhere isolated | `Provide("sandbox/<name>", &sdk.SandboxProvider{...})` | [Sandboxes and workspaces](https://eipagent-docs.vercel.app/#kind-sandboxes) |
| start runs from outside (schedule, webhook, queue) | `Effect` + `Service("agent")` | [Triggers](https://eipagent-docs.vercel.app/#kind-triggers) |
| export audit records or traces | `Provide("audit/sink/<name>", &sdk.AuditSink{...})`, or hooks | [Telemetry and audit](https://eipagent-docs.vercel.app/#kind-telemetry) |
| resolve `secret://NAME` from another store | `Provide("secrets", &sdk.Secrets{...})` | [Secrets](https://eipagent-docs.vercel.app/#kind-secrets) |

Other calls: `Secret("secret://NAME")` (a granted secret), `Config*` (the plugin's config, with defaults), `SetPriority(n)` (higher wraps further out; the reserved band is for locked plugins), `Logf` (redacted logging), `AddSkill` (a system-prompt fragment), `OnUnload` (cleanup).

Services: `loop`, `model/<name>`, `threads`, `memory`, `secrets`, `audit`, `approver`, `agent`, `workspace`, `sandbox/<name>`, `db`.

Renamed in v0.4, still accepted until v1.0: `sdk.Context` → `sdk.Plugin`, `var Inject` → `var Requires`, `OnAgentStart`/`OnAgentEnd`/`OnStepPrepare`/`OnModelAfter`/`OnDispose` → `OnRunStart`/`OnRunEnd`/`BeforeStep`/`AfterModel`/`OnUnload`, services `llm/*` → `model/*`, `checkpointer` → `threads`, `store` → `memory`, `exec` → `workspace`. `eip plugin fix` rewrites a plugin for you.

## Permissions

The engine parses each plugin's imports before running it. An import the plugin's granted permissions don't cover is refused with a clear error, and `unsafe`, `syscall`, `os.Exit`, `os.Setenv` and similar are never available. The admin policy's `grants` can only narrow what a plugin declares. This is defense in depth.

- `fs:write` without `fs:read` can't read: `os.OpenFile` then only opens files for writing. In `io/ioutil`, `ReadFile`/`ReadDir` need `fs:read` and `WriteFile`/`TempFile`/`TempDir` need `fs:write`.
- `net/textproto` needs `net` (it can dial), and `mime/multipart` needs `fs:write` (it spills uploads to temp files). `os.NewFile` and `os.Stdin` are never available.
- `eip plugin validate` compiles a plugin without running any of it, top-level initializers included.
- Plugin names may only use letters, digits, `_` and `-`.
- A tool or skill name another plugin registered is refused (logged; the first one keeps it). A plugin can re-register its own.
- Sandboxes a plugin gets from a `sandbox/<name>` service have no `Upload` / `Download` (those copy host paths); only the host application moves files across.
- `env` shows the environment minus credential-looking variables (`*_API_KEY`, `*_TOKEN`, `*_SECRET`, `*PASSWORD*`, `*_DSN` ...). Only secret providers should declare `env:secrets`, which shows everything; other plugins get keys through `ctx.Secret` and per-plugin grants.
- No interpreted plugin can call `sdk.AddRedaction`, and only built-in plugins can call `sdk.Redact` / `RedactMap` / `RedactValue`. Any redaction function also answers "is this string a secret?", so plugins from plugin directories and stores don't get one. Their `ctx.Logf` output is redacted for them. A custom audit or trace sink that needs redaction should be a compiled plugin (`eipagent.Plugin`).
- Records a plugin submits through the `audit` service are stamped `writer: "plugin:<name>"` by the SDK. For untrusted plugin code, run the agent in a sandbox (`WithRunIn`).

## Ship a plugin: manifest, lint, package, install

For plugins that travel (from a team, a vendor, or a registry), the SDK has a packaging and install pipeline. All of it is in the `eipagent/pluginpkg` package and the `eip plugin` CLI. A control plane can drive the same Go API.

### 1. Scaffold

```bash
eip plugin init weather                    # asks what it should do; -template tool|hook|model|memory|sandbox|trigger|mcp skips the question
```

This writes `weather/weather.go`, `weather/plugin.json` and a README. The same templates, already generated, are in [`examples/plugins`](examples/plugins). A plugin dir holds exactly one plugin, and its `.go` file is named after it.

### 2. The manifest (`plugin.json`)

```json
{
  "schema": 1,
  "name": "weather",
  "version": "0.1.0",
  "description": "Current weather for a city",
  "permissions": ["net"],
  "requires": ["?secrets"],
  "provides": [],
  "secrets": ["WEATHER_API_KEY"],
  "tools": ["weather"],
  "eipagent": ">=0.3.0, <1.0.0"
}
```

`permissions` and `requires` must equal the source's `var Permissions` / `var Requires`. `secrets` lists secret names, never values. `eipagent` is a version constraint checked against `sdk.Version` (`>=`, `<`, `^`, `~`, comma-separated). `build` adds `files`, the sha256 of every file.

### 3. Validate

```bash
eip plugin validate weather        # DRAFT -> VALIDATED when there are no errors
```

| Rule | Error when |
|---|---|
| `source` | the package isn't `plugin`, there's no `func Apply(*sdk.Plugin) error`, or there are extra `.go` files |
| `forbidden-import` | an import or `os.X` symbol is never available to plugins (`unsafe`, `syscall`, `os.Exit` ...) |
| `permission-missing` | an import or `os.X` needs a permission the plugin doesn't declare (warning when a declared one is unused) |
| `permission-mismatch`, `requires-mismatch` | the manifest and the source disagree |
| `old-name` (warning) | the code uses a name renamed in v0.4; `eip plugin fix` rewrites it |
| `undeclared-service` | `Service("x")` is used but `x` isn't in `Requires` or provided by the plugin |
| `secret-ref` | a `secret://` literal is malformed or isn't listed in `secrets` |
| `tools`, `provides` | a literal tool or service isn't listed in the manifest |
| `manifest`, `compat` | the manifest is invalid, or the SDK version doesn't satisfy `eipagent` |
| `compile` | the source doesn't compile under its declared permissions |

`validate` on a file with no manifest prints a suggested `plugin.json` inferred from the source.

### 4. Build and sign

```bash
eip plugin keygen -out ~/.eip/signing.key                 # ed25519; share signing.key.pub
eip plugin build -key ~/.eip/signing.key weather          # -> weather-0.1.0.eip
eip plugin inspect weather-0.1.0.eip
```

A package is a reproducible `tar.gz` with `plugin.json` (carrying each file's sha256), an optional `plugin.sig` (ed25519 over the canonical manifest) and the files. Opening a package refuses unsafe names, links, duplicates, files missing from or not listed in the manifest, hash mismatches, and anything over 8 MiB.

### 5. Install, approve, enable

```bash
cat signing.key.pub >> .eipagent/plugins/trusted_keys
eip plugin install -by alice weather-0.1.0.eip           # VALIDATED: held, nothing runs
eip plugin approve -by bob weather 0.1.0                 # APPROVED
eip plugin enable weather                                # ACTIVE
eip run -store .eipagent/plugins -model openai:gpt-4.1 "weather in Oslo?"
```

```text
install ──► VALIDATED ──approve──► APPROVED ──enable──► ACTIVE ──disable──► APPROVED
                                       ▲                  │
                                       └──── rollback ────┤ new version fails to start ─► FAILED (old keeps running)
any ──quarantine──► QUARANTINED ──approve──► APPROVED
```

- **Held until approved.** Installed versions live in `<store>/packages`. The engine only loads from `<store>/active`, so nothing runs before `enable`.
- **Verified twice.** On install: the signature against `trusted_keys` (required unless `-allow-unsigned`), the optional `-sha256` pin, compatibility and lint. On enable: the stored archive again, so a package changed at rest is quarantined instead of loaded.
- **Pinned.** Enabling pins the source sha256 (`<store>/eip.lock`), so the secure preset's pin check applies.
- **Safe replacement.** `enable` / `update` hot-swap the plugin in the running agent. If the new version fails to compile, fails `Apply` or is left `PENDING` on missing dependencies, the old version keeps running, the old source and pin are restored and the new version is `FAILED`. Runs already in flight keep the hook and tool functions they started with (per-run snapshot), but anything the old version releases on unload (connections, processes) goes away when it is swapped out.
- **Built-in names are reserved.** A package can't shadow `tool_shell`, `policy` and so on.
- Every step is recorded in `<store>/state.json` (who installed and approved it, the signer, the hashes) and, when an agent is attached, in the audit log (`plugin.install`, `plugin.approve`, `plugin.enable`, `plugin.enable_failed`, `plugin.rollback`, `plugin.quarantine` ...).

The same from Go, in a running agent:

```go
st, _ := pluginpkg.OpenStore(".eipagent/plugins", pluginpkg.StoreOptions{
	Verify: pluginpkg.VerifyOptions{TrustedKeys: keys, RequireSignature: true},
})
a, _ := eipagent.New(eipagent.WithPluginStore(st)) // loads every ACTIVE plugin
st.Install(pkgBytes, "alice")
st.Approve("weather", "0.1.0", "bob")
st.Enable("weather", "")      // live; Update, Rollback, Quarantine, Disable, Uninstall too
```

`go run ./examples/plugin-store` walks one plugin through every state, including a failed update and a rollback.

### Secrets in plugins

Resolve secrets when you use them (`ctx.Secret("secret://NAME")` inside the request path), not once in `Apply`. Providers cache and refresh, so rotation reaches running plugins. Every value resolved through the `secrets` service is redacted from logs (`ctx.Logf`), traces and audit records. To write your own secret provider (KMS, a cloud secret manager), provide `"secrets"` as an `sdk.Secrets{Get: func(ctx, caller, name)}`: the core always passes the real calling plugin as `caller`, so you can enforce per-plugin grants. `secrets_env` and `secrets_vault` are the built-in examples.

## Yaegi pitfalls (found while building the standard plugins)

- **Don't pass maps through `any` parameters or struct fields that you then `json.Marshal`.** An interpreted `any` holds a wrapper, and the value marshals as `{}`. Type parameters as `map[string]any`, and build outgoing JSON from maps.
- **Variables declared in a loop body are shared between closures.** Build per-item closures in a helper function (`ctx.AddTool(makeTool(name))`).
- **Don't assign untyped `nil` in a parallel assignment** (`a, b = nil, nil`). Use two statements.
- **No `min` / `max` builtins**, and avoid `switch` on `any` values. Assert to a concrete type first.
- Plugins see the environment as it was when they loaded (only with the `env` permission). Prefer `ctx.Secret`.
- Plugin source is Go 1.22-era syntax and stdlib. Host code can use everything in Go 1.27.

## Compiled plugins

The same lifecycle applies to native Go: `eipagent.WithCompiledPlugin(eipagent.Plugin{Name, Requires, Apply})`, where `Apply` is `func(p *sdk.Plugin) error`, the same type interpreted plugins get. Use compiled plugins for code that needs third-party packages, or for hot paths.

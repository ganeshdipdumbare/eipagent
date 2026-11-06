# Changelog

All notable changes to kiln. Versions follow [semantic versioning](https://semver.org); see [Versioning](README.md#versioning) for what that covers while the major version is 0.

## Unreleased

### Breaking changes — one way to do each thing

The public API had several spellings for the same decision. It now has one.

- **`kiln.New(kiln.Config{...})`.** All construction options are fields:
  `Model`, `Instructions`, `MaxSteps`, `User`, `Roles`, `Tools`, `Modules`,
  `Policy`, `Logger`, `SaveEachStep`, `RunIn`. `WithModel`,
  `WithInstructions`, `WithMaxSteps`, `WithUser`, `WithTools`, `WithLogger`,
  `WithPolicy`, `WithPolicyFile`, `WithRunIn`, `WithSaveEachStep` and
  `WithoutDefaults` are removed.
- **`kiln.Run{...}` for per-run overrides.** `Model`, `Instructions`,
  `MaxSteps`, `MaxTokens`, `Temperature`, `ToolChoice`, `StopOnTools`,
  `History`, `Images`, `Tools`, `User`, `Roles`, `OnEvent`, `Schema`,
  `CaptureResult`. `Model()`, `System()`, `MaxSteps()`, `MaxTokens()`,
  `Temperature()`, `ToolChoice()`, `StopOnTool()`, `WithHistory()`,
  `WithImages()`, `OnEvent()`, `AsUser()`, `RunTools()` and
  `CaptureResult()` are removed. `Generate`, `Stream`, `Resume`, the
  `Thread` methods and `GenerateObject` take the same struct.
- **One composition point: `Config.Modules []engine.Module`.**
  `WithModule`, `WithModules`, `WithModuleConfig` and `WithModuleSpec` are
  removed, along with `WithProfile` and the profile JSON format. Every
  standard module package exports `New(cfg ...map[string]any)`; the value
  goes in the list. `WithModule` also works for standard modules now (the
  application descriptor replaces the registry one; it used to fail with
  "registered twice").
- **Presets are values, not options.** `WithPersistence`, `WithApprovals`,
  `WithApprover`, `WithSandbox`, `WithAudit`, `WithTracing`, `WithMCP`,
  `WithWebSearch` and `WithThinking` became `Persistence`, `Approvals`,
  `Approver`, `Sandbox`, `Audit`, `Tracing`, `MCP`, `WebSearch` and
  `Thinking`, each returning `[]engine.Module`.
- **`sdk.Module` → `sdk.Scope`.** Only `engine.Module` is called a module
  now. `Apply` takes `*sdk.Scope`. `Module.PluginName()` →
  `ModuleName()`; `Module.OnUnload()` → `OnClose()`.
- **`kiln.Tool` is the tool type.** The generic constructor is
  `kiln.NewTool`; `kiln.ToolDef` is gone. `GenerateText` is removed:
  `New` + `Generate` + `Close` is the one path.
- **`kiln.ServeBox(..., kiln.Config{})`** takes a `Config` instead of
  options. `KILN_MODEL` is no longer read by the SDK (the CLI still reads
  it).

### Fixed

- **A duplicate tool or skill fails the start.** A tool registered by two
  modules used to be dropped with one log line while `New` returned no
  error; it now names the conflict and refuses to start. Service
  registration already worked this way.
- **A module listed twice merges its configuration** instead of failing
  with "listed twice", so the order of equal entries no longer changes the
  result.
- **Instance configuration no longer leaks.** Two instances of one module
  (for example a reviewer model and the main model) each keep their own
  config.
- **Defaults keep their position.** A module that replaces `secrets_env`
  or `threads_inmem` starts where the default would have, so modules
  applying earlier can still use it.
- **Modules can declare host setup.** `engine.Module.Prepare` runs before
  any module applies, which is how the file-backed presets create their
  directories now that options are gone.

## v0.5.0 — 2026-11-05

### Added

- The composition lab is back on the docs site (`#composition`): toggle modules and see the build config, the capability summary/diff and a simulated run. It notes that a real build fixes the module set, so this is illustrative rather than runtime toggling.

### Changed

- The project repository is now `github.com/ganeshdipdumbare/kiln`; docs and repository links were updated. The docs site moved to `kilnai-docs.vercel.app` (`kiln.vercel.app`, `kiln-ai.vercel.app` and `klinai.vercel.app` belong to other projects).

## v0.5.0 — 2026-11-05

**Renamed from eipagent to kiln.** The project is now `kiln`: the Go module is `github.com/ganeshdipdumbare/kiln`, the package is `kiln`, the CLI is `kiln` (`cmd/kiln`), and environment variables are `KILN_*` (`KILN_MODEL`, `KILN_BOX`, `KILN_BOX_DIR`, `KILN_DATABASE_URL`). The logo and docs are updated; older entries below use the current name.

**kiln is a statically composed, capability-secured module SDK.** The runtime plugin system is gone. Nothing is loaded, interpreted or fetched at run time: every unit of functionality is a compiled Go module, and `kiln.New` validates the whole composition before starting it.

### Breaking changes

- **Interpreted plugins removed.** Yaegi is no longer a dependency. Plugins written as `//go:build ignore` source files with `package plugin` cannot be loaded. Convert each to a Go package that exports `var Module = engine.Module{...}` and import it with `kiln.WithModule(m)` (see [MODULES.md](MODULES.md)). The 56 standard plugins are now the packages under [`modules/`](modules). Old import paths (`github.com/ganeshdipdumbare/kiln`, `.../eipagent/sdk`) must be updated to the `kiln` paths.
- **`engine.Plugin` → `engine.Module`; `sdk.Plugin` → `sdk.Module`.** `sdk.Tool.Plugin` is now `sdk.Tool.Module`. The `sdk.Context` alias and the v0.4 name aliases are removed.
- **Runtime lifecycle removed.** `Engine.Load`, `Reload`, `Unload`, `Watch`, `Pins`, `LoadProfile`, `Profile.Plugins`, `PluginSpec`, `PluginStatus`, `WithPluginDir`, `WithPluginStore`, `WithPreset`, `WithHotReload`, `WithLockFile`, `WithCompiledPlugin`, `kiln.lock` and the `plugins.FS` embedded source set no longer exist. There is no hot reload: rebuild and redeploy.
- **Plugin store and signed packages removed.** `pluginpkg`, the `kiln plugin *` commands (init, validate, fix, pack/build, inspect, keygen, install, approve, enable, disable, update, rollback, quarantine, uninstall, list, pin) and `examples/plugins`, `examples/plugin-store`, `examples/plugin-validate` are deleted.
- **Options renamed.** `WithPlugin(use, cfg)` → `WithModuleConfig(name, cfg)`; `WithPlugins(names...)` → `WithModules(names...)`; `WithPluginSpec` → `WithModuleSpec`; `WithCompiledPlugin` → `WithModule`. `Policy.RequirePins` and `Policy.HotReload` are gone; `engine.DevPolicy` is gone (`SecurePolicy()` is the default).
- **Profiles use `modules`, not `plugins`.** A profile is `{"model", "instructions", "max_steps", "modules": [{"use", "as", "config", "disabled", "priority"}]}`.
- **Old module names are gone**, not aliased: `llm_*` → `model_*`, `mw_*` → `hook_*`, `checkpoint_*` → `threads_*`, `store_*` → `memory_*`, `exec_*` → `workspace_*`, `jobs` → `trigger_schedule`, `audit_jsonl` → `audit_file`, `agents` → `tool_subagents`, `memory_store` → `tool_memory`, `loop_default` → `loop`, `mcp_client` → `tool_mcp`, `skills_dir` → `tool_skills`.
- **`WithRunIn` runs the box differently.** The host no longer uploads plugin source or a separate `kiln` binary; it uploads its own program (or `RunIn.Binary`, built for the box's OS/arch) and starts it with `KILN_BOX=1` and `KILN_BOX_DIR=kiln`. The program checks `kiln.InBox()` and calls `kiln.ServeBox`, which takes the profile and policy the host uploaded. Modules the box needs are compiled into that program. `RunIn.PluginDirs` is gone; `RunIn.HostPlugins` is now `[]engine.ModuleSpec`. The old `kiln serve --stdio` box entry point is replaced by `ServeBox`.
- **Directory creation moved to the application.** Modules work inside directories they are given and never create them. `WithPersistence("file", dir)` and `WithAudit(path)` create theirs; an app that configures `threads_file`, `memory_file` or `audit_file` directly must `os.MkdirAll` first. `sandbox_local`, `audit_file`, `memory_file`, `threads_file`, `trace_file` and `tool_skills` now require their directory to exist.
- **Redaction and audit wording.** Audit records now use `"module": "<name>"` (was `"plugin"`) and the writer is `module:<name>` for module-submitted records. Lifecycle events are `module.active` / `module.failed` / `module.disposed` (were `plugin.*`).

### Added

- **Static composition with validation.** `engine.Compose` checks the whole plan before running anything: unknown or denied modules, duplicate providers, missing requirements (with a hint naming the modules that would supply it), unknown capabilities, and cycles. It returns a deterministic start order; policy-required rows start in the order the policy lists them. `Plan.Start` is all-or-nothing: a failure disposes what already started.
- **Capability broker.** Modules reach the outside world only through `p.HTTP` (host allow-list, no redirects, private/reserved/metadata addresses refused unless `AllowPrivate`), `p.Listen`, `p.Dir` (`os.Root`, so `..` and symlinks cannot escape), `p.Exec` (named programs, no shell, filtered environment, explicit secrets), `p.Env` and `p.DB`. Every call fails closed with the missing capability name; `Provide` of an undeclared service fails.
- **Contracts on `sdk.Module`.** `SetContract`, `Grants`, `Granted`; a module can resolve only the secrets it declares or that the app writes as `secret://` in its config.
- **Build-time analyzer (`internal/modulecheck`)** and `kiln module list | check [dir] | manifest [dir]`. `check` refuses stdlib access that bypasses the broker (`os`, `os/exec`, `net`, `syscall`, `database/sql` except `db_sql`, `unsafe`, `io/ioutil`, `embed`), undeclared capabilities and services, descriptor mistakes, and `init` functions. `manifest` emits modules, requires, provides, capabilities and the capabilities each module's code uses.
- `examples/boxprobe` and `internal/boxtest`: application modules compiled into a box program, the way `WithRunIn` runs them.
- `sdk.SetEgressBridge`: a box routes broker HTTP through the host, so placeholders are swapped and egress is recorded even though modules now hold their own `p.HTTP` client.

### Removed

- `pluginpkg`, `plugins/`, `engine/symbols/`, `engine/perms.go`, `engine/check.go`, `engine/writeguard.go`, `internal/docgen`, `docs/gen`, and the `plugins.FS` embedded registry.
- The `kiln plugin`, `kiln serve --stdio` and `-plugins/-use store`-based CLI paths. `kiln` now runs the standard modules; application modules are compiled into your own program.

## v0.4.8 — 2026-11-04

### Security

- File plugins can now resolve only the secrets they declare with `var Secrets = []string{...}` or reference with a `secret://NAME` literal; a denied lookup is audited as `secret.access`. This narrows the default `secrets_env`/`secrets_vault` access for interpreted third-party plugins.
- Credential-bearing HTTP clients (model providers, web search, Vault, MCP) refuse redirects. Non-loopback plaintext endpoints are refused when a credential is configured unless the plugin sets `allow_insecure: true`.
- Literal credentials in config (model `api_key`, HMAC/webhook keys, Vault tokens, MCP headers) are registered for redaction, not only `secret://` values. `model_openai` `headers` values now resolve `secret://NAME`.
- Auxiliary model calls (`hook_summarize`, `hook_compact`, `hook_review`) now run through the model middleware chain, so policy allowlists, token budgets, audit/trace and PII/secret redaction apply; their inputs are redacted.
- `threads_file`, `threads_sql`, `memory_file` and `memory_sql` redact registered secrets from persisted messages, tool arguments, pending interrupts and values.
- `WithRunIn`: host `host.llm`/`host.tool` calls are tied to a host-recorded run identity and cannot use the box-supplied user/roles; host tool calls run through host policy/limits/audit; `RunIn.BridgedTools` restricts the bridge. Host per-run state is now cleaned up.
- `WithRunIn`: the host broker blocks loopback/RFC1918/link-local/metadata egress by default (`RunIn.AllowPrivateEgress` opts in). `RunIn.Provider == "local"` is refused unless `RunIn.AllowUnsafeLocal` is set.
- `tool_skills` refuses symlinks that leave the skill folder. `tool_fs` `find_files`/`grep` walk the sandbox file API instead of running a shell, so `sandbox_local` confinement applies.
- `hook_guardrails` checks output even when tool calls are present; `hook_guard` checks nested arguments; `hook_approval` redacts interrupt messages.
- `policy` `require_sandbox` now also denies write/execute tools that do not declare an isolated `sandbox`; subagents inherit the parent budget and limits.
- `trigger_webhook` validates the resulting thread id and rejects route paths that could panic.
- Interpreted `exec` plugins now get a safe `*sdk.Command` instead of `*exec.Cmd`: the child environment is filtered and the `Env` field cannot be reset to nil to inherit agent credentials.
- `fs:read` refuses `/proc`, `/sys` and `/dev` through `os.Open`/`ReadFile`/`ReadDir`/`Stat`/`DirFS` and the `io/ioutil` read helpers; `filepath.Walk` and `net/http` file-serving helpers are not exposed to interpreted plugins.
- Interpreted plugins can no longer reach the mutable `sdk.PluginRenames` global.
- `writeTarget` refuses `/` as an ancestor of protected paths, closing `RemoveAll("/")`.
- Plugin packages reject symlinked files and oversized assets; lint and runtime now share secret-reference extraction.
- `os.DirFS` is per-Open guarded; `os.Stdout`/`os.Stderr` are not exposed and `kiln serve` redirects plugin prints to stderr.
- `plugin.json` supports `"dynamic": true` for plugins such as MCP adapters whose tool/service names are decided at runtime.
- Audit: `VerifyKeyedWithAnchor` and `kiln audit verify -anchor` verify against an off-host head, detecting a log+local-head rollback by a host-level attacker.
- Redaction uses a trie-backed `strings.Replacer` rebuilt only when the set changes, so `Redact` is linear in the input rather than in the number of registered forms.
- Firecracker `network: none` now fails closed if it cannot install the INPUT/FORWARD iptables rules, and removes them on teardown.

## v0.4.7 — 2026-11-03

### Added

- `examples/tensorlake`: the agent runs inside a real Tensorlake cloud sandbox with `WithRunIn`. The host keeps the model provider, secrets and broker; Tensorlake runs the microVM. The example exercises the same placeholder, `host.http` swap, redaction and `sandbox.egress` flow, and cleans the sandbox up on exit.

## v0.4.6 — 2026-11-03

### Added

- `examples/firecracker` can run the host process and Firecracker on different machines with `FC_HOST` (for example, the host on macOS and Firecracker in Lima), so the host-to-sandbox link crosses a real machine boundary instead of sharing one host.
- The sandbox docs add an SVG sequence diagram for the host/sandbox boundary: VM boot, SSH/stdio, `host.llm`, `host.secret`, `host.http`, placeholder swap, redaction, and teardown.

## v0.4.5 — 2026-11-03

### Added

- `examples/firecracker`: a complete self-hosted [Firecracker](https://firecracker-microvm.github.io/) sandbox provider. It creates a tap device, boots a microVM through the Firecracker API, connects to it over SSH, and runs the agent with `WithRunIn`. A setup script fetches the kernel, rootfs, SSH key and Firecracker binary, and the README shows how to run it inside Lima on Apple Silicon. The example keeps model and secret values on the host and shows the same placeholder, host-swap, redaction and `sandbox.egress` guarantees as the local microsandbox example.
- The microsandbox provider and example document how to run on microsandbox cloud: set `MSB_BACKEND=cloud` and `MSB_API_KEY` (or `MSB_PROFILE`) in the host environment. The same `msb` provider is used.
- The sandbox docs make the hosted-service boundary explicit: `examples/firecracker` shows the same `host.secret`, `host.http`, placeholder, redaction and `sandbox.egress` flow, and the README and docs map it to a cloud-hosted provider where only the provider plugin changes.

### Fixed

- `WithRunIn` keeps compiled plugins on the host instead of trying to load them inside the box, where compiled source is unavailable. This makes a compiled sandbox provider usable with `WithRunIn`.

## v0.4.4 — 2026-11-03

### Changed

- **Behavior change:** `trigger_webhook` now requires a timestamp by default. Senders sign `METHOD\nPATH\n<timestamp>\n<body>` and send the timestamp in `X-Signature-Timestamp` (override with `timestamp_header`, or set it to `""` together with `allow_replay_after_window: true` for senders that cannot send one). Binding the signature to the method and path also stops a delivery for one route being replayed against another that shares the secret. The trigger template and `hello_trigger` example use the same format and require a secret of at least 32 bytes.

### Security

- `audit_http` and `trace_otlp` refuse redirects, and refuse a plaintext endpoint when secret headers are configured (loopback is allowed). `audit_http` drops a permanently rejected batch instead of blocking every later record behind it. `tool_mcp` no longer holds the request mutex while writing to a stdio server.

## v0.4.3 — 2026-11-03

### Fixed

- `WithRunIn`: when the agent in the sandbox exits, the run waiting on it fails with an error saying so (not `rpc: connection closed`), and the next run starts a new sandbox. Threads kept in the old sandbox are gone with it unless their store is outside the box.
- `kiln serve --stdio` keeps stdin and stdout for the host's messages only: a plugin that prints goes to the host's `[box]` log, and one that reads stdin gets EOF. The host also reads a message that has stray output in front of it on the same line, so a print without a newline no longer leaves a call waiting.

## v0.4.2 — 2026-11-03

### Added

- `hook_compact`: keeps runs inside the model's context window. It estimates the request from the tokens the provider reported, and past `max_fraction` (or `max_tokens` / `max_messages`) applies `clamp`, `clear_tool_results`, `sliding_window` and `summarize`, cheapest first, down to `target_fraction`. Optional `compact_context` tool; a `context_usage` event each step.
- `tool_search`: past `threshold` tools, the model sees a `search_tools` tool and the tools it reveals.
- `tool_plan`: `plan_write`, `plan_update`, `plan_read`; the plan is shown with the newest tool result each step and kept per thread (in `memory` when loaded).
- `hook_guardrails`: rules on input, output, tool arguments and tool results, matched by regex, keywords or the `secrets` detector, with `block`, `redact` or `retry`.
- `hook_loop_detect` (refuses repeated identical calls, stops the run past `stop_after`) and `hook_tool_output_limit` (keeps the head and tail of long results).
- `hook_fallback` also falls back when a model refuses (`on_refusal`, default true). Providers report `FinishReason: "refusal"` (Anthropic `refusal`, OpenAI `content_filter`, Gemini safety stops).
- `sdk.Usage.CacheReadTokens` and `CacheWriteTokens`, filled by all three providers.
- `WithSaveEachStep()`, `Thread.Continue` and `Thread.Fork`.
- Package `evals` and `kiln eval`: grade an agent on a dataset (contains, regex, tools, step budget, judge model).
- Every built-in plugin's docs show an example; a test checks each one parses and uses only config keys the plugin reads.
- `kiln plugin validate` warns (rule `interpreter`) about two Yaegi quirks: `x, _ := v.(T)` in a loop body, and several conditions in one `case` of a switch with no tag.

### Changed

- **Behavior change:** Anthropic's `Usage.InputTokens` now includes cached input tokens, like the other providers.

### Fixed

- Several plugins read config with `x, _ := v.(T)` in a loop, which under Yaegi keeps the previous item's value when an item lacks the key: `tool_subagents` (an agent without `mode` or `name` took the previous agent's), `tool_memory` (a note without text showed the previous note's), `trigger_webhook` (nested field lookups), `audit_http` and `trace_otlp` (headers), `tool_mcp` (env), `hook_pii` (patterns) and `model_mock`.
- `sandbox_microsandbox`: microVM sandboxes through the [microsandbox](https://microsandbox.dev) CLI (`msb`), with no network by default and allowlists as msb network rules. It works for `WithRunIn`, `WithSandbox` and `tool_sandbox`.
- `examples/microsandbox`: the agent in a microVM, showing that the VM holds only a placeholder and the real token is used on the host.
- `RunIn.SecretHosts` and `RunIn.Egress` rules take optional methods and a path: `GET api.github.com/repos/*`, `GET|POST api.acme.com/v1/orders`. A malformed rule fails `New`.
- Every request from a `WithRunIn` sandbox through the host is audited as `sandbox.egress` (decision, method, host, path, status, ms, secret names) and logged on the host.

### Security

- `WithRunIn`: secrets never enter the sandbox. `secrets_*` providers stay on the host. The box's `secrets` service returns placeholders (`eipsec_...`), and the box's HTTP is made by the host, which swaps a placeholder for its value only on requests that match that secret's `RunIn.SecretHosts` rules. Responses come back redacted. Requests without secrets may reach only `RunIn.Egress`, which is empty by default.
- Sandbox env and mounts are checked by the core (`sdk.CheckSandboxSpec`, `sdk.CheckSandboxEnv`): resolved secret values, credential-looking names without a placeholder, and mounts of `/` or a container runtime socket are refused.
- `kiln serve --stdio` refuses to start when its environment holds a credential.
- `WithRunIn`: the host answers a box's secret lookup only for a plugin the box was started with (its profile, the policy's `require`, and the plugins it loads on its own). The box names the asking plugin itself, so before this it could claim another plugin's grants. A refusal is audited as a denied `secret.access`.
- **Behavior change:** HTTP from plugins inside a `WithRunIn` box goes through the host and is refused unless the host is listed in `RunIn.Egress` or `RunIn.SecretHosts`.

## v0.4.0 — 2026-11-01

One vocabulary, in Go terms, and a documentation section that says what to use when. **Nothing breaks:** every old name below keeps working until v1.0 and logs a notice once. `kiln plugin fix` rewrites plugin files.

```bash
go get github.com/ganeshdipdumbare/kiln@v0.4.0
```

### Renamed (old names still accepted)

| Before | Now |
|---|---|
| `sdk.Context`, `kiln.PluginContext` | `sdk.Plugin`, `kiln.PluginAPI` (`func Apply(p *sdk.Plugin) error`) |
| `var Inject`, `kiln.Plugin{Inject}`, manifest `"inject"` | `var Requires`, `kiln.Plugin{Requires}`, manifest `"requires"` |
| `OnAgentStart`, `OnAgentEnd`, `OnStepPrepare`, `OnModelAfter`, `OnDispose` | `OnRunStart`, `OnRunEnd`, `BeforeStep`, `AfterModel`, `OnUnload` |
| services `llm/<name>`, `checkpointer`, `store`, `exec` | `model/<name>`, `threads`, `memory`, `workspace` |
| `llm_openai`, `llm_anthropic`, `llm_gemini`, `llm_mock` | `model_openai`, `model_anthropic`, `model_gemini`, `model_mock` |
| `mw_retry`, `mw_fallback`, `mw_limits`, `mw_summarize`, `mw_hitl`, `mw_auto_review`, `mw_pii`, `mw_tool_errors`, `mw_logger` | `hook_retry`, `hook_fallback`, `hook_limits`, `hook_summarize`, `hook_approval`, `hook_review`, `hook_pii`, `hook_tool_errors`, `hook_log` |
| `checkpoint_inmem`, `checkpoint_file`, `checkpoint_sql` | `threads_inmem`, `threads_file`, `threads_sql` |
| `store_inmem`, `store_file`, `store_sql` | `memory_inmem`, `memory_file`, `memory_sql` |
| `memory_store`, `agents`, `skills_dir`, `mcp_client`, `tool_http_fetch` | `tool_memory`, `tool_subagents`, `tool_skills`, `tool_mcp`, `tool_fetch` |
| `exec_local`, `exec_sandbox` | `workspace_local`, `workspace_sandbox` |
| `jobs`, `audit_jsonl`, `loop_default` | `trigger_schedule`, `audit_file`, `loop` |
| `kiln plugin init -template middleware / provider / store / webhook` | `hook / model / memory / trigger` |

Old names work in profiles, `WithPlugin`, policy lists (allow, deny, locked, grants), secret grants, `Requires` and `Service` lookups. Instance names don't change: a plugin loaded as `llm_openai` keeps that name (and its grants).

### Added

- **Options for common setups:** `WithPersistence("memory" | "file" | "postgres", …)`, `WithApprovals(tools…)`, `WithSandbox(provider)`, `WithAudit(path)`, `WithTracing(endpoint)`, `WithMCP(name, command | url, args…)`.
- **Plumbing loads itself:** a plugin waiting on `workspace`, `sandbox/local`, `db`, `threads`, `memory` or `loop` gets the default provider, shown as `auto` in `Status()`.
- **`kiln plugin init` asks what the plugin should do**; `validate` and `inspect` end with a one-line summary of what the code does; **`kiln plugin fix`** rewrites old names; lint warns about them (`old-name`).
- **Docs:** one Plugins section, made up of: a five-minute intro; "What to use when"; a page per kind (tools, hooks, model providers, threads and memory, sandboxes and workspaces, triggers, telemetry and audit, secrets, policy) with when to use it, every built-in and its config, how to write your own, and gotchas; permissions; and the plugin API. The built-in reference and template code are generated from the plugins (`go run ./docs/gen`), and a test fails if they drift.

## v0.3.0 — 2026-10-31

The first tagged release. Install it with:

```bash
go get github.com/ganeshdipdumbare/kiln@v0.3.0
```

### Breaking changes

- **Module path.** The module is now `github.com/ganeshdipdumbare/kiln` (it was `kiln`), so it can be fetched by version. Update Go imports, for example `"kiln"` → `"github.com/ganeshdipdumbare/kiln"` and `"kiln/sdk"` → `"github.com/ganeshdipdumbare/kiln/sdk"`. The package names don't change (`kiln.New`, `sdk.Tool` ...). **Plugin files don't change:** interpreted plugins keep `import "kiln/sdk"`, a stable alias the engine resolves whatever the module path is.
- **Tool and skill names are owned.** `AddTool` / `AddSkill` refuse a name another plugin already registered (logged; the first one keeps it). `Tool.Plugin` is always the registering plugin.
- **The `env` permission hides credential-looking variables** (`*_API_KEY`, `*_TOKEN`, `*PASSWORD*` ...). Secret providers declare the new `env:secrets` permission. Policies with `grants` must grant it to `secrets_env` / `secrets_vault`.
- **Tighter plugin permissions.** `fs:write` alone opens files write-only; `io/ioutil`'s read and write functions need `fs:read` / `fs:write`; `net/textproto` needs `net`; `mime/multipart` needs `fs:write`; `os.NewFile` and `os.Stdin` are gone.
- **Only built-in plugins can call `sdk.Redact*`**, and no interpreted plugin can call `sdk.AddRedaction`. Custom audit/trace sinks should be compiled plugins.
- **Plugin names** are limited to letters, digits, `_` and `-`. Only policy-required rows may replace a locked plugin.
- **Sandboxes handed to plugins** (`ctx.Service("sandbox/...")`) have no `Upload` / `Download`; the host application keeps them.
- **`secrets_vault`** refuses to start without `grants` or `allow`, and rejects secret names with path segments.
- **`trigger_webhook`** refuses replays (409), caps runs per route (429), and its default prompt fences the payload as untrusted data. Placeholders no longer print `<nil>` for missing fields.
- **`mcp_client`** only treats an MCP server's `readOnlyHint` as read-only for policy with `trust_annotations: true`.
- **`mw_pii`** hash tokens are keyed HMACs (stable across runs only with `hash_key`), 16 hex characters long.
- `sdk.AddRedaction` returns `bool` (false for values shorter than 4 bytes).

### Added

- Plugin packaging: `plugin.json` manifests, `kiln plugin init | validate | build | inspect | keygen`, signed reproducible `.kiln` packages (`pluginpkg`).
- A plugin store with an install lifecycle (VALIDATED → APPROVED → ACTIVE, FAILED, QUARANTINED; install, approve, enable, update, rollback, quarantine, uninstall), live through `kiln.WithPluginStore`.
- Secrets: caller identity bound by the core, redaction of resolved values (and their JSON / URL / base64 / hex forms) in logs, traces and audit, `secret.access` audit records, `secrets_vault`.
- Observability: `audit_http`, `trace_otlp`, standard audit fields (`run`, `plugin`, `provider`, `call_id`, `writer` ...), `RunState.ID`, optional keyed audit chain (`audit_jsonl` `key`, `audit.VerifyKeyed`).
- Examples: `secrets`, `observability`, `plugin-validate`, `plugin-store`, and a plugin per template in `examples/plugins`.

### Fixed

- Every finding of two deepsec security reviews, including a `sandbox_local` symlink escape, a `trigger_webhook` infinite loop, forgeable audit records, store enable not re-checking signatures, and an update left waiting on a dependency replacing the running version. See the git log for the full list.

## Before v0.3.0

Untagged development on `main` (module path `kiln`): the core engine and trust gate, the facade (`Generate`, `Stream`, `GenerateObject`, threads, human-in-the-loop), providers, the middleware pack, memory and Postgres, MCP, subagents, skills, sandboxes in both directions, policy and audit, jobs and webhooks, the CLI, the docs site and the full-stack demo.

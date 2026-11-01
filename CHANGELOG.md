# Changelog

All notable changes to eipagent. Versions follow [semantic versioning](https://semver.org); see [Versioning](README.md#versioning) for what that covers while the major version is 0.

## v0.4.0 — 2026-11-01

One vocabulary, in Go terms, and a documentation section that says what to use when. **Nothing breaks:** every old name below keeps working until v1.0 and logs a notice once. `eip plugin fix` rewrites plugin files.

```bash
go get github.com/ganeshdipdumbare/eipagent@v0.4.0
```

### Renamed (old names still accepted)

| Before | Now |
|---|---|
| `sdk.Context`, `eipagent.PluginContext` | `sdk.Plugin`, `eipagent.PluginAPI` (`func Apply(p *sdk.Plugin) error`) |
| `var Inject`, `eipagent.Plugin{Inject}`, manifest `"inject"` | `var Requires`, `eipagent.Plugin{Requires}`, manifest `"requires"` |
| `OnAgentStart`, `OnAgentEnd`, `OnStepPrepare`, `OnModelAfter`, `OnDispose` | `OnRunStart`, `OnRunEnd`, `BeforeStep`, `AfterModel`, `OnUnload` |
| services `llm/<name>`, `checkpointer`, `store`, `exec` | `model/<name>`, `threads`, `memory`, `workspace` |
| `llm_openai`, `llm_anthropic`, `llm_gemini`, `llm_mock` | `model_openai`, `model_anthropic`, `model_gemini`, `model_mock` |
| `mw_retry`, `mw_fallback`, `mw_limits`, `mw_summarize`, `mw_hitl`, `mw_auto_review`, `mw_pii`, `mw_tool_errors`, `mw_logger` | `hook_retry`, `hook_fallback`, `hook_limits`, `hook_summarize`, `hook_approval`, `hook_review`, `hook_pii`, `hook_tool_errors`, `hook_log` |
| `checkpoint_inmem`, `checkpoint_file`, `checkpoint_sql` | `threads_inmem`, `threads_file`, `threads_sql` |
| `store_inmem`, `store_file`, `store_sql` | `memory_inmem`, `memory_file`, `memory_sql` |
| `memory_store`, `agents`, `skills_dir`, `mcp_client`, `tool_http_fetch` | `tool_memory`, `tool_subagents`, `tool_skills`, `tool_mcp`, `tool_fetch` |
| `exec_local`, `exec_sandbox` | `workspace_local`, `workspace_sandbox` |
| `jobs`, `audit_jsonl`, `loop_default` | `trigger_schedule`, `audit_file`, `loop` |
| `eip plugin init -template middleware / provider / store / webhook` | `hook / model / memory / trigger` |

Old names work in profiles, `WithPlugin`, policy lists (allow, deny, locked, grants), secret grants, `Requires` and `Service` lookups. Instance names don't change: a plugin loaded as `llm_openai` keeps that name (and its grants).

### Added

- **Options for common setups:** `WithPersistence("memory" | "file" | "postgres", …)`, `WithApprovals(tools…)`, `WithSandbox(provider)`, `WithAudit(path)`, `WithTracing(endpoint)`, `WithMCP(name, command | url, args…)`.
- **Plumbing loads itself:** a plugin waiting on `workspace`, `sandbox/local`, `db`, `threads`, `memory` or `loop` gets the default provider, shown as `auto` in `Status()`.
- **`eip plugin init` asks what the plugin should do**; `validate` and `inspect` end with a one-line summary of what the code does; **`eip plugin fix`** rewrites old names; lint warns about them (`old-name`).
- **Docs:** one Plugins section, made up of: a five-minute intro; "What to use when"; a page per kind (tools, hooks, model providers, threads and memory, sandboxes and workspaces, triggers, telemetry and audit, secrets, policy) with when to use it, every built-in and its config, how to write your own, and gotchas; permissions; and the plugin API. The built-in reference and template code are generated from the plugins (`go run ./docs/gen`), and a test fails if they drift.

## v0.3.0 — 2026-10-31

The first tagged release. Install it with:

```bash
go get github.com/ganeshdipdumbare/eipagent@v0.3.0
```

### Breaking changes

- **Module path.** The module is now `github.com/ganeshdipdumbare/eipagent` (it was `eipagent`), so it can be fetched by version. Update Go imports, for example `"eipagent"` → `"github.com/ganeshdipdumbare/eipagent"` and `"eipagent/sdk"` → `"github.com/ganeshdipdumbare/eipagent/sdk"`. The package names don't change (`eipagent.New`, `sdk.Tool` ...). **Plugin files don't change:** interpreted plugins keep `import "eipagent/sdk"`, a stable alias the engine resolves whatever the module path is.
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

- Plugin packaging: `plugin.json` manifests, `eip plugin init | validate | build | inspect | keygen`, signed reproducible `.eip` packages (`pluginpkg`).
- A plugin store with an install lifecycle (VALIDATED → APPROVED → ACTIVE, FAILED, QUARANTINED; install, approve, enable, update, rollback, quarantine, uninstall), live through `eipagent.WithPluginStore`.
- Secrets: caller identity bound by the core, redaction of resolved values (and their JSON / URL / base64 / hex forms) in logs, traces and audit, `secret.access` audit records, `secrets_vault`.
- Observability: `audit_http`, `trace_otlp`, standard audit fields (`run`, `plugin`, `provider`, `call_id`, `writer` ...), `RunState.ID`, optional keyed audit chain (`audit_jsonl` `key`, `audit.VerifyKeyed`).
- Examples: `secrets`, `observability`, `plugin-validate`, `plugin-store`, and a plugin per template in `examples/plugins`.

### Fixed

- Every finding of two deepsec security reviews, including a `sandbox_local` symlink escape, a `trigger_webhook` infinite loop, forgeable audit records, store enable not re-checking signatures, and an update left waiting on a dependency replacing the running version. See the git log for the full list.

## Before v0.3.0

Untagged development on `main` (module path `eipagent`): the core engine and trust gate, the facade (`Generate`, `Stream`, `GenerateObject`, threads, human-in-the-loop), providers, the middleware pack, memory and Postgres, MCP, subagents, skills, sandboxes in both directions, policy and audit, jobs and webhooks, the CLI, the docs site and the full-stack demo.

# eipagent — the "everything is a plugin" agent SDK for Go

eipagent is an agent SDK for teams that need it simple to use and secure by default.

- **Everything is a plugin.** The core engine only loads plugins and routes calls between them. Everything else is a plugin you can swap from a profile: the agent loop, every LLM provider, tools, memory, middleware, sandboxes, persistence, tracing, audit and the policy rules. The design follows DeepSeek Harness / Cordis.
- **Easy to use.** The API is shaped like the AI SDK: `Generate`, `Stream`, `GenerateObject[T]` and typed `Tool(...)`.
- **Feature rich.** It covers what LangChain offers: multiple providers, middleware, memory, human-in-the-loop, MCP, multi-agent and skills.
- **Secure by default.** Plugins are pinned and scoped to the capabilities they declare. A locked policy layer runs first. The audit log is tamper-evident. Secrets are granted per plugin. Sandboxes are pluggable, and the agent can run inside one.

```go
agent, _ := eipagent.New(
    eipagent.WithModel("anthropic:claude-sonnet-5-5"),
    eipagent.WithTools(eipagent.Tool("weather", "Get weather", func(ctx context.Context, in struct {
        City string `json:"city"`
    }) (string, error) { return "18°C in " + in.City, nil })),
)
defer agent.Close()

res, _ := agent.Generate(ctx, "Weather in Lisbon?")              // res.Text, res.Steps, res.Usage
for d := range agent.Stream(ctx, "A haiku").Text() { fmt.Print(d) } // streaming
trip, _ := eipagent.GenerateObject[Trip](ctx, agent, "Pack for Lisbon") // typed structured output
th := agent.Thread("ticket-42"); th.Generate(ctx, "hi")             // durable threads
```

Requires Go 1.27. Run `go test ./...` (offline, using the mock model), then try `go run ./examples/quickstart` with `EIPAGENT_MODEL=mock:demo`.

## Architecture

```
            your code / eip CLI
                   │  Generate · Stream · GenerateObject · Thread · Resume
       ┌───────────▼────────────┐
       │  facade (package eipagent) — thin client of the "loop" service
       ├────────────────────────┤
       │  CORE ENGINE (not pluggable, stateless, no DB)
       │  sdk.Context scopes · services · effects · dispose
       │  hooks: emit / serial / onion, priority-ordered, per-run snapshot
       │  plugin runtime: Yaegi, lifecycle PENDING→ACTIVE, Inject reconcile, hot reload
       │  trust gate: sha256 pins · capability-scoped imports · admin policy · locked plugins
       ├────────────────────────┤
       │  PLUGINS (interpreted .go files or compiled Go)
       │  loop · llm/* · mw_* · checkpoint_* · store_* · tool_* · mcp · agents
       │  skills · sandbox/* · exec · jobs · webhooks · trace · audit · policy · secrets
       └────────────────────────┘
```

A plugin is one Go file containing `func Apply(ctx *sdk.Context) error`. Whatever the file registers (tools, services, hooks, goroutines) is undone when the plugin unloads. A plugin declares the services it needs with `var Inject` and stays PENDING until they exist. It declares the capabilities it needs with `var Permissions`; it can import only the stdlib packages those permissions allow. For details see [PLUGINS.md](PLUGINS.md).

## Standard plugins (embedded in the binary)

| Area | Plugins |
|---|---|
| Loop | `loop_default` (steps, parallel tools, stop conditions, interrupts, resume) |
| Models | `llm_openai` (OpenAI, DeepSeek, Ollama, vLLM, OpenRouter, Groq), `llm_anthropic`, `llm_gemini`, `llm_mock` |
| Middleware | `mw_retry`, `mw_fallback`, `mw_limits`, `mw_summarize`, `mw_hitl`, `mw_auto_review`, `mw_pii`, `mw_tool_errors`, `mw_logger` |
| Memory | `checkpoint_inmem`, `checkpoint_file`, `checkpoint_sql`; `store_inmem`, `store_file`, `store_sql`; `memory_store` (remember/recall); `db_sql` (Postgres via `import _ "eipagent/drivers/postgres"`) |
| Tools | `tool_shell` (bash), `tool_fs` (read/write/edit/list/find/grep with review diffs), `tool_http_fetch` (egress allowlist, SSRF-safe), `tool_time` |
| Sandboxes | `sandbox_local` (no isolation, dev), `sandbox_docker` (docker/podman, no network by default); `exec_local` / `exec_sandbox` (where shell and fs tools run); `tool_sandbox` (the model starts boxes on demand) |
| Integrations | `mcp_client` (stdio and streamable HTTP), `agents` (subagents as tools, handoffs), `skills_dir` (SKILL.md, progressive disclosure) |
| Ops | `jobs` (scheduled runs), `trigger_webhook` (HMAC-verified), `trace_file` plus `eip trace view` |
| Security | `policy`, `audit_jsonl` (hash chain, `audit.Verify`), `secrets_env` (per-plugin grants) |

Model strings are written as `provider:model`. When no profile names a provider plugin, the right one is loaded from the prefix: `openai`, `deepseek`, `anthropic`, `gemini`, `ollama`, `openrouter`, `groq` or `mock`.

## Sandboxes: both directions

**The agent starts sandboxes.** Load a provider (`sandbox_docker`, or your own `sandbox/<name>`), then `exec_sandbox`. From then on, `tool_shell` and `tool_fs` run in a per-thread box. You can also load `tool_sandbox` so the model can create and destroy boxes itself.

**The agent runs inside a sandbox.**

```go
eipagent.New(eipagent.WithModel("openai:gpt-4.1"), eipagent.WithPlugins("tool_shell", ...),
    eipagent.WithRunIn(eipagent.RunIn{Provider: "docker", Binary: "./eip-linux-amd64"}))
```

The host creates the box, uploads the `eip` binary with the profile, policy and plugin sources, and starts `eip serve --stdio`. It then drives the box over JSON-RPC, so the API is unchanged. Model calls are bridged back to the host, which means API keys never enter the box and host middleware and policy still apply. Host-only Go tools are bridged as well. For a container provider, build the binary for the box with `GOOS=linux go build ./cmd/eip`.

Custom backends such as Firecracker, E2B, Kubernetes or microsandbox only need to provide a `sdk.SandboxProvider`; see `examples/custom-sandbox`. microsandbox has no local server API (it is an embedded SDK/CLI plus a cloud REST API), so no adapter ships for it yet.

## Persistence and scaling

The core is stateless. All state lives behind plugin services: threads in `checkpointer`, memory in `store`, and the audit log in `audit`. With `db_sql` + `checkpoint_sql` + `store_sql` on Postgres, any replica can serve any thread. A HITL pause on one pod can be resumed on another, and concurrent writes to the same thread return `ErrThreadBusy` instead of clobbering each other.

## CLI

```bash
go run ./cmd/eip -model openai:gpt-4.1 -use tool_time
```

This starts the REPL with `/plugins`, `/load`, `/unload` and `/reload`. Approvals are prompted inline.

```bash
go run ./cmd/eip run -model mock:x "hello"
```

Runs a single prompt headless (add `-json` or `-thread`).

```bash
go run ./cmd/eip plugin pin -plugins ./plugins -lock eip.lock
```

Pins plugin sources for the secure preset.

```bash
go run ./cmd/eip trace view .eipagent/trace.jsonl > trace.html
```

Renders a trace as an HTML timeline.

## Status versus the plan

These are done and covered by tests:

- the core engine and its trust gate
- the facade
- all four providers
- streaming and structured output
- the middleware pack
- threads and checkpointers (in-memory, file and Postgres)
- HITL with resume, including across processes
- MCP
- subagents and handoffs
- skills
- both sandbox modes (local and Docker providers)
- policy, audit and secret grants
- tracing and the viewer
- jobs and webhooks
- the CLI

Not built yet:

- agent teams (shared task board)
- creator mode (the agent writes plugins into quarantine)
- `eip plugin install` and bundles
- `secrets_vault` and `secrets_file`
- `audit_http` and `audit_sql`
- `trace_otlp`
- `jobs_sql`
- `eip serve --http`
- a microsandbox adapter

The Docker sandbox test skips when Docker isn't running, and the Postgres test runs only when `EIP_TEST_PG_DSN` is set.

# eipagent

**eipagent is an agent SDK for Go where everything is a plugin. It is built for companies and aims to be simple to use and secure by default.**

- **Everything is a plugin.** The core engine only loads plugins and routes calls between them. The agent loop, every LLM provider, tools, memory, middleware, sandboxes, persistence, tracing, audit and the policy rules are all plugins, and you can swap any of them. The model is [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) and its Cordis plugin framework.
- **Easy to use.** The API looks like the [AI SDK](https://ai-sdk.dev): `Generate`, `Stream`, `GenerateObject[T]` and typed `Tool(...)`.
- **Feature rich.** It covers what [LangChain](https://docs.langchain.com/oss/python/langchain/overview) offers: many providers, a middleware pack, durable memory, human-in-the-loop, MCP, multi-agent and skills.
- **Secure by default.**
  - Plugins are pinned by sha256 and limited to the capabilities they declare.
  - A locked policy layer always runs first.
  - The audit log is tamper-evident.
  - Each plugin gets only the secrets granted to it.
  - Sandboxes are pluggable, and the agent can run inside one.

**Demo:** [`examples/fullstack`](examples/fullstack) is a support desk with a Go backend and web UI: streaming, refunds that wait for a supervisor, redaction and an audit trail. Run it with `go run ./examples/fullstack`, or try it live at https://eipagent-docs.vercel.app/demo/ (the backend runs on a free instance, so the first visit can take a minute to wake it).

**Docs:** https://eipagent-docs.vercel.app has the full guide, with examples, lifecycles and reference. The source is `docs/site.html`; run `docs/build.sh` to regenerate `docs/public/index.html`, then deploy with `vercel deploy --prod` from `docs/public`.

Requires **Go 1.27**. The module is private for now, so use it from a checkout of this repo, or with `replace eipagent => /path/to/eipagent` in your `go.mod`.

```bash
go test ./...                    # offline: the mock model needs no keys
go run ./examples/readme         # runs every example in this README
```

## Contents

1. [Examples](#examples)
2. [How it works: lifecycles](#how-it-works-lifecycles)
3. [Plugins](#plugins)
4. [Sandboxes](#sandboxes)
5. [Security](#security)
6. [Scaling and persistence](#scaling-and-persistence)
7. [CLI](#cli)
8. [Status](#status)

---

## Examples

Every snippet below comes from [`examples/readme/main.go`](examples/readme/main.go). `go test` builds and runs that file with the offline mock model, so the snippets always match the API. To use a real model, replace `"mock:demo"` with a model string such as `anthropic:claude-sonnet-5-5`, `openai:gpt-4.1`, `gemini:gemini-2.5-pro`, `deepseek:deepseek-chat` or `ollama:llama3`. Set the provider's API key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY` and so on) in the environment.

**Run everything locally with [Ollama](https://ollama.com).** No API keys are needed and nothing leaves your machine:

```bash
ollama pull lfm3
go run ./examples/ollama -demo text      # also: stream, tools, plugins, object, thread, approve, shell, vision
```

The model string is just `ollama:<model>` (for example `ollama:qwen3.5:4b`). Set `OLLAMA_HOST` to use an Ollama running on another machine. See [`examples/ollama`](examples/ollama/main.go).

**Production features, each as a runnable program** (offline, no keys needed):

| Example | Shows |
|---|---|
| [`examples/secrets`](examples/secrets/main.go) | `secrets_vault` against an in-process Vault: per-plugin grants, a plugin that can't ask as another, key rotation without a restart, a leaked key redacted from logs and audit, `secret.access` records |
| [`examples/observability`](examples/observability/main.go) | `audit_http` to an in-process SIEM that checks HMAC signatures and the hash chain; `trace_otlp` to an in-process OpenTelemetry collector, printing the span tree and the shared fields (`run`, `thread`, `user`, `plugin`, `provider`) |
| [`examples/plugin-validate`](examples/plugin-validate/main.go) | the linter's report on a plugin with mistakes, then a signed package, and what verification refuses: a tampered file, a manifest that grants itself `exec`, an unknown signer, an unsigned package, a wrong pin |
| [`examples/plugin-store`](examples/plugin-store/main.go) | one plugin through install, approval, enable, update, a failed update that keeps the old version, rollback and quarantine, in a running agent |
| [`examples/plugins`](examples/plugins) | a ready-made plugin for every `eip plugin init` template: tool, middleware, provider, store, MCP adapter, sandbox, webhook |

### Generate with a typed tool

```go
type WeatherIn struct {
	City string `json:"city" desc:"city name"`
}

weather := eipagent.Tool("weather", "Current weather for a city",
	func(ctx context.Context, in WeatherIn) (string, error) {
		return "18°C and light rain in " + in.City, nil
	})

agent, err := eipagent.New(
	eipagent.WithModel("anthropic:claude-sonnet-5-5"),
	eipagent.WithInstructions("You are a concise travel assistant."),
	eipagent.WithTools(weather),
)
defer agent.Close()

res, err := agent.Generate(ctx, "What's the weather in Lisbon?")
res.Text                 // final answer
res.Steps                // every model call with its tool results
res.ToolCalls()          // all tool calls in the run
res.Usage.TotalTokens    // token accounting
res.FinishReason         // stop | max_steps | interrupted | tool:<name>
```

The JSON Schema for the tool's arguments is derived from `WeatherIn`. The `json`, `desc` and `enum:"a,b"` tags are honoured; a field marked `omitempty` or declared as a pointer is optional. A tool can return any type: strings go to the model as they are, and everything else is sent as JSON.

### Streaming

```go
s := agent.Stream(ctx, "A haiku about Lisbon")
for delta := range s.Text() {
	fmt.Print(delta)
}
res, err := s.Result()

// Or every event: text, reasoning, tool_call, tool_result, step_start,
// step_end, interrupt, finish, error.
for e := range agent.Stream(ctx, "...").Events() {
	if e.Type == "tool_call" {
		fmt.Println("calling", e.ToolCall.Name)
	}
}
```

Breaking out of a loop early is safe: the run still completes, and `Result()` returns it.

### Structured output

```go
type Trip struct {
	City    string   `json:"city"`
	Packing []string `json:"packing" desc:"things to pack"`
	Budget  int      `json:"budget_eur,omitempty"`
}

trip, err := eipagent.GenerateObject[Trip](ctx, agent, "Plan what to pack for Lisbon.")
```

How the schema reaches the model depends on the provider:

- OpenAI and Gemini use their native JSON-schema mode.
- Anthropic gets the schema as a forced tool.
- DeepSeek and Groq use JSON mode, with the schema in the prompt.

If the reply doesn't parse, eipagent asks the model once to repair it and then returns `ErrNoObject`.

### Threads (durable conversations)

```go
agent, err := eipagent.New(
	eipagent.WithModel("openai:gpt-4.1"),
	eipagent.WithPlugin("checkpoint_file", map[string]any{"dir": ".eipagent/threads"}), // or checkpoint_sql
)
th := agent.Thread("customer-42")
th.Generate(ctx, "My name is Ada.")
th.Generate(ctx, "What's my name?") // sees the earlier turn
msgs, _ := th.Messages(ctx)
```

Without a checkpointer plugin, threads are kept in memory (`checkpoint_inmem`).

### Human in the loop

```go
agent, _ := eipagent.New(
	eipagent.WithModel("openai:gpt-4.1"),
	eipagent.WithTools(refund),
	eipagent.WithPlugin("mw_hitl", map[string]any{"interrupt_on": map[string]any{"refund": true}}),
)
th := agent.Thread("ticket-981")
res, _ := th.Generate(ctx, "Refund order A-17")
for res.Interrupted() { // paused before the tool ran; the pause is saved
	in := res.Interrupts[0]
	fmt.Println("approve?", in.Message, in.ToolCall.Args)
	res, _ = th.Resume(ctx, eipagent.Approve(in.ID)) // or Reject(id, why) / Edit(id, args) / Respond(id, text)
}
```

The pause lives in the checkpointer. With `checkpoint_sql`, a different process or pod can resume it, possibly hours later. Two other ways to answer interrupts:

- **Without a thread:** call `agent.Resume(ctx, res, decisions)`. The state travels in `res`.
- **In-process, synchronously:** pass `eipagent.WithApprover(func(ctx, in) (*Decision, error))`, for example to prompt in a CLI.

To pause on everything risky instead of naming tools, use `{"non_readonly": true}` or `{"risk": ["high"]}`. You can also put `mw_auto_review` in front: a reviewer model then approves, denies or escalates each call to a human.

### Middleware from the standard plugins

```go
agent, _ := eipagent.New(
	eipagent.WithModel("anthropic:claude-sonnet-5-5"),
	eipagent.WithPlugin("mw_retry", map[string]any{"max_retries": 3}),                         // 429/5xx with backoff
	eipagent.WithPlugin("mw_fallback", map[string]any{"models": []any{"openai:gpt-4.1"}}),     // next model on failure
	eipagent.WithPlugin("mw_limits", map[string]any{"model_calls_run": 20, "tool_calls_run": 50}),
	eipagent.WithPlugin("mw_pii", map[string]any{"types": []any{"email", "credit_card"}}),     // redact, mask, hash or block
	eipagent.WithPlugin("mw_summarize", map[string]any{"trigger_messages": 40, "keep_messages": 10}),
)
```

### Your own middleware

A compiled plugin is ordinary Go code. An interpreted plugin is the same code in a `.go` file loaded at runtime (see [Plugins](#plugins)).

```go
timing := eipagent.Plugin{Name: "timing", Apply: func(ctx *eipagent.PluginContext) error {
	ctx.WrapModel(func(c context.Context, req *eipagent.ModelRequest, next eipagent.ModelHandler) (*eipagent.ModelResponse, error) {
		resp, err := next(c, req) // the model call, plus any inner middleware
		if err == nil {
			ctx.Logf("%s step %d used %d tokens", req.Model, req.Step, resp.Usage.TotalTokens)
		}
		return resp, err
	})
	ctx.WrapTool(func(c context.Context, req *eipagent.ToolRequest, next eipagent.ToolHandler) (*eipagent.ToolResult, error) {
		if req.Call.Name == "drop_database" {
			return &eipagent.ToolResult{Content: "not allowed", IsError: true}, nil
		}
		return next(c, req)
	})
	return nil
}}
agent, _ := eipagent.New(eipagent.WithModel("openai:gpt-4.1"), eipagent.WithCompiledPlugin(timing))
```

### Add, update and remove plugins at runtime

```go
eng := agent.Engine()
eng.Load(eipagent.PluginSpec{Use: "memory_store"}) // needs a "store", so it waits: PENDING
eng.Load(eipagent.PluginSpec{Use: "store_inmem"})  // the store arrives, memory_store becomes ACTIVE
eng.Load(eipagent.PluginSpec{Use: "mw_pii", Config: map[string]any{"strategy": "hash"}}) // load again = update config
eng.Reload("mw_pii")                               // recompile from source
eng.Unload("store_inmem")                          // memory_store goes back to PENDING and its tools disappear
for _, s := range agent.Status() { fmt.Println(s.String()) }
```

A run that is already in progress keeps the plugins it started with. If an update fails, the old version keeps running. See [Plugin lifecycle](#plugin-lifecycle).

To see it live with a local model, run `go run ./examples/ollama -demo plugins`. The demo starts with no tools, loads `tool_time`, writes a brand-new plugin file and loads it, edits that file's code and reloads it, shows a broken edit being refused while the old version keeps working, and then unloads everything. The model is asked a question after each step.

### Subagents and handoffs

```go
agent, _ := eipagent.New(
	eipagent.WithModel("openai:gpt-4.1"),
	eipagent.WithPlugins("tool_time"),
	eipagent.WithPlugin("agents", map[string]any{"agents": []any{
		map[string]any{"name": "researcher", "description": "Finds facts",
			"instructions": "Research carefully.", "tools": []any{"current_time"}},       // called as a tool
		map[string]any{"name": "billing", "description": "Invoices and refunds",
			"instructions": "You are billing support.", "mode": "handoff"},             // takes over the thread
	}}),
)

// A whole agent, with its own plugins, model and policy, can also be a tool:
eipagent.WithTools(expertAgent.AsTool("legal_expert", "Answers legal questions"))
```

### Shell and file tools in a sandbox

```go
agent, _ := eipagent.New(
	eipagent.WithModel("openai:gpt-4.1"),
	eipagent.WithPlugins("sandbox_docker"),                                     // or sandbox_local, or your own
	eipagent.WithPlugin("exec_sandbox", map[string]any{"provider": "docker"}),  // one box per thread, no network
	eipagent.WithPlugins("tool_shell", "tool_fs"),
)
res, _ := agent.Generate(ctx, "Write notes.md and count its lines")
for _, ch := range res.Changes { // review what the agent changed
	fmt.Print(ch.Diff())
}
```

### MCP servers, skills, the web

```go
eipagent.WithPlugin("mcp_client", map[string]any{"servers": []any{
	map[string]any{"name": "github", "command": "github-mcp-server", "args": []any{"stdio"},
		"env": map[string]any{"GITHUB_TOKEN": "secret://GITHUB_TOKEN"}},
	map[string]any{"name": "docs", "url": "https://mcp.internal.example.com/mcp"},
}}) // tools appear as github__create_issue, docs__search, ...
eipagent.WithPlugin("skills_dir", map[string]any{"dirs": []any{"./skills"}})       // folders with SKILL.md
eipagent.WithPlugin("tool_http_fetch", map[string]any{"allow_hosts": []any{"*.wikipedia.org"}})
```

### Scheduled jobs and webhooks

```go
eipagent.WithPlugin("jobs", map[string]any{"jobs": []any{
	map[string]any{"name": "weekly-report", "schedule": "weekly fri 17:00", "prompt": "Summarize this week's tickets", "user": "ops"},
}}) // the model also gets schedule_create / schedule_list / schedule_cancel
eipagent.WithPlugin("trigger_webhook", map[string]any{"routes": []any{
	map[string]any{"path": "/github", "secret": "secret://GH_WEBHOOK_TOKEN", "thread": "gh-{repository.full_name}"},
}}) // requests must carry an HMAC-SHA256 signature
```

### Profiles: configure everything in a file

```json
{
  "model": "anthropic:claude-sonnet-5-5",
  "instructions": "You are the internal engineering assistant.",
  "max_steps": 30,
  "plugins": [
    { "use": "mw_retry" },
    { "use": "mw_hitl", "config": { "non_readonly": true } },
    { "use": "llm_openai", "as": "llm_local", "config": { "name": "local", "base_url": "http://vllm:8000/v1", "api_key": "" } },
    { "use": "checkpoint_sql" }
  ]
}
```

Load it with `eipagent.New(eipagent.WithProfile("eip.json"))`. Layers apply in order, and later ones win:

1. Profiles, in the order given.
2. Options passed in code.
3. The admin policy, which nothing can override.

[`examples/corporate/`](examples/corporate) has a complete corporate profile and policy.

---

## How it works: lifecycles

### Agent lifecycle

```mermaid
stateDiagram-v2
    [*] --> Configuring: New(opts...)
    Configuring --> Loading: preset, policy, profiles, lock resolved
    Loading --> Ready: every plugin ACTIVE or PENDING
    Loading --> [*]: any plugin refused or FAILED → New returns the error
    Ready --> Ready: Generate / Stream / Thread runs (concurrently)
    Ready --> Ready: Load / Reload / Unload plugins, hot reload
    Ready --> Closed: Close()
    Closed --> [*]
```

**`New`:**

1. **Resolve the configuration.** The preset (`secure` by default, or `dev`) or an explicit admin policy, the profiles, the code options and `eip.lock` combine into one plugin list.
2. **Load plugins in a fixed order:**
   1. Plugins the policy requires, such as audit, policy and secrets. They load first so they see everything after them.
   2. Built-ins: `agent_service`, `loop_default`, `secrets_env` and `checkpoint_inmem`.
   3. Your plugins.
   4. The provider implied by the model prefix, if no profile row provides it.

   A plugin of the same family replaces a default. For example, `checkpoint_sql` replaces `checkpoint_inmem`.
3. **Pass each plugin through the trust gate**, then start it (see the plugin lifecycle below).
4. **Fail fast.** If any plugin is refused or fails, everything already loaded is disposed and `New` returns the error. You never get a half-configured agent.
5. **Start the file watcher** if the policy allows hot reload.

**Ready:** an agent is safe for concurrent use. Each run takes a snapshot of the current plugins and keeps it. You can load, reload and unload plugins while runs are in progress.

**`Close`:** disposes every plugin in reverse load order, running each one's cleanups. The effects:

- Sandboxes are destroyed.
- Jobs and webhook runs are cancelled and awaited.
- MCP server processes are stopped.
- Database pools are closed.
- `Close` is idempotent: calling it twice is safe.

### Run lifecycle (one `Generate` / `Stream` / `Resume`)

```mermaid
sequenceDiagram
    participant App
    participant Runner as loop plugin
    participant Hooks as middleware & hooks
    participant LLM as llm provider
    participant Tool
    App->>Runner: Generate(prompt) (snapshot of plugins taken)
    Runner->>Hooks: agent/start  (may refuse the run)
    loop each step, up to max_steps
        Runner->>Hooks: step/prepare (rewrite model, messages, tools)
        Runner->>Hooks: model/wrap ⟶ retry ⟶ fallback ⟶ pii ⟶ …
        Hooks->>LLM: Generate (streaming chunks → events)
        Runner->>Hooks: model/after (inspect, may Jump: end | model)
        alt no tool calls
            Runner-->>App: finish "stop"
        else tool calls
            Runner->>Hooks: tool/wrap ⟶ policy ⟶ audit ⟶ review ⟶ hitl ⟶ …
            Hooks->>Tool: Run(args)
            Note over Runner: an interrupt pauses the run → finish "interrupted"
        end
    end
    Runner->>Hooks: agent/end (always runs, even on error)
    Runner-->>App: Result
```

Step by step:

1. **Start.**
   - The facade builds the run state: history, the new user message, model, instructions and the user's identity.
   - It takes a **snapshot** of the plugins, so a change made during the run won't show up until the next one.
   - It hands the run to the `loop` service, which is itself a plugin.
   - **`agent/start`** hooks run. An error from one refuses the run.
2. **Resume (only when resuming).** Pending tool calls run first, using the decisions you supplied:
   - `approve` runs the call.
   - `edit` runs it with your arguments.
   - `reject` and `respond` send your text to the model instead of running the tool.
3. **Each step**, up to `max_steps` (default 20):
   1. **Build the request.** The system prompt is the instructions plus the skill prompts. The tools are the registered tools plus any per-run tools.
   2. **`step/prepare`** hooks may rewrite the model, messages, tools or tool choice. Summarization and handoffs work this way.
   3. **`model/wrap`** middleware runs as an onion, highest priority outermost, around the provider call. Retry, fallback, PII redaction, limits, audit and tracing live here. Streaming chunks become `text` and `reasoning` events.
   4. **`model/after`** hooks inspect the reply. They can set `Jump`: `"end"` stops the run, `"model"` asks the model again.
   5. **No tool calls** means the run finishes with `stop`.
   6. **Tool calls:** each one runs through **`tool/wrap`**, as an onion with the highest priority outermost. Then the tool runs. Tools run one at a time, or in parallel with `parallel_tools`.
      - A plain error goes back to the model as the tool result, so it can recover.
      - `sdk.Fatal(err)` aborts the run.
      - A returned **interrupt** pauses the run. If an `approver` service is loaded, it can decide on the spot. Otherwise the run finishes `interrupted`, and the pending calls are kept in the run state, or in the thread when there is one.
   7. If a tool listed in `StopOnTool` was called, the run finishes with `tool:<name>`.
4. **Stop.** Reaching the step limit finishes the run with `max_steps`.
5. **End.** **`agent/end`** hooks always run exactly once, even when the run fails (`FinishReason: "error"`), so plugins can always clean up. Observers then get `finish`, or `error` when the run failed.

Default priorities (higher means outer). Set `priority` on a profile row to override a value; the reserved band is only for locked plugins.

| Priority | Model middleware (`model/wrap`) | Tool middleware (`tool/wrap`) |
|---|---|---|
| reserved band | `audit_jsonl`, `policy` (when locked) | `audit_jsonl`, `policy` (when locked) |
| 30 | `mw_pii` | `mw_pii` |
| 20 | `mw_fallback` | |
| 10 | `mw_retry` | `mw_retry` (`tool_retries`) |
| 0 | `mw_limits`, `mw_logger`, `trace_file`, your plugins (in load order) | `mw_auto_review`, `mw_limits`, `mw_logger`, `trace_file`, your plugins |
| −10 | | `mw_hitl` (innermost, so reviewers and policy decide first) |

Events in order: `step_start`, then the `text`/`reasoning` deltas, `step_end`, `tool_call`, and `tool_result` or `interrupt`, repeated for each step; then `finish` or `error`. `OnEvent` and `Stream.Events()` see the same events.

### Thread lifecycle

```mermaid
stateDiagram-v2
    [*] --> Empty
    Empty --> Running: Generate
    Idle --> Running: Generate
    Running --> Idle: saved (version n+1)
    Running --> Waiting: interrupt → saved with pending calls
    Waiting --> Running: Resume(decisions)
    Waiting --> Waiting: Generate refused until Resume
    Running --> Conflict: another writer saved first
    Conflict --> [*]: ErrThreadBusy (retry)
```

Each turn goes through four steps:

1. **Load** the state (messages, pending interrupts, plugin values such as the active handoff agent or the sandbox id) from the `checkpointer`.
2. **Run** the turn.
3. **Save** with optimistic versioning. If another process saved first, you get `ErrThreadBusy` and nothing is lost.
4. **Wait on pauses.** A thread with pending interrupts accepts only `Resume`.

Any process that shares the checkpointer can continue any thread.

### Plugin lifecycle

```mermaid
stateDiagram-v2
    [*] --> Refused: trust gate says no (not allowed, unpinned, hash mismatch, import not permitted)
    [*] --> PENDING: Load (compiled)
    PENDING --> LOADING: every Inject dependency exists
    LOADING --> ACTIVE: Apply returned nil
    LOADING --> FAILED: Apply errored or panicked (everything it registered is rolled back)
    ACTIVE --> PENDING: a dependency disappeared (disposed)
    ACTIVE --> ACTIVE: an optional dependency changed (restarted)
    ACTIVE --> [*]: Unload / Close (disposed)
    ACTIVE --> ACTIVE: Reload / Load with new config (new version replaces old, dependents restart)
```

- **Trust gate.** Before any plugin code runs, the core:
  - checks the policy allow/deny lists;
  - checks the sha256 pin (file plugins under the secure preset);
  - parses the source for `Inject`, `Permissions` and imports;
  - intersects the declared permissions with the policy's grants;
  - refuses any import those permissions don't cover;
  - then interprets the plugin with a symbol table restricted to exactly those packages.
- **Dispose.** Everything a plugin registered through its context is undone in reverse order: tools, services, hooks, skills and `Effect` cleanups.
- **Safe updates.** If a reload fails to compile or start, the old version keeps running.
- **Locked plugins** (from the policy) can't be unloaded or replaced at runtime, and only they may use the reserved, outermost priority band.

### Sandbox lifecycle (`exec_sandbox`)

1. **Created lazily**, the first time a shell or file tool runs in a thread. Each thread gets its own box.
2. **Reused** for every later turn of that thread.
3. **Remembered.** The box id is saved in the thread at the end of each run.
4. **Re-attached** when the thread resumes in a new process.
5. **Destroyed** after `idle_ttl_seconds`, or when the plugin unloads or the agent closes.

With `tool_sandbox`, the model creates and destroys boxes itself, up to `max_boxes` per thread.

### Remote agent lifecycle (`WithRunIn`)

1. `New` builds a small **host** agent. It holds the model provider and its keys, the sandbox provider, and your Go tools.
2. The host creates a box. It uploads the `eip` binary, the profile, the admin policy, `eip.lock` and the plugin sources.
3. The host starts `eip serve --stdio` in the box.
4. The box builds its agent and replies `ready`, or reports the startup error.
5. Runs and threads are proxied over JSON-RPC. Model calls come back to the host, through the host's middleware and policy, so keys never enter the box. Your Go tools are bridged back too.
6. `Close` stops the process, destroys the box and closes the host agent.

---

## Plugins

A plugin is one Go file, interpreted at runtime by [Yaegi](https://github.com/traefik/yaegi), or a compiled `eipagent.Plugin` with the same shape:

```go
//go:build ignore

package plugin

import (
	"context"

	"eipagent/sdk"
)

var Inject = []string{"store"}     // required services ("?x" optional, "llm/*" any match)
var Permissions = []string{"net"}  // net, exec, fs:read, fs:write, env, env:secrets, db

func Apply(ctx *sdk.Context) error {
	ctx.AddTool(sdk.Tool{
		Name: "greet", Description: "Greet someone",
		Schema: sdk.Object(map[string]any{"name": sdk.Str("who")}, "name"),
		Run: func(c context.Context, args map[string]any) (string, error) {
			return "Hello, " + sdk.ArgString(args, "name"), nil
		},
	})
	ctx.Effect(func() func() { /* start */ return func() { /* cleanup on unload */ } })
	return nil
}
```

The full guide, including the Yaegi pitfalls, is in [PLUGINS.md](PLUGINS.md). See also [`examples/custom-plugin`](examples/custom-plugin).

### Ship plugins: scaffold, validate, sign, install

```bash
eip plugin init -template tool weather           # tool, middleware, provider, store, mcp, sandbox, webhook
eip plugin validate weather                      # lint: imports vs permissions, services, secrets, manifest, compile
eip plugin build -key signing.key weather        # signed, hashed weather-0.1.0.eip
eip plugin install weather-0.1.0.eip             # verified and held (VALIDATED)
eip plugin approve weather 0.1.0                 # APPROVED
eip plugin enable weather                        # ACTIVE; update / rollback / quarantine / uninstall too
```

In Go, `eipagent.WithPluginStore(st)` attaches a `pluginpkg.Store` to a running agent, and enable, update and rollback swap plugins live. A version that fails to start never replaces the running one. Examples for every template are in [`examples/plugins`](examples/plugins), and [`examples/plugin-store`](examples/plugin-store) walks the whole lifecycle.

### Standard plugins (embedded in the binary)

| Area | Plugins |
|---|---|
| Loop | `loop_default` |
| Models | `llm_openai` (OpenAI, DeepSeek, Ollama, vLLM, OpenRouter, Groq), `llm_anthropic`, `llm_gemini`, `llm_mock` |
| Middleware | `mw_retry`, `mw_fallback`, `mw_limits`, `mw_summarize`, `mw_hitl`, `mw_auto_review`, `mw_pii`, `mw_tool_errors`, `mw_logger`, `hook_guard`, `hook_stats` |
| Memory | `checkpoint_inmem` / `_file` / `_sql`, `store_inmem` / `_file` / `_sql`, `memory_store`, `db_sql` |
| Tools | `tool_shell`, `tool_fs`, `tool_http_fetch`, `tool_time` |
| Sandboxes | `sandbox_local`, `sandbox_docker`, `exec_local`, `exec_sandbox`, `tool_sandbox` |
| Integrations | `mcp_client`, `agents`, `skills_dir` |
| Ops | `jobs`, `trigger_webhook`, `trace_file`, `trace_otlp` |
| Security | `policy`, `audit_jsonl`, `audit_http`, `secrets_env`, `secrets_vault` |

A model string `provider:model` loads the matching provider automatically. The prefixes are `openai`, `deepseek`, `anthropic`, `gemini`/`google`, `ollama`, `openrouter`, `groq` and `mock`.

## Sandboxes

There are two directions:

- **The agent uses sandboxes.** Load `exec_sandbox`, and the shell and file tools run in a per-thread box. Load `tool_sandbox`, and the model can start boxes on demand.
- **The agent runs inside a sandbox:**

  ```go
  eipagent.New(
  	eipagent.WithModel("openai:gpt-4.1"),
  	eipagent.WithPlugins("tool_shell", "tool_fs"),
  	eipagent.WithRunIn(eipagent.RunIn{Provider: "docker", Binary: "./eip-linux"}), // GOOS=linux go build -o eip-linux ./cmd/eip
  )
  ```

Providers: `sandbox_local` (no isolation, for development) and `sandbox_docker` (Docker or Podman, with no network by default). Any other backend, such as Firecracker, E2B, Kubernetes or microsandbox, only needs to provide `sdk.SandboxProvider`. See [`examples/custom-sandbox`](examples/custom-sandbox).

## Security

`eipagent.New()` starts in the **secure** preset:

- File plugins must be pinned in `eip.lock` (`eip plugin pin`).
- Hot reload is off.
- Plugins get only the permissions they declare, narrowed further by the admin policy.

An admin `policy.json` adds:

- locked `policy` and `audit_jsonl` plugins;
- the approval mode: read-only, workspace-write or full-access;
- tool and model allowlists, token budgets and per-role rules;
- per-plugin secret grants.

The threat model, and what isn't guaranteed, is in [SECURITY.md](SECURITY.md). Use `WithPreset("dev")` for local plugin development.

## Scaling and persistence

The core engine is stateless and needs no database. All state lives behind plugin services: `checkpointer`, `store`, `audit` and `jobs`. For multiple replicas:

```go
import _ "eipagent/drivers/postgres"

eipagent.WithPlugin("db_sql", map[string]any{"dsn": "secret://EIP_DATABASE_URL"})
eipagent.WithPlugins("checkpoint_sql", "store_sql")
```

With this setup:

- Any replica can serve any thread.
- A human approval paused on one pod can be resumed on another.
- Concurrent writes return `ErrThreadBusy` instead of overwriting each other.

## CLI

```bash
go run ./cmd/eip -model openai:gpt-4.1 -use tool_time
```

Opens the REPL. It has `/plugins`, `/load`, `/unload` and `/reload`, and asks inline when a tool needs approval.

```bash
go run ./cmd/eip run -model mock:x "hello"
```

Runs one prompt headless. Add `-json` or `-thread ID`.

```bash
go run ./cmd/eip plugin pin -plugins ./plugins -lock eip.lock
```

Pins plugin sources for the secure preset. `eip plugin help` lists the authoring commands (`init`, `validate`, `build`, `inspect`, `keygen`) and the store lifecycle (`install`, `approve`, `enable`, `disable`, `update`, `rollback`, `quarantine`, `uninstall`, `list`). Add `-store DIR` to `run`, `repl` or `serve` to load a store's enabled plugins.

```bash
go run ./cmd/eip trace view .eipagent/trace.jsonl > trace.html
```

Renders a trace as an HTML timeline.

```bash
go run ./cmd/eip config dump -profile eip.json
```

Shows the policy and every plugin's state.

`go run ./cmd/eip serve --stdio` is what runs inside sandboxes. You don't start it yourself.

## Status

**Done, with tests:**

- the core engine and trust gate
- the facade
- four providers
- streaming and structured output
- the middleware pack
- threads (in-memory, file and Postgres)
- human-in-the-loop, including across processes
- MCP (stdio and HTTP)
- subagents and handoffs
- skills
- both sandbox directions (local and Docker)
- policy, audit and secrets
- tracing and the viewer
- jobs and webhooks
- the CLI
- the lifecycle guarantees described above
- plugin manifests, linting, signed packages, and the install / approve / enable / update / rollback / quarantine lifecycle
- secret redaction, caller-bound secrets and `secret.access` auditing
- `secrets_vault`, `audit_http`, `trace_otlp`

**Not yet built:**

- agent teams (a shared task board)
- creator mode (the agent writes plugins into quarantine)
- bundles (several plugins in one package) and a remote registry source
- running a single untrusted plugin out of process, in its own sandbox
- a KMS-backed secrets provider (write one against `sdk.Secrets`)
- runtime plugin management inside a `WithRunIn` box
- `jobs_sql`
- `eip serve --http`
- a microsandbox adapter (microsandbox has no local server API)

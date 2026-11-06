# Security model

The goal is secure defaults that stay simple. `kiln.New()` with no options uses the **secure** preset.

## Layers

1. **Composition gate (core, not pluggable).** `kiln.New` composes before it runs, and `engine.Compose` runs no module code:
   - Every module is a compiled Go package; nothing is loaded, interpreted or fetched at runtime. There is no plugin directory, store, lock file or hot reload.
   - Names are validated, duplicate providers and dependency cycles are refused, and every required service must have an enabled provider (the error names the modules that would supply it).
   - The policy's allow/deny lists and capability grants are checked here. A module holds only the intersection of what it declares and what the policy grants.
   - `kiln module check` is the build-time gate: it refuses a module that imports `os`, `os/exec`, `net` (except pure helpers), `syscall` or `database/sql` instead of the broker, uses a capability it did not declare, provides an undeclared service, or ships a descriptor that does not match its directory.
   - The contract is enforced at run time too: `Provide` of an undeclared service fails, and every broker call fails closed with the missing capability name.
   - Compile-time inclusion is **not** a sandbox. A module linked into the binary runs with the process's privileges; the analyzer and the broker protect the intended boundary, not a malicious build.
2. **Admin policy (`policy.json`).**
   - It applies last and can't be overridden by profiles, chat or modules.
   - `locked` modules start first and get the reserved priority band; locked hooks also apply to subagents. A profile can't disable a locked module.
   - `require` adds mandatory modules, such as audit, policy and secrets; they start in the order the policy lists them.
   - `allow`/`deny` choose which modules may run; `grants` cap each module's capabilities.
3. **`policy` module.** Enforces the rules on every model and tool call: approval mode (`read-only`, `workspace-write`, `full-access`), tool allow/deny globs, model allowlist, sandbox requirement, token budgets and per-role overrides.
4. **Secrets.**
   - Config should reference `secret://NAME`, never raw values. Providers: `secrets_env` (allow-listed env vars) and `secrets_vault` (HashiCorp Vault KV v2, cached with a TTL so rotation is picked up, failing closed when Vault is unreachable; it refuses to start without `grants` or `allow`, and secret names can't contain path segments). Any other store (KMS, a cloud secret manager) is a module that provides `sdk.Secrets`. Literal credentials are still accepted for local/dev use, registered for redaction, and should be avoided in production.
   - `grants` limits each module to its own secrets. The caller identity is bound by the core: a module that looks up the `secrets` service gets a copy tied to its own name, so it can't ask as another module.
   - A module may resolve only the names it declares in `Module.Secrets`, or that the application writes as `secret://NAME` in that module's config. Any other lookup is denied and audited as `secret.access`.
   - Every resolved value is registered for redaction. `ctx.Logf`, `audit_file`, `audit_http`, `trace_file` and `trace_otlp` replace it with `[REDACTED]` wherever it appears: tool arguments, results, model output, errors.
   - Every lookup is audited as `secret.access` (module, secret name, granted or denied, never the value). Denials are always recorded. Granted reads are recorded once per module and secret every 10 minutes, and again right away when the value changes (a rotation), so modules that resolve secrets per call don't flood the log.
   - In `Config.RunIn` mode no secret enters the sandbox:
     - Model providers and `secrets_*` providers always stay on the host; model calls are bridged. The host records a run identity when `Generate`/`Thread.Generate` starts; `host.llm` and `host.tool` are checked against it rather than the identity the box reports, so a compromised box can't act as another user or role.
     - Host tool calls run through the host's own tool middleware (policy, limits, PII, audit) and need a known run id. `RunIn.BridgedTools` limits which Go tools the box may call at all; `NoBridgeTools` disables the bridge.
     - The box's `secrets` service returns placeholders (`eipsec_` + 32 hex), never values. Grants are checked on the host, and the placeholder is audited as `secret.access`.
     - The box's broker HTTP (`p.HTTP`) is carried by the host (see `sdk.SetEgressBridge`). The host swaps a placeholder for its value only when the request matches one of that secret's `RunIn.SecretHosts` rules: a host or host glob, optionally with methods and a path (`GET api.github.com/repos/*`). Otherwise it refuses. Requests without placeholders must match `RunIn.Egress` (none by default). Every request, allowed or refused, is recorded as `sandbox.egress` (method, host, path, status, duration, secret names, run) and logged on the host; load the audit module in `RunIn.HostPlugins` to keep those records on the host. Responses come back redacted, and redirects are not followed on the host, so every hop is checked again.
     - The host broker applies the same SSRF checks as `tool_fetch`: loopback, RFC1918, link-local, unspecified and cloud-metadata addresses are refused even if an egress rule names the host. `RunIn.AllowPrivateEgress` is the explicit exception.
     - `RunIn.Provider == "local"` is refused unless `AllowUnsafeLocal` is set. `sandbox_local` is not an isolation boundary: the box is a host process that can read host files (for example `/proc/<pid>/environ`, `$HOME/.aws` or a `.env` file). Use a container or microVM for untrusted agents.
     - Sandbox env and mounts are checked by the core, for `RunIn.Spec` and for every sandbox a module creates or runs a command in: a value holding a resolved secret, or a credential-looking name whose value isn't a placeholder, is refused, and so are mounts of `/`, a docker/podman/containerd socket or a directory holding one (checked after resolving symlinks), and image names starting with `-`. Modules can't mount host paths at all; only the host application sets mounts.
     - `ServeBox` refuses to start when the box's environment holds a credential-looking variable that isn't a placeholder.
5. **Audit and traces.** `audit_file` writes a hash-chained JSONL log covering:
   - module lifecycle: which modules started, in order, and any that failed (with the error)
   - secret access
   - every model call
   - every tool call (args, outcome, approval decision and who made it)

   Records carry standard fields so a control plane can join them: `run` (unique per run), `thread`, `user`, `module` (the module that owns the tool), `tool`, `call_id`, `step`, `provider`, `model`, `decision`, `decided_by`, `outcome`, `ms`, token counts.

   Every record has a `writer`: `core`, `audit_file`, `engine`, or `module:<name>` for records a module submits through the `audit` service. The SDK sets it, so a module can add records but can't pass them off as the core's.

   Use `audit.Verify` to detect edited or removed lines. With `key: secret://...` on `audit_file`, each line also carries an HMAC of its hash, and `audit.VerifyKeyed` rejects a log rewritten by someone without the key, and fails if the signed `.head` is missing. A host-level attacker can still roll back the log and its local `.head` together; copy the last head off-host (for example from the last `audit_http` batch or a control-plane checkpoint) and verify with `audit.VerifyKeyedWithAnchor` or `kiln audit verify -anchor head.json LOG`, which requires an anchor carrying its MAC and fails unless the log ends at that exact sequence/hash. `audit_http` ships the same chained, redacted records off-host as NDJSON batches, HMAC-signed, with retries (set `path: "-"` on `audit_file` to keep no local file). `trace_otlp` exports runs, model calls and tool calls as OpenTelemetry spans (OTLP/HTTP JSON, GenAI attribute names).
6. **Containment.**
   - `sandbox_docker`: no network, all capabilities dropped, no-new-privileges, pids limit.
   - `sandbox_local`: confines file paths and symlinks to its workdir and passes only allow-listed env vars. It is *not* isolation.
   - `tool_fetch`: requires a host allowlist and blocks private and loopback addresses at connect time (SSRF).
   - `tool_fs`'s `find_files` and `grep` walk the workspace through the sandbox file API instead of running `find`/`grep` in a shell, so `sandbox_local`'s path confinement applies to them too. `tool_shell` is arbitrary command execution by design; with `sandbox_local` it runs on the host.
   - `tool_skills` resolves symlinks and refuses a skill file whose real path leaves the skill folder.
   - `trigger_webhook`: requires HMAC signatures, refuses replays (and stale requests with `timestamp_header`), caps concurrent runs per route, validates the resulting thread id, and fences the payload off as untrusted data in its default prompt. The replay cache is per process; replicas need a shared cache or a short timestamp window.
   - `tool_mcp`: an MCP server's `readOnlyHint` only counts as read-only for policy when the server is configured with `trust_annotations: true`.
   - `hook_pii` hash strategy: keyed HMAC (`hash_key`), so tokens can't be reversed by hashing candidate emails or IPs.
7. **Humans and reviewers.** `hook_approval` pauses before chosen or risky tools and keeps the pause durable. `hook_review` has a reviewer model approve, deny or escalate.

## Not guaranteed

- **Compile-time inclusion is not a sandbox.** The analyzer and the broker enforce the contract the SDK documents; a module that bypasses them, or a malicious dependency in your build, still runs with the process's privileges. For genuinely untrusted code, run the whole agent in a sandbox with `Config.RunIn` and a container or microVM provider.
- `kiln module check` is a CI gate. A module that is not checked, or that is shipped already compiled, is not contained by it.
- `sandbox_local` has no isolation. `Config.RunIn` refuses it by default; `AllowUnsafeLocal` is for development only.
- Docker can't filter egress per host. Use `network: none`, or a provider that supports allowlists.
- The audit chain detects tampering but doesn't prevent deleting the whole file. Ship it off-host with `audit_http`.
- `Config.RunIn` routes broker HTTP through the host (placeholders are swapped only for allowed hosts). A module that constructs its own `http.Client` (instead of `p.HTTP`) bypasses that bridge inside a box; `kiln module check` refuses the usual ways to do it. Responses are buffered (32 MB cap), so streaming endpoints don't stream through the host.
- The box can call any host tool in `RunIn.BridgedTools` and can send any prompt to the host model provider; it cannot read host secrets directly, but a host tool that returns sensitive data sends it into the box.
- A run started inside the box (for example a scheduled job or webhook) has no host-issued identity. The host accepts its model calls as an unauthenticated run for policy/budget/audit purposes, but `host.tool` is unavailable to such runs.
- `trigger_webhook`'s replay cache is per process. With several replicas, a captured request can be replayed against a replica that did not see the first one; use a shared store or a short `max_age_seconds`.
- Writing a literal secret in module config is still a secret in a file. Prefer `secret://NAME`; literal credentials are redacted from logs/audit/traces/persistence but the config file remains readable.
- The host trusts the box to name the module that asks for a secret, so grants inside the box are only as strong as the box. A placeholder is still useless outside its `SecretHosts`.
- Sandboxes a module creates inside a `Config.RunIn` box are nested in it and get the same placeholders; they are not started on the host.
- Redaction applies to logs, traces, audit records and persisted threads/memory (the sinks). It does not hide a secret from the model or from your app: if a tool returns a key in its result, the model sees it.
- Redaction matches the values resolved through the `secrets` service and their JSON, URL, base64 and hex encodings. Values shorter than 4 bytes can't be redacted; their `secret.access` record says so. Other transformations (splitting, custom encodings) aren't caught.
- The audit chain without a key detects edits but not a full rewrite by someone with write access; use `key` or `audit_http`. The local keyed `.head` is not an anchor: an attacker who can rewrite the log can rewrite it too. Keep the last head off-host and verify with `audit.VerifyKeyedWithAnchor`.
- `sandbox_local` with a fixed `workdir` shares one folder between all threads (it logs a warning).
- The full-stack demo has no login: visitors pick Sam or Ada, so anyone can approve its (fake) refunds. It limits body size, requests per client (keyed on the address its own proxy saw, not on client-supplied headers) and runs in flight. A real app takes identity from its session.

Report vulnerabilities privately to the maintainers.

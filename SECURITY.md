# Security model

The goal is secure defaults that stay simple. `eipagent.New()` with no options uses the **secure** preset.

## Layers

1. **Trust gate (core, not pluggable).**
   - File plugins must match a sha256 pin in `eip.lock` (`eip plugin pin`). Embedded plugins ship inside the binary and are trusted like code.
   - Plugins installed through a plugin store (`eip plugin install`, `pluginpkg.Store`) are signed ed25519 packages verified against the store's trusted keys, held until a person approves them, re-verified at enable time (a package changed at rest is quarantined), and pinned when enabled. A version that fails to start never replaces the running one. See [PLUGINS.md](PLUGINS.md#ship-a-plugin-manifest-lint-package-install).
   - `eip plugin validate` lints a plugin before it ships: forbidden imports, permissions that don't match what the code uses, services not declared in `Inject`, malformed or undeclared secret references, and a compile under the declared permissions.
   - Each plugin is interpreted with only the stdlib packages its permissions allow (`net`, `exec`, `fs:read`, `fs:write`, `env`, `db`).
   - The admin policy can allow or deny plugins and narrow grants.
2. **Admin policy (`policy.json`).**
   - It loads last and can't be overridden by profiles, chat or plugins.
   - `locked` plugins can't be unloaded or replaced at runtime, and they get the reserved priority band. Locked hooks also apply to subagents.
   - `require` adds mandatory plugins, such as audit, policy and secrets.
3. **`policy` plugin.** Enforces the rules on every model and tool call: approval mode (`read-only`, `workspace-write`, `full-access`), tool allow/deny globs, model allowlist, sandbox requirement, token budgets and per-role overrides.
4. **Secrets.**
   - Config references `secret://NAME`, never raw values. Providers: `secrets_env` (allow-listed env vars) and `secrets_vault` (HashiCorp Vault KV v2, cached with a TTL so rotation is picked up, failing closed when Vault is unreachable). Any other store (KMS, a cloud secret manager) is a plugin that provides `sdk.Secrets`.
   - `grants` limits each plugin to its own secrets. The caller identity is bound by the core: a plugin that looks up the `secrets` service gets a copy tied to its own name, so it can't ask as another plugin.
   - Every resolved value is registered for redaction. `ctx.Logf`, `audit_jsonl`, `audit_http`, `trace_file` and `trace_otlp` replace it with `[REDACTED]` wherever it appears: tool arguments, results, model output, errors.
   - Every lookup is audited as `secret.access` (plugin, secret name, granted or denied, never the value). Denials are always recorded. Granted reads are recorded once per plugin and secret every 10 minutes, and again right away when the value changes (a rotation), so plugins that resolve secrets per call don't flood the log.
   - In `WithRunIn` mode keys never enter the sandbox, because model calls are bridged to the host.
5. **Audit and traces.** `audit_jsonl` writes a hash-chained JSONL log covering:
   - plugin lifecycle, with the sha256 and permissions of each plugin, and store events (install, approve, enable, failed enable, rollback, quarantine, uninstall, with who did it)
   - secret access
   - every model call
   - every tool call (args, outcome, approval decision and who made it)

   Records carry standard fields so a control plane can join them: `run` (unique per run), `thread`, `user`, `plugin` (the plugin that owns the tool), `tool`, `call_id`, `step`, `provider`, `model`, `decision`, `decided_by`, `outcome`, `ms`, token counts.

   Use `audit.Verify` to detect edited or removed lines. `audit_http` ships the same chained, redacted records off-host as NDJSON batches, HMAC-signed, with retries (set `path: "-"` on `audit_jsonl` to keep no local file). `trace_otlp` exports runs, model calls and tool calls as OpenTelemetry spans (OTLP/HTTP JSON, GenAI attribute names).
6. **Containment.**
   - `sandbox_docker`: no network, all capabilities dropped, no-new-privileges, pids limit.
   - `sandbox_local`: confines file paths and symlinks to its workdir and passes only allow-listed env vars. It is *not* isolation.
   - `tool_http_fetch`: requires a host allowlist and blocks private and loopback addresses at connect time (SSRF).
   - `trigger_webhook`: requires HMAC signatures.
7. **Humans and reviewers.** `mw_hitl` pauses before chosen or risky tools and keeps the pause durable. `mw_auto_review` has a reviewer model approve, deny or escalate.

## Not guaranteed

- **Yaegi capability scoping is not a security boundary.** A malicious plugin could still misuse services it is handed. For example, it could replace function fields on a shared service struct. Signing, approval and pinning establish *who* published a plugin and *that someone approved it*, not that it is safe. Only enable plugins you trust. For untrusted code, run the whole agent in a sandbox with `WithRunIn` and a container or microVM provider. Running a single plugin out of process, in its own sandbox, is not built yet.
- `sandbox_local` has no isolation.
- Docker can't filter egress per host. Use `network: none`, or a provider that supports allowlists.
- The audit chain detects tampering but doesn't prevent deleting the whole file. Ship it off-host with `audit_http`.
- Redaction applies to logs, traces and audit records (the sinks). It does not hide a secret from the model or from your app: if a tool returns a key in its result, the model sees it.
- Redaction matches the exact values resolved through the `secrets` service (4 bytes or longer). A secret that a plugin reads some other way, or transforms (base64, splits), is not caught.
- Records written before `audit_http` loaded are not re-sent; the receiver sees the chain start at the first record it gets.

Report vulnerabilities privately to the maintainers.

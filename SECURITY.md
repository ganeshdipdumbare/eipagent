# Security model

The goal is secure defaults that stay simple. `eipagent.New()` with no options uses the **secure** preset.

## Layers

1. **Trust gate (core, not pluggable).**
   - File plugins must match a sha256 pin in `eip.lock` (`eip plugin pin`). Embedded plugins ship inside the binary and are trusted like code.
   - Each plugin is interpreted with only the stdlib packages its permissions allow (`net`, `exec`, `fs:read`, `fs:write`, `env`, `db`).
   - The admin policy can allow or deny plugins and narrow grants.
2. **Admin policy (`policy.json`).**
   - It loads last and can't be overridden by profiles, chat or plugins.
   - `locked` plugins can't be unloaded or replaced at runtime, and they get the reserved priority band. Locked hooks also apply to subagents.
   - `require` adds mandatory plugins, such as audit, policy and secrets.
3. **`policy` plugin.** Enforces the rules on every model and tool call: approval mode (`read-only`, `workspace-write`, `full-access`), tool allow/deny globs, model allowlist, sandbox requirement, token budgets and per-role overrides.
4. **Secrets.** Config references `secret://NAME`, never raw values. `secrets_env` only exposes allow-listed names, and `grants` limits each plugin to its own secrets. The plugin identity comes from the core, not from the plugin. In `WithRunIn` mode keys never enter the sandbox, because model calls are bridged to the host.
5. **Audit.** `audit_jsonl` writes a hash-chained JSONL log covering:
   - plugin lifecycle, with the sha256 and permissions of each plugin
   - every model call (model, tokens, user, thread)
   - every tool call (args, outcome, approval decision and who made it)

   Use `audit.Verify` to detect edited or removed lines.
6. **Containment.**
   - `sandbox_docker`: no network, all capabilities dropped, no-new-privileges, pids limit.
   - `sandbox_local`: confines file paths and symlinks to its workdir and passes only allow-listed env vars. It is *not* isolation.
   - `tool_http_fetch`: requires a host allowlist and blocks private and loopback addresses at connect time (SSRF).
   - `trigger_webhook`: requires HMAC signatures.
7. **Humans and reviewers.** `mw_hitl` pauses before chosen or risky tools and keeps the pause durable. `mw_auto_review` has a reviewer model approve, deny or escalate.

## Not guaranteed

- **Yaegi capability scoping is not a security boundary.** A malicious plugin could still misuse services it is handed. For example, it could replace function fields on a shared service struct. Only load plugins you trust, and pin them. For untrusted code, use `WithRunIn` with a container or microVM provider.
- `sandbox_local` has no isolation.
- Docker can't filter egress per host. Use `network: none`, or a provider that supports allowlists.
- The audit chain detects tampering but doesn't prevent deleting the whole file. Ship it off-host (an `audit_http` plugin is not built yet).

Report vulnerabilities privately to the maintainers.

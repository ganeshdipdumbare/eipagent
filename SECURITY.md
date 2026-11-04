# Security model

The goal is secure defaults that stay simple. `eipagent.New()` with no options uses the **secure** preset.

## Layers

1. **Trust gate (core, not pluggable).**
   - File plugins must match a sha256 pin in `eip.lock` (`eip plugin pin`). Embedded plugins ship inside the binary and are trusted like code.
   - Plugins installed through a plugin store (`eip plugin install`, `pluginpkg.Store`) are signed ed25519 packages verified against the store's trusted keys, held until a person approves them, re-verified at enable time (a package changed at rest is quarantined), and pinned when enabled. A version that fails to start never replaces the running one. See [PLUGINS.md](PLUGINS.md#ship-a-plugin-manifest-lint-package-install).
   - Enabling a version re-verifies everything checked at install, the signature included, so a package swapped in the store directory (even with `state.json` edited to match), or one whose signing key was removed from `trusted_keys`, is quarantined instead of loaded.
   - `fs:write` alone can't read files (`os.OpenFile` is write-only without `fs:read`). `env` hides credential-looking variables; only secret providers declare `env:secrets`. Interpreted plugins can't call `sdk.AddRedaction`, and only built-in ones can call `sdk.Redact*` (otherwise it would answer "is this string a secret?").
   - A file plugin may resolve only the secrets it declared with `var Secrets = []string{"NAME"}` or referenced with a `secret://NAME` literal. Dynamic names need `var Secrets`; the admin policy's `grants` narrows them further. Compiled and embedded plugins are app/binary code and are not restricted this way.
   - Literal config values used as credentials (a model `api_key`, a webhook/HMAC key, a Vault token, MCP headers) are registered for redaction too, not only `secret://` values.
   - Every client that sends a credential refuses HTTP redirects, so `x-api-key`, `x-goog-api-key`, `X-Vault-Token`, `X-Subscription-Token` and custom auth headers cannot be replayed to another host. Non-loopback plaintext endpoints are refused when a credential is configured unless the plugin sets `allow_insecure: true`.
   - Persistence providers (`threads_file`, `threads_sql`, `memory_file`, `memory_sql`) redact registered secrets from messages, tool arguments, pending interrupts and memory values before writing them.
   - The permission tables also gate `net/textproto` (it can dial) behind `net`, `io/ioutil`'s read and write functions behind `fs:read` / `fs:write`, and `mime/multipart` behind `fs:write`; `os.NewFile` and `os.Stdin` are never available.
   - Plugin names are restricted to letters, digits, `_` and `-`, so a name can't point outside the plugin directories. Only policy-required rows may replace a locked plugin; a profile row can't.
   - A plugin can't take over another plugin's tool or skill name, and sandboxes handed to plugins have no host-path `Upload` / `Download`.
   - An update that fails to start, or is left waiting on a missing dependency, never replaces the running version.
   - `eip plugin validate` compiles a plugin without executing it, and lints it before it ships: forbidden imports, permissions that don't match what the code uses, services not declared in `Inject`, malformed or undeclared secret references, and a compile under the declared permissions.
   - Each plugin is interpreted with only the stdlib packages its permissions allow (`net`, `exec`, `fs:read`, `fs:write`, `env`, `db`).
   - The admin policy can allow or deny plugins and narrow grants.
2. **Admin policy (`policy.json`).**
   - It loads last and can't be overridden by profiles, chat or plugins.
   - `locked` plugins can't be unloaded or replaced at runtime, and they get the reserved priority band. Locked hooks also apply to subagents.
   - `require` adds mandatory plugins, such as audit, policy and secrets.
3. **`policy` plugin.** Enforces the rules on every model and tool call: approval mode (`read-only`, `workspace-write`, `full-access`), tool allow/deny globs, model allowlist, sandbox requirement, token budgets and per-role overrides.
4. **Secrets.**
   - Config should reference `secret://NAME`, never raw values. Providers: `secrets_env` (allow-listed env vars) and `secrets_vault` (HashiCorp Vault KV v2, cached with a TTL so rotation is picked up, failing closed when Vault is unreachable; it refuses to start without `grants` or `allow`, and secret names can't contain path segments). Any other store (KMS, a cloud secret manager) is a plugin that provides `sdk.Secrets`. Literal credentials are still accepted for local/dev use, registered for redaction, and should be avoided in production.
   - `grants` limits each plugin to its own secrets. The caller identity is bound by the core: a plugin that looks up the `secrets` service gets a copy tied to its own name, so it can't ask as another plugin.
   - File plugins are additionally limited to the secret names they declare with `var Secrets = []string{...}` or reference with a `secret://NAME` literal. A file plugin that builds a secret name dynamically must declare it (or a glob) in `var Secrets`; otherwise the lookup is denied and audited as `secret.access`.
   - Every resolved value is registered for redaction. `ctx.Logf`, `audit_file`, `audit_http`, `trace_file` and `trace_otlp` replace it with `[REDACTED]` wherever it appears: tool arguments, results, model output, errors.
   - Every lookup is audited as `secret.access` (plugin, secret name, granted or denied, never the value). Denials are always recorded. Granted reads are recorded once per plugin and secret every 10 minutes, and again right away when the value changes (a rotation), so plugins that resolve secrets per call don't flood the log.
   - In `WithRunIn` mode no secret enters the sandbox:
     - Model providers and `secrets_*` providers always stay on the host; model calls are bridged. The host records a run identity when `Generate`/`Thread.Generate` starts; `host.llm` and `host.tool` are checked against it rather than the identity the box reports, so a compromised box can't act as another user or role.
     - Host tool calls run through the host's own tool middleware (policy, limits, PII, audit) and need a known run id. `RunIn.BridgedTools` limits which Go tools the box may call at all; `NoBridgeTools` disables the bridge.
     - The box's `secrets` service returns placeholders (`eipsec_` + 32 hex), never values. Grants are checked on the host, and the placeholder is audited as `secret.access`.
     - The box's HTTP (`http.DefaultTransport`) is made by the host. The host swaps a placeholder for its value only when the request matches one of that secret's `RunIn.SecretHosts` rules: a host or host glob, optionally with methods and a path (`GET api.github.com/repos/*`). Otherwise it refuses. Requests without placeholders must match `RunIn.Egress` (none by default). Every request, allowed or refused, is recorded as `sandbox.egress` (method, host, path, status, duration, secret names, run) and logged on the host; load the audit plugin in `RunIn.HostPlugins` to keep those records on the host. Responses come back redacted, and redirects are not followed on the host, so every hop is checked again.
     - The host broker applies the same SSRF checks as `tool_fetch`: loopback, RFC1918, link-local, unspecified and cloud-metadata addresses are refused even if an egress rule names the host. `RunIn.AllowPrivateEgress` is the explicit exception.
     - `RunIn.Provider == "local"` is refused unless `AllowUnsafeLocal` is set. `sandbox_local` is not an isolation boundary: the box is a host process that can read host files (for example `/proc/<pid>/environ`, `$HOME/.aws` or a `.env` file). Use a container or microVM for untrusted agents.
     - Sandbox env and mounts are checked by the core, for `RunIn.Spec` and for every sandbox a plugin creates or runs a command in: a value holding a resolved secret, or a credential-looking name whose value isn't a placeholder, is refused, and so are mounts of `/`, a docker/podman/containerd socket or a directory holding one (checked after resolving symlinks), and image names starting with `-`. Plugins can't mount host paths at all; only the host application sets mounts.
     - `eip serve --stdio` refuses to start when its environment holds a credential-looking variable that isn't a placeholder.
5. **Audit and traces.** `audit_file` writes a hash-chained JSONL log covering:
   - plugin lifecycle, with the sha256 and permissions of each plugin, and store events (install, approve, enable, failed enable, rollback, quarantine, uninstall, with who did it)
   - secret access
   - every model call
   - every tool call (args, outcome, approval decision and who made it)

   Records carry standard fields so a control plane can join them: `run` (unique per run), `thread`, `user`, `plugin` (the plugin that owns the tool), `tool`, `call_id`, `step`, `provider`, `model`, `decision`, `decided_by`, `outcome`, `ms`, token counts.

   Every record has a `writer`: `core`, `audit_file`, or `plugin:<name>` for records a plugin submits through the `audit` service. The SDK sets it, so a plugin can add records but can't pass them off as the core's.

   Use `audit.Verify` to detect edited or removed lines. With `key: secret://...` on `audit_file`, each line also carries an HMAC of its hash, and `audit.VerifyKeyed` rejects a log rewritten by someone without the key. A host-level attacker can still roll back the log and its local `.head` together; copy the last head off-host (for example from the last `audit_http` batch or a control-plane checkpoint) and verify with `audit.VerifyKeyedWithAnchor` or `eip audit verify -anchor head.json LOG`, which fails unless the log ends at that exact sequence/hash. `audit_http` ships the same chained, redacted records off-host as NDJSON batches, HMAC-signed, with retries (set `path: "-"` on `audit_file` to keep no local file). `trace_otlp` exports runs, model calls and tool calls as OpenTelemetry spans (OTLP/HTTP JSON, GenAI attribute names).
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

- **Yaegi capability scoping is not a security boundary.** A malicious plugin could still misuse services it is handed. For example, it could replace function fields on a shared service struct. Signing, approval and pinning establish *who* published a plugin and *that someone approved it*, not that it is safe. Only enable plugins you trust. For untrusted code, run the whole agent in a sandbox with `WithRunIn` and a container or microVM provider. Running a single plugin out of process, in its own sandbox, is not built yet.
- `sandbox_local` has no isolation. `WithRunIn` refuses it by default; `AllowUnsafeLocal` is for development only.
- Docker can't filter egress per host. Use `network: none`, or a provider that supports allowlists.
- The audit chain detects tampering but doesn't prevent deleting the whole file. Ship it off-host with `audit_http`.
- `WithRunIn` placeholders are swapped only for HTTP made through `http.DefaultTransport` (clients without their own `Transport`). A plugin that needs the raw value in-process (a database DSN, env for a stdio MCP server) or dials with its own transport (`tool_fetch`) can't use a secret in the box; load it on the host (`RunIn.HostPlugins`). Responses are buffered (32 MB cap), so streaming endpoints don't stream through the host.
- The box can call any host tool in `RunIn.BridgedTools` and can send any prompt to the host model provider; it cannot read host secrets directly, but a host tool that returns sensitive data sends it into the box.
- A run started inside the box (for example a scheduled job or webhook) has no host-issued identity. The host accepts its model calls as an unauthenticated run for policy/budget/audit purposes, but `host.tool` is unavailable to such runs.
- `trigger_webhook`'s replay cache is per process. With several replicas, a captured request can be replayed against a replica that did not see the first one; use a shared store or a short `max_age_seconds`.
- Writing a literal secret in plugin config is still a secret in a file. Prefer `secret://NAME`; literal credentials are redacted from logs/audit/traces/persistence but the config file remains readable.
- The host trusts the box to name the plugin that asks for a secret, so grants inside the box are only as strong as the box. A placeholder is still useless outside its `SecretHosts`.
- Sandboxes a plugin creates inside a `WithRunIn` box are nested in it and get the same placeholders; they are not started on the host.
- Redaction applies to logs, traces, audit records and persisted threads/memory (the sinks). It does not hide a secret from the model or from your app: if a tool returns a key in its result, the model sees it.
- Redaction matches the values resolved through the `secrets` service and their JSON, URL, base64 and hex encodings. Values shorter than 4 bytes can't be redacted; their `secret.access` record says so. Other transformations (splitting, custom encodings) aren't caught.
- The audit chain without a key detects edits but not a full rewrite by someone with write access; use `key` or `audit_http`. The local keyed `.head` is not an anchor: an attacker who can rewrite the log can rewrite it too. Keep the last head off-host and verify with `audit.VerifyKeyedWithAnchor`.
- `sandbox_local` with a fixed `workdir` shares one folder between all threads (it logs a warning).
- The full-stack demo has no login: visitors pick Sam or Ada, so anyone can approve its (fake) refunds. It limits body size, requests per client (keyed on the address its own proxy saw, not on client-supplied headers) and runs in flight. A real app takes identity from its session. A secret that a plugin reads some other way, or transforms (base64, splits), is not caught.
- Records written before `audit_http` loaded are not re-sent; the receiver sees the chain start at the first record it gets.

Report vulnerabilities privately to the maintainers.

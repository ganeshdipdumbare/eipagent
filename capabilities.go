package kiln

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/modules/std"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Capability names a module may declare in Module.Permissions. A module
// holds a capability only if it declares it and the admin policy grants it.
// The constants match sdk.Perm*; import kiln only.
const (
	PermNet        = sdk.PermNet
	PermExec       = sdk.PermExec
	PermFSRead     = sdk.PermFSRead
	PermFSWrite    = sdk.PermFSWrite
	PermEnv        = sdk.PermEnv
	PermEnvSecrets = sdk.PermEnvSecrets
	PermDB         = sdk.PermDB
)

// Presets are preconfigured standard modules for Config.Modules. They are
// values, not a second way to compose: put them in the same list.
//
//	a, err := kiln.New(kiln.Config{
//		Model:   "openai:gpt-4.1",
//		Modules: append(kiln.Approvals("refund"), kiln.Audit(".kiln/audit.jsonl")...),
//	})

// Persistence keeps conversations (threads, including paused approvals)
// and long-term memory across restarts.
//
//	Persistence("memory", "")                    // threads_inmem + memory_inmem (default)
//	Persistence("file", ".kiln/data")            // threads_file + memory_file
//	Persistence("postgres", "secret://DSN")      // db_sql + threads_sql + memory_sql
//
// For postgres, import the driver once: import _ "github.com/ganeshdipdumbare/kiln/drivers/postgres".
func Persistence(kind, target string) []Module {
	switch strings.ToLower(kind) {
	case "file":
		if target == "" {
			target = ".kiln"
		}
		dir := filepath.Join(target, "threads")
		t := must("threads_file")
		t.Config = map[string]any{"dir": dir}
		// Modules work inside directories they are given; the engine creates
		// them before the module applies.
		t.Prepare = func() error { return os.MkdirAll(dir, 0o700) }
		m := must("memory_file")
		m.Config = map[string]any{"path": filepath.Join(target, "memory.json")}
		return []Module{t, m}
	case "postgres", "sql":
		db := must("db_sql")
		db.Config = map[string]any{"dsn": target}
		return []Module{db, must("threads_sql"), must("memory_sql")}
	default:
		return []Module{must("threads_inmem"), must("memory_inmem")}
	}
}

// Approvals pauses before the named tools until a person decides
// (approve, edit, reject or respond); resume with Thread.Resume.
func Approvals(tools ...string) []Module {
	m := must("hook_approval")
	on := map[string]any{}
	for _, t := range tools {
		on[t] = true
	}
	m.Config = map[string]any{"interrupt_on": on}
	return []Module{m}
}

// Approver decides human-in-the-loop interrupts synchronously (for example
// by prompting in a CLI). Return nil to leave one pending.
func Approver(fn func(ctx context.Context, in Interrupt) (*Decision, error)) []Module {
	return []Module{{Name: "approver_inline", Provides: []string{"approver"}, Apply: func(m *sdk.Scope) error {
		return m.Provide("approver", &sdk.Approver{Decide: fn})
	}}}
}

// Sandbox runs shell and file tools inside a sandbox per thread instead of
// on the host: "docker", "microsandbox", "local" (no isolation, for
// development) or a provider you load yourself.
func Sandbox(provider string) []Module {
	ws := must("workspace_sandbox")
	ws.Config = map[string]any{"provider": provider}
	if provider == "docker" || provider == "microsandbox" || provider == "local" {
		return []Module{must("sandbox_" + provider), ws}
	}
	return []Module{ws}
}

// Audit writes a tamper-evident audit log (hash-chained JSON lines) of
// every model call, tool call, approval and module change. Verify it with
// audit.Verify.
func Audit(path string) []Module {
	m := must("audit_file")
	m.Config = map[string]any{"path": path}
	m.Prepare = func() error { return os.MkdirAll(filepath.Dir(path), 0o700) }
	return []Module{m}
}

// Tracing sends traces of every run, model call and tool call to an
// OpenTelemetry collector over OTLP/HTTP, e.g. "http://localhost:4318".
func Tracing(endpoint string) []Module {
	m := must("trace_otlp")
	m.Config = map[string]any{"endpoint": endpoint}
	return []Module{m}
}

// MCP adds the tools of an MCP server, named "<name>__<tool>". The target
// is a command and its arguments (stdio), or one https URL.
//
//	MCP("github", "npx", "-y", "@modelcontextprotocol/server-github")
//	MCP("docs", "https://mcp.example.com/mcp")
func MCP(name, target string, args ...string) []Module {
	srv := map[string]any{"name": name}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		srv["url"] = target
	} else {
		a := make([]any, len(args))
		for i, x := range args {
			a[i] = x
		}
		srv["command"], srv["args"] = target, a
	}
	m := must("tool_mcp")
	m.Config = map[string]any{"servers": []any{srv}}
	return []Module{m}
}

// WebSearch gives the model a web_search tool. The backend is "duckduckgo"
// (no key), "tavily" (secret://TAVILY_API_KEY), "brave"
// (secret://BRAVE_API_KEY) or "searxng" (set base_url in its Config).
func WebSearch(backend string) []Module {
	m := must("tool_websearch")
	m.Config = map[string]any{"backend": backend}
	return []Module{m}
}

// Thinking turns on model reasoning at the given effort: low, medium, high,
// xhigh or max. Each provider gets its own setting.
func Thinking(effort string) []Module {
	m := must("hook_thinking")
	m.Config = map[string]any{"effort": effort}
	return []Module{m}
}

// must returns a standard module descriptor. Names are compile-time
// constants, so a missing one is a broken build of modules/std.
func must(name string) engine.Module {
	m, ok := std.Get(name)
	if !ok {
		panic("kiln: standard module " + name + " is missing from modules/std")
	}
	return m
}

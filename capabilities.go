package kiln

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ganeshdipdumbare/kiln/engine"
)

// Options for common setups. Each one loads standard modules with
// sensible config, and its comment names them, so moving to WithPlugin
// for finer control is one step. Calling one twice merges (approvals,
// MCP servers) or replaces (the rest).

// WithPersistence keeps conversations (threads, including paused
// approvals) and long-term memory across restarts.
//
//	WithPersistence("memory", "")                 // threads_inmem + memory_inmem (the default; lost on exit)
//	WithPersistence("file", ".kiln/data")     // threads_file + memory_file under that directory
//	WithPersistence("postgres", "secret://DSN")   // db_sql + threads_sql + memory_sql; any replica serves any thread
//
// For postgres, import the driver once: import _ "github.com/ganeshdipdumbare/kiln/drivers/postgres".
func WithPersistence(kind, target string) Option {
	return func(c *config) {
		var add []engine.ModuleSpec
		switch strings.ToLower(kind) {
		case "file":
			if target == "" {
				target = ".kiln"
			}
			add = []engine.ModuleSpec{
				{Use: "threads_file", Config: map[string]any{"dir": filepath.Join(target, "threads")}},
				{Use: "memory_file", Config: map[string]any{"path": filepath.Join(target, "memory.json")}},
			}
			// The modules work inside directories they are given; the facade
			// creates them before the agent starts.
			dir := target
			c.prepare = append(c.prepare, func() error { return os.MkdirAll(filepath.Join(dir, "threads"), 0o700) })
		case "postgres", "sql":
			add = []engine.ModuleSpec{
				{Use: "db_sql", Config: map[string]any{"dsn": target}},
				{Use: "threads_sql"}, {Use: "memory_sql"},
			}
		default: // "memory"
			add = []engine.ModuleSpec{{Use: "threads_inmem"}, {Use: "memory_inmem"}}
		}
		c.specs = replaceKinds(c.specs, add, "threads_", "memory_", "db_sql")
	}
}

// WithApprovals pauses before the named tools until a person decides
// (approve, edit, reject or respond); resume with Thread.Resume. Loads
// hook_approval.
func WithApprovals(tools ...string) Option {
	return func(c *config) {
		for i, s := range c.specs {
			if s.Name() == "hook_approval" {
				on, _ := s.Config["interrupt_on"].(map[string]any)
				for _, t := range tools {
					on[t] = true
				}
				c.specs[i] = s
				return
			}
		}
		on := map[string]any{}
		for _, t := range tools {
			on[t] = true
		}
		c.specs = append(c.specs, engine.ModuleSpec{Use: "hook_approval", Config: map[string]any{"interrupt_on": on}})
	}
}

// WithSandbox runs shell and file tools inside a sandbox per thread
// instead of on the host: "docker" (sandbox_docker), "microsandbox"
// (sandbox_microsandbox: a microVM), "local" (sandbox_local: no
// isolation, for development) or a provider you load yourself. Loads sandbox_<provider> + workspace_sandbox.
func WithSandbox(provider string) Option {
	return func(c *config) {
		add := []engine.ModuleSpec{{Use: "workspace_sandbox", Config: map[string]any{"provider": provider}}}
		if provider == "docker" || provider == "microsandbox" || provider == "local" {
			add = append([]engine.ModuleSpec{{Use: "sandbox_" + provider}}, add...)
		}
		c.specs = replaceKinds(c.specs, add, "workspace_")
	}
}

// WithAudit writes a tamper-evident audit log (hash-chained JSON lines)
// of every model call, tool call, approval and module change. Loads
// audit_file. Verify it with audit.Verify.
func WithAudit(path string) Option {
	return func(c *config) {
		c.prepare = append(c.prepare, func() error { return os.MkdirAll(filepath.Dir(path), 0o700) })
		c.specs = replaceKinds(c.specs, []engine.ModuleSpec{{Use: "audit_file", Config: map[string]any{"path": path}}}, "audit_file")
	}
}

// WithTracing sends traces of every run, model call and tool call to an
// OpenTelemetry collector over OTLP/HTTP, e.g. "http://localhost:4318".
// Loads trace_otlp.
func WithTracing(endpoint string) Option {
	return func(c *config) {
		c.specs = replaceKinds(c.specs, []engine.ModuleSpec{{Use: "trace_otlp", Config: map[string]any{"endpoint": endpoint}}}, "trace_otlp")
	}
}

// WithMCP adds the tools of an MCP server, named "<name>__<tool>". The
// target is a command and its arguments (stdio), or one https URL
// (streamable HTTP). Loads tool_mcp.
//
//	WithMCP("github", "npx", "-y", "@modelcontextprotocol/server-github")
//	WithMCP("docs", "https://mcp.example.com/mcp")
func WithMCP(name, target string, args ...string) Option {
	return func(c *config) {
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
		for i, s := range c.specs {
			if s.Name() == "tool_mcp" {
				list, _ := s.Config["servers"].([]any)
				s.Config["servers"] = append(list, srv)
				c.specs[i] = s
				return
			}
		}
		c.specs = append(c.specs, engine.ModuleSpec{Use: "tool_mcp", Config: map[string]any{"servers": []any{srv}}})
	}
}

// WithWebSearch gives the model a web_search tool. The backend is
// "duckduckgo" (no key), "tavily" (secret://TAVILY_API_KEY), "brave"
// (secret://BRAVE_API_KEY) or "searxng" (set base_url with WithPlugin).
// Loads tool_websearch; add tool_fetch to let it read the pages.
func WithWebSearch(backend string) Option {
	return func(c *config) {
		c.specs = replaceKinds(c.specs, []engine.ModuleSpec{{Use: "tool_websearch", Config: map[string]any{"backend": backend}}}, "tool_websearch")
	}
}

// WithThinking turns on model reasoning at the given effort: low,
// medium, high, xhigh or max. Each provider gets its own setting
// (Anthropic adaptive thinking, OpenAI reasoning_effort, Gemini
// thinkingConfig). Loads hook_thinking.
func WithThinking(effort string) Option {
	return func(c *config) {
		c.specs = replaceKinds(c.specs, []engine.ModuleSpec{{Use: "hook_thinking", Config: map[string]any{"effort": effort}}}, "hook_thinking")
	}
}

// replaceKinds drops rows whose module (current or old name) starts with
// any of the prefixes, then appends add.
func replaceKinds(specs, add []engine.ModuleSpec, prefixes ...string) []engine.ModuleSpec {
	var out []engine.ModuleSpec
	for _, s := range specs {
		drop := false
		for _, p := range prefixes {
			if strings.HasPrefix(s.Use, p) {
				drop = true
			}
		}
		if !drop {
			out = append(out, s)
		}
	}
	return append(out, add...)
}

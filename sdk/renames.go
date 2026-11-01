package sdk

// Built-in plugins renamed in v0.4 so each name says what the plugin does
// (model_, tool_, hook_, threads_, memory_, workspace_, trigger_ ...).
// The old names keep working until v1.0: loading, policy lists, grants
// and locks all accept them.
var PluginRenames = map[string]string{
	"llm_openai":       "model_openai",
	"llm_anthropic":    "model_anthropic",
	"llm_gemini":       "model_gemini",
	"llm_mock":         "model_mock",
	"tool_http_fetch":  "tool_fetch",
	"memory_store":     "tool_memory",
	"agents":           "tool_subagents",
	"skills_dir":       "tool_skills",
	"mcp_client":       "tool_mcp",
	"mw_retry":         "hook_retry",
	"mw_fallback":      "hook_fallback",
	"mw_limits":        "hook_limits",
	"mw_summarize":     "hook_summarize",
	"mw_hitl":          "hook_approval",
	"mw_auto_review":   "hook_review",
	"mw_pii":           "hook_pii",
	"mw_tool_errors":   "hook_tool_errors",
	"mw_logger":        "hook_log",
	"checkpoint_inmem": "threads_inmem",
	"checkpoint_file":  "threads_file",
	"checkpoint_sql":   "threads_sql",
	"store_inmem":      "memory_inmem",
	"store_file":       "memory_file",
	"store_sql":        "memory_sql",
	"exec_local":       "workspace_local",
	"exec_sandbox":     "workspace_sandbox",
	"jobs":             "trigger_schedule",
	"audit_jsonl":      "audit_file",
	"loop_default":     "loop",
}

// CanonicalPlugin maps a pre-v0.4 plugin name to its name now; other
// names pass through.
func CanonicalPlugin(name string) string {
	if n, ok := PluginRenames[name]; ok {
		return n
	}
	return name
}

// PluginAliases returns name with its old or new name, if it has one, so
// lookups keyed by either (grants, policy lists) find it.
func PluginAliases(name string) []string {
	out := []string{name}
	if n, ok := PluginRenames[name]; ok {
		return append(out, n)
	}
	for old, n := range PluginRenames {
		if n == name {
			out = append(out, old)
		}
	}
	return out
}

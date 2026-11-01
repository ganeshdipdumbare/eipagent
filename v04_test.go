package eipagent_test

// v0.4: new plugin names, plumbing that loads itself, capability options,
// and the old names still working.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/eipagent"
	"github.com/ganeshdipdumbare/eipagent/internal/mcptest"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

func statusOf(a *eipagent.Agent) map[string]eipagent.PluginStatus {
	out := map[string]eipagent.PluginStatus{}
	for _, s := range a.Status() {
		out[s.Name] = s
	}
	return out
}

func TestShellToolsGetAWorkspaceAutomatically(t *testing.T) {
	a := mockAgent(t, nil, eipagent.WithPlugins("tool_shell"))
	st := statusOf(a)
	if st["tool_shell"].State != "ACTIVE" {
		t.Fatalf("tool_shell: %s missing %v", st["tool_shell"].State, st["tool_shell"].Missing)
	}
	for _, n := range []string{"workspace_local", "sandbox_local"} {
		if !st[n].Auto || st[n].State != "ACTIVE" {
			t.Fatalf("%s should be auto-loaded: %+v", n, st[n])
		}
	}
	if !strings.Contains(st["workspace_local"].String(), "auto") {
		t.Fatal("status row doesn't say auto")
	}
}

func TestNewAndOldPluginNamesAreTheSamePlugin(t *testing.T) {
	for _, use := range []string{"hook_pii", "mw_pii"} {
		a := mockAgent(t, []any{map[string]any{"text": "ok"}}, eipagent.WithPlugin(use, map[string]any{"types": []any{"email"}}))
		if st := statusOf(a)[use]; st.State != "ACTIVE" || st.Source != use {
			t.Fatalf("%s: %+v", use, st)
		}
	}
	// Old service names resolve to the new ones.
	var seen []string
	probe := eipagent.Plugin{Name: "probe", Inject: []string{"llm/*", "checkpointer"}, Apply: func(p *eipagent.PluginAPI) error {
		for _, n := range []string{"llm/mock", "model/mock", "checkpointer", "threads"} {
			if p.HasService(n) {
				seen = append(seen, n)
			}
		}
		return nil
	}}
	mockAgent(t, nil, eipagent.WithCompiledPlugin(probe))
	if len(seen) != 4 {
		t.Fatalf("old and new service names should both resolve: %v", seen)
	}
}

func TestWithPersistenceFile(t *testing.T) {
	dir := t.TempDir()
	mk := func(reply string) *eipagent.Agent {
		return mockAgent(t, []any{map[string]any{"text": reply}}, eipagent.WithPersistence("file", dir))
	}
	a := mk("first")
	if _, err := a.Thread("t1").Generate(bg, "hello"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b := mk("second")
	msgs, err := b.Thread("t1").Messages(bg)
	if err != nil || len(msgs) < 2 {
		t.Fatalf("thread didn't survive a restart: %v %d", err, len(msgs))
	}
	if _, err := os.Stat(filepath.Join(dir, "threads")); err != nil {
		t.Fatal("threads not under the given directory")
	}
	if st := statusOf(b); st["threads_file"].State != "ACTIVE" || st["threads_inmem"].State != "" {
		t.Fatalf("threads_file should replace the in-memory default: %+v", st)
	}
}

func TestWithApprovalsPausesTheTool(t *testing.T) {
	ran := false
	a := mockAgent(t, []any{
		map[string]any{"tool": "wire", "args": map[string]any{"amount": 5}},
		map[string]any{"text": "done"},
	}, eipagent.WithApprovals("wire"),
		eipagent.WithTools(eipagent.Tool("wire", "Send money", func(c context.Context, in struct {
			Amount int `json:"amount"`
		}) (string, error) {
			ran = true
			return "sent", nil
		})))
	res, err := a.Thread("t").Generate(bg, "pay")
	if err != nil || !res.Interrupted() || ran {
		t.Fatalf("want a pause before wire: %v %+v ran=%v", err, res, ran)
	}
	if _, err := a.Thread("t").Resume(bg, eipagent.Approve(res.Interrupts[0].ID)); err != nil || !ran {
		t.Fatalf("approve: %v ran=%v", err, ran)
	}
}

func TestWithAuditTracingAndMCP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		resp := mcptest.Handle(req)
		if resp == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	a := mockAgent(t, nil,
		eipagent.WithAudit(auditPath),
		eipagent.WithTracing(srv.URL),
		eipagent.WithMCP("srv", srv.URL))
	st := statusOf(a)
	for _, n := range []string{"audit_file", "trace_otlp", "tool_mcp"} {
		if st[n].State != "ACTIVE" {
			t.Fatalf("%s: %+v", n, st[n])
		}
	}
	if a.Engine().Root().Snapshot().Tool("srv__echo") == nil {
		t.Fatal("MCP tools missing")
	}
	if _, err := os.Stat(auditPath); err != nil {
		t.Fatal("audit log not written where asked")
	}
}

// A plugin written against the v0.3 API (sdk.Context, Inject,
// OnAgentStart, old service names) still loads and runs.
func TestV03PluginStillWorks(t *testing.T) {
	dir := t.TempDir()
	src := `//go:build ignore

package plugin

import (
	"context"

	"eipagent/sdk"
)

var Inject = []string{"store"}

func Apply(ctx *sdk.Context) error {
	ctx.OnAgentStart(func(c context.Context, st *sdk.RunState) error { return nil })
	ctx.OnDispose(func() {})
	_ = ctx.Service("store").(*sdk.Store)
	ctx.AddTool(sdk.Tool{Name: "legacy", Run: func(c context.Context, a map[string]any) (string, error) { return "ok", nil }})
	return nil
}
`
	os.WriteFile(filepath.Join(dir, "legacy.go"), []byte(src), 0o644)
	a := mockAgent(t, nil, eipagent.WithPreset("dev"), eipagent.WithPluginDir(dir), eipagent.WithPlugins("store_inmem", "legacy"))
	if st := statusOf(a)["legacy"]; st.State != "ACTIVE" {
		t.Fatalf("v0.3 plugin: %+v", st)
	}
	var _ *sdk.Context = (*sdk.Plugin)(nil) // the alias is the same type
}

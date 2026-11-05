package kiln_test

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

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/internal/mcptest"
)

func statusOf(a *kiln.Agent) map[string]kiln.ModuleStatus {
	out := map[string]kiln.ModuleStatus{}
	for _, s := range a.Status() {
		out[s.Name] = s
	}
	return out
}

func TestShellToolsGetAWorkspaceAutomatically(t *testing.T) {
	a := mockAgent(t, nil, kiln.WithModules("tool_shell"))
	st := statusOf(a)
	if st["tool_shell"].State != "ACTIVE" {
		t.Fatalf("tool_shell: %s", st["tool_shell"].State)
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

func TestWithPersistenceFile(t *testing.T) {
	dir := t.TempDir()
	mk := func(reply string) *kiln.Agent {
		return mockAgent(t, []any{map[string]any{"text": reply}}, kiln.WithPersistence("file", dir))
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
	}, kiln.WithApprovals("wire"),
		kiln.WithTools(kiln.Tool("wire", "Send money", func(c context.Context, in struct {
			Amount int `json:"amount"`
		}) (string, error) {
			ran = true
			return "sent", nil
		})))
	res, err := a.Thread("t").Generate(bg, "pay")
	if err != nil || !res.Interrupted() || ran {
		t.Fatalf("want a pause before wire: %v %+v ran=%v", err, res, ran)
	}
	if _, err := a.Thread("t").Resume(bg, kiln.Approve(res.Interrupts[0].ID)); err != nil || !ran {
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
		kiln.WithAudit(auditPath),
		kiln.WithTracing(srv.URL),
		kiln.WithMCP("srv", srv.URL))
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

package kiln_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/internal/mcptest"
)

func mcpScript() []any {
	return []any{
		map[string]any{"tool": "test__add", "args": map[string]any{"a": 2, "b": 3}},
		map[string]any{"tool": "test__echo", "args": map[string]any{"text": "hi"}},
		map[string]any{"text": "done"},
	}
}

func checkMCP(t *testing.T, server map[string]any) {
	t.Helper()
	a := mockAgent(t, mcpScript(), kiln.WithModuleConfig("tool_mcp", map[string]any{"servers": []any{server}}))
	res, err := a.Generate(bg, "use mcp")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 3 || res.Steps[0].ToolResults[0].Content != "5" || res.Steps[1].ToolResults[0].Content != "hi" {
		t.Fatalf("got %+v", res.Steps)
	}
}

func TestMCPStdio(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mcpserver")
	if out, err := exec.Command("go", "build", "-o", bin, "./internal/testdata/mcpserver").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	checkMCP(t, map[string]any{"name": "test", "command": bin})
}

func TestMCPStreamableHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		resp := mcptest.Handle(req)
		if resp == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Mcp-Session-Id", "s1")
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(resp)
		w.Write([]byte("event: message\ndata: " + string(b) + "\n\n"))
	}))
	defer srv.Close()
	t.Setenv("MCP_TOKEN", "Bearer tok")
	checkMCP(t, map[string]any{"name": "test", "url": srv.URL, "headers": map[string]any{"Authorization": "secret://MCP_TOKEN"}})
}

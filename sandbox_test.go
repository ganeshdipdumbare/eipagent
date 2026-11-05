package kiln_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/kiln"
)

func TestExecSandboxShellAndFiles(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(filepath.Dir(root), "outside.txt"), []byte("secret"), 0o644)
	a := mockAgent(t, []any{
		map[string]any{"tool": "write_file", "args": map[string]any{"path": "hello.txt", "content": "hello\nworld\n"}},
		map[string]any{"tool": "edit_file", "args": map[string]any{"path": "hello.txt", "old_text": "world", "new_text": "there"}},
		map[string]any{"tool": "bash", "args": map[string]any{"command": "cat hello.txt; echo $SECRET_X"}},
		map[string]any{"tool": "read_file", "args": map[string]any{"path": "../outside.txt"}},
		map[string]any{"tool": "grep", "args": map[string]any{"pattern": "the.e"}},
		map[string]any{"text": "done"},
	}, kiln.WithModuleConfig("sandbox_local", map[string]any{"root": root}),
		kiln.WithModuleConfig("workspace_sandbox", map[string]any{"provider": "local"}),
		kiln.WithModules("tool_shell", "tool_fs"))
	t.Setenv("SECRET_X", "leaked")
	res, err := a.Generate(bg, "go")
	if err != nil {
		t.Fatal(err)
	}
	r := func(i int) string { return res.Steps[i].ToolResults[0].Content }
	if !strings.Contains(r(2), "hello\nthere") || strings.Contains(r(2), "leaked") || !strings.Contains(r(2), "[exit code 0]") {
		t.Fatalf("bash: %q", r(2))
	}
	if !strings.Contains(r(3), "outside the sandbox") {
		t.Fatalf("jail escape must fail: %q", r(3))
	}
	if !strings.Contains(r(4), "hello.txt:2:there") {
		t.Fatalf("grep: %q", r(4))
	}
	if len(res.Changes) != 2 || !strings.Contains(res.Changes[1].Diff(), "-world\n+there") {
		t.Fatalf("changes: %+v", res.Changes)
	}
	ents, _ := os.ReadDir(root)
	if len(ents) != 1 {
		t.Fatalf("expected one per-thread sandbox dir, got %d", len(ents))
	}
	a.Close()
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Fatal("sandboxes must be destroyed on unload")
	}
}

func TestToolSandbox(t *testing.T) {
	root := t.TempDir()
	a := mockAgent(t, []any{
		map[string]any{"tool": "sandbox_create"},
		map[string]any{"tool": "sandbox_list"},
		map[string]any{"text": "made"},
	}, kiln.WithModuleConfig("sandbox_local", map[string]any{"root": root}),
		kiln.WithModuleConfig("tool_sandbox", map[string]any{"provider": "local", "max_boxes": 1}))
	res, err := a.Generate(bg, "make one")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(res.Steps[1].ToolResults[0].Content)
	if !strings.HasPrefix(id, "local-") {
		t.Fatalf("list: %q", id)
	}
}

func TestDockerSandbox(t *testing.T) {
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker not available")
	}
	a := mockAgent(t, []any{
		map[string]any{"tool": "bash", "args": map[string]any{"command": "echo hi > f && cat f && (wget -q -T 2 http://example.com -O- >/dev/null 2>&1 && echo NET || echo NONET)"}},
		map[string]any{"text": "done"},
	}, kiln.WithModules("sandbox_docker", "workspace_sandbox", "tool_shell"))
	res, err := a.Generate(bg, "go")
	if err != nil {
		t.Fatal(err)
	}
	out := res.Steps[0].ToolResults[0].Content
	if !strings.Contains(out, "hi") || !strings.Contains(out, "NONET") {
		t.Fatalf("docker: %q", out)
	}
}

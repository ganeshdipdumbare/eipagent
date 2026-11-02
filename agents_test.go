package eipagent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/eipagent"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

func TestSubagentAsTool(t *testing.T) {
	var calledBy string
	secret := eipagent.Tool("lookup_secret", "only the researcher may use this", func(ctx context.Context, in struct{}) (string, error) {
		calledBy = "researcher"
		return "42", nil
	})
	other := eipagent.Tool("other", "not shared", func(ctx context.Context, in struct{}) (string, error) { return "", nil })
	// Main agent calls researcher; researcher (same mock script) calls lookup_secret, answers, main answers.
	a := mockAgent(t, []any{
		map[string]any{"tool": "researcher", "args": map[string]any{"task": "find the number"}},
		map[string]any{"tool": "lookup_secret"},
		map[string]any{"text": "the number is 42"},
		map[string]any{"text": "Researcher says 42."},
	}, eipagent.WithTools(secret, other), eipagent.WithPlugin("tool_subagents", map[string]any{"agents": []any{
		map[string]any{"name": "researcher", "description": "finds facts", "instructions": "You research.", "tools": []any{"lookup_secret"}},
	}}))
	res, err := a.Generate(bg, "what is the number?")
	if err != nil || res.Text != "Researcher says 42." || calledBy != "researcher" {
		t.Fatalf("%v %+v", err, res)
	}
	if got := res.Steps[0].ToolResults[0].Content; got != "the number is 42" {
		t.Fatalf("subagent answer should be the tool result, got %q", got)
	}
}

func TestHandoff(t *testing.T) {
	var sysSeen []string
	spy := eipagent.Plugin{Name: "spy", Apply: func(ctx *sdk.Plugin) error {
		ctx.WrapModel(func(c context.Context, req *eipagent.ModelRequest, next eipagent.ModelHandler) (*eipagent.ModelResponse, error) {
			sysSeen = append(sysSeen, req.System)
			return next(c, req)
		})
		return nil
	}}
	a := mockAgent(t, []any{
		map[string]any{"tool": "transfer_to_billing"},
		map[string]any{"text": "Billing here."},
		map[string]any{"text": "still billing"},
	}, eipagent.WithCompiledPlugin(spy), eipagent.WithPlugin("tool_subagents", map[string]any{"agents": []any{
		map[string]any{"name": "billing", "description": "invoices", "instructions": "You are billing.", "mode": "handoff"},
	}}))
	th := a.Thread("h")
	if _, err := th.Generate(bg, "invoice question"); err != nil {
		t.Fatal(err)
	}
	if _, err := th.Generate(bg, "more"); err != nil {
		t.Fatal(err)
	}
	if len(sysSeen) != 3 || sysSeen[1] != "You are billing." || sysSeen[2] != "You are billing." {
		t.Fatalf("handoff should persist across turns: %q", sysSeen)
	}
}

func TestAgentAsTool(t *testing.T) {
	inner := mockAgent(t, []any{map[string]any{"text": "inner answer"}})
	outer := mockAgent(t, []any{map[string]any{"tool": "expert", "args": map[string]any{"task": "q"}}, map[string]any{"text": "ok"}},
		eipagent.WithTools(inner.AsTool("expert", "an expert")))
	res, err := outer.Generate(bg, "ask")
	if err != nil || res.Steps[0].ToolResults[0].Content != "inner answer" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestSkills(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pdf"), 0o755)
	os.WriteFile(filepath.Join(dir, "pdf", "SKILL.md"), []byte("---\nname: pdf\ndescription: Work with PDF files\n---\n# PDF\nUse pdftotext."), 0o644)
	os.WriteFile(filepath.Join(dir, "pdf", "ref.md"), []byte("reference"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("nope"), 0o644)
	var sys string
	spy := eipagent.Plugin{Name: "spy", Apply: func(ctx *sdk.Plugin) error {
		ctx.WrapModel(func(c context.Context, req *eipagent.ModelRequest, next eipagent.ModelHandler) (*eipagent.ModelResponse, error) {
			sys = req.System
			return next(c, req)
		})
		return nil
	}}
	a := mockAgent(t, []any{
		map[string]any{"tool": "load_skill", "args": map[string]any{"name": "pdf"}},
		map[string]any{"tool": "read_skill_file", "args": map[string]any{"name": "pdf", "path": "ref.md"}},
		map[string]any{"tool": "read_skill_file", "args": map[string]any{"name": "pdf", "path": "../secret.txt"}},
		map[string]any{"text": "done"},
	}, eipagent.WithCompiledPlugin(spy), eipagent.WithPlugin("tool_skills", map[string]any{"dirs": []any{dir}}))
	res, err := a.Generate(bg, "read a pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sys, "- pdf: Work with PDF files") || strings.Contains(sys, "pdftotext") {
		t.Fatalf("only the index belongs in the prompt: %q", sys)
	}
	r := func(i int) string { return res.Steps[i].ToolResults[0].Content }
	if r(0) != "# PDF\nUse pdftotext." || r(1) != "reference" || !strings.Contains(r(2), "escapes") {
		t.Fatalf("got %q %q %q", r(0), r(1), r(2))
	}
}

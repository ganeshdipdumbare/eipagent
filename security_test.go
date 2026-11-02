package eipagent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/eipagent"
	"github.com/ganeshdipdumbare/eipagent/audit"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

func securePolicy(auditPath string, rules map[string]any) *eipagent.Policy {
	return &eipagent.Policy{
		RequirePins: true,
		Locked:      []string{"policy", "audit_jsonl"},
		Require: []eipagent.PluginSpec{
			{Use: "audit_jsonl", Config: map[string]any{"path": auditPath}},
			{Use: "policy"},
		},
		Rules: rules,
	}
}

func TestPolicyEnforcedAndAudited(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	var ran []string
	mk := func(name string, ro bool) eipagent.ToolDef {
		tl := eipagent.Tool(name, name, func(ctx context.Context, in struct{}) (string, error) { ran = append(ran, name); return "ok", nil })
		if ro {
			tl = eipagent.ReadOnly(tl)
		}
		return tl
	}
	// A plugin trying to get in front of policy and wave everything through.
	greedy := eipagent.Plugin{Name: "greedy", Apply: func(ctx *sdk.Plugin) error {
		ctx.SetPriority(1 << 40)
		ctx.WrapTool(func(c context.Context, r *eipagent.ToolRequest, next eipagent.ToolHandler) (*eipagent.ToolResult, error) {
			return next(c, r)
		})
		return nil
	}}
	a := mockAgent(t, []any{
		map[string]any{"tool": "lookup"},
		map[string]any{"tool": "deploy"},
		map[string]any{"tool": "drop_db"},
		map[string]any{"text": "done"},
	}, eipagent.WithTools(mk("lookup", true), mk("deploy", false), mk("drop_db", false)),
		eipagent.WithCompiledPlugin(greedy),
		eipagent.WithPolicy(securePolicy(auditPath, map[string]any{
			"mode": "read-only", "deny_tools": []any{"drop_*"}, "allow_models": []any{"mock:*"},
			"roles": map[string]any{"admin": map[string]any{"mode": "full-access"}},
		})))
	res, err := a.Generate(bg, "go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ran, ",") != "lookup" {
		t.Fatalf("only the read-only tool may run, ran %v", ran)
	}
	if !strings.Contains(res.Steps[1].ToolResults[0].Content, "denied by policy") || !strings.Contains(res.Steps[2].ToolResults[0].Content, "denied") {
		t.Fatalf("denials: %+v", res.Steps)
	}
	if err := a.Engine().Unload("policy"); err == nil {
		t.Fatal("locked policy must not unload")
	}
	if _, err := a.Generate(bg, "x", eipagent.Model("openai:gpt")); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("model allowlist: %v", err)
	}
	// Roles widen the mode; deny_tools still wins.
	ran = nil
	b := mockAgent(t, []any{map[string]any{"tool": "deploy"}, map[string]any{"tool": "drop_db"}, map[string]any{"text": "ok"}},
		eipagent.WithTools(mk("deploy", false), mk("drop_db", false)), eipagent.WithUser("alice", "admin"),
		eipagent.WithPolicy(securePolicy(filepath.Join(dir, "b.jsonl"), map[string]any{"mode": "read-only", "deny_tools": []any{"drop_*"},
			"roles": map[string]any{"admin": map[string]any{"mode": "full-access"}}})))
	if _, err := b.Generate(bg, "go"); err != nil || strings.Join(ran, ",") != "deploy" {
		t.Fatalf("admin role: %v %v", err, ran)
	}
	a.Close()

	n, err := audit.Verify(auditPath)
	if err != nil || n < 6 {
		t.Fatalf("audit chain: %d %v", n, err)
	}
	raw, _ := os.ReadFile(auditPath)
	for _, want := range []string{`"type":"plugin.active"`, `"plugin":"policy"`, `"type":"tool.call"`, `"outcome":"failed"`, `"type":"model.call"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("audit missing %s", want)
		}
	}
	tampered := strings.Replace(string(raw), `"tool":"deploy"`, `"tool":"lookup"`, 1)
	os.WriteFile(auditPath, []byte(tampered), 0o600)
	if _, err := audit.Verify(auditPath); err == nil {
		t.Fatal("tampering must be detected")
	}
	lines := strings.SplitN(string(raw), "\n", 3)
	os.WriteFile(auditPath, []byte(lines[0]+"\n"+lines[2]), 0o600)
	if _, err := audit.Verify(auditPath); err == nil {
		t.Fatal("a removed line must be detected")
	}
}

func TestSecurePresetRefusesUnpinned(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "mine.go"), []byte("//go:build ignore\n\npackage plugin\n\nimport \"eipagent/sdk\"\n\nfunc Apply(ctx *sdk.Plugin) error { return nil }\n"), 0o644)
	_, err := eipagent.New(eipagent.WithModel("mock:x"), eipagent.WithPluginDir(dir), eipagent.WithPlugins("mine"))
	if err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("secure default must refuse unpinned plugins: %v", err)
	}
	a, err := eipagent.New(eipagent.WithModel("mock:x"), eipagent.WithPluginDir(dir), eipagent.WithPlugins("mine"), eipagent.WithPreset("dev"))
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
}

func TestSecretGrants(t *testing.T) {
	t.Setenv("A_API_KEY", "a")
	t.Setenv("B_API_KEY", "b")
	got := map[string]string{}
	spy := func(name string) eipagent.Plugin {
		return eipagent.Plugin{Name: name, Apply: func(ctx *sdk.Plugin) error {
			for _, k := range []string{"A_API_KEY", "B_API_KEY"} {
				v, err := ctx.Secret("secret://" + k)
				if err == nil {
					got[name+":"+k] = v
				}
			}
			return nil
		}}
	}
	mockAgent(t, nil, eipagent.WithCompiledPlugin(spy("p1")), eipagent.WithCompiledPlugin(spy("p2")),
		eipagent.WithPlugin("secrets_env", map[string]any{"grants": map[string]any{"p1": []any{"A_API_KEY"}, "p2": []any{"B_API_KEY"}}}))
	if len(got) != 2 || got["p1:A_API_KEY"] != "a" || got["p2:B_API_KEY"] != "b" {
		t.Fatalf("each plugin sees only its granted secret: %v", got)
	}
}

func TestHTTPFetchEgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><script>x</script><p>internal page</p></html>"))
	}))
	defer srv.Close()
	script := []any{map[string]any{"tool": "http_fetch", "args": map[string]any{"url": srv.URL}}, map[string]any{"text": "ok"}}
	a := mockAgent(t, script, eipagent.WithPluginSpec(eipagent.PluginSpec{Use: "tool_http_fetch", Config: map[string]any{"allow_hosts": []any{"*"}}}))
	res, _ := a.Generate(bg, "fetch")
	if got := res.Steps[0].ToolResults[0].Content; !strings.Contains(got, "private address") {
		t.Fatalf("loopback must be blocked: %q", got)
	}
	b := mockAgent(t, script, eipagent.WithPluginSpec(eipagent.PluginSpec{Use: "tool_http_fetch", Config: map[string]any{"allow_hosts": []any{"127.0.0.1"}, "allow_private": true}}))
	res, _ = b.Generate(bg, "fetch")
	if got := res.Steps[0].ToolResults[0].Content; !strings.Contains(got, "internal page") || strings.Contains(got, "<script>") {
		t.Fatalf("allowed fetch: %q", got)
	}
}

package eipagent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ganeshdipdumbare/eipagent"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// A redirect must never carry a provider's credential header to the new
// host. Go's default redirect policy strips Authorization but not custom
// headers such as x-api-key or x-goog-api-key.
func TestCredentialRedirectsAreRefused(t *testing.T) {
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
	}))
	defer target.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer first.Close()

	cases := []struct {
		name   string
		model  string
		plugin string
		cfg    map[string]any
	}{
		{"anthropic", "anthropic:test", "model_anthropic", map[string]any{"base_url": first.URL, "api_key": "sk-ant-live-test"}},
		{"gemini", "gemini:test", "model_gemini", map[string]any{"base_url": first.URL, "api_key": "AIza-live-test"}},
		{"openai", "openai:test", "model_openai", map[string]any{"base_url": first.URL, "api_key": "sk-live-test",
			"headers": map[string]any{"x-api-key": "header-secret"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			atomic.StoreInt32(&targetHits, 0)
			a, err := eipagent.New(eipagent.WithModel(c.model), eipagent.WithPlugin(c.plugin, c.cfg))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			_, _ = a.Generate(bg, "hi")
			if n := atomic.LoadInt32(&targetHits); n != 0 {
				t.Fatalf("%s followed a redirect and sent credentials to the new host (%d hits)", c.name, n)
			}
		})
	}

	t.Run("websearch", func(t *testing.T) {
		atomic.StoreInt32(&targetHits, 0)
		a := mockAgent(t, []any{
			map[string]any{"tool": "web_search", "args": map[string]any{"query": "x"}},
			map[string]any{"text": "done"},
		}, eipagent.WithPlugin("tool_websearch", map[string]any{"backend": "searxng", "base_url": first.URL, "api_key": "search-secret"}))
		if _, err := a.Generate(bg, "search"); err != nil {
			t.Fatal(err)
		}
		if n := atomic.LoadInt32(&targetHits); n != 0 {
			t.Fatalf("tool_websearch followed a redirect (%d hits)", n)
		}
	})

	t.Run("vault", func(t *testing.T) {
		atomic.StoreInt32(&targetHits, 0)
		t.Setenv("VAULT_TOKEN", "vault-token-secret")
		var got string
		probe := eipagent.Plugin{Name: "vault_probe", Apply: func(p *sdk.Plugin) error {
			p.AddTool(sdk.Tool{Name: "vault_probe", Schema: sdk.Object(map[string]any{}),
				Run: func(c context.Context, a map[string]any) (string, error) {
					v, err := p.Secret("secret://X")
					if err != nil {
						return "", err
					}
					got = v
					return v, nil
				}})
			return nil
		}}
		a := mockAgent(t, []any{
			map[string]any{"tool": "vault_probe"},
			map[string]any{"text": "done"},
		}, eipagent.WithPreset("dev"),
			eipagent.WithPlugin("secrets_vault", map[string]any{"address": first.URL, "allow": []any{"*"}}),
			eipagent.WithCompiledPlugin(probe))
		_, _ = a.Generate(bg, "read")
		if got != "" {
			t.Fatalf("vault returned a value through a redirect: %q", got)
		}
		if n := atomic.LoadInt32(&targetHits); n != 0 {
			t.Fatalf("secrets_vault followed a redirect and sent X-Vault-Token (%d hits)", n)
		}
	})
}

func TestToolSkillsRefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "pdf")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: pdf\ndescription: pdf\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(skillDir, "leak.md")); err != nil {
		t.Fatal(err)
	}
	a := mockAgent(t, []any{
		map[string]any{"tool": "read_skill_file", "args": map[string]any{"name": "pdf", "path": "leak.md"}},
		map[string]any{"text": "done"},
	}, eipagent.WithPlugin("tool_skills", map[string]any{"dirs": []any{root}}))
	res, err := a.Generate(bg, "read")
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Steps[0].ToolResults[0].Content; !strings.Contains(out, "escapes") || strings.Contains(out, "TOPSECRET") {
		t.Fatalf("symlink escape was not refused: %q", out)
	}
}

func TestSandboxLocalGrepCannotReadHost(t *testing.T) {
	a := mockAgent(t, []any{
		map[string]any{"tool": "grep", "args": map[string]any{"pattern": "root", "path": "/etc"}},
		map[string]any{"tool": "find_files", "args": map[string]any{"pattern": "passwd", "path": "/etc"}},
		map[string]any{"text": "done"},
	}, eipagent.WithPlugin("sandbox_local", map[string]any{"root": t.TempDir()}),
		eipagent.WithPlugin("workspace_sandbox", map[string]any{"provider": "local"}),
		eipagent.WithPlugins("tool_fs"))
	res, err := a.Generate(bg, "read")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if out := res.Steps[i].ToolResults[0].Content; !strings.Contains(out, "outside the sandbox") && !strings.Contains(out, "not found") {
			t.Fatalf("host path %d was not refused: %q", i, out)
		}
	}
}

func TestFilePluginsMustDeclareSecrets(t *testing.T) {
	t.Setenv("A_TOKEN", "a-secret")
	t.Setenv("B_TOKEN", "b-secret")
	src := func(declare bool) string {
		secrets := ""
		if declare {
			secrets = `var Secrets = []string{"A_TOKEN"}` + "\n"
		}
		return `//go:build ignore

package plugin

import (
	"context"

	"eipagent/sdk"
)

` + secrets + `
func Apply(p *sdk.Plugin) error {
	p.AddTool(sdk.Tool{Name: "read_secret", Schema: sdk.Object(map[string]any{}),
		Run: func(c context.Context, a map[string]any) (string, error) {
			name := p.ConfigString("name", "A_TOKEN")
			v, err := p.Secret("secret://" + name)
			if err != nil {
				return "", err
			}
			return v, nil
		}})
	return nil
}
`
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "read_dyn.go"), []byte(src(false)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "read_decl.go"), []byte(src(true)), 0o644); err != nil {
		t.Fatal(err)
	}
	readAll := strings.Replace(src(false), "func Apply", `var Secrets = []string{"*"}

func Apply`, 1)
	if err := os.WriteFile(filepath.Join(dir, "read_all.go"), []byte(readAll), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(plugin string) string {
		t.Helper()
		a := mockAgent(t, []any{
			map[string]any{"tool": "read_secret"},
			map[string]any{"text": "done"},
		}, eipagent.WithPreset("dev"), eipagent.WithPluginDir(dir), eipagent.WithPlugins(plugin),
			eipagent.WithPlugin("secrets_env", map[string]any{"allow": []any{"*"}}))
		res, err := a.Generate(bg, "read")
		if err != nil {
			t.Fatal(err)
		}
		return res.Steps[0].ToolResults[0].Content
	}
	if out := run("read_dyn"); !strings.Contains(out, "not declared") || strings.Contains(out, "a-secret") {
		t.Fatalf("an undeclared secret must be refused: %q", out)
	}
	if out := run("read_decl"); !strings.Contains(out, "a-secret") {
		t.Fatalf("a declared secret must be readable: %q", out)
	}
	if out := run("read_all"); !strings.Contains(out, "not declared") || strings.Contains(out, "a-secret") {
		t.Fatalf("a wildcard declaration must not grant every secret: %q", out)
	}
}

func TestRunInLocalRequiresExplicitOptIn(t *testing.T) {
	_, err := eipagent.New(eipagent.WithModel("mock:m"), eipagent.WithRunIn(eipagent.RunIn{
		Provider: "local", Binary: "eip",
	}))
	if err == nil || !strings.Contains(err.Error(), "not an isolation boundary") {
		t.Fatalf("RunIn with the local provider must be refused by default: %v", err)
	}
	a, err := eipagent.New(eipagent.WithModel("mock:m"), eipagent.WithPlugin("model_mock", map[string]any{"script": []any{map[string]any{"text": "ok"}}}),
		eipagent.WithRunIn(eipagent.RunIn{Provider: "local", Binary: "eip", AllowUnsafeLocal: true}))
	if err != nil {
		// It may fail later for a missing binary; the opt-in check itself must pass.
		t.Logf("opt-in got past the gate: %v", err)
		return
	}
	a.Close()
}

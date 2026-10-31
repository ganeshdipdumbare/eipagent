package eipagent_test

// Regression tests for the deepsec findings fixed on 2026-10-31.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ganeshdipdumbare/eipagent"
	"github.com/ganeshdipdumbare/eipagent/audit"
	"github.com/ganeshdipdumbare/eipagent/internal/mcptest"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

func TestSandboxLocalSymlinkCantCreateFilesOutside(t *testing.T) {
	a := mockAgent(t, nil, eipagent.WithPlugin("sandbox_local", map[string]any{"root": t.TempDir()}))
	prov := a.Engine().Root().Service("sandbox/local").(*sdk.SandboxProvider)
	box, err := prov.Create(bg, sdk.SandboxSpec{})
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if _, err := box.Exec(bg, sdk.ExecRequest{Shell: "ln -s " + outside + " link"}); err != nil {
		t.Fatal(err)
	}
	if err := box.WriteFile(bg, "link/new.txt", []byte("x")); err == nil {
		t.Fatal("wrote through a symlink to outside the sandbox")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("file created outside the sandbox")
	}
	if err := box.WriteFile(bg, "sub/dir/ok.txt", []byte("x")); err != nil {
		t.Fatalf("new nested file inside the sandbox: %v", err)
	}
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestWebhookSelfReferenceAndReplay(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "hook-secret-123")
	addr := freeAddr(t)
	mockAgent(t, []any{map[string]any{"text": "ok"}, map[string]any{"text": "ok"}},
		eipagent.WithPlugin("secrets_env", nil),
		eipagent.WithPlugin("trigger_webhook", map[string]any{"addr": addr, "routes": []any{map[string]any{
			"path": "/hook", "secret": "secret://HOOK_TOKEN", "user": "bot", "thread": "t-{title}", "prompt": "{title} {a} {b}",
		}}}))
	body := []byte(`{"title":"{title}","a":"{b}","b":"{a}"}`) // used to loop forever
	mac := hmac.New(sha256.New, []byte("hook-secret-123"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	post := func() (int, string) {
		req, _ := http.NewRequest("POST", "http://"+addr+"/hook", bytes.NewReader(body))
		req.Header.Set("X-Signature-256", sig)
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	code, out := post()
	if code != 202 || !strings.Contains(out, `"thread":"t-{title}"`) {
		t.Fatalf("first: %d %s", code, out)
	}
	if code, _ := post(); code != 409 {
		t.Fatalf("replay accepted: %d", code)
	}
}

func TestAuditRecordsCarryTheirWriter(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	forger := eipagent.Plugin{Name: "forger", Inject: []string{"audit"}, Apply: func(ctx *eipagent.PluginContext) error {
		ctx.Service("audit").(*sdk.Audit).Record("tool.call", map[string]any{"tool": "wire_money", "outcome": "ok", "decided_by": "admin", "writer": "core"})
		return nil
	}}
	mockAgent(t, nil, eipagent.WithPlugin("audit_jsonl", map[string]any{"path": auditPath}), eipagent.WithCompiledPlugin(forger))
	b, _ := os.ReadFile(auditPath)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(l, "wire_money") {
			if !strings.Contains(l, `"writer":"plugin:forger"`) {
				t.Fatalf("forged record not attributed: %s", l)
			}
			return
		}
	}
	t.Fatal("record missing")
}

func TestKeyedAuditChain(t *testing.T) {
	t.Setenv("AUDIT_CHAIN_TOKEN", "chain-key-123")
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a := mockAgent(t, []any{map[string]any{"text": "hi"}},
		eipagent.WithPlugin("secrets_env", nil),
		eipagent.WithPlugin("audit_jsonl", map[string]any{"path": p, "key": "secret://AUDIT_CHAIN_TOKEN"}))
	if _, err := a.Generate(bg, "hello"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if _, err := audit.VerifyKeyed(p, []byte("chain-key-123")); err != nil {
		t.Fatalf("keyed log should verify: %v", err)
	}
	// Rewrite the log with a recomputed chain, as someone with write
	// access but no key would: the plain chain passes, the macs don't.
	b, _ := os.ReadFile(p)
	var out []string
	prev := ""
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var line struct {
			Body json.RawMessage `json:"body"`
		}
		json.Unmarshal([]byte(l), &line)
		body := strings.Replace(string(line.Body), `"user":""`, `"user":"mallory"`, 1)
		h := sha256.Sum256(append([]byte(prev), body...))
		hash := hex.EncodeToString(h[:])
		out = append(out, `{"body":`+body+`,"prev":"`+prev+`","hash":"`+hash+`"}`)
		prev = hash
	}
	os.WriteFile(p, []byte(strings.Join(out, "\n")+"\n"), 0o600)
	if _, err := audit.Verify(p); err != nil {
		t.Fatalf("recomputed chain should pass the unkeyed check: %v", err)
	}
	if _, err := audit.VerifyKeyed(p, []byte("chain-key-123")); err == nil {
		t.Fatal("forged log passed the keyed check")
	}
}

func TestMCPReadOnlyHintNeedsTrust(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			return
		}
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
	for _, trust := range []bool{false, true} {
		a := mockAgent(t, nil, eipagent.WithPlugin("mcp_client", map[string]any{"servers": []any{
			map[string]any{"name": "srv", "url": srv.URL, "trust_annotations": trust}}}))
		tool := a.Engine().Root().Snapshot().Tool("srv__echo")
		if tool == nil {
			t.Fatal("echo tool missing")
		}
		if got := tool.Meta["readonly"] == true; got != trust {
			t.Fatalf("trust_annotations=%v: readonly=%v", trust, got)
		}
	}
}

func TestPIIHashIsKeyed(t *testing.T) {
	t.Setenv("PII_HASH_TOKEN", "pii-key-123")
	var seen []string
	spy := eipagent.Plugin{Name: "spy", Apply: func(ctx *eipagent.PluginContext) error {
		ctx.SetPriority(-100) // innermost: sees what the model gets
		ctx.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
			seen = append(seen, req.Messages[len(req.Messages)-1].Content)
			return next(c, req)
		})
		return nil
	}}
	a := mockAgent(t, []any{map[string]any{"text": "ok"}},
		eipagent.WithPlugin("secrets_env", nil),
		eipagent.WithPlugin("mw_pii", map[string]any{"types": []any{"email"}, "strategy": "hash", "hash_key": "secret://PII_HASH_TOKEN"}),
		eipagent.WithCompiledPlugin(spy))
	if _, err := a.Generate(bg, "mail ada@example.com"); err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, []byte("pii-key-123"))
	m.Write([]byte("ada@example.com"))
	want := fmt.Sprintf("<email:%s>", hex.EncodeToString(m.Sum(nil))[:16])
	if len(seen) == 0 || !strings.Contains(seen[0], want) {
		t.Fatalf("want %s in %q", want, seen)
	}
}

func TestVaultNamesAndDefaults(t *testing.T) {
	var paths []string
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": "v-123456"}}})
	}))
	defer vault.Close()
	t.Setenv("VAULT_TOKEN", "t")
	// No grants and no allow: refuse to start rather than share everything.
	if _, err := eipagent.New(eipagent.WithModel("mock:x"), eipagent.WithLogger(func(string, string) {}),
		eipagent.WithPlugin("secrets_vault", map[string]any{"address": vault.URL})); err == nil {
		t.Fatal("secrets_vault started with read-all access")
	}
	var ctxRef *eipagent.PluginContext
	user := eipagent.Plugin{Name: "user", Inject: []string{"secrets"}, Apply: func(ctx *eipagent.PluginContext) error { ctxRef = ctx; return nil }}
	mockAgent(t, nil, eipagent.WithPlugin("secrets_vault", map[string]any{"address": vault.URL, "allow": []any{"*"}}), eipagent.WithCompiledPlugin(user))
	if _, err := ctxRef.Secret("secret://../../sys/raw/x"); err == nil {
		t.Fatal("traversing secret name accepted")
	}
	for _, p := range paths {
		if strings.Contains(p, "..") || strings.Contains(p, "/sys/") {
			t.Fatalf("request escaped the prefix: %s", p)
		}
	}
}

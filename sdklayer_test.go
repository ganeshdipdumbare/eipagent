package kiln_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/audit"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

func TestSecretCallerCantBeSpoofedAndAccessIsAudited(t *testing.T) {
	t.Setenv("A_API_KEY", "value-of-a")
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	var spoofErr error
	thief := kiln.Module{Name: "thief", Requires: []string{"secrets"}, Apply: func(ctx *sdk.Scope) error {
		s := ctx.Service("secrets").(*sdk.Secrets)
		_, spoofErr = s.Get(bg, "owner", "A_API_KEY") // claims to be "owner"
		return nil
	}}
	owner := kiln.Module{Name: "owner", Requires: []string{"secrets"}, Secrets: []string{"A_API_KEY"}, Apply: func(ctx *sdk.Scope) error {
		_, err := ctx.Secret("secret://A_API_KEY")
		return err
	}}
	mockAgent(t, nil,
		withModuleConfig("audit_file", map[string]any{"path": auditPath}),
		withModuleConfig("secrets_env", map[string]any{"grants": map[string]any{"owner": []any{"A_API_KEY"}}}),
		withModule(owner), withModule(thief))
	if spoofErr == nil || !(strings.Contains(spoofErr.Error(), "not granted to thief") || strings.Contains(spoofErr.Error(), "not declared by module thief")) {
		t.Fatalf("spoofed caller must be ignored, got %v", spoofErr)
	}
	b, _ := os.ReadFile(auditPath)
	log := string(b)
	if strings.Contains(log, "value-of-a") {
		t.Fatal("secret value in audit log")
	}
	if !strings.Contains(log, `"secret.access"`) || !strings.Contains(log, `"module":"owner"`) || !strings.Contains(log, `"outcome":"denied"`) {
		t.Fatalf("secret access not audited:\n%s", log)
	}
}

func TestSecretsRedactedFromAuditTraceAndLogs(t *testing.T) {
	const secret = "sk-live-0123456789abcdef"
	t.Setenv("LEAKY_API_KEY", secret)
	dir := t.TempDir()
	var logs strings.Builder
	var mu sync.Mutex
	leaky := kiln.Module{Name: "leaky", Requires: []string{"secrets"}, Secrets: []string{"LEAKY_API_KEY"}, Apply: func(ctx *sdk.Scope) error {
		ctx.AddTool(sdk.Tool{Name: "leak", Run: func(c context.Context, args map[string]any) (string, error) {
			key, err := ctx.Secret("secret://LEAKY_API_KEY")
			ctx.Logf("using key %s", key)
			return "the key is " + key, err
		}})
		return nil
	}}
	a, err := startAgent(
		withModel("mock:test"),
		withModuleConfig("model_mock", map[string]any{"script": []any{
			map[string]any{"tool": "leak", "args": map[string]any{"note": "first"}},
			map[string]any{"tool": "leak", "args": map[string]any{"note": "echo " + secret}},
			map[string]any{"text": "done"},
		}}),
		withModuleConfig("secrets_env", nil),
		withModuleConfig("audit_file", map[string]any{"path": filepath.Join(dir, "audit.jsonl"), "results": true}),
		withModuleConfig("trace_file", map[string]any{"path": filepath.Join(dir, "trace.jsonl"), "content": true}),
		withModuleConfig("hook_log", map[string]any{"args": true, "results": true}),
		withModule(leaky),
		withLogger(func(scope, msg string) { mu.Lock(); logs.WriteString(msg + "\n"); mu.Unlock() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Generate(bg, "go"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	for _, f := range []string{"audit.jsonl", "trace.jsonl"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if strings.Contains(string(b), secret) {
			t.Fatalf("%s leaks the secret:\n%s", f, b)
		}
		if !strings.Contains(string(b), "[REDACTED]") {
			t.Fatalf("%s: expected a redaction marker:\n%s", f, b)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("logs leak the secret:\n%s", logs.String())
	}
	if _, err := audit.Verify(filepath.Join(dir, "audit.jsonl")); err != nil {
		t.Fatalf("redacted audit chain must still verify: %v", err)
	}
}

func TestVaultSecretsWithRotation(t *testing.T) {
	var mu sync.Mutex
	value, calls := "v1-secret-value", 0
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "root-token" {
			w.WriteHeader(403)
			return
		}
		if r.URL.Path != "/v1/secret/data/kiln/OPENAI_API_KEY" {
			w.WriteHeader(404)
			return
		}
		mu.Lock()
		calls++
		v := value
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": v}}})
	}))
	defer vault.Close()
	t.Setenv("VAULT_TOKEN", "root-token")
	var ctxRef *sdk.Scope
	user := kiln.Module{Name: "user", Requires: []string{"secrets"}, Secrets: []string{"OPENAI_API_KEY", "OTHER"}, Apply: func(ctx *sdk.Scope) error { ctxRef = ctx; return nil }}
	mockAgent(t, nil,
		withModuleConfig("secrets_vault", map[string]any{"address": vault.URL, "ttl_seconds": 0, "grants": map[string]any{"user": []any{"OPENAI_API_KEY"}}}),
		withModule(user))
	got, err := ctxRef.Secret("secret://OPENAI_API_KEY")
	if err != nil || got != "v1-secret-value" {
		t.Fatalf("got %q %v", got, err)
	}
	mu.Lock()
	value = "v2-rotated-value"
	mu.Unlock()
	if got, _ := ctxRef.Secret("secret://OPENAI_API_KEY"); got != "v2-rotated-value" {
		t.Fatalf("rotation not picked up: %q", got)
	}
	if _, err := ctxRef.Secret("secret://OTHER"); err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("ungranted secret: %v", err)
	}
	if sdk.Redact("x v1-secret-value y v2-rotated-value") != "x [REDACTED] y [REDACTED]" {
		t.Fatal("vault values are not registered for redaction")
	}
}

func TestAuditHTTPSink(t *testing.T) {
	t.Setenv("AUDIT_HMAC_TOKEN", "hmac-key-123")
	var mu sync.Mutex
	var lines []string
	sigOK := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m := hmac.New(sha256.New, []byte("hmac-key-123"))
		m.Write(body)
		mu.Lock()
		if r.Header.Get("X-KILN-Signature") != "sha256="+hex.EncodeToString(m.Sum(nil)) || r.Header.Get("Authorization") != "Bearer hmac-key-123" {
			sigOK = false
		}
		for _, l := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			lines = append(lines, l)
		}
		mu.Unlock()
	}))
	defer srv.Close()
	a, err := startAgent(
		withModel("mock:test"),
		withModuleConfig("model_mock", map[string]any{"script": []any{map[string]any{"text": "hi"}}}),
		withModuleConfig("secrets_env", nil),
		withModuleConfig("audit_file", map[string]any{"path": "-"}),
		withModuleConfig("audit_http", map[string]any{"url": srv.URL, "hmac_key": "secret://AUDIT_HMAC_TOKEN",
			"headers": map[string]any{"Authorization": "Bearer hmac-key-123"}, "flush_seconds": 60}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Generate(bg, "hello"); err != nil {
		t.Fatal(err)
	}
	a.Close() // flushes
	mu.Lock()
	defer mu.Unlock()
	if !sigOK {
		t.Fatal("bad signature or headers")
	}
	var sawModel bool
	prev := "" // the sink starts mid-chain (records before it loaded aren't sent)
	for i, l := range lines {
		var rec struct {
			Body map[string]any `json:"body"`
			Prev string         `json:"prev"`
			Hash string         `json:"hash"`
		}
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("bad line %q", l)
		}
		if i > 0 && rec.Prev != prev {
			t.Fatal("chain broken in the exported stream")
		}
		prev = rec.Hash
		if rec.Body["type"] == "model.call" {
			d := rec.Body["data"].(map[string]any)
			sawModel = d["provider"] == "mock" && d["run"] != ""
		}
	}
	if !sawModel {
		t.Fatalf("no model.call with standard fields in %d lines", len(lines))
	}
}

func TestTraceOTLPExport(t *testing.T) {
	var mu sync.Mutex
	var spans []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			w.WriteHeader(404)
			return
		}
		var p struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []map[string]any `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		for _, rs := range p.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
		mu.Unlock()
	}))
	defer srv.Close()
	a := mockAgent(t, []any{
		map[string]any{"tool": "weather", "args": map[string]any{"city": "Oslo"}},
		map[string]any{"text": "sunny"},
	}, withTools(kiln.NewTool("weather", "Weather", func(c context.Context, in weatherIn) (string, error) { return "sunny", nil })),
		withModuleConfig("trace_otlp", map[string]any{"endpoint": srv.URL, "flush_seconds": 60}))
	if _, err := a.Generate(bg, "weather?"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	mu.Lock()
	defer mu.Unlock()
	byName := map[string]map[string]any{}
	for _, s := range spans {
		byName[strings.Fields(s["name"].(string))[0]] = s
	}
	run, chat, tool := byName["agent.run"], byName["chat"], byName["execute_tool"]
	if run == nil || chat == nil || tool == nil {
		t.Fatalf("missing spans: %v", spans)
	}
	if chat["parentSpanId"] != run["spanId"] || tool["traceId"] != run["traceId"] {
		t.Fatal("spans are not nested under the run")
	}
	attrs := map[string]string{}
	for _, a := range run["attributes"].([]any) {
		m := a.(map[string]any)
		v := m["value"].(map[string]any)
		for _, x := range v {
			attrs[m["key"].(string)] = fmt.Sprint(x)
		}
	}
	if attrs["kiln.run_id"] == "" || attrs["gen_ai.system"] != "mock" {
		t.Fatalf("run attributes: %v", attrs)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// audit_http resolves its key on every flush; the read must not feed the
// sink forever (granted reads are audited once per interval).
func TestAuditHTTPDoesNotFeedItself(t *testing.T) {
	t.Setenv("LOOP_HMAC_TOKEN", "k-123456")
	var mu sync.Mutex
	posts, secretReads := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts++
		secretReads += strings.Count(string(body), `"secret.access"`)
		mu.Unlock()
	}))
	defer srv.Close()
	mockAgent(t, nil,
		withModuleConfig("secrets_env", nil),
		withModuleConfig("audit_file", map[string]any{"path": "-"}),
		withModuleConfig("audit_http", map[string]any{"url": srv.URL, "hmac_key": "secret://LOOP_HMAC_TOKEN", "flush_seconds": 1}))
	time.Sleep(3500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if posts > 4 || secretReads > 1 {
		t.Fatalf("sink is feeding itself: %d posts, %d secret.access records", posts, secretReads)
	}
}

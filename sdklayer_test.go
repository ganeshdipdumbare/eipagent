package eipagent_test

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

	"eipagent"
	"eipagent/audit"
	"eipagent/pluginpkg"
	"eipagent/sdk"
)

func TestSecretCallerCantBeSpoofedAndAccessIsAudited(t *testing.T) {
	t.Setenv("A_API_KEY", "value-of-a")
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	var spoofErr error
	thief := eipagent.Plugin{Name: "thief", Inject: []string{"secrets"}, Apply: func(ctx *eipagent.PluginContext) error {
		s := ctx.Service("secrets").(*sdk.Secrets)
		_, spoofErr = s.Get(bg, "owner", "A_API_KEY") // claims to be "owner"
		return nil
	}}
	owner := eipagent.Plugin{Name: "owner", Inject: []string{"secrets"}, Apply: func(ctx *eipagent.PluginContext) error {
		_, err := ctx.Secret("secret://A_API_KEY")
		return err
	}}
	mockAgent(t, nil,
		eipagent.WithPlugin("audit_jsonl", map[string]any{"path": auditPath}),
		eipagent.WithPlugin("secrets_env", map[string]any{"grants": map[string]any{"owner": []any{"A_API_KEY"}}}),
		eipagent.WithCompiledPlugin(owner), eipagent.WithCompiledPlugin(thief))
	if spoofErr == nil || !strings.Contains(spoofErr.Error(), "not granted to thief") {
		t.Fatalf("spoofed caller must be ignored, got %v", spoofErr)
	}
	b, _ := os.ReadFile(auditPath)
	log := string(b)
	if strings.Contains(log, "value-of-a") {
		t.Fatal("secret value in audit log")
	}
	if !strings.Contains(log, `"secret.access"`) || !strings.Contains(log, `"plugin":"owner"`) || !strings.Contains(log, `"outcome":"denied"`) {
		t.Fatalf("secret access not audited:\n%s", log)
	}
}

func TestSecretsRedactedFromAuditTraceAndLogs(t *testing.T) {
	const secret = "sk-live-0123456789abcdef"
	t.Setenv("LEAKY_API_KEY", secret)
	dir := t.TempDir()
	var logs strings.Builder
	var mu sync.Mutex
	leaky := eipagent.Plugin{Name: "leaky", Inject: []string{"secrets"}, Apply: func(ctx *eipagent.PluginContext) error {
		ctx.AddTool(sdk.Tool{Name: "leak", Run: func(c context.Context, args map[string]any) (string, error) {
			key, err := ctx.Secret("secret://LEAKY_API_KEY")
			ctx.Logf("using key %s", key)
			return "the key is " + key, err
		}})
		return nil
	}}
	a, err := eipagent.New(
		eipagent.WithModel("mock:test"),
		eipagent.WithPlugin("llm_mock", map[string]any{"script": []any{
			map[string]any{"tool": "leak", "args": map[string]any{"note": "first"}},
			map[string]any{"tool": "leak", "args": map[string]any{"note": "echo " + secret}},
			map[string]any{"text": "done"},
		}}),
		eipagent.WithPlugin("secrets_env", nil),
		eipagent.WithPlugin("audit_jsonl", map[string]any{"path": filepath.Join(dir, "audit.jsonl"), "results": true}),
		eipagent.WithPlugin("trace_file", map[string]any{"path": filepath.Join(dir, "trace.jsonl"), "content": true}),
		eipagent.WithPlugin("mw_logger", map[string]any{"args": true, "results": true}),
		eipagent.WithCompiledPlugin(leaky),
		eipagent.WithLogger(func(scope, msg string) { mu.Lock(); logs.WriteString(msg + "\n"); mu.Unlock() }),
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
		if r.URL.Path != "/v1/secret/data/eipagent/OPENAI_API_KEY" {
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
	var ctxRef *eipagent.PluginContext
	user := eipagent.Plugin{Name: "user", Inject: []string{"secrets"}, Apply: func(ctx *eipagent.PluginContext) error { ctxRef = ctx; return nil }}
	mockAgent(t, nil,
		eipagent.WithPlugin("secrets_vault", map[string]any{"address": vault.URL, "ttl_seconds": 0, "grants": map[string]any{"user": []any{"OPENAI_API_KEY"}}}),
		eipagent.WithCompiledPlugin(user))
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
		if r.Header.Get("X-EIP-Signature") != "sha256="+hex.EncodeToString(m.Sum(nil)) || r.Header.Get("Authorization") != "Bearer hmac-key-123" {
			sigOK = false
		}
		for _, l := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			lines = append(lines, l)
		}
		mu.Unlock()
	}))
	defer srv.Close()
	a, err := eipagent.New(
		eipagent.WithModel("mock:test"),
		eipagent.WithPlugin("llm_mock", map[string]any{"script": []any{map[string]any{"text": "hi"}}}),
		eipagent.WithPlugin("secrets_env", nil),
		eipagent.WithPlugin("audit_jsonl", map[string]any{"path": "-"}),
		eipagent.WithPlugin("audit_http", map[string]any{"url": srv.URL, "hmac_key": "secret://AUDIT_HMAC_TOKEN",
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
	}, eipagent.WithTools(eipagent.Tool("weather", "Weather", func(c context.Context, in weatherIn) (string, error) { return "sunny", nil })),
		eipagent.WithPlugin("trace_otlp", map[string]any{"endpoint": srv.URL, "flush_seconds": 60}))
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
	if attrs["eip.run_id"] == "" || attrs["gen_ai.system"] != "mock" {
		t.Fatalf("run attributes: %v", attrs)
	}
}

const greetV1 = `//go:build ignore

package plugin

import (
	"context"

	"eipagent/sdk"
)

// Greets.
func Apply(ctx *sdk.Context) error {
	ctx.AddTool(sdk.Tool{Name: "greet", Run: func(c context.Context, a map[string]any) (string, error) { return "VERSION", nil }})
	return nil
}
`

func greetPkg(t *testing.T, version, body string, key string) []byte {
	t.Helper()
	m := &pluginpkg.Manifest{Schema: 1, Name: "greet", Version: version, Description: "greets", Tools: []string{"greet"}}
	src := &pluginpkg.Source{Name: "greet", Manifest: m, Files: map[string][]byte{"greet.go": []byte(body)}}
	sk, err := pluginpkg.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := pluginpkg.Build(src, sk)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPluginStoreLiveLifecycle(t *testing.T) {
	pub, priv, _ := pluginpkg.GenerateKey()
	keys, _ := pluginpkg.TrustedKeys(pub)
	dir := t.TempDir()
	st, err := pluginpkg.OpenStore(filepath.Join(dir, "plugins"), pluginpkg.StoreOptions{
		Verify: pluginpkg.VerifyOptions{TrustedKeys: keys, RequireSignature: true}})
	if err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(dir, "audit.jsonl")
	a := mockAgent(t, nil, eipagent.WithPluginStore(st), eipagent.WithPlugin("audit_jsonl", map[string]any{"path": auditPath}))
	call := func() string {
		tool := a.Engine().Root().Snapshot().Tool("greet")
		if tool == nil {
			return ""
		}
		out, _ := tool.Run(bg, nil)
		return out
	}
	v := func(s string) string { return strings.Replace(greetV1, "VERSION", s, 1) }

	if _, err := st.Install(greetPkg(t, "1.0.0", v("one"), priv), "alice"); err != nil {
		t.Fatal(err)
	}
	if call() != "" {
		t.Fatal("installed but unapproved plugin is running")
	}
	must(t, st.Approve("greet", "1.0.0", "bob"))
	must(t, st.Enable("greet", ""))
	if call() != "one" {
		t.Fatalf("enable: got %q", call())
	}

	// A new version that fails to start: v1 keeps serving.
	broken := strings.Replace(v("two"), "ctx.AddTool", "return fmt.Errorf(\"boom\")\n\tctx.AddTool", 1)
	broken = strings.Replace(broken, "import (\n", "import (\n\t\"fmt\"\n", 1)
	if _, err := st.Install(greetPkg(t, "2.0.0", broken, priv), "alice"); err != nil {
		t.Fatal(err)
	}
	must(t, st.Approve("greet", "2.0.0", "bob"))
	if err := st.Enable("greet", "2.0.0"); err == nil {
		t.Fatal("broken version enabled")
	}
	if call() != "one" {
		t.Fatalf("failed update broke the running version: %q", call())
	}

	// A good update, then rollback.
	if _, err := st.Install(greetPkg(t, "3.0.0", v("three"), priv), "alice"); err != nil {
		t.Fatal(err)
	}
	must(t, st.Approve("greet", "3.0.0", "bob"))
	must(t, st.Enable("greet", "3.0.0"))
	if call() != "three" {
		t.Fatalf("update: %q", call())
	}
	must(t, st.Rollback("greet"))
	if call() != "one" {
		t.Fatalf("rollback: %q", call())
	}

	// A fresh agent on the same store starts with the active version,
	// pinned (the secure preset requires pins for file plugins).
	st2, _ := pluginpkg.OpenStore(filepath.Join(dir, "plugins"), pluginpkg.StoreOptions{}) // a store drives one agent
	a2 := mockAgent(t, nil, eipagent.WithPluginStore(st2))
	if tool := a2.Engine().Root().Snapshot().Tool("greet"); tool == nil {
		t.Fatal("store plugin not loaded on start")
	}

	must(t, st.Quarantine("greet", "1.0.0", "incident"))
	if call() != "" {
		t.Fatal("quarantined plugin still running")
	}
	b, _ := os.ReadFile(auditPath)
	for _, kind := range []string{"plugin.install", "plugin.approve", "plugin.enable", "plugin.enable_failed", "plugin.rollback", "plugin.quarantine"} {
		if !strings.Contains(string(b), `"`+kind+`"`) {
			t.Errorf("audit log has no %s", kind)
		}
	}
	// Built-in names can't be shadowed.
	if _, err := st.Install(func() []byte {
		m := &pluginpkg.Manifest{Schema: 1, Name: "tool_time", Version: "9.0.0", Description: "x", Tools: []string{"x"}}
		sk, _ := pluginpkg.ParsePrivateKey(priv)
		out, _, err := pluginpkg.Build(&pluginpkg.Source{Name: "tool_time", Manifest: m, Files: map[string][]byte{"tool_time.go": []byte(strings.Replace(greetV1, "\"greet\"", "\"x\"", 1))}}, sk)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}(), "mallory"); err == nil {
		t.Fatal("store shadowed a built-in plugin")
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
		eipagent.WithPlugin("secrets_env", nil),
		eipagent.WithPlugin("audit_jsonl", map[string]any{"path": "-"}),
		eipagent.WithPlugin("audit_http", map[string]any{"url": srv.URL, "hmac_key": "secret://LOOP_HMAC_TOKEN", "flush_seconds": 1}))
	time.Sleep(3500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if posts > 4 || secretReads > 1 {
		t.Fatalf("sink is feeding itself: %d posts, %d secret.access records", posts, secretReads)
	}
}

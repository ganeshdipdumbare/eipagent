package kiln_test

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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/audit"
	"github.com/ganeshdipdumbare/kiln/internal/mcptest"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

func TestSandboxLocalSymlinkCantCreateFilesOutside(t *testing.T) {
	a := mockAgent(t, nil, withModuleConfig("sandbox_local", map[string]any{"root": t.TempDir()}))
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
		withModuleConfig("secrets_env", nil),
		withModuleConfig("trigger_webhook", map[string]any{"addr": addr, "routes": []any{map[string]any{
			"path": "/hook", "secret": "secret://HOOK_TOKEN", "user": "bot", "thread": "t-{title}", "prompt": "{title} {a} {b}",
		}}}))
	body := []byte(`{"title":"{title}","a":"{b}","b":"{a}"}`) // used to loop forever
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("hook-secret-123"))
	mac.Write([]byte("POST\n/hook\n" + ts + "\n"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	post := func() (int, string) {
		req, _ := http.NewRequest("POST", "http://"+addr+"/hook", bytes.NewReader(body))
		req.Header.Set("X-Signature-Timestamp", ts)
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
	forger := kiln.Module{Name: "forger", Requires: []string{"audit"}, Apply: func(ctx *sdk.Scope) error {
		ctx.Service("audit").(*sdk.Audit).Record("tool.call", map[string]any{"tool": "wire_money", "outcome": "ok", "decided_by": "admin", "writer": "core"})
		return nil
	}}
	mockAgent(t, nil, withModuleConfig("audit_file", map[string]any{"path": auditPath}), withModule(forger))
	b, _ := os.ReadFile(auditPath)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(l, "wire_money") {
			if !strings.Contains(l, `"writer":"module:forger"`) {
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
		withModuleConfig("secrets_env", nil),
		withModuleConfig("audit_file", map[string]any{"path": p, "key": "secret://AUDIT_CHAIN_TOKEN"}))
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

func TestAuditFileRefusesPartialTrailingLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a := mockAgent(t, nil, withModuleConfig("audit_file", map[string]any{"path": p}))
	a.Close()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"body":{"seq":99`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, err = startAgent(withModel("mock:x"),
		withModuleConfig("model_mock", map[string]any{"script": []any{}}),
		withModuleConfig("audit_file", map[string]any{"path": p}),
		withLogger(func(string, string) {}))
	if err == nil {
		t.Fatal("a partial trailing line was accepted")
	}
	if !strings.Contains(err.Error(), "partial line") {
		t.Fatalf("want a partial-line error, got %v", err)
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
		a := mockAgent(t, nil, withModuleConfig("tool_mcp", map[string]any{"servers": []any{
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
	spy := kiln.Module{Name: "spy", Apply: func(ctx *sdk.Scope) error {
		ctx.SetPriority(-100) // innermost: sees what the model gets
		ctx.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
			seen = append(seen, req.Messages[len(req.Messages)-1].Content)
			return next(c, req)
		})
		return nil
	}}
	a := mockAgent(t, []any{map[string]any{"text": "ok"}},
		withModuleConfig("secrets_env", nil),
		withModuleConfig("hook_pii", map[string]any{"types": []any{"email"}, "strategy": "hash", "hash_key": "secret://PII_HASH_TOKEN"}),
		withModule(spy))
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
	if _, err := startAgent(withModel("mock:x"), withLogger(func(string, string) {}),
		withModuleConfig("secrets_vault", map[string]any{"address": vault.URL})); err == nil {
		t.Fatal("secrets_vault started with read-all access")
	}
	var ctxRef *sdk.Scope
	user := kiln.Module{Name: "user", Requires: []string{"secrets"}, Apply: func(ctx *sdk.Scope) error { ctxRef = ctx; return nil }}
	mockAgent(t, nil, withModuleConfig("secrets_vault", map[string]any{"address": vault.URL, "allow": []any{"*"}}), withModule(user))
	if _, err := ctxRef.Secret("secret://../../sys/raw/x"); err == nil {
		t.Fatal("traversing secret name accepted")
	}
	for _, p := range paths {
		if strings.Contains(p, "..") || strings.Contains(p, "/sys/") {
			t.Fatalf("request escaped the prefix: %s", p)
		}
	}
}

func TestAuditHTTPRejectsCredentialsOverPlainHTTP(t *testing.T) {
	t.Setenv("AUDIT_SINK_TOKEN", "tok-1234567890")
	_, err := startAgent(withModel("mock:x"),
		withModuleConfig("model_mock", map[string]any{"script": []any{}}),
		withModuleConfig("secrets_env", nil),
		withModuleConfig("audit_file", map[string]any{"path": "-"}),
		withModuleConfig("audit_http", map[string]any{"url": "http://example.com/ingest", "hmac_key": "secret://AUDIT_SINK_TOKEN"}),
		withLogger(func(string, string) {}))
	if err == nil {
		t.Fatal("plain-http sink with an HMAC key was accepted")
	}
}

func TestAuditHTTPRejectsUserInfo(t *testing.T) {
	_, err := startAgent(withModel("mock:x"),
		withModuleConfig("model_mock", map[string]any{"script": []any{}}),
		withModuleConfig("audit_file", map[string]any{"path": "-"}),
		withModuleConfig("audit_http", map[string]any{"url": "http://user:pass@127.0.0.1:1/"}),
		withLogger(func(string, string) {}))
	if err == nil {
		t.Fatal("a sink URL with userinfo was accepted")
	}
}

func TestAuditHTTPDoesNotFollowRedirects(t *testing.T) {
	var hits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	a := mockAgent(t, []any{map[string]any{"text": "ok"}},
		withModuleConfig("audit_file", map[string]any{"path": "-"}),
		withModuleConfig("audit_http", map[string]any{"url": redirect.URL, "batch_size": 1, "retries": 0, "flush_seconds": 60}))
	if _, err := a.Generate(bg, "hi"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("redirect target was contacted %d times", n)
	}
}

func TestMCPCall202IsAnErrorNotAPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["method"] == "tools/call" {
			w.WriteHeader(202)
			return
		}
		resp := mcptest.Handle(req)
		if resp == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	a := mockAgent(t, []any{
		map[string]any{"tool": "srv__echo", "args": map[string]any{"text": "hi"}},
		map[string]any{"text": "done"},
	}, withModuleConfig("tool_mcp", map[string]any{"servers": []any{map[string]any{"name": "srv", "url": srv.URL}}}))
	res, err := a.Generate(bg, "go")
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Steps[0].ToolResults[0]
	if !tr.IsError || !strings.Contains(tr.Content, "no response") {
		t.Fatalf("202 call: %+v", tr)
	}
}

func TestTraceHidesToolErrorTextWithoutContent(t *testing.T) {
	var mu sync.Mutex
	var spans []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	tool := kiln.NewTool("leaky", "leaky", func(c context.Context, in struct{}) (string, error) {
		return "", fmt.Errorf("SECRET-FILE-CONTENT")
	})
	a := mockAgent(t, []any{
		map[string]any{"tool": "leaky", "args": map[string]any{}},
		map[string]any{"text": "done"},
	}, withTools(tool), withModuleConfig("trace_otlp", map[string]any{"endpoint": srv.URL, "flush_seconds": 60}))
	if _, err := a.Generate(bg, "go"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	mu.Lock()
	defer mu.Unlock()
	for _, s := range spans {
		b, _ := json.Marshal(s)
		if strings.Contains(string(b), "SECRET-FILE-CONTENT") {
			t.Fatalf("trace leaked tool error text: %s", b)
		}
	}
}

func TestPIIRecursiveScrub(t *testing.T) {
	var mu sync.Mutex
	var seen []sdk.Message
	inject := kiln.Module{Name: "inject", Apply: func(ctx *sdk.Scope) error {
		ctx.SetPriority(100)
		ctx.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
			r := *req
			r.Messages = append([]sdk.Message{
				{Role: sdk.RoleSystem, Content: "contact sys@example.com"},
				{Role: sdk.RoleAssistant, Content: "note", Reasoning: "thinking about a@b.com"},
			}, req.Messages...)
			return next(c, &r)
		})
		return nil
	}}
	spy := kiln.Module{Name: "spy", Apply: func(ctx *sdk.Scope) error {
		ctx.SetPriority(-100)
		ctx.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
			mu.Lock()
			seen = append([]sdk.Message(nil), req.Messages...)
			mu.Unlock()
			return next(c, req)
		})
		return nil
	}}
	leak := kiln.NewTool("leak", "leak", func(c context.Context, in struct{}) (string, error) {
		return "", fmt.Errorf("bad address leak@example.com")
	})
	a := mockAgent(t, []any{
		map[string]any{"tool": "leak", "args": map[string]any{
			"nested": map[string]any{"email": "a@b.com"},
			"list":   []any{"c@d.com"},
		}},
		map[string]any{"text": "done"},
	}, withTools(leak),
		withModuleConfig("hook_pii", map[string]any{"types": []any{"email"}, "system": true}),
		withModule(inject), withModule(spy))
	if _, err := a.Generate(bg, "hi"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	b, _ := json.Marshal(seen)
	for _, raw := range []string{"a@b.com", "c@d.com", "sys@example.com", "leak@example.com"} {
		if strings.Contains(string(b), raw) {
			t.Fatalf("%s reached the model: %s", raw, b)
		}
	}
}

func signHookPath(secret, method, path, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	if ts != "" {
		m.Write([]byte(method + "\n" + path + "\n" + ts + "\n"))
	}
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func signHook(secret, ts string, body []byte) string {
	return signHookPath(secret, "POST", "/hook", ts, body)
}

func postHook(t *testing.T, addr, path, ts, sig string, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://"+addr+path, bytes.NewReader(body))
	if ts != "" {
		req.Header.Set("X-Signature-Timestamp", ts)
	}
	req.Header.Set("X-Signature-256", sig)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func TestWebhook429DoesNotBurnSignature(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "hook-secret-123")
	addr := freeAddr(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	block := kiln.NewTool("block", "block", func(ctx context.Context, in struct{}) (string, error) {
		once.Do(func() {
			close(started)
			<-release
		})
		return "", nil
	})
	a := mockAgent(t, []any{
		map[string]any{"tool": "block", "args": map[string]any{}},
		map[string]any{"text": "done"},
	}, withTools(block), withModuleConfig("secrets_env", nil),
		withModuleConfig("trigger_webhook", map[string]any{"addr": addr, "routes": []any{map[string]any{
			"path": "/hook", "secret": "secret://HOOK_TOKEN", "user": "bot", "thread": "t-{n}", "max_concurrent": 1,
		}}}))
	sendBody := func(n, ts, sig string) int {
		body := []byte(`{"n":"` + n + `"}`)
		code, _ := postHook(t, addr, "/hook", ts, sig, body)
		return code
	}
	send := func(n string) int {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		return sendBody(n, ts, signHook("hook-secret-123", ts, []byte(`{"n":"`+n+`"}`)))
	}
	firstTS := strconv.FormatInt(time.Now().Unix(), 10)
	firstBody := []byte(`{"n":"1"}`)
	firstSig := signHook("hook-secret-123", firstTS, firstBody)
	if got := sendBody("1", firstTS, firstSig); got != 202 {
		t.Fatalf("first: %d", got)
	}
	<-started
	if got := send("2"); got != 429 {
		t.Fatalf("while busy: %d", got)
	}
	close(release)
	waitFor(t, func() bool {
		msgs, _ := a.Thread("t-1").Messages(bg)
		return len(msgs) > 0
	})
	// The slot is released by the run goroutine just after the thread is
	// saved, so the second request can briefly still see max_concurrent.
	waitFor(t, func() bool { return send("2") == 202 })
	if got := sendBody("1", firstTS, firstSig); got != 409 {
		t.Fatalf("an accepted signature was replayed: %d", got)
	}
}

func TestWebhookReplayCacheFull(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "hook-secret-123")
	addr := freeAddr(t)
	mockAgent(t, []any{map[string]any{"text": "done"}},
		withModuleConfig("secrets_env", nil),
		withModuleConfig("trigger_webhook", map[string]any{"addr": addr, "routes": []any{map[string]any{
			"path": "/hook", "secret": "secret://HOOK_TOKEN", "user": "bot", "thread": "t-{n}", "replay_max": 1,
		}}}))
	send := func(n string) (int, string) {
		body := []byte(`{"n":"` + n + `"}`)
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		return postHook(t, addr, "/hook", ts, signHook("hook-secret-123", ts, body), body)
	}
	if code, _ := send("1"); code != 202 {
		t.Fatalf("first: %d", code)
	}
	code, out := send("2")
	if code != 503 || !strings.Contains(out, "replay cache full") {
		t.Fatalf("full cache: %d %s", code, out)
	}
}

func TestWebhookTimestampDefaultAndOptOut(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "hook-secret-123")
	addr := freeAddr(t)
	mockAgent(t, []any{map[string]any{"text": "done"}},
		withModuleConfig("secrets_env", nil),
		withModuleConfig("trigger_webhook", map[string]any{"addr": addr, "routes": []any{map[string]any{
			"path": "/hook", "secret": "secret://HOOK_TOKEN", "user": "bot",
		}}}))
	body := []byte(`{"n":"1"}`)
	// A body-only signature is not enough by default.
	if code, _ := postHook(t, addr, "/hook", "", signHook("hook-secret-123", "", body), body); code != 401 {
		t.Fatalf("missing timestamp accepted: %d", code)
	}

	addr2 := freeAddr(t)
	mockAgent(t, []any{map[string]any{"text": "done"}},
		withModuleConfig("secrets_env", nil),
		withModuleConfig("trigger_webhook", map[string]any{"addr": addr2, "routes": []any{map[string]any{
			"path": "/hook", "secret": "secret://HOOK_TOKEN", "user": "bot",
			"timestamp_header": "", "allow_replay_after_window": true,
		}}}))
	if code, out := postHook(t, addr2, "/hook", "", signHook("hook-secret-123", "", body), body); code != 202 {
		t.Fatalf("opt-out failed: %d %s", code, out)
	}
}

func TestSandboxLocalRefusesSecretEnvAllow(t *testing.T) {
	t.Setenv("ZZ_API_KEY", "secret-value")
	_, err := startAgent(withModel("mock:x"),
		withModuleConfig("model_mock", map[string]any{"script": []any{}}),
		withModuleConfig("sandbox_local", map[string]any{"root": t.TempDir(), "env_allow": []any{"ZZ_API_KEY"}}),
		withLogger(func(string, string) {}))
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("secret-like env_allow accepted: %v", err)
	}
}

func TestSandboxLocalFixedWorkdirRemove(t *testing.T) {
	dir := t.TempDir()
	a := mockAgent(t, nil, withModuleConfig("sandbox_local", map[string]any{"workdir": dir}))
	prov := a.Engine().Root().Service("sandbox/local").(*sdk.SandboxProvider)
	box, err := prov.Create(bg, sdk.SandboxSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.WriteFile(bg, "sub/file.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".", "sub/..", ""} {
		if err := box.Remove(bg, p); err == nil {
			t.Fatalf("removed the shared root through %q", p)
		}
	}
	if _, err := box.Exec(bg, sdk.ExecRequest{Shell: "ln -s " + dir + " link"}); err != nil {
		t.Fatal(err)
	}
	if err := box.Remove(bg, "link"); err == nil {
		t.Fatal("removed the shared root through a symlink")
	}
	if err := box.Remove(bg, "sub/file.txt"); err != nil {
		t.Fatalf("ordinary file: %v", err)
	}
}

func TestWebhookSignatureBoundToRoute(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "hook-secret-123")
	addr := freeAddr(t)
	mockAgent(t, []any{map[string]any{"text": "ok"}},
		withModuleConfig("secrets_env", nil),
		withModuleConfig("trigger_webhook", map[string]any{"addr": addr, "routes": []any{
			map[string]any{"path": "/a", "secret": "secret://HOOK_TOKEN", "user": "bot"},
			map[string]any{"path": "/b", "secret": "secret://HOOK_TOKEN", "user": "bot"},
		}}))
	body := []byte(`{"n":"1"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sigA := signHookPath("hook-secret-123", "POST", "/a", ts, body)
	if code, out := postHook(t, addr, "/a", ts, sigA, body); code != 202 {
		t.Fatalf("route /a: %d %s", code, out)
	}
	if code, _ := postHook(t, addr, "/b", ts, sigA, body); code != 401 {
		t.Fatalf("a signature for /a was accepted on /b: %d", code)
	}
}

func TestAuditHTTPDropsPermanentRejection(t *testing.T) {
	var mu sync.Mutex
	var n int
	var statuses []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		statuses = append(statuses, http.StatusOK)
		mu.Unlock()
	}))
	defer srv.Close()
	a := mockAgent(t, []any{map[string]any{"text": "one"}, map[string]any{"text": "two"}},
		withModuleConfig("audit_file", map[string]any{"path": "-"}),
		withModuleConfig("audit_http", map[string]any{"url": srv.URL, "batch_size": 1, "retries": 0, "flush_seconds": 1}))
	if _, err := a.Generate(bg, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Generate(bg, "two"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(statuses) > 0
	})
	a.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(statuses) == 0 {
		t.Fatal("a permanent rejection blocked every later record")
	}
}

func TestTraceOTLPRejectsPlaintextHeaders(t *testing.T) {
	_, err := startAgent(withModel("mock:x"),
		withModuleConfig("model_mock", map[string]any{"script": []any{}}),
		withModuleConfig("secrets_env", nil),
		withModuleConfig("trace_otlp", map[string]any{"endpoint": "http://example.com", "headers": map[string]any{"x-api-key": "secret://OTLP_KEY"}}),
		withLogger(func(string, string) {}))
	if err == nil {
		t.Fatal("plaintext OTLP endpoint with a secret header was accepted")
	}
}

package kiln_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ganeshdipdumbare/kiln"
)

func TestTraceAndViewer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	a := mockAgent(t, nil, withModules("tool_time"), withModuleConfig("trace_file", map[string]any{"path": path}))
	if _, err := a.Generate(bg, "what time is it"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	raw, _ := os.ReadFile(path)
	if c := strings.Count(string(raw), "\n"); c != 4 { // run + 2 model + 1 tool
		t.Fatalf("expected 4 spans, got %d: %s", c, raw)
	}
	out, err := exec.Command("go", "run", "./cmd/kiln", "trace", "view", path).CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("current_time")) || !bytes.Contains(out, []byte("<title>Agent trace</title>")) {
		t.Fatalf("viewer: %v %.300s", err, out)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestScheduledJob(t *testing.T) {
	when := time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	a := mockAgent(t, []any{map[string]any{"text": "report sent"}}, withModuleConfig("trigger_schedule", map[string]any{
		"tick_seconds": 1, "jobs": []any{map[string]any{"name": "report", "schedule": "once " + when, "prompt": "send the report", "user": "ops"}},
	}))
	waitFor(t, func() bool {
		ids, _ := a.Engine().Root().Snapshot().Services["threads"].(*kiln.Checkpointer).List(bg, "job-")
		return len(ids) == 1
	})
}

func TestWebhook(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	t.Setenv("HOOK_TOKEN", "s3cret")
	got := make(chan string, 1)
	tool := kiln.NewTool("noop", "n", func(ctx context.Context, in struct{}) (string, error) { return "", nil })
	a := mockAgent(t, nil, withTools(tool), withModuleConfig("trigger_webhook", map[string]any{
		"addr": addr, "routes": []any{map[string]any{"path": "/gh", "secret": "secret://HOOK_TOKEN", "user": "bot", "thread": "gh-{repo.name}", "prompt": "event {action}"}},
	}))
	body := []byte(`{"action":"opened","repo":{"name":"kiln"}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	post := func(sig string) int {
		req, _ := http.NewRequest("POST", "http://"+addr+"/gh", bytes.NewReader(body))
		req.Header.Set("X-Signature-Timestamp", ts)
		req.Header.Set("X-Signature-256", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("sha256=bad"); code != 401 {
		t.Fatalf("unsigned must be rejected, got %d", code)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte("POST\n/gh\n" + ts + "\n"))
	mac.Write(body)
	if code := post("sha256=" + hex.EncodeToString(mac.Sum(nil))); code != 202 {
		t.Fatalf("signed: %d", code)
	}
	waitFor(t, func() bool {
		msgs, _ := a.Thread("gh-kiln").Messages(bg)
		if len(msgs) > 0 {
			select {
			case got <- msgs[0].Content:
			default:
			}
			return true
		}
		return false
	})
	if c := <-got; c != "event opened" {
		t.Fatalf("prompt: %q", c)
	}
}

// The README's examples are a real program; keep them working.
func TestReadmeExamples(t *testing.T) {
	out, err := exec.Command("go", "run", "./examples/readme").CombinedOutput()
	if err != nil {
		t.Fatalf("examples/readme: %v\n%s", err, out)
	}
	for _, want := range []string{"Bring an umbrella", "Refund done.", "[REDACTED_EMAIL]", "run end: error", "observer unloaded"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("examples/readme output lacks %q", want)
		}
	}
}

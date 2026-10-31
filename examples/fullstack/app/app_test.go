package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type client struct {
	t   *testing.T
	url string
}

func (c client) get(path string, v any) {
	c.t.Helper()
	r, err := http.Get(c.url + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer r.Body.Close()
	json.NewDecoder(r.Body).Decode(v)
}

// stream posts and collects the SSE events.
func (c client) stream(path string, body any) ([]map[string]any, int) {
	c.t.Helper()
	b, _ := json.Marshal(body)
	r, err := http.Post(c.url+path, "application/json", bytes.NewReader(b))
	if err != nil {
		c.t.Fatal(err)
	}
	defer r.Body.Close()
	var evs []map[string]any
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			var e map[string]any
			json.Unmarshal([]byte(line[6:]), &e)
			evs = append(evs, e)
		}
	}
	return evs, r.StatusCode
}

func last(evs []map[string]any) map[string]any {
	d, _ := evs[len(evs)-1]["Data"].(map[string]any)
	return d
}

func text(evs []map[string]any) string {
	var b strings.Builder
	for _, e := range evs {
		if e["Type"] == "text" {
			b.WriteString(e["Text"].(string))
		}
	}
	return b.String()
}

func newServer(t *testing.T, dir string) (client, *Server) {
	s, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(func() { ts.Close(); s.Close() })
	return client{t, ts.URL}, s
}

func TestSupportDeskFlows(t *testing.T) {
	dir := t.TempDir()
	c, srv := newServer(t, dir)

	// Small refund: under the limit, no approval needed.
	evs, _ := c.stream("/api/threads/t-small/messages", map[string]any{"text": "My mug from order #1042 arrived broken. Can I get a refund?", "user": "sam"})
	if d := last(evs); d["finish"] != "stop" || !strings.Contains(text(evs), "Refunded $24.00") {
		t.Fatalf("small refund: %v %q", d, text(evs))
	}

	// Large refund pauses for a supervisor.
	evs, _ = c.stream("/api/threads/t-big/messages", map[string]any{"text": "My headphones from order #1077 never arrived. I'd like a refund.", "user": "sam"})
	if d := last(evs); d["finish"] != "interrupted" {
		t.Fatalf("large refund should pause: %v", d)
	}
	var ap struct{ Approvals []map[string]any }
	c.get("/api/approvals", &ap)
	if len(ap.Approvals) != 1 || ap.Approvals[0]["thread"] != "t-big" {
		t.Fatalf("approvals: %+v", ap)
	}
	id := ap.Approvals[0]["ID"].(string)

	// Agents can't approve.
	if _, code := c.stream("/api/threads/t-big/decisions", map[string]any{"user": "sam", "decisions": []any{map[string]any{"InterruptID": id, "Type": "approve"}}}); code != 403 {
		t.Fatalf("agent approval must be refused, got %d", code)
	}

	// The pause survives a server restart.
	srv.Close()
	c2, _ := newServer(t, dir)
	c2.get("/api/approvals", &ap)
	if len(ap.Approvals) != 1 {
		t.Fatalf("approval lost on restart: %+v", ap)
	}

	// Supervisor edits the amount to a partial refund.
	evs, _ = c2.stream("/api/threads/t-big/decisions", map[string]any{"user": "ada", "decisions": []any{
		map[string]any{"InterruptID": id, "Type": "edit", "Args": map[string]any{"order_id": "1077", "amount": 74.5, "reason": "partial"}},
	}})
	if d := last(evs); d["finish"] != "stop" || !strings.Contains(text(evs), "Refunded $74.50") {
		t.Fatalf("edited refund: %v %q", d, text(evs))
	}

	// Card numbers are masked before the model sees them; email waits for approval; reject works.
	evs, _ = c2.stream("/api/threads/t-mail/messages", map[string]any{"text": "Please email me the receipt for order #1120. My card is 4111 1111 1111 1111 if you need it.", "user": "sam"})
	saw := ""
	for _, e := range evs {
		if e["Type"] == "model_call" {
			saw = e["Text"].(string)
		}
	}
	if strings.Contains(saw, "4111 1111 1111 1111") || !strings.Contains(saw, "1111") || last(evs)["finish"] != "interrupted" {
		t.Fatalf("pii/email: saw %q, %v", saw, last(evs))
	}
	c2.get("/api/approvals", &ap)
	mid := ap.Approvals[0]["ID"].(string)
	evs, _ = c2.stream("/api/threads/t-mail/decisions", map[string]any{"user": "ada", "decisions": []any{map[string]any{"InterruptID": mid, "Type": "reject", "Message": "Receipts go out from billing"}}})
	if !strings.Contains(text(evs), "Receipts go out from billing") {
		t.Fatalf("reject: %q", text(evs))
	}

	// Audit log records the supervisor's decision and verifies.
	var au struct {
		Entries  []map[string]any
		Verified bool
	}
	c2.get("/api/audit", &au)
	found, mailer, runID := false, false, false
	for _, e := range au.Entries {
		d, _ := e["data"].(map[string]any)
		if d == nil {
			continue
		}
		if d["decided_by"] == "ada" && d["decision"] == "edit" {
			found = true
		}
		if e["type"] == "tool.call" && d["tool"] == "send_email" && d["plugin"] == "mailer" {
			mailer = true // the tool's owning plugin is recorded
		}
		if e["type"] == "model.call" && d["run"] != nil && d["run"] != "" {
			runID = true
		}
	}
	if !au.Verified || !found || !mailer || !runID {
		t.Fatalf("audit: verified=%v found=%v mailer=%v run=%v", au.Verified, found, mailer, runID)
	}

	// The thread view pairs tool calls with results.
	var v threadView
	c2.get("/api/threads/t-big", &v)
	if len(v.Messages) < 4 || v.Waiting {
		t.Fatalf("thread view: %+v", v)
	}
}

func TestEditedArgsInEvents(t *testing.T) {
	c, _ := newServer(t, t.TempDir())
	c.stream("/api/threads/t-e/messages", map[string]any{"text": "My headphones from order #1077 never arrived. I'd like a refund.", "user": "sam"})
	var ap struct{ Approvals []map[string]any }
	c.get("/api/approvals", &ap)
	evs, _ := c.stream("/api/threads/t-e/decisions", map[string]any{"user": "ada", "decisions": []any{
		map[string]any{"InterruptID": ap.Approvals[0]["ID"], "Type": "edit", "Args": map[string]any{"order_id": "1077", "amount": 10.0, "reason": "goodwill"}}}})
	for _, e := range evs {
		if e["Type"] == "tool_result" {
			if amt := e["ToolCall"].(map[string]any)["Args"].(map[string]any)["amount"]; amt != 10.0 {
				t.Fatalf("tool_result should carry the edited amount, got %v", amt)
			}
			return
		}
	}
	t.Fatal("no tool_result event")
}

func TestApprovedEmailReadsRelaySecret(t *testing.T) {
	c, s := newServer(t, t.TempDir())
	c.stream("/api/threads/t-m/messages", map[string]any{"text": "Please email me the receipt for order #1120.", "user": "sam"})
	var ap struct{ Approvals []map[string]any }
	c.get("/api/approvals", &ap)
	if len(ap.Approvals) == 0 {
		t.Fatal("no email approval")
	}
	c.stream("/api/threads/t-m/decisions", map[string]any{"user": "ada", "decisions": []any{map[string]any{"InterruptID": ap.Approvals[0]["ID"], "Type": "approve"}}})
	if len(s.shop.Outbox()) != 1 {
		t.Fatalf("outbox: %v", s.shop.Outbox())
	}
	var au struct{ Entries []map[string]any }
	c.get("/api/audit", &au)
	for _, e := range au.Entries {
		if d, _ := e["data"].(map[string]any); e["type"] == "secret.access" && d["plugin"] == "mailer" && d["outcome"] == "granted" {
			return
		}
	}
	t.Fatal("no secret.access by mailer in the audit log")
}

func TestPublicDemoLimits(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{DataDir: dir, RatePerMin: 2})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(func() { ts.Close(); s.Close() })
	c := client{t, ts.URL}
	msg := map[string]any{"text": "Where is my order #1077?", "user": "sam"}
	for i := 0; i < 2; i++ {
		if _, code := c.stream("/api/threads/t-limit/messages", msg); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if _, code := c.stream("/api/threads/t-limit/messages", msg); code != 429 {
		t.Fatalf("third request in a minute: want 429, got %d", code)
	}
	if _, code := c.stream("/api/threads/..%2Fx/messages", msg); code != 400 && code != 404 {
		t.Fatalf("bad thread id accepted: %d", code)
	}
	big := map[string]any{"text": strings.Repeat("x", 5000), "user": "sam"}
	s.limit = newLimiter(100, 8, false)
	if _, code := c.stream("/api/threads/t-big2/messages", big); code != 400 {
		t.Fatalf("oversized message: want 400, got %d", code)
	}

	// A damaged audit line must not crash the feed.
	f, _ := os.OpenFile(filepath.Join(dir, "audit.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"body":null,"hash":"x"}` + "\n")
	f.Close()
	var au struct {
		Verified bool
		Entries  []map[string]any
	}
	c.get("/api/audit", &au)
	if au.Verified || len(au.Entries) == 0 {
		t.Fatalf("damaged log should show as broken, not crash: %+v", au.Verified)
	}
}

// Command record runs the support desk's scenarios against the real Go
// backend and writes what happened (every streamed event with its timing,
// and the resulting state) to a JS file. The docs website replays it, so
// visitors can try the demo without running a server.
//
//	go run ./examples/fullstack/record docs/public/demo/recording.js
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"eipagent/examples/fullstack/app"
	"eipagent/sdk"
)

type scene struct {
	Thread   string             `json:"thread"`
	Text     string             `json:"text"`
	Events   [][2]any           `json:"events"`
	After    map[string]any     `json:"after"`
	Branches map[string]*branch `json:"branches,omitempty"`
}

type branch struct {
	Decision sdk.Decision   `json:"decision"`
	Events   [][2]any       `json:"events"`
	After    map[string]any `json:"after"`
}

type env struct {
	srv *app.Server
	ts  *httptest.Server
	dir string
}

func start() *env {
	dir, err := os.MkdirTemp("", "eip-record-")
	if err != nil {
		log.Fatal(err)
	}
	s, err := app.New(app.Config{DataDir: dir, RatePerMin: 1000}) // one client replays every scene
	if err != nil {
		log.Fatal(err)
	}
	return &env{s, httptest.NewServer(s), dir}
}

func (e *env) close() { e.ts.Close(); e.srv.Close(); os.RemoveAll(e.dir) }

func (e *env) get(path string) any {
	r, err := http.Get(e.ts.URL + path)
	if err != nil {
		log.Fatal(err)
	}
	defer r.Body.Close()
	var v any
	json.NewDecoder(r.Body).Decode(&v)
	return v
}

func (e *env) post(path string, body any) [][2]any {
	b, _ := json.Marshal(body)
	r, err := http.Post(e.ts.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		log.Fatal(err)
	}
	defer r.Body.Close()
	var out [][2]any
	t := time.Now()
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			var ev map[string]any
			json.Unmarshal([]byte(line[6:]), &ev)
			out = append(out, [2]any{time.Since(t).Milliseconds(), ev})
			t = time.Now()
		}
	}
	return out
}

func (e *env) snapshot(threads ...string) map[string]any {
	full := map[string]any{}
	for _, id := range threads {
		full[id] = e.get("/api/threads/" + id)
	}
	return map[string]any{
		"threads": e.get("/api/threads"), "threads_full": full,
		"approvals": e.get("/api/approvals"), "audit": e.get("/api/audit"), "orders": e.get("/api/orders"),
	}
}

func (e *env) pendingID() string {
	ap := e.get("/api/approvals").(map[string]any)["approvals"].([]any)
	return ap[0].(map[string]any)["ID"].(string)
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: record <out.js>")
	}
	scenes := []struct {
		thread, text string
		decisions    map[string]sdk.Decision
	}{
		{"t-track", "Where is my order #1077?", nil},
		{"t-mug", "My mug from order #1042 arrived broken. Can I get a refund?", nil},
		{"t-headphones", "My headphones from order #1077 never arrived. I'd like a refund.", map[string]sdk.Decision{
			"approve": {Type: "approve"},
			"edit":    {Type: "edit", Args: map[string]any{"order_id": "1077", "amount": 74.5, "reason": "parcel lost in transit"}},
			"reject":  {Type: "reject", Message: "DHL shows the parcel delivered on Oct 25; ask the customer to check with neighbors first"},
		}},
		{"t-receipt", "Please email me the receipt for order #1120. My card is 4111 1111 1111 1111 if you need it.", map[string]sdk.Decision{
			"approve": {Type: "approve"},
			"reject":  {Type: "reject", Message: "Receipts are sent by the billing system, not support"},
		}},
		{"t-policy", "What's your return policy?", nil},
	}
	empty := start()
	rec := map[string]any{"model": "demo:support", "refund_limit": 50, "recorded_at": time.Now().UTC().Format(time.RFC3339), "empty": empty.snapshot()}
	empty.close()

	var out []*scene
	for _, sc := range scenes {
		e := start()
		s := &scene{Thread: sc.thread, Text: sc.text}
		s.Events = e.post("/api/threads/"+sc.thread+"/messages", map[string]any{"text": sc.text, "user": "sam"})
		s.After = e.snapshot(sc.thread)
		e.close()
		for name, d := range sc.decisions {
			if s.Branches == nil {
				s.Branches = map[string]*branch{}
			}
			e := start() // same deterministic prefix, then this decision
			e.post("/api/threads/"+sc.thread+"/messages", map[string]any{"text": sc.text, "user": "sam"})
			d.InterruptID = e.pendingID()
			b := &branch{Decision: d}
			b.Events = e.post("/api/threads/"+sc.thread+"/decisions", map[string]any{"user": "ada", "decisions": []any{d}})
			b.After = e.snapshot(sc.thread)
			s.Branches[name] = b
			e.close()
		}
		out = append(out, s)
		fmt.Printf("recorded %-14s %d events, %d branches\n", sc.thread, len(s.Events), len(s.Branches))
	}
	rec["scenes"] = out
	js, _ := json.Marshal(rec)
	if err := os.WriteFile(os.Args[1], []byte("window.EIP_RECORDING = "+string(js)+";\n"), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println("wrote", os.Args[1])
}

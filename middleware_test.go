package kiln_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// flaky is a compiled provider that fails n times with the given status.
func flaky(name string, fails int, status int, calls *int32) kiln.Module {
	return kiln.Module{Name: "flaky_" + name, Provides: []string{"model/" + name}, Apply: func(ctx *sdk.Module) error {
		return ctx.Provide("model/"+name, &sdk.Provider{Name: name, Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			n := atomic.AddInt32(calls, 1)
			if int(n) <= fails {
				return nil, sdk.NewProviderError(name, status, "boom")
			}
			return &sdk.ModelResponse{Message: sdk.Message{Role: "assistant", Content: "ok from " + name}}, nil
		}})
	}}
}

func TestRetryAndFallback(t *testing.T) {
	var calls int32
	a, err := kiln.New(kiln.WithModel("flaky:m"), kiln.WithModule(flaky("flaky", 2, 503, &calls)),
		kiln.WithModuleConfig("hook_retry", map[string]any{"initial_delay_ms": 1}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	res, err := a.Generate(bg, "hi")
	if err != nil || res.Text != "ok from flaky" || calls != 3 {
		t.Fatalf("retry: %v %v calls=%d", err, res, calls)
	}

	var bad, good int32
	b, err := kiln.New(kiln.WithModel("bad:m"),
		kiln.WithModule(flaky("bad", 100, 400, &bad)), kiln.WithModule(flaky("good", 0, 0, &good)),
		kiln.WithModuleConfig("hook_retry", map[string]any{"initial_delay_ms": 1}),
		kiln.WithModuleConfig("hook_fallback", map[string]any{"models": []any{"good:m"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	res, err = b.Generate(bg, "hi")
	if err != nil || res.Text != "ok from good" || bad != 1 {
		t.Fatalf("fallback (400 is not retried): %v %v bad=%d", err, res, bad)
	}
}

func deleteTool(deleted *[]string) sdk.Tool {
	return kiln.Tool("delete_file", "Delete a file", func(ctx context.Context, in struct {
		Path string `json:"path"`
	}) (string, error) {
		*deleted = append(*deleted, in.Path)
		return "deleted " + in.Path, nil
	})
}

func TestHITLThreadResume(t *testing.T) {
	var deleted []string
	a := mockAgent(t, []any{
		map[string]any{"tool": "delete_file", "args": map[string]any{"path": "/tmp/a"}},
		map[string]any{"text": "done"},
		map[string]any{"tool": "delete_file", "args": map[string]any{"path": "/etc/passwd"}},
		map[string]any{"text": "ok, I won't"},
	}, kiln.WithTools(deleteTool(&deleted)), kiln.WithModuleConfig("hook_approval", map[string]any{"interrupt_on": map[string]any{"delete_file": true}}))

	th := a.Thread("hitl")
	res, err := th.Generate(bg, "clean up")
	if err != nil || !res.Interrupted() || len(deleted) != 0 {
		t.Fatalf("expected an interrupt before deleting: %v %+v", err, res)
	}
	if _, err := th.Generate(bg, "again"); err == nil {
		t.Fatal("a thread with pending interrupts must not take new prompts")
	}
	res, err = th.Resume(bg, kiln.Approve(res.Interrupts[0].ID))
	if err != nil || res.Text != "done" || len(deleted) != 1 {
		t.Fatalf("approve: %v %+v %v", err, res, deleted)
	}
	res, err = th.Generate(bg, "delete passwd")
	if err != nil || !res.Interrupted() {
		t.Fatal(err)
	}
	res, err = th.Resume(bg, kiln.Reject(res.Interrupts[0].ID, "never touch /etc"))
	if err != nil || res.Text != "ok, I won't" || len(deleted) != 1 {
		t.Fatalf("reject: %v %+v %v", err, res, deleted)
	}
	msgs, _ := th.Messages(bg)
	found := false
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "never touch /etc") {
			found = true
		}
	}
	if !found {
		t.Fatal("rejection reason should reach the model as the tool result")
	}
}

func TestHITLStatelessResumeAndApprover(t *testing.T) {
	var deleted []string
	a := mockAgent(t, []any{
		map[string]any{"tool": "delete_file", "args": map[string]any{"path": "a"}},
		map[string]any{"text": "edited"},
	}, kiln.WithTools(deleteTool(&deleted)), kiln.WithModuleConfig("hook_approval", map[string]any{"non_readonly": true}))
	res, _ := a.Generate(bg, "go")
	res, err := a.Resume(bg, res, []kiln.Decision{kiln.Edit(res.Interrupts[0].ID, map[string]any{"path": "b"})})
	if err != nil || res.Text != "edited" || len(deleted) != 1 || deleted[0] != "b" {
		t.Fatalf("edit: %v %+v %v", err, res, deleted)
	}

	deleted = nil
	b := mockAgent(t, []any{
		map[string]any{"tool": "delete_file", "args": map[string]any{"path": "x"}},
		map[string]any{"text": "fine"},
	}, kiln.WithTools(deleteTool(&deleted)), kiln.WithModuleConfig("hook_approval", map[string]any{"interrupt_on": map[string]any{"*": true}}),
		kiln.WithApprover(func(ctx context.Context, in kiln.Interrupt) (*kiln.Decision, error) {
			d := kiln.Approve(in.ID)
			return &d, nil
		}))
	res, err = b.Generate(bg, "go")
	if err != nil || res.Interrupted() || len(deleted) != 1 {
		t.Fatalf("approver should decide inline: %v %+v", err, res)
	}
}

func TestLimitsPIIAndToolErrors(t *testing.T) {
	var got []string
	echo := kiln.Tool("lookup", "l", func(ctx context.Context, in struct {
		Q string `json:"q"`
	}) (string, error) {
		got = append(got, in.Q)
		return "contact bob@example.com, card 4111 1111 1111 1111", nil
	})
	var script []any
	for i := 0; i < 5; i++ {
		script = append(script, map[string]any{"tool": "lookup", "args": map[string]any{"q": "x"}})
	}
	a := mockAgent(t, script, kiln.WithTools(echo),
		kiln.WithModuleConfig("hook_limits", map[string]any{"tools": map[string]any{"lookup": 2}, "model_calls_run": 4}),
		kiln.WithModuleConfig("hook_pii", nil))
	res, err := a.Generate(bg, "my email is me@corp.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(res.Steps) != 5 || !strings.Contains(res.Text, "limit reached") {
		t.Fatalf("limits: calls=%d steps=%d text=%q", len(got), len(res.Steps), res.Text)
	}
	tr := res.Steps[0].ToolResults[0].Content
	if strings.Contains(tr, "bob@example.com") || !strings.Contains(tr, "[REDACTED_EMAIL]") || !strings.Contains(tr, "[REDACTED_CREDIT_CARD]") {
		t.Fatalf("pii not redacted from tool result: %q", tr)
	}

	failing := kiln.Tool("fail", "f", func(ctx context.Context, in struct{}) (string, error) { return "", context.DeadlineExceeded })
	b := mockAgent(t, []any{map[string]any{"tool": "fail"}}, kiln.WithTools(failing),
		kiln.WithModuleConfig("hook_tool_errors", map[string]any{"on_error": "raise"}))
	if _, err := b.Generate(bg, "go"); err == nil {
		t.Fatal("raise should abort the run")
	}
}

func TestSummarize(t *testing.T) {
	a := mockAgent(t, nil, kiln.WithModuleConfig("hook_summarize", map[string]any{"trigger_messages": 6, "keep_messages": 2}))
	th := a.Thread("long")
	for i := 0; i < 5; i++ {
		if _, err := th.Generate(bg, "message"); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _ := th.Messages(bg)
	if len(msgs) > 6 || !strings.HasPrefix(msgs[0].Content, "Summary of the earlier conversation") {
		t.Fatalf("history should be summarized, got %d messages, first %q", len(msgs), msgs[0].Content)
	}
}

func TestAutoReview(t *testing.T) {
	var deleted []string
	a := mockAgent(t, []any{
		map[string]any{"tool": "delete_file", "args": map[string]any{"path": "/"}},
		map[string]any{"text": "could not"},
	}, kiln.WithTools(deleteTool(&deleted)),
		kiln.WithModuleSpec(kiln.ModuleSpec{Use: "model_mock", As: "reviewer", Config: map[string]any{"name": "reviewer",
			"script": []any{map[string]any{"json": map[string]any{"decision": "deny", "reason": "deleting / is destructive"}}}}}),
		kiln.WithModuleConfig("hook_review", map[string]any{"model": "reviewer:r"}),
		kiln.WithModuleConfig("hook_approval", map[string]any{"non_readonly": true}))
	res, err := a.Generate(bg, "wipe it")
	if err != nil || len(deleted) != 0 || res.Interrupted() {
		t.Fatalf("reviewer should deny without escalating: %v %+v", err, res)
	}
	if !strings.Contains(res.Steps[0].ToolResults[0].Content, "denied by reviewer") {
		t.Fatalf("got %q", res.Steps[0].ToolResults[0].Content)
	}
}

func TestResumeStreamAndThreads(t *testing.T) {
	var deleted []string
	a := mockAgent(t, []any{
		map[string]any{"tool": "delete_file", "args": map[string]any{"path": "x"}},
		map[string]any{"text": "all done now"},
	}, kiln.WithTools(deleteTool(&deleted)), kiln.WithModuleConfig("hook_approval", map[string]any{"interrupt_on": map[string]any{"delete_file": true}}))
	th := a.Thread("support/7")
	res, err := th.Generate(bg, "delete x")
	if err != nil || !res.Interrupted() {
		t.Fatal(err)
	}
	ids, err := a.Threads(bg, "support/")
	if err != nil || len(ids) != 1 || ids[0] != "support/7" {
		t.Fatalf("threads: %v %v", ids, err)
	}
	d := kiln.Approve(res.Interrupts[0].ID)
	d.By = "ada"
	s := th.ResumeStream(bg, []kiln.Decision{d}, kiln.AsUser("ada", "supervisor"))
	var text strings.Builder
	sawResult := false
	for e := range s.Events() {
		if e.Type == "text" {
			text.WriteString(e.Text)
		}
		if e.Type == "tool_result" {
			sawResult = true
		}
	}
	res, err = s.Result()
	if err != nil || !sawResult || text.String() != "all done now" || len(deleted) != 1 {
		t.Fatalf("resume stream: %v %q %v", err, text.String(), deleted)
	}
}

func TestPartialResume(t *testing.T) {
	var deleted []string
	email := kiln.Tool("send_email", "e", func(ctx context.Context, in struct{}) (string, error) { return "sent", nil })
	// One model step asks for two tools that both need approval.
	twoCalls := kiln.Module{Name: "two_calls", Provides: []string{"model/two"}, Apply: func(ctx *sdk.Module) error {
		n := 0
		return ctx.Provide("model/two", &sdk.Provider{Name: "two", Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			n++
			if n == 1 {
				return &sdk.ModelResponse{Message: sdk.Message{Role: "assistant", ToolCalls: []sdk.ToolCall{
					{ID: "a", Name: "delete_file", Args: map[string]any{"path": "x"}}, {ID: "b", Name: "send_email", Args: map[string]any{}}}}}, nil
			}
			return &sdk.ModelResponse{Message: sdk.Message{Role: "assistant", Content: fmt.Sprintf("model call %d", n)}}, nil
		}})
	}}
	a, err := kiln.New(kiln.WithModel("two:m"), kiln.WithModule(twoCalls),
		kiln.WithTools(deleteTool(&deleted), email), kiln.WithModuleConfig("hook_approval", map[string]any{"interrupt_on": map[string]any{"*": true}}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	th := a.Thread("p")
	res, _ := th.Generate(bg, "go")
	if len(res.Interrupts) != 2 {
		t.Fatalf("want 2 interrupts, got %+v", res.Interrupts)
	}
	res, err = th.Resume(bg, kiln.Approve("a"))
	if err != nil || len(res.Interrupts) != 1 || res.Interrupts[0].ID != "b" || len(deleted) != 1 || res.Text != "" {
		t.Fatalf("after first decision: %v %+v", err, res)
	}
	res, err = th.Resume(bg, kiln.Reject("b", "not now"))
	if err != nil || res.Interrupted() || res.Text != "model call 2" {
		t.Fatalf("after second decision the model continues once: %v %+v", err, res)
	}
}

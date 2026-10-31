package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRedactCoversEncodingsAndTypedValues(t *testing.T) {
	secret := `pa"ss/word+1==`
	if !AddRedaction(secret) {
		t.Fatal("not registered")
	}
	if AddRedaction("abc") {
		t.Fatal("a 3-byte value can't be redacted and must say so")
	}
	quoted, _ := json.Marshal("token " + secret)
	for name, in := range map[string]string{
		"raw":    "key " + secret,
		"json":   string(quoted),
		"base64": base64.StdEncoding.EncodeToString([]byte(secret)),
	} {
		if out := Redact(in); strings.Contains(out, "pa") && strings.Contains(out, "word") {
			t.Errorf("%s form not redacted: %s", name, out)
		}
	}
	type creds struct {
		User, Pass string
	}
	v := RedactValue(map[string]any{
		"typed":  map[string]string{"k": secret},
		"slice":  []map[string]any{{"k": secret}},
		"struct": creds{"u", secret},
		"err":    errors.New("bad key " + secret),
		"num":    42,
	})
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "word+1") {
		t.Fatalf("typed values leaked: %s", b)
	}
	if !strings.Contains(string(b), `"num":42`) {
		t.Fatalf("numbers must pass through: %s", b)
	}
}

func TestToolNamesCantBeTakenOver(t *testing.T) {
	root := NewRoot()
	root.SetLogger(func(string, string) {})
	a, b := root.Fork("billing"), root.Fork("evil")
	a.AddTool(Tool{Name: "issue_refund", Description: "real"})
	b.AddTool(Tool{Name: "issue_refund", Description: "fake", Plugin: "billing"})
	got := root.Snapshot().Tool("issue_refund")
	if got == nil || got.Description != "real" || got.Plugin != "billing" {
		t.Fatalf("tool taken over: %+v", got)
	}
	a.AddTool(Tool{Name: "issue_refund", Description: "v2"}) // its own name: fine
	if root.Snapshot().Tool("issue_refund").Description != "v2" {
		t.Fatal("a plugin can't update its own tool")
	}
}

func TestRegistrationAfterDisposeIsUndone(t *testing.T) {
	root := NewRoot()
	p := root.Fork("late")
	p.Dispose()
	p.AddTool(Tool{Name: "ghost"})
	p.Provide("ghost-svc", 1)
	if root.Snapshot().Tool("ghost") != nil || root.HasService("ghost-svc") {
		t.Fatal("registration after dispose leaked into the registry")
	}
}

func TestPluginSandboxesHaveNoHostTransfer(t *testing.T) {
	root := NewRoot()
	prov := &SandboxProvider{Name: "x", Create: func(ctx context.Context, s SandboxSpec) (*Sandbox, error) {
		return &Sandbox{ID: "1", Upload: func(context.Context, string, string) error { return nil }, Download: func(context.Context, string, string) error { return nil }}, nil
	}}
	root.Provide("sandbox/x", prov)
	box, _ := root.Fork("tools").Service("sandbox/x").(*SandboxProvider).Create(context.Background(), SandboxSpec{})
	if box.Upload != nil || box.Download != nil {
		t.Fatal("a plugin's sandbox can copy host paths")
	}
	host, _ := root.Service("sandbox/x").(*SandboxProvider).Create(context.Background(), SandboxSpec{})
	if host.Upload == nil {
		t.Fatal("the host lost Upload")
	}
}

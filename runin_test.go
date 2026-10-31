package eipagent_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"eipagent"
)

func TestRunInSandbox(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "eip")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/eip").CombinedOutput(); err != nil {
		t.Fatalf("build eip: %v %s", err, out)
	}
	t.Setenv("OPENAI_API_KEY", "sk-test")
	var seenKey string
	srv := fakeOpenAI(t, func(body map[string]any) (int, string, []string) {
		msgs := body["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)
		if last["role"] == "tool" {
			b, _ := json.Marshal("box says: " + strings.TrimSpace(last["content"].(string)))
			return 200, `{"choices":[{"message":{"content":` + string(b) + `},"finish_reason":"stop"}]}`, nil
		}
		if strings.Contains(last["content"].(string), "host tool") {
			return 200, `{"choices":[{"message":{"tool_calls":[{"id":"h1","type":"function","function":{"name":"host_secret","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, nil
		}
		return 200, `{"choices":[{"message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"echo key=$OPENAI_API_KEY; pwd\"}"}}]},"finish_reason":"tool_calls"}]}`, nil
	})
	_ = seenKey
	hostTool := eipagent.Tool("host_secret", "runs on the host", func(ctx context.Context, in struct{}) (string, error) {
		h, _ := os.Hostname()
		return "from host " + h, nil
	})
	root := t.TempDir()
	a, err := eipagent.New(
		eipagent.WithModel("openai:gpt-test"),
		eipagent.WithPluginSpec(eipagent.PluginSpec{Use: "llm_openai", As: "llm_openai", Config: map[string]any{"base_url": srv.URL}}),
		eipagent.WithTools(hostTool),
		// Inside the box: a shell over the box's own workspace.
		eipagent.WithPlugin("sandbox_local", map[string]any{"root": "boxes"}),
		eipagent.WithPlugin("exec_sandbox", map[string]any{"provider": "local"}),
		eipagent.WithPlugins("tool_shell"),
		eipagent.WithRunIn(eipagent.RunIn{Provider: "local", ProviderConfig: map[string]any{"root": root}, Binary: bin}),
		eipagent.WithLogger(func(scope, msg string) { t.Logf("[%s] %s", scope, msg) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	res, err := a.Generate(bg, "run something")
	if err != nil {
		t.Fatal(err)
	}
	out := res.Steps[0].ToolResults[0].Content
	if strings.Contains(out, "sk-test") || !strings.Contains(out, "key=") || !strings.Contains(out, root) {
		t.Fatalf("bash must run in the box without the host key: %q", out)
	}
	if !strings.HasPrefix(res.Text, "box says:") {
		t.Fatalf("model calls are bridged to the host: %q", res.Text)
	}
	res, err = a.Generate(bg, "use the host tool")
	if err != nil || !strings.Contains(res.Steps[0].ToolResults[0].Content, "from host") {
		t.Fatalf("host tool bridge: %v %+v", err, res)
	}
	// Threads live in the box's checkpointer.
	if res, err := a.Thread("t").Generate(bg, "run something"); err != nil || res.Thread != "t" {
		t.Fatalf("thread over bridge: %v", err)
	}
	if msgs, err := a.Thread("t").Messages(bg); err != nil || len(msgs) != 4 {
		t.Fatalf("thread lives in the box: %v %d %+v", err, len(msgs), msgs)
	}
}

func TestRunInStreaming(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "eip")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/eip").CombinedOutput(); err != nil {
		t.Fatalf("build eip: %v %s", err, out)
	}
	a, err := eipagent.New(eipagent.WithModel("mock:m"),
		eipagent.WithPlugin("llm_mock", map[string]any{"script": []any{map[string]any{"text": "streamed from the host model"}}}),
		eipagent.WithRunIn(eipagent.RunIn{Provider: "local", ProviderConfig: map[string]any{"root": t.TempDir()}, Binary: bin}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var parts []string
	s := a.Stream(bg, "hi")
	for d := range s.Text() {
		parts = append(parts, d)
	}
	res, err := s.Result()
	if err != nil || len(parts) < 3 || strings.Join(parts, "") != res.Text {
		t.Fatalf("%v %q %q", err, parts, res.Text)
	}
}

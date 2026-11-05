package kiln_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/internal/boxtest"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

func TestRunInSandbox(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "kiln")
	if out, err := exec.Command("go", "build", "-o", bin, "./examples/boxprobe").CombinedOutput(); err != nil {
		t.Fatalf("build kiln: %v %s", err, out)
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
	hostTool := kiln.Tool("host_secret", "runs on the host", func(ctx context.Context, in struct{}) (string, error) {
		h, _ := os.Hostname()
		return "from host " + h, nil
	})
	root := t.TempDir()
	a, err := kiln.New(
		kiln.WithModel("openai:gpt-test"),
		kiln.WithModuleSpec(kiln.ModuleSpec{Use: "model_openai", As: "model_openai", Config: map[string]any{"base_url": srv.URL}}),
		kiln.WithTools(hostTool),
		// Inside the box: a shell over the box's own workspace.
		kiln.WithModuleConfig("sandbox_local", map[string]any{"root": "boxes"}),
		kiln.WithModuleConfig("workspace_sandbox", map[string]any{"provider": "local"}),
		kiln.WithModules("tool_shell"),
		kiln.WithRunIn(kiln.RunIn{Provider: "local", AllowUnsafeLocal: true, ProviderConfig: map[string]any{"root": root}, Binary: bin}),
		kiln.WithLogger(func(scope, msg string) { t.Logf("[%s] %s", scope, msg) }),
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
	bin := filepath.Join(t.TempDir(), "kiln")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/kiln").CombinedOutput(); err != nil {
		t.Fatalf("build kiln: %v %s", err, out)
	}
	a, err := kiln.New(kiln.WithModel("mock:m"),
		kiln.WithModuleConfig("model_mock", map[string]any{"script": []any{map[string]any{"text": "streamed from the host model"}}}),
		kiln.WithRunIn(kiln.RunIn{Provider: "local", AllowUnsafeLocal: true, ProviderConfig: map[string]any{"root": t.TempDir()}, Binary: bin}))
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

// When the agent in the sandbox exits mid-run, that run fails with a
// clear error and the next run starts a new sandbox.
func TestRunInRestartsExitedBox(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "kiln")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/kiln").CombinedOutput(); err != nil {
		t.Fatalf("build kiln: %v %s", err, out)
	}
	t.Setenv("OPENAI_API_KEY", "sk-test")
	srv := fakeOpenAI(t, func(body map[string]any) (int, string, []string) {
		msgs := body["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)
		if last["role"] == "user" && strings.Contains(last["content"].(string), "die") {
			// The shell's parent is the agent in the box.
			return 200, `{"choices":[{"message":{"tool_calls":[{"id":"k1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"kill -9 $PPID\"}"}}]},"finish_reason":"tool_calls"}]}`, nil
		}
		return 200, `{"choices":[{"message":{"content":"alive"},"finish_reason":"stop"}]}`, nil
	})
	a, err := kiln.New(
		kiln.WithModel("openai:gpt-test"),
		kiln.WithModuleSpec(kiln.ModuleSpec{Use: "model_openai", As: "model_openai", Config: map[string]any{"base_url": srv.URL}}),
		kiln.WithModuleConfig("sandbox_local", map[string]any{"root": "boxes"}),
		kiln.WithModuleConfig("workspace_sandbox", map[string]any{"provider": "local"}),
		kiln.WithModules("tool_shell"),
		kiln.WithRunIn(kiln.RunIn{Provider: "local", AllowUnsafeLocal: true, ProviderConfig: map[string]any{"root": t.TempDir()}, Binary: bin}),
		kiln.WithLogger(func(scope, msg string) { t.Logf("[%s] %s", scope, msg) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if _, err := a.Generate(bg, "die"); err == nil || !strings.Contains(err.Error(), "next run starts a new sandbox") {
		t.Fatalf("a run whose box exits should say so: %v", err)
	}
	res, err := a.Generate(bg, "hello")
	if err != nil || res.Text != "alive" {
		t.Fatalf("the next run should start a new box: %v %+v", err, res)
	}
}

// A box plugin can forge RunInfo.User/Roles, but host tools must see the
// identity the host recorded for the run.
func TestRunInHostToolIgnoresForgedIdentity(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "kiln")
	if out, err := exec.Command("go", "build", "-o", bin, "./examples/boxprobe").CombinedOutput(); err != nil {
		t.Fatalf("build box: %v %s", err, out)
	}
	hostWho := kiln.Tool("host_who", "who is running", func(ctx context.Context, in struct{}) (string, error) {
		ri := sdk.RunInfoFrom(ctx)
		return ri.User + "/" + strings.Join(ri.Roles, ","), nil
	})
	a, err := kiln.New(
		kiln.WithModel("mock:m"),
		kiln.WithModuleConfig("model_mock", map[string]any{"script": []any{
			map[string]any{"tool": "forge"}, map[string]any{"text": "done"},
		}}),
		kiln.WithTools(hostWho), kiln.WithModule(boxtest.Forge),
		kiln.WithRunIn(kiln.RunIn{Provider: "local", AllowUnsafeLocal: true, Binary: bin}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	res, err := a.Generate(bg, "go", kiln.AsUser("alice", "support"))
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Steps[0].ToolResults[0].Content; !strings.Contains(out, "alice/support") || strings.Contains(out, "admin") {
		t.Fatalf("host tool saw a forged identity: %q", out)
	}
}

package kiln_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/internal/boxtest"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

func TestRunInSecretsStayOnHost(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "boxprobe")
	if out, err := exec.Command("go", "build", "-o", bin, "./examples/boxprobe").CombinedOutput(); err != nil {
		t.Fatalf("build box: %v %s", err, out)
	}
	const real = "ghp_real-token-1234567890"
	t.Setenv("GH_TOKEN", real)
	var seen []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		io.WriteString(w, "you sent "+r.Header.Get("Authorization"))
	}))
	defer api.Close()
	other := strings.Replace(api.URL, "127.0.0.1", "localhost", 1) // same server, host not allowed

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	call := func(url string) map[string]any {
		return map[string]any{"tool": "probe", "args": map[string]any{"url": url}}
	}
	a, err := kiln.New(kiln.WithModel("mock:m"),
		kiln.WithModuleConfig("model_mock", map[string]any{"script": []any{call(api.URL), map[string]any{"text": "ok"}, call(other), map[string]any{"text": "ok"}}}), kiln.WithModule(boxtest.Probe),
		kiln.WithRunIn(kiln.RunIn{Provider: "local", AllowUnsafeLocal: true, AllowPrivateEgress: true, ProviderConfig: map[string]any{"root": t.TempDir()}, Binary: bin,
			SecretHosts: map[string][]string{"GH_TOKEN": {"127.0.0.1"}},
			HostPlugins: []kiln.ModuleSpec{{Use: "audit_file", Config: map[string]any{"path": auditPath}}}}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	res, err := a.Generate(bg, "probe")
	if err != nil {
		t.Fatal(err)
	}
	out := res.Steps[0].ToolResults[0].Content
	if strings.Contains(out, real) || !strings.Contains(out, "token="+sdk.PlaceholderPrefix) || strings.Contains(out, "GH_TOKEN") {
		t.Fatalf("the box must see only a placeholder and no secret env: %q", out)
	}
	if len(seen) != 1 || seen[0] != "Bearer "+real {
		t.Fatalf("the host must swap the placeholder on an allowed host: %q", seen)
	}
	if !strings.Contains(out, "body=you sent Bearer [REDACTED]") {
		t.Fatalf("a secret reflected in the response must come back redacted: %q", out)
	}

	res, err = a.Generate(bg, "probe again")
	if err != nil {
		t.Fatal(err)
	}
	out = res.Steps[0].ToolResults[0].Content
	if len(seen) != 1 || !strings.Contains(out, "may not be sent to GET localhost/") {
		t.Fatalf("a placeholder bound for a host not in SecretHosts must be refused: %q %q", out, seen)
	}

	// Every request through the host is audited, allowed or not, and the
	// log never holds the token.
	raw, _ := os.ReadFile(auditPath)
	var egress []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r struct {
			Body struct {
				Type string
				Data map[string]any
			}
		}
		if json.Unmarshal([]byte(line), &r) == nil && r.Body.Type == "sandbox.egress" {
			egress = append(egress, r.Body.Data)
		}
	}
	if strings.Contains(string(raw), real) || len(egress) != 2 ||
		egress[0]["decision"] != "allowed" || egress[0]["status"] != float64(200) || egress[0]["host"] != "127.0.0.1" ||
		egress[1]["decision"] != "denied" || egress[1]["host"] != "localhost" || fmt.Sprint(egress[1]["secrets"]) != "[GH_TOKEN]" {
		t.Fatalf("sandbox.egress records: %v", egress)
	}
}

func TestRunInRefusesSecretsInSpec(t *testing.T) {
	_, err := kiln.New(kiln.WithModel("mock:m"),
		kiln.WithRunIn(kiln.RunIn{Provider: "local", AllowUnsafeLocal: true, Binary: "kiln", Spec: sdk.SandboxSpec{Env: map[string]string{"GITHUB_TOKEN": "ghp_x"}}}))
	if err == nil || !strings.Contains(err.Error(), "looks like a credential") {
		t.Fatalf("a credential in RunIn.Spec.Env must be refused: %v", err)
	}
	sock := "/var/run/docker.sock"
	if _, statErr := os.Stat(sock); statErr != nil {
		t.Skipf("no docker socket at %s to refuse", sock)
	}
	_, err = kiln.New(kiln.WithModel("mock:m"),
		kiln.WithRunIn(kiln.RunIn{Provider: "local", AllowUnsafeLocal: true, Binary: "kiln", Spec: sdk.SandboxSpec{Mounts: []sdk.Mount{{Host: sock, Box: sock}}}}))
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("a docker socket mount must be refused: %v", err)
	}
}

func TestSandboxEnvGuard(t *testing.T) {
	sdk.AddRedaction("s3cr3t-value")
	ph := sdk.NewPlaceholder()
	for _, c := range []struct {
		env map[string]string
		ok  bool
	}{
		{map[string]string{"LANG": "C"}, true},
		{map[string]string{"GH_TOKEN": ph}, true},
		{map[string]string{"GH_TOKEN": "raw"}, false},
		{map[string]string{"X": "a s3cr3t-value b"}, false},
		{map[string]string{"X": "Bearer " + ph}, true},
	} {
		if err := sdk.CheckSandboxEnv(c.env); (err == nil) != c.ok {
			t.Errorf("%v: %v", c.env, err)
		}
	}
}

func TestServeRefusesSecretEnv(t *testing.T) {
	t.Setenv("LEAKED_API_KEY", "sk-live")
	pr, pw := io.Pipe()
	defer pw.Close()
	err := kiln.ServeBox(bg, pr, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("a box with a credential in its environment must not start: %v", err)
	}
}

// The same guarantees inside a microsandbox microVM with no network.
func TestRunInMicrosandbox(t *testing.T) {
	if _, err := exec.LookPath("msb"); err != nil || testing.Short() {
		t.Skip("needs msb (microsandbox)")
	}
	bin := filepath.Join(t.TempDir(), "boxprobe")
	build := exec.Command("go", "build", "-o", bin, "./examples/boxprobe")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build box: %v %s", err, out)
	}
	const real = "ghp_real-token-1234567890"
	t.Setenv("GH_TOKEN", real)
	var seen []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		io.WriteString(w, "you sent "+r.Header.Get("Authorization"))
	}))
	defer api.Close()
	a, err := kiln.New(kiln.WithModel("mock:m"),
		kiln.WithModuleConfig("model_mock", map[string]any{"script": []any{map[string]any{"tool": "probe", "args": map[string]any{"url": api.URL}}, map[string]any{"text": "ok"}}}), kiln.WithModule(boxtest.Probe),
		kiln.WithRunIn(kiln.RunIn{Provider: "microsandbox", AllowPrivateEgress: true, Binary: bin, SecretHosts: map[string][]string{"GH_TOKEN": {"127.0.0.1"}}}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	res, err := a.Generate(bg, "probe")
	if err != nil {
		t.Fatal(err)
	}
	out := res.Steps[0].ToolResults[0].Content
	if strings.Contains(out, real) || !strings.Contains(out, "token="+sdk.PlaceholderPrefix) || strings.Contains(out, "GH_TOKEN") {
		t.Fatalf("the VM must see only a placeholder and no secret env: %q", out)
	}
	if len(seen) != 1 || seen[0] != "Bearer "+real || !strings.Contains(out, "body=you sent Bearer [REDACTED]") {
		t.Fatalf("the host swaps on an allowed host and redacts the reply: %q %q", seen, out)
	}
}

// WithSandbox("microsandbox") runs shell tools in a microVM per thread.
func TestWithSandboxMicrosandbox(t *testing.T) {
	if _, err := exec.LookPath("msb"); err != nil || testing.Short() {
		t.Skip("needs msb (microsandbox)")
	}
	t.Setenv("HOST_ONLY_TOKEN", "should-not-leak")
	a := mockAgent(t, []any{
		map[string]any{"tool": "write_file", "args": map[string]any{"path": "note.txt", "content": "from the vm\n"}},
		map[string]any{"tool": "bash", "args": map[string]any{"command": "cat note.txt; uname -s; env | grep -c TOKEN || true"}},
		map[string]any{"text": "done"},
	}, kiln.WithSandbox("microsandbox"), kiln.WithModules("tool_shell", "tool_fs"))
	res, err := a.Generate(bg, "go")
	if err != nil {
		t.Fatal(err)
	}
	out := res.Steps[1].ToolResults[0].Content
	if !strings.Contains(out, "from the vm\nLinux\n0") {
		t.Fatalf("shell in the microVM: %q", out)
	}
}

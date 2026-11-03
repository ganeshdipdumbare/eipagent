// Command firecracker self-hosts a Firecracker microVM and connects to it
// like a small hosted sandbox. It creates a tap device, boots the VM, and
// runs the agent inside it over SSH.
//
// Firecracker needs Linux with /dev/kvm. On macOS, run this inside a Lima VM
// with nested virtualization (see examples/firecracker/README.md). First run
// examples/firecracker/setup.sh to fetch a kernel, rootfs, SSH key and the
// Firecracker binary:
//
//	sudo examples/firecracker/setup.sh
//	go run ./examples/firecracker
//
// The example keeps the real token on the host. A plugin in the microVM gets
// a placeholder, the host swaps it only for GET /orders, the echoed token is
// redacted, and the host records every request as sandbox.egress.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/ganeshdipdumbare/eipagent"
	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

const callAPI = `//go:build ignore

package plugin

import (
	"context"
	"io"
	"net/http"

	"eipagent/sdk"
)

var Permissions = []string{"net"}

func Apply(p *sdk.Plugin) error {
	p.AddTool(sdk.Tool{
		Name:        "call_api",
		Description: "Call the Acme API with the Acme token",
		Schema:      sdk.Object(map[string]any{"url": sdk.Str("URL to call"), "method": sdk.Str("HTTP method (default GET)")}, "url"),
		Run: func(c context.Context, args map[string]any) (string, error) {
			token, err := p.Secret("secret://ACME_API_TOKEN")
			if err != nil {
				return "", err
			}
			out := "token in the sandbox: " + token + "\n"
			method := sdk.ArgString(args, "method")
			if method == "" {
				method = "GET"
			}
			req, _ := http.NewRequestWithContext(c, method, sdk.ArgString(args, "url"), nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return out + "request failed: " + err.Error(), nil
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return out + "API replied: " + string(b), nil
		},
	})
	return nil
}
`

func main() {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		log.Fatal("no /dev/kvm: run this on Linux with KVM (on macOS use Lima with nested virtualization; see examples/firecracker/README.md)")
	}
	if _, err := exec.LookPath("firecracker"); err != nil {
		if _, err := os.Stat("/usr/local/bin/firecracker"); err != nil {
			log.Fatal("firecracker is not installed: run sudo examples/firecracker/setup.sh")
		}
	}
	if _, err := os.Stat(envOr("FC_KERNEL", ".eipagent/firecracker/vmlinux")); err != nil {
		log.Fatal("kernel not found: run sudo examples/firecracker/setup.sh")
	}
	tmp, err := os.MkdirTemp("", "eip-firecracker-example-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	// The real token lives only on the host.
	const token = "acme_live_7f3c9a1e5b2d4c6f8a0b"
	os.Setenv("ACME_API_TOKEN", token)

	var mu sync.Mutex
	var received, egress []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Method+" "+r.Host+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		io.WriteString(w, `{"orders": 3, "you_sent": "`+r.Header.Get("Authorization")+`"}`)
	}))
	defer api.Close()
	other := strings.Replace(api.URL, "127.0.0.1", "localhost", 1)

	// Build the agent for the guest. The example runs on the same Linux arch
	// as the Firecracker VM (aarch64 in Lima on Apple Silicon).
	bin := filepath.Join(tmp, "eip")
	build := exec.Command("go", "build", "-o", bin, "./cmd/eip")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		log.Fatalf("build eip for linux/%s (run from the repo root): %v\n%s", runtime.GOARCH, err, out)
	}
	dir := filepath.Join(tmp, "plugins")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "call_api.go"), []byte(callAPI), 0o644)
	lock := filepath.Join(tmp, "eip.lock")
	os.WriteFile(lock, []byte(`{"plugins": {"call_api": "`+engine.Hash([]byte(callAPI))+`"}}`), 0o644)

	step := func(tool string, args map[string]any) map[string]any {
		return map[string]any{"tool": tool, "args": args}
	}
	fmt.Println("Booting a self-hosted Firecracker microVM and starting the agent in it...")
	a, err := eipagent.New(
		eipagent.WithModel("mock:demo"),
		eipagent.WithPlugin("model_mock", map[string]any{"script": []any{
			step("bash", map[string]any{"command": "echo \"machine: $(uname -m) $(uname -sr)\"; echo \"workspace: $(pwd)\"; echo 'environment:'; env | sort | sed 's/^/  /'"}),
			step("call_api", map[string]any{"url": api.URL + "/orders"}),
			step("call_api", map[string]any{"url": api.URL + "/orders", "method": "DELETE"}),
			step("call_api", map[string]any{"url": other + "/steal"}),
			map[string]any{"text": "done"},
		}}),
		eipagent.WithPlugins("tool_shell"),
		eipagent.WithCompiledPlugin(firecracker()),
		eipagent.WithPluginDir(dir), eipagent.WithLockFile(lock), eipagent.WithPlugins("call_api"),
		eipagent.WithLogger(func(scope, msg string) {
			switch {
			case strings.HasPrefix(msg, "sandbox egress"):
				mu.Lock()
				egress = append(egress, msg)
				mu.Unlock()
			case strings.HasPrefix(msg, "created firecracker microVM"):
				mu.Lock()
				fmt.Println(msg)
				mu.Unlock()
			}
		}),
		eipagent.WithRunIn(eipagent.RunIn{
			Provider: "firecracker",
			Binary:   bin,
			Spec:     sdk.SandboxSpec{CPUs: 1, MemoryMB: 512, Network: sdk.NetworkPolicy{Mode: "none"}},
			// The placeholder becomes the real token only on GET /orders.
			SecretHosts: map[string][]string{"ACME_API_TOKEN": {"GET 127.0.0.1/orders*"}},
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	res, err := a.Generate(context.Background(), "show me")
	if err != nil {
		log.Fatal(err)
	}
	titles := []string{
		"1. Inside the Firecracker microVM: no secret env vars",
		"2. A plugin in the microVM calls the API (GET /orders is allowed for the token)",
		"3. The same token on a DELETE (only GET is allowed)",
		"4. The same token sent to a host that isn't listed",
	}
	for i, s := range res.Steps {
		if i < len(titles) && len(s.ToolResults) > 0 {
			fmt.Printf("\n%s\n%s\n", titles[i], indent(s.ToolResults[0].Content))
		}
	}
	mu.Lock()
	fmt.Printf("\nOn the host, the Acme API received:\n")
	for _, r := range received {
		fmt.Println("  " + r)
	}
	fmt.Printf("\nThe host's record of every request from the microVM (sandbox.egress):\n")
	for _, r := range egress {
		fmt.Println("  " + strings.TrimPrefix(r, "sandbox egress "))
	}
	mu.Unlock()
	fmt.Println("\nThe real token was only ever on the host: the microVM held a placeholder, and the reply came back redacted.")
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

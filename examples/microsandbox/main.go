// Command microsandbox runs an agent inside a microsandbox microVM and
// shows that no secret enters it:
//
//   - the VM has no network and no secret env vars;
//   - a module in the VM asks for secret://ACME_API_TOKEN and gets a
//     placeholder (eipsec_...), never the value;
//   - its HTTP request goes to the host, which swaps the placeholder for
//     the real token because the API's host is listed for that secret;
//   - the API's reply echoes the token, and it comes back redacted;
//   - the token may only be used for GET requests under /orders, so a
//     DELETE to the same API is refused, and so is any other host;
//   - the host records every request the VM makes, allowed or not.
//
// Needs msb (https://microsandbox.dev) on an arm64 Mac or a Linux host
// with KVM. Run it from the repo root:
//
//	go run ./examples/microsandbox
//
// To run the same example on microsandbox cloud (private beta), select the
// cloud backend with MSB_BACKEND=cloud and set MSB_API_KEY:
//
//	MSB_BACKEND=cloud MSB_API_KEY=... go run ./examples/microsandbox
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

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// callAPI is the module that runs inside the VM. It resolves the token and
// calls the API with it, then reports what it held and what came back. It
// is compiled into this program, so the VM binary (this same program, see
// main) has it too.
var callAPI = engine.Module{
	Name:        "call_api",
	Permissions: []string{"net"},
	Secrets:     []string{"ACME_API_TOKEN"},
	Apply: func(p *sdk.Module) error {
		client, err := p.HTTP(sdk.HTTPOptions{AllowHosts: []string{"127.0.0.1", "localhost"}, AllowPrivate: true})
		if err != nil {
			return err
		}
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
				resp, err := client.Do(req)
				if err != nil {
					return out + "request failed: " + err.Error(), nil
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return out + "API replied: " + string(b), nil
			},
		})
		return nil
	},
}

func main() {
	if kiln.InBox() {
		// Inside the VM this same program serves the agent over stdio.
		if err := kiln.ServeBox(context.Background(), os.Stdin, os.Stdout, kiln.WithModule(callAPI)); err != nil {
			log.Fatal(err)
		}
		return
	}
	if _, err := exec.LookPath("msb"); err != nil {
		log.Fatal("msb is not installed: see https://microsandbox.dev")
	}
	tmp, err := os.MkdirTemp("", "kiln-microsandbox-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	// The real token lives only in the host's environment.
	const token = "acme_live_7f3c9a1e5b2d4c6f8a0b"
	os.Setenv("ACME_API_TOKEN", token)

	// A stand-in for the Acme API on the host. It records the token it
	// receives, and echoes it back (as a careless API might).
	var mu sync.Mutex
	var received, egress []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Method+" "+r.Host+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		io.WriteString(w, `{"orders": 3, "you_sent": "`+r.Header.Get("Authorization")+`"}`)
	}))
	defer api.Close()
	other := strings.Replace(api.URL, "127.0.0.1", "localhost", 1) // same server under a name that isn't allowed

	// This program, built for the VM's OS and architecture, is what runs in
	// the VM.
	bin := filepath.Join(tmp, "agent")
	build := exec.Command("go", "build", "-o", bin, "./examples/microsandbox")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		log.Fatalf("build the example for linux/%s (run from the repo root): %v\n%s", runtime.GOARCH, err, out)
	}

	step := func(tool string, args map[string]any) map[string]any {
		return map[string]any{"tool": tool, "args": args}
	}
	if os.Getenv("MSB_BACKEND") == "cloud" {
		if out, err := exec.Command("msb", "context").CombinedOutput(); err == nil {
			fmt.Printf("%s\n", out)
		}
		fmt.Println("Booting a microsandbox cloud sandbox (no network) and starting the agent in it…")
	} else {
		fmt.Println("Booting a microsandbox VM (no network) and starting the agent in it…")
	}
	a, err := kiln.New(
		kiln.WithModel("mock:demo"),
		kiln.WithModuleConfig("model_mock", map[string]any{"script": []any{
			step("bash", map[string]any{"command": "echo \"kernel: $(uname -sr)\"; echo 'environment:'; env | sort | sed 's/^/  /'"}),
			step("call_api", map[string]any{"url": api.URL + "/orders"}),
			step("call_api", map[string]any{"url": api.URL + "/orders", "method": "DELETE"}),
			step("call_api", map[string]any{"url": other + "/steal"}),
			map[string]any{"text": "done"},
		}}),
		kiln.WithModules("tool_shell"),
		kiln.WithModule(callAPI),
		kiln.WithLogger(func(scope, msg string) {
			if strings.HasPrefix(msg, "sandbox egress") {
				mu.Lock()
				egress = append(egress, msg)
				mu.Unlock()
			}
		}),
		kiln.WithRunIn(kiln.RunIn{
			Provider: "microsandbox",
			Binary:   bin,
			Spec:     sdk.SandboxSpec{Network: sdk.NetworkPolicy{Mode: "none"}},
			// The placeholder becomes the real token only on GET requests
			// under /orders on this host.
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
		"1. Inside the VM: no secret env vars",
		"2. A module in the VM calls the API (GET /orders is allowed for the token)",
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
	fmt.Printf("\nThe host's record of every request from the VM (sandbox.egress):\n")
	for _, r := range egress {
		fmt.Println("  " + strings.TrimPrefix(r, "sandbox egress "))
	}
	mu.Unlock()
	fmt.Println("\nThe real token was only ever on the host: the VM held a placeholder, and the reply came back redacted.")
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

// Command tensorlake runs the agent inside a real Tensorlake cloud sandbox.
// The host process stays local; Tensorlake runs the VM, and kiln connects
// to it through Tensorlake's process API.
//
// Create an API key at https://docs.tensorlake.ai/sandboxes/quickstart and set
// TENSORLAKE_API_KEY:
//
//	TENSORLAKE_API_KEY=... go run ./examples/tensorlake
//
// The real token stays on the host. A module in the sandbox gets only an
// eipsec_ placeholder, the host swaps it for the allowed request, the reply is
// redacted, and every request is recorded as sandbox.egress.
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
	"github.com/ganeshdipdumbare/kiln/modules/model_mock"
	"github.com/ganeshdipdumbare/kiln/modules/tool_shell"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// callAPI is the module that runs inside the sandbox. It is compiled into
// this program, so the sandbox binary (this same program, see main) has it.
var callAPI = engine.Module{
	Name:        "call_api",
	Permissions: []string{"net"},
	Secrets:     []string{"ACME_API_TOKEN"},
	Apply: func(p *sdk.Scope) error {
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
		// Inside the sandbox this same program serves the agent over stdio.
		if err := kiln.ServeBox(context.Background(), os.Stdin, os.Stdout, kiln.Config{Modules: []kiln.Module{callAPI}}); err != nil {
			log.Fatal(err)
		}
		return
	}
	if os.Getenv("TENSORLAKE_API_KEY") == "" {
		log.Fatal("TENSORLAKE_API_KEY is not set: create a key at https://docs.tensorlake.ai/sandboxes/quickstart")
	}
	tmp, err := os.MkdirTemp("", "kiln-tensorlake-*")
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

	// Tensorlake's default Ubuntu image is x86_64. Set TL_GUEST_ARCH if your
	// image is arm64.
	guestArch := os.Getenv("TL_GUEST_ARCH")
	if guestArch == "" {
		guestArch = "amd64"
	}
	// This program, built for the sandbox's OS and architecture, is what runs
	// in the sandbox.
	bin := filepath.Join(tmp, "agent")
	build := exec.Command("go", "build", "-o", bin, "./examples/tensorlake")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+guestArch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		log.Fatalf("build the example for linux/%s (run from the repo root): %v\n%s", guestArch, err, out)
	}

	step := func(tool string, args map[string]any) map[string]any {
		return map[string]any{"tool": tool, "args": args}
	}
	hostname, _ := os.Hostname()
	fmt.Printf("Host process: %s/%s (model, secrets, broker live here)\n", runtime.GOOS, hostname)
	fmt.Println("Creating a Tensorlake cloud sandbox and starting the agent in it...")
	a, err := kiln.New(kiln.Config{
		Model: "mock:demo",
		Modules: []kiln.Module{
			model_mock.New(map[string]any{"script": []any{
				step("bash", map[string]any{"command": "echo \"machine: $(uname -m) $(uname -sr)\"; echo \"workspace: $(pwd)\"; echo 'environment:'; env | sort | sed 's/^/  /'"}),
				step("call_api", map[string]any{"url": api.URL + "/orders"}),
				step("call_api", map[string]any{"url": api.URL + "/orders", "method": "DELETE"}),
				step("call_api", map[string]any{"url": other + "/steal"}),
				map[string]any{"text": "done"},
			}}),
			tool_shell.New(),
			callAPI,
			tensorlakeProvider(),
		},
		Logger: func(scope, msg string) {
			switch {
			case strings.HasPrefix(msg, "sandbox egress"):
				mu.Lock()
				egress = append(egress, msg)
				mu.Unlock()
			case strings.HasPrefix(msg, "created Tensorlake sandbox"):
				mu.Lock()
				fmt.Println(msg)
				mu.Unlock()
			}
		},
		RunIn: &kiln.RunIn{
			Provider:           "tensorlake",
			Binary:             bin,
			Spec:               sdk.SandboxSpec{Network: sdk.NetworkPolicy{Mode: "none"}},
			AllowPrivateEgress: true, // the demo API is a local httptest server
			SecretHosts: map[string][]string{
				"ACME_API_TOKEN": {"GET 127.0.0.1/orders*"},
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	res, err := a.Generate(context.Background(), "show me")
	if err != nil {
		log.Fatal(err)
	}
	titles := []string{
		"1. Inside the Tensorlake sandbox: no secret env vars",
		"2. A module in the sandbox calls the API (GET /orders is allowed for the token)",
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
	fmt.Printf("\nThe host's record of every request from the sandbox (sandbox.egress):\n")
	for _, r := range egress {
		fmt.Println("  " + strings.TrimPrefix(r, "sandbox egress "))
	}
	mu.Unlock()
	if err := checkSecretRedeem(res, received, egress, token); err != nil {
		a.Close()
		log.Fatal(err)
	}
	fmt.Println("\nSecret redemption checks: OK (placeholder in the sandbox, real token only at the host API, redacted reply, denied egress).")
	fmt.Println("The real token was only ever on the host: the cloud sandbox held a placeholder, and the reply came back redacted.")
}

func checkSecretRedeem(res *kiln.Result, received, egress []string, real string) error {
	if len(res.Steps) < 4 {
		return fmt.Errorf("secret check: expected 4 steps, got %d", len(res.Steps))
	}
	tool := func(i int) string {
		if len(res.Steps[i].ToolResults) == 0 {
			return ""
		}
		return res.Steps[i].ToolResults[0].Content
	}
	get := tool(1)
	if strings.Contains(get, real) {
		return fmt.Errorf("secret check: the real token appeared in the sandbox")
	}
	if !strings.Contains(get, sdk.PlaceholderPrefix) || !strings.Contains(get, "[REDACTED]") {
		return fmt.Errorf("secret check: expected a placeholder and a redacted reply, got %q", get)
	}
	for _, i := range []int{2, 3} {
		if !strings.Contains(tool(i), "may not be sent") {
			return fmt.Errorf("secret check: step %d was not refused: %q", i, tool(i))
		}
	}
	if !strings.Contains(strings.Join(received, "\n"), real) {
		return fmt.Errorf("secret check: the host API never received the real token")
	}
	joined := strings.Join(egress, "\n")
	if !strings.Contains(joined, "allowed GET 127.0.0.1/orders") || !strings.Contains(joined, "denied DELETE 127.0.0.1/orders") {
		return fmt.Errorf("secret check: sandbox.egress is missing the allowed/denied records:\n%s", joined)
	}
	return nil
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

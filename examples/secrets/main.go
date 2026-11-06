// Command secrets shows how kiln keeps API keys safe, offline:
//
//   - secrets_vault reads secret://NAME from HashiCorp Vault (a fake Vault
//     runs in-process here) and re-reads it after ttl_seconds, so a
//     rotated key reaches running modules without a restart;
//
//   - grants give each module only its own secrets, and the core binds
//     the caller, so a module can't ask as another one;
//
//   - every value handed out is redacted from logs, traces and audit,
//     even when a tool leaks it into its result;
//
//   - every read is audited as secret.access.
//
//     go run ./examples/secrets
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/modules/audit_file"
	"github.com/ganeshdipdumbare/kiln/modules/hook_log"
	"github.com/ganeshdipdumbare/kiln/modules/model_mock"
	"github.com/ganeshdipdumbare/kiln/modules/secrets_vault"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

func main() {
	ctx := context.Background()
	tmp, _ := os.MkdirTemp("", "kiln-secrets-*")
	defer os.RemoveAll(tmp)

	// A stand-in for Vault's KV v2 API, holding one key we can rotate.
	var mu sync.Mutex
	stripeKey := "sk_live_51HxDEMOoriginal"
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "dev-root-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		v := map[string]string{"/v1/secret/data/kiln/STRIPE_API_KEY": stripeKey}[r.URL.Path]
		mu.Unlock()
		if v == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": v}}})
	}))
	defer vault.Close()
	os.Setenv("VAULT_TOKEN", "dev-root-token") // in production: a Vault agent token_file

	// billing is granted the Stripe key. Its tool leaks the key into its
	// result on purpose, to show redaction. It is a compiled application
	// module (Config.Modules): it declares the one secret it may resolve.
	billing := kiln.Module{
		Name:     "billing",
		Requires: []string{"secrets"},
		Secrets:  []string{"STRIPE_API_KEY"},
		Apply: func(pc *sdk.Scope) error {
			pc.AddTool(kiln.NewTool("charge", "Charge a customer",
				func(c context.Context, in struct {
					Amount float64 `json:"amount"`
				}) (string, error) {
					key, err := pc.Secret("secret://STRIPE_API_KEY") // resolved per call: rotation-safe
					if err != nil {
						return "", err
					}
					pc.Logf("charging $%.2f with key %s", in.Amount, key)
					return fmt.Sprintf("charged $%.2f (debug: used key %s)", in.Amount, key), nil
				}))
			return nil
		},
	}
	// analytics has no grant. It tries to read the key directly, claiming
	// to be billing.
	var stolen error
	analytics := kiln.Module{Name: "analytics", Requires: []string{"secrets"}, Apply: func(pc *sdk.Scope) error {
		s := pc.Service("secrets").(*sdk.Secrets)
		_, stolen = s.Get(context.Background(), "billing", "STRIPE_API_KEY")
		return nil
	}}

	var logs []string
	auditPath := filepath.Join(tmp, "audit.jsonl")
	a, err := kiln.New(kiln.Config{
		Model: "mock:demo",
		Modules: []kiln.Module{
			model_mock.New(map[string]any{"script": []any{
				map[string]any{"tool": "charge", "args": map[string]any{"amount": 42}},
				map[string]any{"text": "Charged $42."},
				map[string]any{"tool": "charge", "args": map[string]any{"amount": 7}},
				map[string]any{"text": "Charged $7."},
			}}),
			secrets_vault.New(map[string]any{
				"address": vault.URL, "ttl_seconds": 0, // 0 = re-read every time, to show rotation
				"grants": map[string]any{"billing": []any{"STRIPE_API_KEY"}},
			}),
			audit_file.New(map[string]any{"path": auditPath, "results": true}),
			hook_log.New(map[string]any{"results": true}),
			billing,
			analytics,
		},
		Logger: func(scope, msg string) { logs = append(logs, "["+scope+"] "+msg) },
	})
	check(err)
	defer a.Close()

	fmt.Println("1. Grants are per module, and the caller can't be faked")
	fmt.Printf("   analytics asking as billing: %v\n\n", stolen)

	fmt.Println("2. The model calls billing's tool; the result leaks the key")
	res, err := a.Generate(ctx, "charge $42")
	check(err)
	fmt.Printf("   the tool returned:  %s\n", res.Steps[0].ToolResults[0].Content)
	fmt.Println("   (the model and your app see real values; sinks never do)")

	fmt.Println("\n3. Rotate the key in Vault: the next call uses the new one")
	mu.Lock()
	stripeKey = "sk_live_51HxDEMOrotated"
	mu.Unlock()
	res, err = a.Generate(ctx, "charge $7")
	check(err)
	fmt.Printf("   the tool returned:  %s\n", res.Steps[0].ToolResults[0].Content)

	fmt.Println("\n4. What the logs and the audit log contain")
	for _, l := range logs {
		if strings.Contains(l, "key") {
			fmt.Println("   log:  ", l)
		}
	}
	f, _ := os.Open(auditPath)
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line struct {
			Body struct {
				Type string         `json:"type"`
				Data map[string]any `json:"data"`
			} `json:"body"`
		}
		json.Unmarshal(sc.Bytes(), &line)
		d := line.Body.Data
		switch line.Body.Type {
		case "secret.access":
			fmt.Printf("   audit: secret.access  %s -> %s (%v)\n", d["module"], d["secret"], d["outcome"])
		case "tool.call":
			fmt.Printf("   audit: tool.call      %s result=%q\n", d["tool"], d["result"])
		}
	}
	b, _ := os.ReadFile(auditPath)
	leaked := strings.Contains(string(b), "sk_live_") || strings.Contains(strings.Join(logs, "\n"), "sk_live_")
	fmt.Printf("\n   any key in the logs or audit log? %v\n", leaked)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

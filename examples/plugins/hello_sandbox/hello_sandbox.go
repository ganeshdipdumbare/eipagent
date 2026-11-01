//go:build ignore

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"eipagent/sdk"
)

// hello_sandbox is a sandbox provider, registered as "sandbox/hello_sandbox",
// for a remote sandbox service with a small JSON API:
//
//	POST /sandboxes            {image, cpus, memory_mb, network} -> {id}
//	POST /sandboxes/<id>/exec  {cmd, args, shell, stdin, timeout} -> {stdout, stderr, exit_code}
//	DELETE /sandboxes/<id>
//
// Adapt the requests to your service (microsandbox, E2B, Firecracker...).
//
// Config: url (required), token ("secret://hello_sandbox_TOKEN").
var Permissions = []string{"net"}

var Requires = []string{"?secrets"}

func api(p *sdk.Plugin, c context.Context, method, url, tokenRef string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(c, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok, err := p.Secret(tokenRef); err == nil && tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("hello_sandbox: %s %s: HTTP %d", method, url, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func open(p *sdk.Plugin, base, tokenRef, id string) *sdk.Sandbox {
	return &sdk.Sandbox{
		ID:       id,
		Provider: "hello_sandbox",
		Exec: func(c context.Context, req sdk.ExecRequest) (*sdk.ExecResult, error) {
			var out struct {
				Stdout   string `json:"stdout"`
				Stderr   string `json:"stderr"`
				ExitCode int    `json:"exit_code"`
			}
			in := map[string]any{"cmd": req.Cmd, "args": req.Args, "shell": req.Shell, "stdin": string(req.Stdin), "timeout": req.TimeoutSeconds}
			t := time.Now()
			if err := api(p, c, "POST", base+"/sandboxes/"+id+"/exec", tokenRef, in, &out); err != nil {
				return nil, err
			}
			return &sdk.ExecResult{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode, DurationMS: time.Since(t).Milliseconds()}, nil
		},
		Close: func(c context.Context) error {
			return api(p, c, "DELETE", base+"/sandboxes/"+id, tokenRef, nil, nil)
		},
	}
}

func Apply(p *sdk.Plugin) error {
	base := strings.TrimRight(p.ConfigString("url", ""), "/")
	if base == "" {
		return fmt.Errorf("hello_sandbox: config url is required")
	}
	tokenRef := p.ConfigString("token", "secret://hello_sandbox_TOKEN")
	return p.Provide("sandbox/hello_sandbox", &sdk.SandboxProvider{
		Name:      "hello_sandbox",
		Isolation: "remote",
		Create: func(c context.Context, spec sdk.SandboxSpec) (*sdk.Sandbox, error) {
			var out struct {
				ID string `json:"id"`
			}
			in := map[string]any{"image": spec.Image, "cpus": spec.CPUs, "memory_mb": spec.MemoryMB, "network": spec.Network.Mode}
			if err := api(p, c, "POST", base+"/sandboxes", tokenRef, in, &out); err != nil {
				return nil, err
			}
			return open(p, base, tokenRef, out.ID), nil
		},
		Attach: func(c context.Context, id string) (*sdk.Sandbox, error) {
			return open(p, base, tokenRef, id), nil
		},
	})
}

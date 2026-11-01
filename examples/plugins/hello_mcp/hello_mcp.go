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

// hello_mcp exposes a remote tool API as tools. At load it GETs
// <url>/tools ([{name, description, schema}]) and registers each as
// hello_mcp__<name>; calls POST <url>/tools/<name> with the JSON args.
// For real MCP servers use the built-in mcp_client plugin; this template
// shows how to adapt any HTTP tool API the same way.
//
// Config: url (required), token ("secret://hello_mcp_TOKEN").
var Permissions = []string{"net"}

var Requires = []string{"?secrets"}

type remoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
}

func call(p *sdk.Plugin, c context.Context, client *http.Client, method, url, tokenRef string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(c, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok, err := p.Secret(tokenRef); err == nil && tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return b, nil
}

func addTool(p *sdk.Plugin, client *http.Client, base, tokenRef string, t remoteTool) {
	p.AddTool(sdk.Tool{
		Name:        "hello_mcp__" + t.Name,
		Description: t.Description,
		Schema:      t.Schema,
		Run: func(c context.Context, args map[string]any) (string, error) {
			body, _ := json.Marshal(args)
			b, err := call(p, c, client, "POST", base+"/tools/"+t.Name, tokenRef, body)
			return string(b), err
		},
	})
}

func Apply(p *sdk.Plugin) error {
	base := strings.TrimRight(p.ConfigString("url", ""), "/")
	if base == "" {
		return fmt.Errorf("hello_mcp: config url is required")
	}
	tokenRef := p.ConfigString("token", "secret://hello_mcp_TOKEN")
	client := &http.Client{Timeout: 30 * time.Second}
	b, err := call(p, context.Background(), client, "GET", base+"/tools", tokenRef, nil)
	if err != nil {
		return err
	}
	var tools []remoteTool
	if err := json.Unmarshal(b, &tools); err != nil {
		return fmt.Errorf("hello_mcp: bad tool list: %v", err)
	}
	for _, t := range tools {
		addTool(p, client, base, tokenRef, t)
	}
	return nil
}

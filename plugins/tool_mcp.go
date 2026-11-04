//go:build ignore

package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"eipagent/sdk"
)

// Model Context Protocol client: connects to MCP servers and registers
// their tools as "<server>__<tool>".
//
// Config:
//
//	servers: [{
//	  name:    "github",
//	  command: "npx", args: ["-y", "@modelcontextprotocol/server-github"], env: {"K": "secret://K"},   // stdio
//	  url:     "https://mcp.example.com/mcp", headers: {"Authorization": "secret://MCP_TOKEN"},        // or streamable HTTP
//	  tools:   ["only", "these"],
//	  trust_annotations: false,  // true: treat the server's readOnlyHint as read-only for policy
//	  allow_insecure: false,     // allow http with headers (loopback is always allowed)
//	  timeout_seconds: 60
//	}]
//
// Example:
//
//	eipagent.WithPlugin("tool_mcp", map[string]any{"servers": []any{
//		map[string]any{"name": "github", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-github"},
//			"env": map[string]any{"GITHUB_PERSONAL_ACCESS_TOKEN": "secret://GITHUB_TOKEN"}},
//		map[string]any{"name": "docs", "url": "https://mcp.example.com/mcp"},
//	}})
//	// Shortcut: eipagent.WithMCP("github", "npx", "-y", "@modelcontextprotocol/server-github")
var Permissions = []string{"net", "exec"}

var Requires = []string{"?secrets"} // secret:// keys/headers are optional

// rpcMsg is only decoded into. Outgoing messages are built as maps:
// Yaegi can't json.Marshal interface fields of interpreted structs.
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type client struct {
	name    string
	timeout time.Duration
	// call sends a request and waits for its result.
	call   func(c context.Context, method string, params map[string]any) (json.RawMessage, error)
	notify func(method string, params map[string]any) error
	close  func()
}

func Apply(p *sdk.Plugin) error {
	servers := p.ConfigList("servers")
	if len(servers) == 0 {
		return errors.New("tool_mcp: config servers is empty")
	}
	for _, s := range servers {
		cl, err := connect(p, s)
		if err != nil {
			return err
		}
		p.OnUnload(cl.close)
		if err := register(p, cl, s); err != nil {
			return err
		}
	}
	return nil
}

func request(id *int64, method string, params map[string]any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		m["id"] = *id
	}
	if params != nil {
		m["params"] = params
	}
	return m
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func resolveMap(p *sdk.Plugin, raw any) (map[string]string, error) {
	out := map[string]string{}
	m, _ := raw.(map[string]any)
	for k, v := range m {
		s := ""
		if x, ok := v.(string); ok {
			s = x
		}
		r, err := p.SecretValue(s)
		if err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, nil
}

func connect(p *sdk.Plugin, s map[string]any) (*client, error) {
	name := str(s, "name")
	if name == "" {
		return nil, errors.New("tool_mcp: every server needs a name")
	}
	timeout := 60 * time.Second
	if t, ok := s["timeout_seconds"].(float64); ok {
		timeout = time.Duration(t) * time.Second
	}
	var cl *client
	var err error
	switch {
	case str(s, "command") != "":
		cl, err = stdioClient(p, name, s)
	case str(s, "url") != "":
		cl, err = httpClient(p, name, s)
	default:
		return nil, fmt.Errorf("tool_mcp %s: needs command or url", name)
	}
	if err != nil {
		return nil, err
	}
	cl.name, cl.timeout = name, timeout
	c, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err = cl.call(c, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "eipagent", "version": "0.1.0"},
	})
	if err != nil {
		cl.close()
		return nil, fmt.Errorf("tool_mcp %s: initialize: %w", name, err)
	}
	if err := cl.notify("notifications/initialized", map[string]any{}); err != nil {
		p.Logf("tool_mcp %s: notifications/initialized: %v", name, err)
	}
	return cl, nil
}

func stdioClient(p *sdk.Plugin, name string, s map[string]any) (*client, error) {
	var args []string
	if raw, ok := s["args"].([]any); ok {
		for _, a := range raw {
			args = append(args, fmt.Sprint(a))
		}
	}
	env, err := resolveMap(p, s["env"])
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(str(s, "command"), args...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tool_mcp %s: start: %w", name, err)
	}
	var mu sync.Mutex
	var writeMu sync.Mutex
	var seq int64
	waiting := map[int64]chan rpcMsg{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var m rpcMsg
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil || m.Method != "" {
				continue // notifications and server requests are ignored
			}
			mu.Lock()
			ch := waiting[*m.ID]
			delete(waiting, *m.ID)
			mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}()
	write := func(m map[string]any) error {
		b, _ := json.Marshal(m)
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err := stdin.Write(append(b, '\n'))
		return err
	}
	return &client{
		call: func(c context.Context, method string, params map[string]any) (json.RawMessage, error) {
			mu.Lock()
			seq++
			id := seq
			ch := make(chan rpcMsg, 1)
			waiting[id] = ch
			mu.Unlock()
			forget := func() {
				mu.Lock()
				delete(waiting, id)
				mu.Unlock()
			}
			if err := write(request(&id, method, params)); err != nil {
				forget()
				return nil, err
			}
			select {
			case m := <-ch:
				if m.Error != nil {
					return nil, fmt.Errorf("%s: %s", method, m.Error.Message)
				}
				return m.Result, nil
			case <-done:
				forget()
				return nil, fmt.Errorf("server %s exited", name)
			case <-c.Done():
				forget()
				return nil, c.Err()
			}
		},
		notify: func(method string, params map[string]any) error {
			return write(request(nil, method, params))
		},
		close: func() {
			stdin.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cmd.Process.Kill()
			}
			cmd.Wait()
		},
	}, nil
}

func httpClient(p *sdk.Plugin, name string, s map[string]any) (*client, error) {
	url := str(s, "url")
	headers, err := resolveMap(p, s["headers"])
	if err != nil {
		return nil, err
	}
	rawHeaders := map[string]string{}
	if m, ok := s["headers"].(map[string]any); ok {
		for k, v := range m {
			if str, ok := v.(string); ok {
				rawHeaders[k] = str
			}
		}
	}
	allowInsecure, _ := s["allow_insecure"].(bool)
	if err := sdk.CheckCredentialURL(url, len(headers) > 0, allowInsecure); err != nil {
		return nil, fmt.Errorf("tool_mcp %s: %w", name, err)
	}
	timeout := 60 * time.Second
	if t, ok := s["timeout_seconds"].(float64); ok {
		timeout = time.Duration(t) * time.Second
	}
	hc := &http.Client{CheckRedirect: sdk.RefuseRedirect}
	var mu sync.Mutex
	var seq int64
	session := ""
	post := func(c context.Context, m map[string]any) (*rpcMsg, error) {
		b, _ := json.Marshal(m)
		req, err := http.NewRequestWithContext(c, "POST", url, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		for k, ref := range rawHeaders {
			val, err := p.SecretValue(ref)
			if err != nil {
				return nil, err
			}
			req.Header.Set(k, val)
		}
		mu.Lock()
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		mu.Unlock()
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			mu.Lock()
			session = sid
			mu.Unlock()
		}
		wantID, isCall := m["id"].(int64)
		if !isCall {
			if resp.StatusCode >= 300 {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
				return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
			}
			return nil, nil
		}
		if resp.StatusCode == 202 {
			return nil, errors.New("mcp: no response")
		}
		if resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
		}
		var out *rpcMsg
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			err = sdk.ReadSSE(resp.Body, func(ev, data string) error {
				var r rpcMsg
				if json.Unmarshal([]byte(data), &r) == nil && r.ID != nil && *r.ID == wantID && r.Method == "" {
					out = &r
					return io.EOF
				}
				return nil
			})
			if err == io.EOF {
				err = nil
			}
		} else {
			var r rpcMsg
			err = json.NewDecoder(resp.Body).Decode(&r)
			if err == nil && (r.ID == nil || *r.ID != wantID || r.Method != "") {
				return nil, errors.New("mcp: no response")
			}
			out = &r
		}
		if err != nil {
			return nil, err
		}
		if out == nil {
			return nil, errors.New("mcp: no response")
		}
		return out, nil
	}
	return &client{
		call: func(c context.Context, method string, params map[string]any) (json.RawMessage, error) {
			mu.Lock()
			seq++
			id := seq
			mu.Unlock()
			r, err := post(c, request(&id, method, params))
			if err != nil {
				return nil, err
			}
			if r == nil {
				return nil, errors.New("mcp: no response")
			}
			if r.Error != nil {
				return nil, fmt.Errorf("%s: %s", method, r.Error.Message)
			}
			return r.Result, nil
		},
		notify: func(method string, params map[string]any) error {
			c, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_, err := post(c, request(nil, method, params))
			return err
		},
		close: func() {
			mu.Lock()
			sid := session
			mu.Unlock()
			if sid != "" {
				req, _ := http.NewRequest("DELETE", url, nil)
				req.Header.Set("Mcp-Session-Id", sid)
				if resp, err := hc.Do(req); err == nil {
					resp.Body.Close()
				}
			}
		},
	}, nil
}

func register(p *sdk.Plugin, cl *client, s map[string]any) error {
	trustAnn, _ := s["trust_annotations"].(bool)
	allow := map[string]bool{}
	if raw, ok := s["tools"].([]any); ok {
		for _, t := range raw {
			allow[fmt.Sprint(t)] = true
		}
	}
	cursor := ""
	for page := 0; page < 50; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		c, cancel := context.WithTimeout(context.Background(), cl.timeout)
		raw, err := cl.call(c, "tools/list", params)
		cancel()
		if err != nil {
			return fmt.Errorf("tool_mcp %s: %w", cl.name, err)
		}
		var list struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
				Annotations map[string]any `json:"annotations"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		for _, t := range list.Tools {
			if len(allow) > 0 && !allow[t.Name] {
				continue
			}
			meta := map[string]any{"mcp_server": cl.name}
			// readOnlyHint is the server's own claim. The policy's read-only
			// mode treats "readonly" as permission, so it's only trusted for
			// servers the operator vouches for (trust_annotations: true).
			if ro, ok := t.Annotations["readOnlyHint"].(bool); ok && ro {
				meta["mcp_read_only_hint"] = true
				if trustAnn {
					meta["readonly"] = true
				}
			}
			if d, ok := t.Annotations["destructiveHint"].(bool); ok && d {
				meta["risk"] = "high"
			}
			// Built in a helper: Yaegi shares loop-body variables between closures.
			p.AddTool(mcpTool(cl, t.Name, t.Description, t.InputSchema, meta))
		}
		if list.NextCursor == "" {
			return nil
		}
		cursor = list.NextCursor
	}
	return nil
}

func mcpTool(cl *client, toolName, desc string, schema, meta map[string]any) sdk.Tool {
	return sdk.Tool{
		Name:        cl.name + "__" + toolName,
		Description: desc,
		Schema:      schema,
		Meta:        meta,
		Run: func(c context.Context, args map[string]any) (string, error) {
			c2, cancel := context.WithTimeout(c, cl.timeout)
			defer cancel()
			raw, err := cl.call(c2, "tools/call", map[string]any{"name": toolName, "arguments": args})
			if err != nil {
				return "", err
			}
			var res struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				StructuredContent map[string]any `json:"structuredContent"`
				IsError           bool           `json:"isError"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return "", err
			}
			var parts []string
			for _, part := range res.Content {
				if part.Type == "text" {
					parts = append(parts, part.Text)
				} else {
					parts = append(parts, "["+part.Type+" content]")
				}
			}
			out := strings.Join(parts, "\n")
			if out == "" && res.StructuredContent != nil {
				out = sdk.ToJSON(res.StructuredContent)
			}
			if res.IsError {
				return "", errors.New(out)
			}
			return out, nil
		},
	}
}

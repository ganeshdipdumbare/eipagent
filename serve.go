package eipagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/rpc"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// Serve runs an agent that is driven over JSON-RPC on r/w — this is
// `eip serve --stdio`, what runs inside a sandbox for WithRunIn. Model
// calls, secrets, HTTP and host-only tools are bridged back to the host
// (bridge_llm, bridge_secrets, host.http, bridge_tools), so API keys
// and other secrets never enter the sandbox.
func Serve(ctx context.Context, r io.Reader, w io.Writer, opts ...Option) error {
	conn := rpc.New(r, w, nil)
	go conn.Serve()
	defer conn.Close()

	// Fail closed: a box whose environment holds a credential was set up
	// wrong, and every plugin and command in it could read the value.
	for _, kv := range os.Environ() {
		name, v, _ := strings.Cut(kv, "=")
		if sdk.SecretLike(name) && v != "" && len(sdk.FindPlaceholders(v)) == 0 {
			err := fmt.Errorf("serve: refusing to start: the sandbox environment holds %s; secrets stay on the host", name)
			conn.Notify("box.error", err.Error())
			return err
		}
	}

	var hello wireHello
	if err := conn.Call(ctx, "host.hello", struct{}{}, &hello); err != nil {
		return fmt.Errorf("serve: hello: %w", err)
	}
	chunks := &chunkRouter{subs: map[string]func(sdk.Chunk){}}
	conn.Handle("host.llm.chunk", func(_ context.Context, raw json.RawMessage) (any, error) {
		var c wireChunk
		if err := json.Unmarshal(raw, &c); err == nil {
			chunks.send(c)
		}
		return nil, nil
	})
	opts = append(opts, withoutProviderAutoload(),
		WithCompiledPlugin(bridgeLLM(conn, chunks, hello.Providers)),
		WithCompiledPlugin(bridgeTools(conn, hello.Tools)))
	if hello.Secrets {
		opts = append(opts, withBridgedSecrets(bridgeSecrets(conn)))
	}
	if hello.Egress {
		// Plugins' HTTP goes through the host (clients with their own
		// Transport dial directly, which a no-network box refuses).
		http.DefaultTransport = &hostTransport{conn: conn}
	}
	a, err := New(opts...)
	if err != nil {
		conn.Notify("box.error", err.Error())
		return err
	}
	defer a.Close()

	conn.Handle("agent.run", func(c context.Context, raw json.RawMessage) (any, error) {
		var wr wireRun
		if err := json.Unmarshal(raw, &wr); err != nil {
			return nil, err
		}
		ro := &runOptions{}
		if wr.Streaming {
			ro.onEvent = func(e sdk.Event) { conn.Notify("agent.event", wireEvent{RunID: wr.RunID, Event: e}) }
		}
		st := stateFromWire(wr)
		if st.Model == "" {
			st.Model = a.model
		}
		if st.System == "" {
			st.System = a.instructions
		}
		return a.run(c, st, ro)
	})
	conn.Handle("agent.thread", func(c context.Context, raw json.RawMessage) (any, error) {
		var wr wireRun
		if err := json.Unmarshal(raw, &wr); err != nil {
			return nil, err
		}
		th := a.Thread(wr.Thread)
		if len(wr.Decisions) > 0 {
			return th.Resume(c, wr.Decisions...)
		}
		var ro []RunOption
		if wr.Streaming {
			ro = append(ro, OnEvent(func(e sdk.Event) { conn.Notify("agent.event", wireEvent{RunID: wr.RunID, Event: e}) }))
		}
		if wr.User != "" {
			ro = append(ro, AsUser(wr.User, wr.Roles...))
		}
		if wr.MaxSteps > 0 {
			ro = append(ro, MaxSteps(wr.MaxSteps))
		}
		return th.Generate(c, wr.Prompt, ro...)
	})
	conn.Handle("thread.get", func(c context.Context, raw json.RawMessage) (any, error) {
		var wr wireRun
		json.Unmarshal(raw, &wr)
		msgs, err := a.Thread(wr.Thread).Messages(c)
		if err != nil {
			return nil, err
		}
		pend, err := a.Thread(wr.Thread).Pending(c)
		return wireThreadState{msgs, pend}, err
	})
	conn.Handle("agent.status", func(context.Context, json.RawMessage) (any, error) {
		var rows []string
		for _, s := range a.Status() {
			rows = append(rows, s.String())
		}
		return rows, nil
	})
	conn.Notify("box.ready", struct{}{})
	select {
	case <-conn.Done():
	case <-ctx.Done():
	}
	return nil
}

func stateFromWire(wr wireRun) *sdk.RunState {
	st := &sdk.RunState{Thread: wr.Thread, User: wr.User, Roles: wr.Roles, Model: wr.Model, System: wr.System,
		Messages: wr.Messages, MaxSteps: wr.MaxSteps, StopOnTools: wr.StopOnTools, ToolChoice: wr.ToolChoice,
		ResponseSchema: wr.ResponseSchema, ResponseName: wr.ResponseName, Temperature: wr.Temperature,
		MaxTokens: wr.MaxTokens, Pending: wr.Pending, Decisions: wr.Decisions, Values: wr.Values}
	if st.Values == nil {
		st.Values = map[string]any{}
	}
	return st
}

type chunkRouter struct {
	mu   sync.Mutex
	subs map[string]func(sdk.Chunk)
}

func (r *chunkRouter) sub(id string, fn func(sdk.Chunk)) func() {
	r.mu.Lock()
	r.subs[id] = fn
	r.mu.Unlock()
	return func() { r.mu.Lock(); delete(r.subs, id); r.mu.Unlock() }
}

func (r *chunkRouter) send(c wireChunk) {
	r.mu.Lock()
	fn := r.subs[c.CallID]
	r.mu.Unlock()
	if fn != nil {
		fn(c.Chunk)
	}
}

var callSeq atomic.Int64

// bridgeLLM provides model/<p> for every provider the host offers; calls
// run on the host, through the host's own middleware and policy.
func bridgeLLM(conn *rpc.Conn, chunks *chunkRouter, providers []string) engine.Plugin {
	return engine.Plugin{Name: "bridge_llm", Apply: func(ctx *sdk.Context) error {
		for _, p := range providers {
			name := p
			err := ctx.Provide("model/"+name, &sdk.Provider{Name: "bridge:" + name, Capabilities: map[string]bool{"streaming": true, "tools": true, "structured_output": true},
				Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
					id := fmt.Sprintf("m%d", callSeq.Add(1))
					wr := wireModelReq{CallID: id, Model: req.Model, System: req.System, Messages: req.Messages, Tools: toWireTools(req.Tools),
						ToolChoice: req.ToolChoice, ResponseSchema: req.ResponseSchema, ResponseName: req.ResponseName,
						Temperature: req.Temperature, MaxTokens: req.MaxTokens, Extra: req.Extra, Thread: req.Thread, User: req.User, Step: req.Step,
						Stream: req.Stream != nil}
					if req.Stream != nil {
						defer chunks.sub(id, req.Stream)()
					}
					var resp sdk.ModelResponse
					if err := conn.Call(c, "host.llm", wr, &resp); err != nil {
						return nil, err
					}
					return &resp, nil
				}})
			if err != nil {
				return err
			}
		}
		return nil
	}}
}

// bridgeTools exposes host-only tools (inline Go tools) inside the box.
func bridgeTools(conn *rpc.Conn, tools []wireTool) engine.Plugin {
	return engine.Plugin{Name: "bridge_tools", Apply: func(ctx *sdk.Context) error {
		for _, t := range tools {
			name := t.Name
			ctx.AddTool(sdk.Tool{Name: name, Description: t.Description, Schema: t.Schema, Meta: t.Meta,
				Run: func(c context.Context, args map[string]any) (string, error) {
					var out string
					err := conn.Call(c, "host.tool", wireToolCall{Name: name, Args: args, Info: sdk.RunInfoFrom(c)}, &out)
					if err != nil && strings.HasPrefix(err.Error(), "rpc: ") {
						return "", sdk.Fatal(err)
					}
					return out, err
				}})
		}
		return nil
	}}
}

// bridgeSecrets provides "secrets" in the box: each lookup returns a
// placeholder from the host, never a value.
func bridgeSecrets(conn *rpc.Conn) engine.Plugin {
	return engine.Plugin{Name: "bridge_secrets", Apply: func(ctx *sdk.Context) error {
		return ctx.Provide("secrets", &sdk.Secrets{Get: func(c context.Context, caller, name string) (string, error) {
			var ph string
			err := conn.Call(c, "host.secret", wireSecretReq{Caller: caller, Name: name}, &ph)
			return ph, err
		}})
	}}
}

// hostTransport sends HTTP requests to the host (host.http), which
// swaps placeholders for secrets on allowed hosts and makes the call.
type hostTransport struct{ conn *rpc.Conn }

func (t *hostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	h := map[string][]string(req.Header.Clone())
	if req.Host != "" && req.Host != req.URL.Host {
		h["Host"] = []string{req.Host}
	}
	var out wireHTTPResp
	if err := t.conn.Call(req.Context(), "host.http", wireHTTPReq{Method: req.Method, URL: req.URL.String(), Header: h, Body: body}, &out); err != nil {
		return nil, err
	}
	return &http.Response{Status: fmt.Sprintf("%d %s", out.Status, http.StatusText(out.Status)), StatusCode: out.Status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header(out.Header),
		Body: io.NopCloser(bytes.NewReader(out.Body)), ContentLength: int64(len(out.Body)), Request: req}, nil
}

// withBridgedSecrets makes p the box's only "secrets" provider.
func withBridgedSecrets(p engine.Plugin) Option {
	return func(c *config) {
		c.bridgedSecrets = &p
		c.compiled = append(c.compiled, p)
	}
}

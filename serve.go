package kiln

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/rpc"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// ServeBox runs the agent inside a sandbox box, driven over JSON-RPC on
// r/w (stdin and stdout of the box process). It is called by the program
// itself when InBox reports true, with the same options the host used.
// The box takes its profile and policy from the files the host uploaded
// (KILN_BOX_DIR), so it cannot run anything the host did not allow.
// Model calls, secrets, HTTP and host-only tools are bridged back to the
// host, so API keys and other secrets never enter the box.
func ServeBox(ctx context.Context, r io.Reader, w io.Writer, cfg Config) error {
	// Keep module prints off the JSON-RPC stream.
	oldStdout := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = oldStdout }()
	conn := rpc.New(r, w, nil)
	go conn.Serve()
	defer conn.Close()

	// Fail closed: a box whose environment holds a credential was set up
	// wrong, and every module and command in it could read the value.
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

	cfg.RunIn = nil
	cfg.Tools = nil // host-only tools come back through bridge_tools
	prof, pol, err := boxInputs(cfg)
	if err != nil {
		conn.Notify("box.error", err.Error())
		return err
	}
	// The host keeps model providers and secret providers; the box gets
	// bridges to them instead.
	prof.Modules = dropRows(prof.Modules, isHostRow)
	pol.Require = dropSpecs(pol.Require, isHostRow)
	// The bridges are the box's rows for the host's model, tools and
	// secrets, so they start before the modules that need them.
	bridges := []engine.Module{bridgeLLM(conn, chunks, hello.Providers), bridgeTools(conn, hello.Tools)}
	if hello.Secrets {
		bridges = append(bridges, bridgeSecrets(conn))
	}
	cfg.Modules = append(cfg.Modules, bridges...)
	var bridgeRows []engine.ModuleSpec
	for _, b := range bridges {
		bridgeRows = append(bridgeRows, engine.ModuleSpec{Use: b.Name})
	}
	prof.Modules = append(bridgeRows, prof.Modules...)
	if hello.Egress {
		// Every broker HTTP request is made by the host, which swaps secret
		// placeholders on allowed hosts and records egress.
		ht := &hostTransport{conn: conn}
		sdk.SetEgressBridge(ht.RoundTrip)
		defer sdk.SetEgressBridge(nil)
	}
	a := newAgent(cfg, prof)
	plan, err := compose(cfg, prof, pol, a)
	if err != nil {
		conn.Notify("box.error", err.Error())
		return err
	}
	eng, err := plan.Start()
	if err != nil {
		conn.Notify("box.error", err.Error())
		return err
	}
	a.eng = eng
	defer a.Close()

	conn.Handle("agent.run", func(c context.Context, raw json.RawMessage) (any, error) {
		var wr wireRun
		if err := json.Unmarshal(raw, &wr); err != nil {
			return nil, err
		}
		var run Run
		if wr.Streaming {
			run.OnEvent = func(e sdk.Event) { conn.Notify("agent.event", wireEvent{RunID: wr.RunID, Event: e}) }
		}
		st := stateFromWire(wr)
		if st.Model == "" {
			st.Model = a.model
		}
		if st.System == "" {
			st.System = a.instructions
		}
		return a.exec(c, st, run)
	})
	conn.Handle("agent.thread", func(c context.Context, raw json.RawMessage) (any, error) {
		var wr wireRun
		if err := json.Unmarshal(raw, &wr); err != nil {
			return nil, err
		}
		th := a.Thread(wr.Thread)
		if len(wr.Decisions) > 0 {
			return th.Resume(c, wr.Decisions)
		}
		run := Run{User: wr.User, Roles: wr.Roles, MaxSteps: wr.MaxSteps, RunID: wr.RunID}
		if wr.Streaming {
			run.OnEvent = func(e sdk.Event) { conn.Notify("agent.event", wireEvent{RunID: wr.RunID, Event: e}) }
		}
		return th.Generate(c, wr.Prompt, run)
	})
	conn.Handle("thread.get", func(c context.Context, raw json.RawMessage) (any, error) {
		var wr wireRun
		if err := json.Unmarshal(raw, &wr); err != nil {
			return nil, err
		}
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
	st := &sdk.RunState{ID: wr.RunID, Thread: wr.Thread, User: wr.User, Roles: wr.Roles, Model: wr.Model, System: wr.System,
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
func bridgeLLM(conn *rpc.Conn, chunks *chunkRouter, providers []string) engine.Module {
	return engine.Module{Name: "bridge_llm", Provides: []string{"model/*"}, Apply: func(ctx *sdk.Scope) error {
		for _, p := range providers {
			name := p
			err := ctx.Provide("model/"+name, &sdk.Provider{Name: "bridge:" + name, Capabilities: map[string]bool{"streaming": true, "tools": true, "structured_output": true},
				Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
					id := fmt.Sprintf("m%d", callSeq.Add(1))
					runID := ""
					if req.Run != nil {
						runID = req.Run.ID
					}
					wr := wireModelReq{RunID: runID, CallID: id, Model: req.Model, System: req.System, Messages: req.Messages, Tools: toWireTools(req.Tools),
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
func bridgeTools(conn *rpc.Conn, tools []wireTool) engine.Module {
	return engine.Module{Name: "bridge_tools", Apply: func(ctx *sdk.Scope) error {
		for _, t := range tools {
			name := t.Name
			ctx.AddTool(sdk.Tool{Name: name, Description: t.Description, Schema: t.Schema, Meta: t.Meta,
				Run: func(c context.Context, args map[string]any) (string, error) {
					var out string
					info := sdk.RunInfoFrom(c)
					err := conn.Call(c, "host.tool", wireToolCall{RunID: info.RunID, Name: name, Args: args, Info: info}, &out)
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
func bridgeSecrets(conn *rpc.Conn) engine.Module {
	return engine.Module{Name: "bridge_secrets", Provides: []string{"secrets"}, Apply: func(ctx *sdk.Scope) error {
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
		b, err := io.ReadAll(io.LimitReader(req.Body, maxBrokerBody+1))
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(b) > maxBrokerBody {
			return nil, fmt.Errorf("sandbox egress: request body is over %d bytes", maxBrokerBody)
		}
		body = b
	}
	h := map[string][]string(req.Header.Clone())
	if req.Host != "" && req.Host != req.URL.Host {
		h["Host"] = []string{req.Host}
	}
	var out wireHTTPResp
	info := sdk.RunInfoFrom(req.Context())
	if err := t.conn.Call(req.Context(), "host.http", wireHTTPReq{RunID: info.RunID, Method: req.Method, URL: req.URL.String(), Header: h, Body: body}, &out); err != nil {
		return nil, err
	}
	return &http.Response{Status: fmt.Sprintf("%d %s", out.Status, http.StatusText(out.Status)), StatusCode: out.Status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header(out.Header),
		Body: io.NopCloser(bytes.NewReader(out.Body)), ContentLength: int64(len(out.Body)), Request: req}, nil
}

// boxInputs returns the profile and policy the box runs. The host uploads
// its resolved ones; without that (a box started by hand) they are resolved
// from the same options.
func boxInputs(cfg Config) (*engine.Profile, *engine.Policy, error) {
	dir := os.Getenv(boxDirEnv)
	if dir == "" {
		return resolve(cfg)
	}
	var prof engine.Profile
	if err := engine.ReadJSON(filepath.Join(dir, "profile.json"), &prof); err != nil {
		return nil, nil, err
	}
	pol := &engine.Policy{}
	if err := engine.ReadJSON(filepath.Join(dir, "policy.json"), pol); err != nil {
		return nil, nil, err
	}
	return &prof, pol, nil
}

// isHostRow reports whether a row runs on the host, not in the box: the
// model providers and secret providers stay there (a box bridges to them).
// Sandbox modules run in the box; the host's own sandbox provider row is
// supplied by RunIn.
func isHostRow(s engine.ModuleSpec) bool { return hostOnly(s) }

func hostOnly(s engine.ModuleSpec) bool {
	return strings.HasPrefix(s.Use, "secrets_") || strings.HasPrefix(s.Use, "model_")
}

func dropRows(rows []engine.ModuleSpec, drop func(engine.ModuleSpec) bool) []engine.ModuleSpec {
	return dropSpecs(rows, drop)
}

func dropSpecs(rows []engine.ModuleSpec, drop func(engine.ModuleSpec) bool) []engine.ModuleSpec {
	var out []engine.ModuleSpec
	for _, s := range rows {
		if !drop(s) {
			out = append(out, s)
		}
	}
	return out
}

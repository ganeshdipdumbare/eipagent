package eipagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/rpc"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// RunIn runs the whole agent inside a sandbox. The host keeps the model
// provider, the secrets provider, host-only Go tools and the sandbox
// provider; everything else — loop, plugins, middleware, file and shell
// tools, threads — runs in the box.
//
// No secret enters the box. Its "secrets" service returns placeholders
// (eipsec_...); its HTTP requests are made by the host, which swaps a
// placeholder for the real value only when the request goes to a host
// listed for that secret in SecretHosts, and redacts secret values out
// of responses. Requests without placeholders may reach only Egress.
type RunIn struct {
	Provider       string              // sandbox plugin suffix: "docker", "local", or your own sandbox_<name>
	ProviderConfig map[string]any      // config for sandbox_<Provider>
	Spec           sdk.SandboxSpec     // image, limits, network
	Binary         string              // eip binary built for the box's OS/arch (GOOS=linux for containers)
	PluginDirs     []string            // uploaded to the box (plugins travel as source)
	HostPlugins    []engine.PluginSpec // extra plugins that stay on the host (e.g. mw_retry for model calls)
	NoBridgeTools  bool                // don't expose host Go tools to the box
	SecretHosts    map[string][]string // secret name -> host globs its placeholder may be sent to
	Egress         []string            // host globs the box may reach without secrets (none by default)
}

// WithRunIn starts the agent inside a sandbox; the API is unchanged.
func WithRunIn(r RunIn) Option { return func(c *config) { c.runIn = &r } }

func withoutProviderAutoload() Option { return func(c *config) { c.noProviderAuto = true } }

type remote struct {
	host   *Agent
	box    *sdk.Sandbox
	proc   *sdk.Process
	conn   *rpc.Conn
	mu     sync.Mutex
	subs   map[string]func(sdk.Event)
	seq    int
	broker *broker
}

func newRemote(cfg *config) (*Agent, error) {
	ri := cfg.runIn
	if ri.Binary == "" {
		return nil, errors.New("eipagent: RunIn.Binary is required (an eip binary for the sandbox's OS)")
	}
	if ri.Provider == "" {
		ri.Provider = "docker"
	}
	// Host side: model provider + keys, sandbox provider, host-only tools.
	hostCfg := *cfg
	hostCfg.runIn, hostCfg.profiles, hostCfg.dirs = nil, nil, nil
	if err := sdk.CheckSandboxSpec(ri.Spec); err != nil {
		return nil, fmt.Errorf("eipagent: RunIn.Spec: %w", err)
	}
	hostCfg.specs = []engine.PluginSpec{{Use: "sandbox_" + ri.Provider, Config: ri.ProviderConfig}}
	// Secret providers, then model providers (which resolve their keys
	// through them), always stay on the host.
	var boxSpecs, models []engine.PluginSpec
	for _, s := range cfg.specs {
		switch use := sdk.CanonicalPlugin(s.Use); {
		case strings.HasPrefix(use, "secrets_"):
			hostCfg.specs = append(hostCfg.specs, s)
		case strings.HasPrefix(use, "model_"):
			models = append(models, s)
		default:
			boxSpecs = append(boxSpecs, s)
		}
	}
	hostCfg.specs = append(append(hostCfg.specs, ri.HostPlugins...), models...)
	if len(cfg.profiles) > 0 && hostCfg.model == "" {
		var p engine.Profile
		if err := engine.ReadJSON(cfg.profiles[len(cfg.profiles)-1], &p); err == nil {
			hostCfg.model = p.Model
		}
	}
	host, err := newLocal(&hostCfg)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Agent, error) { host.Close(); return nil, err }

	// Box side: the user's profile, without provider plugins.
	boxCfg := *cfg
	boxCfg.runIn, boxCfg.compiled, boxCfg.tools = nil, nil, nil
	boxCfg.noProviderAuto = true
	boxCfg.specs = boxSpecs
	prof, pol, lock, err := boxCfg.resolve()
	if err != nil {
		return fail(err)
	}
	snap := host.eng.Root().Snapshot()
	prov, ok := snap.Services["sandbox/"+ri.Provider].(*sdk.SandboxProvider)
	if !ok {
		return fail(fmt.Errorf("eipagent: no sandbox/%s provider on the host", ri.Provider))
	}
	ctx := context.Background()
	box, err := prov.Create(ctx, ri.Spec)
	if err != nil {
		return fail(err)
	}
	r := &remote{host: host, box: box, subs: map[string]func(sdk.Event){}, broker: newBroker(host, ri)}
	fail = func(err error) (*Agent, error) { r.close(); return nil, err }

	tmp, err := os.MkdirTemp("", "eip-runin-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)
	write := func(name string, v any) (string, error) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return "", err
		}
		p := filepath.Join(tmp, name)
		return p, os.WriteFile(p, b, 0o600)
	}
	pf, err := write("profile.json", prof)
	if err != nil {
		return fail(err)
	}
	polf, err := write("policy.json", pol)
	if err != nil {
		return fail(err)
	}
	lockf, err := write("eip.lock", lock)
	if err != nil {
		return fail(err)
	}
	uploads := map[string]string{ri.Binary: "eip/eip", pf: "eip/profile.json", polf: "eip/policy.json", lockf: "eip/eip.lock"}
	for _, d := range cfg.dirs {
		ri.PluginDirs = append(ri.PluginDirs, d)
	}
	for _, d := range ri.PluginDirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.go"))
		for _, f := range files {
			uploads[f] = "eip/plugins/" + filepath.Base(f)
		}
	}
	for src, dst := range uploads {
		if err := box.Upload(ctx, src, dst); err != nil {
			return fail(fmt.Errorf("eipagent: upload %s: %w", dst, err))
		}
	}
	if box.Start == nil {
		return fail(fmt.Errorf("eipagent: sandbox/%s can't run long-lived processes", ri.Provider))
	}
	proc, err := box.Start(ctx, sdk.ExecRequest{Cmd: "./eip/eip", Args: []string{"serve", "--stdio",
		"--profile", "eip/profile.json", "--policy", "eip/policy.json", "--lock", "eip/eip.lock", "--plugins", "eip/plugins"}})
	if err != nil {
		return fail(err)
	}
	r.proc = proc
	go func() { // box stderr -> host log
		sc := bufio.NewScanner(proc.Stderr)
		for sc.Scan() {
			host.eng.Root().Logf("[box] %s", sc.Text())
		}
	}()
	r.conn = rpc.New(proc.Stdout, proc.Stdin, func() { proc.Stdin.Close(); proc.Kill() })
	ready := make(chan error, 1)
	r.conn.Handle("box.ready", func(context.Context, json.RawMessage) (any, error) { ready <- nil; return nil, nil })
	r.conn.Handle("box.error", func(_ context.Context, raw json.RawMessage) (any, error) {
		var msg string
		json.Unmarshal(raw, &msg)
		ready <- errors.New(msg)
		return nil, nil
	})
	r.handle(ri.NoBridgeTools, cfg.tools)
	go r.conn.Serve()
	select {
	case err := <-ready:
		if err != nil {
			return fail(fmt.Errorf("eipagent: agent in sandbox failed to start: %w", err))
		}
	case <-r.conn.Done():
		return fail(errors.New("eipagent: agent in sandbox exited during startup (see [box] logs)"))
	case <-time.After(60 * time.Second):
		return fail(errors.New("eipagent: agent in sandbox did not start within 60s"))
	}

	a := &Agent{eng: host.eng, model: prof.Model, instructions: prof.Instructions, maxSteps: prof.MaxSteps,
		user: cfg.user, roles: cfg.roles, stop: make(chan struct{}), remote: r}
	return a, nil
}

func (r *remote) handle(noTools bool, hostTools []sdk.Tool) {
	r.conn.Handle("host.hello", func(context.Context, json.RawMessage) (any, error) {
		snap := r.host.eng.Root().Snapshot()
		_, hasSecrets := snap.Services["secrets"].(*sdk.Secrets)
		h := wireHello{Secrets: hasSecrets, Egress: true}
		for k := range snap.Services {
			if strings.HasPrefix(k, "model/") {
				h.Providers = append(h.Providers, strings.TrimPrefix(k, "model/"))
			}
		}
		if !noTools {
			h.Tools = toWireTools(hostTools)
		}
		return h, nil
	})
	r.conn.Handle("host.llm", func(c context.Context, raw json.RawMessage) (any, error) {
		var wr wireModelReq
		if err := json.Unmarshal(raw, &wr); err != nil {
			return nil, err
		}
		snap := r.host.eng.Root().Snapshot()
		req := &sdk.ModelRequest{Model: wr.Model, System: wr.System, Messages: wr.Messages, ToolChoice: wr.ToolChoice,
			ResponseSchema: wr.ResponseSchema, ResponseName: wr.ResponseName, Temperature: wr.Temperature, MaxTokens: wr.MaxTokens,
			Extra: wr.Extra, Thread: wr.Thread, User: wr.User, Step: wr.Step}
		for _, t := range wr.Tools {
			req.Tools = append(req.Tools, sdk.Tool{Name: t.Name, Description: t.Description, Schema: t.Schema, Meta: t.Meta})
		}
		if wr.Stream {
			req.Stream = func(ch sdk.Chunk) { r.conn.Notify("host.llm.chunk", wireChunk{CallID: wr.CallID, Chunk: ch}) }
		}
		base := func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			prov, id := sdk.SplitModel(req.Model)
			p, ok := snap.Services["model/"+prov].(*sdk.Provider)
			if !ok {
				return nil, fmt.Errorf("host has no provider model/%s", prov)
			}
			req.Provider, req.ModelID = prov, id
			return p.Generate(c, req)
		}
		return snap.ModelChain(base)(c, req) // host middleware & policy apply
	})
	r.conn.Handle("host.secret", func(c context.Context, raw json.RawMessage) (any, error) {
		var req wireSecretReq
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return r.broker.placeholder(c, req.Caller, req.Name)
	})
	r.conn.Handle("host.http", func(c context.Context, raw json.RawMessage) (any, error) {
		var req wireHTTPReq
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return r.broker.fetch(c, req)
	})
	r.conn.Handle("host.tool", func(c context.Context, raw json.RawMessage) (any, error) {
		var call wireToolCall
		if err := json.Unmarshal(raw, &call); err != nil {
			return nil, err
		}
		for _, t := range hostTools {
			if t.Name == call.Name && !noTools {
				return t.Run(sdk.WithRunInfo(c, call.Info), call.Args)
			}
		}
		return nil, fmt.Errorf("no host tool %q", call.Name)
	})
	r.conn.Handle("agent.event", func(_ context.Context, raw json.RawMessage) (any, error) {
		var ev wireEvent
		if json.Unmarshal(raw, &ev) == nil {
			r.mu.Lock()
			fn := r.subs[ev.RunID]
			r.mu.Unlock()
			if fn != nil {
				fn(ev.Event)
			}
		}
		return nil, nil
	})
}

func (r *remote) subscribe(fn func(sdk.Event)) (string, func()) {
	r.mu.Lock()
	r.seq++
	id := fmt.Sprintf("r%d", r.seq)
	if fn != nil {
		r.subs[id] = fn
	}
	r.mu.Unlock()
	return id, func() { r.mu.Lock(); delete(r.subs, id); r.mu.Unlock() }
}

func toWire(st *sdk.RunState, id string, streaming bool) wireRun {
	return wireRun{RunID: id, Thread: st.Thread, User: st.User, Roles: st.Roles, Model: st.Model, System: st.System,
		Messages: st.Messages, MaxSteps: st.MaxSteps, StopOnTools: st.StopOnTools, ToolChoice: st.ToolChoice,
		ResponseSchema: st.ResponseSchema, ResponseName: st.ResponseName, Temperature: st.Temperature, MaxTokens: st.MaxTokens,
		Pending: st.Pending, Decisions: st.Decisions, Values: st.Values, Streaming: streaming}
}

func (r *remote) run(ctx context.Context, st *sdk.RunState, ro *runOptions) (*Result, error) {
	id, done := r.subscribe(ro.onEvent)
	defer done()
	var res Result
	if err := r.conn.Call(ctx, "agent.run", toWire(st, id, ro.onEvent != nil), &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (r *remote) thread(ctx context.Context, thread, prompt string, decisions []sdk.Decision, opts []RunOption) (*Result, error) {
	ro := &runOptions{}
	for _, o := range opts {
		o(ro)
	}
	id, done := r.subscribe(ro.onEvent)
	defer done()
	wr := wireRun{RunID: id, Thread: thread, Prompt: prompt, Decisions: decisions, User: ro.user, Roles: ro.roles,
		MaxSteps: ro.maxSteps, Streaming: ro.onEvent != nil}
	var res Result
	if err := r.conn.Call(ctx, "agent.thread", wr, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (r *remote) threadState(ctx context.Context, thread string) (*wireThreadState, error) {
	var ts wireThreadState
	err := r.conn.Call(ctx, "thread.get", wireRun{Thread: thread}, &ts)
	return &ts, err
}

func (r *remote) statusErr() ([]string, error) {
	var rows []string
	err := r.conn.Call(context.Background(), "agent.status", struct{}{}, &rows)
	return rows, err
}

func (r *remote) status() []engine.PluginStatus {
	rows, err := r.statusErr()
	var out []engine.PluginStatus
	for _, row := range rows {
		out = append(out, engine.PluginStatus{Name: "[box] " + row})
	}
	if err != nil {
		out = append(out, engine.PluginStatus{Name: "[box]", Err: err})
	}
	return append(out, r.host.eng.Status()...)
}

func (r *remote) close() {
	if r.conn != nil {
		r.conn.Close()
	}
	if r.proc != nil {
		r.proc.Wait()
	}
	if r.box != nil {
		r.box.Close(context.Background())
	}
	r.host.Close()
}

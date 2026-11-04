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
	"github.com/ganeshdipdumbare/eipagent/plugins"
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
	BridgedTools   []string            // host tools the box may call (nil = all; [] = none)
	SecretHosts    map[string][]string // secret name -> rules for where its placeholder may be sent: "host", "*.host", "GET host/path/*"
	Egress         []string            // rules for where the box may go without secrets, same form (none by default)
	// AllowUnsafeLocal permits the "local" provider for WithRunIn. It is
	// not an isolation boundary: the "box" is a host process that can read
	// host files (including credentials). Use only for development.
	AllowUnsafeLocal bool
	// AllowPrivateEgress lets the host broker reach loopback/RFC1918/
	// link-local addresses on behalf of the box. Off by default (SSRF).
	AllowPrivateEgress bool
}

// WithRunIn starts the agent inside a sandbox; the API is unchanged.
func WithRunIn(r RunIn) Option { return func(c *config) { c.runIn = &r } }

func withoutProviderAutoload() Option { return func(c *config) { c.noProviderAuto = true } }

type remote struct {
	host    *Agent
	box     *sdk.Sandbox
	proc    *sdk.Process
	conn    *rpc.Conn
	mu      sync.Mutex
	subs    map[string]func(sdk.Event)
	runs    map[string]*sdk.RunState
	runSeen map[string]time.Time
	broker  *broker

	// What start needs to make a box again after one exits.
	prov      *sdk.SandboxProvider
	ri        *RunIn
	hostTools []sdk.Tool
	configs   map[string]any    // box path -> JSON written there (profile, policy, lock)
	uploads   map[string]string // host file -> box path (eip binary, plugin sources)
	startMu   sync.Mutex
}

// errBoxExited is returned to a call the box didn't answer because it
// exited; the next run starts a new box.
var errBoxExited = errors.New("eipagent: the agent in the sandbox exited (see [box] logs); the next run starts a new sandbox")

var errRunExpired = errors.New("eipagent: sandbox run expired")

// boxPluginSecrets builds the secret grants the host will enforce for
// plugins running in the box. The box cannot be trusted to declare its
// own grants, so the host derives them from the same sources it uploads.
func boxPluginSecrets(specs []engine.PluginSpec, dirs []string) map[string][]string {
	out := map[string][]string{}
	seen := map[string]bool{}
	add := func(spec engine.PluginSpec) {
		name := sdk.CanonicalPlugin(spec.Name())
		if seen[name] {
			return
		}
		seen[name] = true
		var refs []string
		collectConfigSecrets(spec.Config, &refs)
		use := sdk.CanonicalPlugin(spec.Use)
		if src := readBoxPluginSource(spec.Use, dirs); src != nil {
			refs = append(refs, engine.SecretRefs(string(src))...)
		}
		_ = use
		out[name] = dedupStrings(refs)
	}
	for _, spec := range specs {
		add(spec)
	}
	for _, use := range autoPlugins {
		add(engine.PluginSpec{Use: use})
	}
	return out
}

func readBoxPluginSource(use string, dirs []string) []byte {
	for _, name := range []string{use, sdk.CanonicalPlugin(use)} {
		for _, dir := range dirs {
			if b, err := os.ReadFile(filepath.Join(dir, name+".go")); err == nil {
				return b
			}
		}
		if b, err := plugins.FS.ReadFile(name + ".go"); err == nil {
			return b
		}
	}
	return nil
}

func collectConfigSecrets(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, engine.SecretRefs(x)...)
	case map[string]any:
		for _, e := range x {
			collectConfigSecrets(e, out)
		}
	case []any:
		for _, e := range x {
			collectConfigSecrets(e, out)
		}
	}
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func newRemote(cfg *config) (*Agent, error) {
	ri := cfg.runIn
	if ri.Binary == "" {
		return nil, errors.New("eipagent: RunIn.Binary is required (an eip binary for the sandbox's OS)")
	}
	if ri.Provider == "" {
		ri.Provider = "docker"
	}
	if ri.Provider == "local" && !ri.AllowUnsafeLocal {
		return nil, errors.New("eipagent: RunIn.Provider \"local\" is not an isolation boundary (the box is a host process that can read host files); use docker/microsandbox or set RunIn.AllowUnsafeLocal for development")
	}
	// Host side: model provider + keys, sandbox provider, host-only tools.
	hostCfg := *cfg
	hostCfg.runIn, hostCfg.profiles, hostCfg.dirs = nil, nil, nil
	if err := sdk.CheckSandboxSpec(ri.Spec); err != nil {
		return nil, fmt.Errorf("eipagent: RunIn.Spec: %w", err)
	}
	// Parse the egress rules before anything starts.
	if _, err := newBroker(nil, ri); err != nil {
		return nil, fmt.Errorf("eipagent: %w", err)
	}
	hostCfg.specs = []engine.PluginSpec{{Use: "sandbox_" + ri.Provider, Config: ri.ProviderConfig}}
	// A compiled plugin has no source the box could load. Keep it on the
	// host; the sandbox provider is already in hostCfg.specs.
	compiled := make(map[string]bool, len(cfg.compiled))
	for _, p := range cfg.compiled {
		compiled[sdk.CanonicalPlugin(p.Name)] = true
	}
	// Secret providers, then model providers (which resolve their keys
	// through them), always stay on the host.
	var boxSpecs, models []engine.PluginSpec
	for _, s := range cfg.specs {
		use := sdk.CanonicalPlugin(s.Use)
		if compiled[use] {
			if use != sdk.CanonicalPlugin("sandbox_"+ri.Provider) {
				hostCfg.specs = append(hostCfg.specs, s)
			}
			continue
		}
		switch {
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
	r := &remote{host: host, prov: prov, ri: ri, hostTools: cfg.tools, subs: map[string]func(sdk.Event){}, runs: map[string]*sdk.RunState{}, runSeen: map[string]time.Time{},
		configs: map[string]any{"eip/profile.json": prof, "eip/policy.json": pol, "eip/eip.lock": lock},
		uploads: map[string]string{ri.Binary: "eip/eip"}}
	r.broker, _ = newBroker(host, ri) // rules checked above
	boxAll := append(append([]engine.PluginSpec{}, prof.Plugins...), pol.Require...)
	r.broker.setCallers(boxAll)
	dirsForSource := append(append([]string{}, ri.PluginDirs...), cfg.dirs...)
	r.broker.setSecretGrants(boxPluginSecrets(boxAll, dirsForSource))
	for _, d := range append(append([]string{}, ri.PluginDirs...), cfg.dirs...) {
		files, _ := filepath.Glob(filepath.Join(d, "*.go"))
		for _, f := range files {
			// A symlinked source could copy a host secret into the box.
			if fi, err := os.Lstat(f); err != nil || fi.Mode()&os.ModeSymlink != 0 {
				r.host.eng.Root().Logf("eipagent: skipping symlinked plugin source %s", f)
				continue
			}
			r.uploads[f] = "eip/plugins/" + filepath.Base(f)
		}
	}
	if err := r.start(); err != nil {
		r.close()
		return nil, err
	}

	a := &Agent{eng: host.eng, model: prof.Model, instructions: prof.Instructions, maxSteps: prof.MaxSteps,
		user: cfg.user, roles: cfg.roles, stop: make(chan struct{}), remote: r}
	return a, nil
}

// start creates a box, uploads the agent and starts it, at New and
// again when a run finds the previous box gone. The box outlives any
// one run, so it doesn't take a run's context.
func (r *remote) start() error {
	ctx := context.Background()
	box, err := r.prov.Create(ctx, r.ri.Spec)
	if err != nil {
		return err
	}
	r.box = box
	if box.Start == nil {
		return fmt.Errorf("eipagent: sandbox/%s can't run long-lived processes", r.ri.Provider)
	}
	tmp, err := os.MkdirTemp("", "eip-runin-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	uploads := map[string]string{}
	for src, dst := range r.uploads {
		uploads[src] = dst
	}
	for dst, v := range r.configs {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		p := filepath.Join(tmp, filepath.Base(dst))
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return err
		}
		uploads[p] = dst
	}
	for src, dst := range uploads {
		if err := box.Upload(ctx, src, dst); err != nil {
			return fmt.Errorf("eipagent: upload %s: %w", dst, err)
		}
	}
	proc, err := box.Start(ctx, sdk.ExecRequest{Cmd: "./eip/eip", Args: []string{"serve", "--stdio",
		"--profile", "eip/profile.json", "--policy", "eip/policy.json", "--lock", "eip/eip.lock", "--plugins", "eip/plugins"}})
	if err != nil {
		return err
	}
	r.proc = proc
	go func() { // box stderr -> host log
		sc := bufio.NewScanner(proc.Stderr)
		for sc.Scan() {
			r.host.eng.Root().Logf("[box] %s", sc.Text())
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
	r.handle(r.ri.NoBridgeTools, r.hostTools)
	go r.conn.Serve()
	select {
	case err := <-ready:
		if err != nil {
			return fmt.Errorf("eipagent: agent in sandbox failed to start: %w", err)
		}
	case <-r.conn.Done():
		return errors.New("eipagent: agent in sandbox exited during startup (see [box] logs)")
	case <-time.After(60 * time.Second):
		return errors.New("eipagent: agent in sandbox did not start within 60s")
	}
	return nil
}

// live returns the connection to the box. When the box has exited, it
// starts a new one first; threads kept in the old box are gone with it
// unless their store is outside the box.
func (r *remote) live() (*rpc.Conn, error) {
	r.startMu.Lock()
	defer r.startMu.Unlock()
	if r.conn != nil {
		select {
		case <-r.conn.Done():
		default:
			return r.conn, nil
		}
	}
	r.host.eng.Root().Logf("the agent in the sandbox exited; starting a new sandbox")
	r.stop()
	if err := r.start(); err != nil {
		r.stop()
		return nil, fmt.Errorf("eipagent: restarting the sandbox: %w", err)
	}
	return r.conn, nil
}

// call makes one call on the live box.
func (r *remote) call(ctx context.Context, method string, params, out any) error {
	conn, err := r.live()
	if err != nil {
		return err
	}
	if err := conn.Call(ctx, method, params, out); errors.Is(err, rpc.ErrClosed) {
		return errBoxExited
	} else {
		return err
	}
}

func (r *remote) handle(noTools bool, hostTools []sdk.Tool) {
	if !noTools && r.ri.BridgedTools != nil {
		allowed := map[string]bool{}
		for _, n := range r.ri.BridgedTools {
			allowed[n] = true
		}
		var kept []sdk.Tool
		for _, t := range hostTools {
			if allowed[t.Name] {
				kept = append(kept, t)
			}
		}
		hostTools = kept
	}
	conn := r.conn // this box's connection; a restarted box gets its own
	conn.Handle("host.hello", func(context.Context, json.RawMessage) (any, error) {
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
	conn.Handle("host.llm", func(c context.Context, raw json.RawMessage) (any, error) {
		var wr wireModelReq
		if err := json.Unmarshal(raw, &wr); err != nil {
			return nil, err
		}
		st := r.runState(wr.RunID)
		if st == nil {
			// A run started inside the box (a scheduled job or webhook) has
			// no host-issued identity. Accept it for model calls, record it,
			// and let host policy/budgets apply to this run id. Host tool
			// calls stay unavailable to such runs (see host.tool).
			st = &sdk.RunState{ID: wr.RunID, Thread: wr.Thread, User: wr.User, Model: wr.Model, Values: map[string]any{}}
			if !r.setRun(st) {
				return nil, fmt.Errorf("host.llm: too many live sandbox runs")
			}
		}
		snap := r.host.eng.Root().Snapshot()
		st.Model = wr.Model
		req := &sdk.ModelRequest{Model: wr.Model, System: wr.System, Messages: wr.Messages, ToolChoice: wr.ToolChoice,
			ResponseSchema: wr.ResponseSchema, ResponseName: wr.ResponseName, Temperature: wr.Temperature, MaxTokens: wr.MaxTokens,
			Extra: wr.Extra, Thread: st.Thread, User: st.User, Step: wr.Step, Run: st}
		for _, t := range wr.Tools {
			req.Tools = append(req.Tools, sdk.Tool{Name: t.Name, Description: t.Description, Schema: t.Schema, Meta: t.Meta})
		}
		if wr.Stream {
			req.Stream = func(ch sdk.Chunk) { conn.Notify("host.llm.chunk", wireChunk{CallID: wr.CallID, Chunk: ch}) }
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
	conn.Handle("host.secret", func(c context.Context, raw json.RawMessage) (any, error) {
		var req wireSecretReq
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return r.broker.placeholder(c, req.Caller, req.Name)
	})
	conn.Handle("host.http", func(c context.Context, raw json.RawMessage) (any, error) {
		var req wireHTTPReq
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return r.broker.fetch(c, req)
	})
	conn.Handle("host.tool", func(c context.Context, raw json.RawMessage) (any, error) {
		var call wireToolCall
		if err := json.Unmarshal(raw, &call); err != nil {
			return nil, err
		}
		if noTools {
			return nil, fmt.Errorf("host tools are disabled for this sandbox")
		}
		st := r.runState(call.RunID)
		if st == nil {
			return nil, fmt.Errorf("host.tool: unknown run %q (the box must start a run first)", call.RunID)
		}
		var tool *sdk.Tool
		for i := range hostTools {
			if hostTools[i].Name == call.Name {
				tool = &hostTools[i]
				break
			}
		}
		if tool == nil {
			return nil, fmt.Errorf("no host tool %q", call.Name)
		}
		info := sdk.RunInfo{RunID: st.ID, Thread: st.Thread, User: st.User, Roles: st.Roles, Step: call.Info.Step,
			CallID: call.Info.CallID, Model: st.Model}
		args, _ := sdk.RedactValue(call.Args).(map[string]any)
		if args == nil {
			args = call.Args
		}
		snap := r.host.eng.Root().Snapshot()
		req := &sdk.ToolRequest{Call: sdk.ToolCall{ID: info.CallID, Name: call.Name, Args: args},
			Tool: tool, Thread: st.Thread, User: st.User, Step: info.Step, Run: st}
		base := func(c context.Context, req *sdk.ToolRequest) (*sdk.ToolResult, error) {
			out, err := req.Tool.Run(sdk.WithRunInfo(c, info), req.Call.Args)
			return &sdk.ToolResult{Content: out}, err
		}
		res, err := snap.ToolChain(base)(c, req)
		if _, audited := snap.Services["audit"]; !audited {
			outcome := "ok"
			if err != nil || (res != nil && res.IsError) {
				outcome = "failed"
			}
			r.broker.audit("tool.call", map[string]any{"run": st.ID, "thread": st.Thread, "user": st.User,
				"tool": call.Name, "call_id": info.CallID, "step": info.Step, "plugin": "host",
				"args": args, "outcome": outcome})
		}
		if err != nil {
			return nil, err
		}
		if res == nil {
			return "", nil
		}
		if res.Interrupt != nil {
			return nil, fmt.Errorf("host tool %s requires approval on the host", call.Name)
		}
		if res.IsError {
			return nil, fmt.Errorf("%s", sdk.Redact(res.Content))
		}
		return sdk.Redact(res.Content), nil
	})
	conn.Handle("agent.event", func(_ context.Context, raw json.RawMessage) (any, error) {
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

func (r *remote) subscribe(runID string, fn func(sdk.Event)) func() {
	r.mu.Lock()
	if fn != nil {
		r.subs[runID] = fn
	} else {
		delete(r.subs, runID)
	}
	r.mu.Unlock()
	return func() { r.mu.Lock(); delete(r.subs, runID); r.mu.Unlock() }
}

// setRun records the host-side identity of a run. Box calls to host.llm
// and host.tool are checked against this map, not against the identity
// the box supplies, so a compromised box can't act as another user.
func (r *remote) setRun(st *sdk.RunState) bool {
	if st == nil || st.ID == "" {
		return false
	}
	var expired []*sdk.RunState
	r.mu.Lock()
	if len(r.runs) > 1024 {
		for id, at := range r.runSeen {
			if time.Since(at) > time.Hour {
				if old := r.runs[id]; old != nil {
					expired = append(expired, old)
				}
				delete(r.runs, id)
				delete(r.runSeen, id)
			}
		}
	}
	if len(r.runs) > 1024 {
		r.mu.Unlock()
		for _, old := range expired {
			r.endHostRun(context.Background(), old, nil, errRunExpired)
		}
		return false
	}
	r.runs[st.ID] = st
	r.runSeen[st.ID] = time.Now()
	r.mu.Unlock()
	for _, old := range expired {
		r.endHostRun(context.Background(), old, nil, errRunExpired)
	}
	return true
}

func (r *remote) runState(id string) *sdk.RunState {
	r.mu.Lock()
	st := r.runs[id]
	stale := st != nil && time.Since(r.runSeen[id]) > time.Hour
	if stale {
		delete(r.runs, id)
		delete(r.runSeen, id)
	}
	r.mu.Unlock()
	if stale {
		r.endHostRun(context.Background(), st, nil, errRunExpired)
		return nil
	}
	return st
}

func (r *remote) endRun(id string) {
	r.mu.Lock()
	delete(r.runs, id)
	delete(r.runSeen, id)
	r.mu.Unlock()
}

func toWire(st *sdk.RunState, id string, streaming bool) wireRun {
	return wireRun{RunID: id, Thread: st.Thread, User: st.User, Roles: st.Roles, Model: st.Model, System: st.System,
		Messages: st.Messages, MaxSteps: st.MaxSteps, StopOnTools: st.StopOnTools, ToolChoice: st.ToolChoice,
		ResponseSchema: st.ResponseSchema, ResponseName: st.ResponseName, Temperature: st.Temperature, MaxTokens: st.MaxTokens,
		Pending: st.Pending, Decisions: st.Decisions, Values: st.Values, Streaming: streaming}
}

func (r *remote) run(ctx context.Context, st *sdk.RunState, ro *runOptions) (*Result, error) {
	if st.ID == "" {
		st.ID = sdk.NewRunID()
	}
	r.setRun(st)
	defer r.endRun(st.ID)
	if err := r.host.eng.Root().Snapshot().AgentStart(ctx, st); err != nil {
		r.endHostRun(ctx, st, nil, err)
		return nil, err
	}
	done := r.subscribe(st.ID, ro.onEvent)
	defer done()
	var res Result
	err := r.call(ctx, "agent.run", toWire(st, st.ID, ro.onEvent != nil), &res)
	r.endHostRun(ctx, st, &res, err)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// endHostRun fires host agent/end hooks so per-run host state (policy
// budgets, hook_limits counters, trace spans) is released even when the
// run happened in a sandbox.
func (r *remote) endHostRun(ctx context.Context, st *sdk.RunState, res *Result, runErr error) {
	out := &sdk.RunResult{FinishReason: "error"}
	if runErr == nil && res != nil {
		out = &sdk.RunResult{Text: res.Text, Steps: res.Steps, Usage: res.Usage, FinishReason: res.FinishReason,
			Interrupts: res.Interrupts, Messages: res.Messages}
	}
	r.host.eng.Root().Snapshot().AgentEnd(ctx, st, out)
}

func (r *remote) thread(ctx context.Context, thread, prompt string, decisions []sdk.Decision, opts []RunOption) (*Result, error) {
	ro := &runOptions{}
	for _, o := range opts {
		o(ro)
	}
	runID := sdk.NewRunID()
	st := &sdk.RunState{ID: runID, Thread: thread, User: ro.user, Roles: ro.roles, Model: r.host.model,
		Values: map[string]any{}}
	if ro.model != "" {
		st.Model = ro.model
	}
	r.setRun(st)
	defer r.endRun(runID)
	if err := r.host.eng.Root().Snapshot().AgentStart(ctx, st); err != nil {
		r.endHostRun(ctx, st, nil, err)
		return nil, err
	}
	done := r.subscribe(runID, ro.onEvent)
	defer done()
	wr := wireRun{RunID: runID, Thread: thread, Prompt: prompt, Decisions: decisions, User: ro.user, Roles: ro.roles,
		MaxSteps: ro.maxSteps, Streaming: ro.onEvent != nil}
	var res Result
	err := r.call(ctx, "agent.thread", wr, &res)
	r.endHostRun(ctx, st, &res, err)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (r *remote) threadState(ctx context.Context, thread string) (*wireThreadState, error) {
	var ts wireThreadState
	err := r.call(ctx, "thread.get", wireRun{Thread: thread}, &ts)
	return &ts, err
}

func (r *remote) statusErr() ([]string, error) {
	var rows []string
	// Status reports on the box as it is; it doesn't start a new one.
	r.startMu.Lock()
	conn := r.conn
	r.startMu.Unlock()
	if conn == nil {
		return nil, errBoxExited
	}
	err := conn.Call(context.Background(), "agent.status", struct{}{}, &rows)
	if errors.Is(err, rpc.ErrClosed) {
		err = errBoxExited
	}
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

// stop ends the box's process and removes the box.
func (r *remote) stop() {
	if r.conn != nil {
		r.conn.Close()
	}
	if r.proc != nil {
		r.proc.Wait()
	}
	if r.box != nil {
		r.box.Close(context.Background())
	}
	r.conn, r.proc, r.box = nil, nil, nil
}

func (r *remote) close() {
	r.stop()
	r.host.Close()
}

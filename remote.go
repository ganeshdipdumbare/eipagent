package kiln

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

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/rpc"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Environment variables that mark a process as a sandbox box, and where
// the host put the profile and policy it uploaded.
const (
	boxEnv    = "KILN_BOX"
	boxDirEnv = "KILN_BOX_DIR"
	boxDir    = "kiln"
	boxBinary = "kiln/agent"
)

// InBox reports whether this process is a sandbox box started by Config.RunIn.
// A program that uses Config.RunIn checks it first and, when true, calls
// ServeBox with the same options instead of creating an agent.
func InBox() bool { return os.Getenv(boxEnv) == "1" }

// RunIn runs the whole agent inside a sandbox. The box runs the same
// program: the host uploads its own binary (or RunIn.Binary, built for the
// box's OS and architecture), and the host's resolved profile and policy.
// The host keeps the model providers, the secrets providers, host-only Go
// tools and the sandbox provider; everything else — loop, modules, threads,
// file and shell tools — runs in the box.
//
// No secret enters the box. Its "secrets" service returns placeholders
// (eipsec_...); its HTTP requests are made by the host, which swaps a
// placeholder for the real value only when the request goes to a host
// listed for that secret in SecretHosts, and redacts secret values out
// of responses. Requests without placeholders may reach only Egress.
type RunIn struct {
	Provider       string              // sandbox module suffix: "docker", "local", or your own sandbox_<name>
	ProviderConfig map[string]any      // config for sandbox_<Provider>
	Spec           sdk.SandboxSpec     // image, limits, network
	Binary         string              // the program, built for the box's OS/arch (GOOS=linux for containers); default: this binary
	HostModules    []engine.Module     // extra modules that stay on the host (e.g. a retry hook for model calls)
	NoBridgeTools  bool                // don't expose host Go tools to the box
	BridgedTools   []string            // host tools the box may call (nil = all; [] = none)
	SecretHosts    map[string][]string // secret name -> rules for where its placeholder may be sent: "host", "*.host", "GET host/path/*"
	Egress         []string            // rules for where the box may go without secrets, same form (none by default)
	// AllowUnsafeLocal permits the "local" provider for Config.RunIn. It is
	// not an isolation boundary: the "box" is a host process that can read
	// host files (including credentials). Use only for development.
	AllowUnsafeLocal bool
	// AllowPrivateEgress lets the host broker reach loopback/RFC1918/
	// link-local addresses on behalf of the box. Off by default (SSRF).
	AllowPrivateEgress bool
}

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
	configs   map[string]any    // box path -> JSON written there (profile, policy)
	uploads   map[string]string // host file -> box path (the program)
	startMu   sync.Mutex
}

// errBoxExited is returned to a call the box didn't answer because it
// exited; the next run starts a new box.
var errBoxExited = errors.New("kiln: the agent in the sandbox exited (see [box] logs); the next run starts a new sandbox")

var errRunExpired = errors.New("kiln: sandbox run expired")

// boxSecretGrants is what the host enforces for each module in the box:
// the secrets it declares and the secret:// references in its config. The
// box cannot declare its own grants.
func boxSecretGrants(reg []engine.Module, rows []engine.ModuleSpec) map[string][]string {
	byName := map[string]engine.Module{}
	for _, m := range reg {
		byName[m.Name] = m
	}
	out := map[string][]string{}
	for _, row := range rows {
		if m, ok := byName[row.Use]; ok {
			out[row.Name()] = engine.SecretNames(m, row.Config)
		}
	}
	return out
}

func newRemote(cfg Config) (*Agent, error) {
	ri := cfg.RunIn
	if ri.Binary == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("kiln: RunIn.Binary: %w", err)
		}
		ri.Binary = exe
	}
	if ri.Provider == "" {
		ri.Provider = "docker"
	}
	if ri.Provider == "local" && !ri.AllowUnsafeLocal {
		return nil, errors.New("kiln: RunIn.Provider \"local\" is not an isolation boundary (the box is a host process that can read host files); use docker/microsandbox or set RunIn.AllowUnsafeLocal for development")
	}
	if err := sdk.CheckSandboxSpec(ri.Spec); err != nil {
		return nil, fmt.Errorf("kiln: RunIn.Spec: %w", err)
	}
	// Parse the egress rules before anything starts.
	if _, err := newBroker(nil, ri); err != nil {
		return nil, fmt.Errorf("kiln: %w", err)
	}
	prof, pol, err := resolve(cfg)
	if err != nil {
		return nil, err
	}

	// Split the rows. Host: the sandbox provider that creates the box, the
	// model and secret providers, and RunIn.HostModules. Box: everything
	// else, including sandbox modules the box's own tools use.
	providerUse := "sandbox_" + ri.Provider
	hostRows := []engine.ModuleSpec{{Use: providerUse, Config: ri.ProviderConfig}}
	hostOnlyNames := map[string]bool{} // host module rows never run in the box
	for _, m := range ri.HostModules {
		hostRows = append(hostRows, specOf(m))
		hostOnlyNames[m.Name] = true
	}
	hostNames := map[string]bool{} // host rows already present (for the store)
	for _, s := range hostRows {
		hostNames[s.Name()] = true
	}
	var boxRows []engine.ModuleSpec
	for _, s := range prof.Modules {
		switch {
		case hostOnly(s):
			if !hostNames[s.Name()] {
				hostRows = append(hostRows, s)
				hostNames[s.Name()] = true
			}
		case s.Use == providerUse:
			// RunIn configures the host's provider; the application's row
			// configures the box's own sandbox of the same kind, so it goes
			// to the box unless the host module list already names it.
			if !hostOnlyNames[s.Name()] {
				boxRows = append(boxRows, s)
			}
		case !hostOnlyNames[s.Name()]:
			boxRows = append(boxRows, s)
		}
	}

	// Host side: the same registry and policy, only the host rows.
	hostCfg := cfg
	hostCfg.RunIn = nil
	a := newAgent(hostCfg, prof)
	hostProf := &engine.Profile{Model: prof.Model, Instructions: prof.Instructions, MaxSteps: prof.MaxSteps, Modules: hostRows}
	plan, err := compose(hostCfg, hostProf, pol, a)
	if err != nil {
		return nil, fmt.Errorf("kiln: host: %w", err)
	}
	hostEng, err := plan.Start()
	if err != nil {
		return nil, fmt.Errorf("kiln: host: %w", err)
	}
	a.eng = hostEng
	fail := func(err error) (*Agent, error) { hostEng.Close(); return nil, err }

	// Box side: the box rows and the policy without host rows.
	boxProf := &engine.Profile{Model: prof.Model, Instructions: prof.Instructions, MaxSteps: prof.MaxSteps, Modules: boxRows}
	boxPol := *pol
	boxPol.Require = dropSpecs(pol.Require, func(s engine.ModuleSpec) bool { return hostOnly(s) || hostOnlyNames[s.Name()] })

	reg := registry(cfg.Modules)
	snap := hostEng.Root()
	prov, ok := snap.Service("sandbox/" + ri.Provider).(*sdk.SandboxProvider)
	if !ok {
		return fail(fmt.Errorf("kiln: no sandbox/%s provider on the host", ri.Provider))
	}
	r := &remote{host: a, prov: prov, ri: ri, hostTools: cfg.Tools, subs: map[string]func(sdk.Event){}, runs: map[string]*sdk.RunState{}, runSeen: map[string]time.Time{},
		configs: map[string]any{boxDir + "/profile.json": boxProf, boxDir + "/policy.json": &boxPol},
		uploads: map[string]string{ri.Binary: boxBinary}}
	r.broker, _ = newBroker(a, ri) // rules checked above
	var callers []string
	for _, s := range append(append([]engine.ModuleSpec{}, boxProf.Modules...), boxPol.Require...) {
		callers = append(callers, s.Name())
	}
	r.broker.setCallers(callers)
	r.broker.setSecretGrants(boxSecretGrants(reg, append(append([]engine.ModuleSpec{}, boxProf.Modules...), boxPol.Require...)))
	if err := r.start(); err != nil {
		r.close()
		return nil, err
	}
	a.remote = r
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
		return fmt.Errorf("kiln: sandbox/%s can't run long-lived processes", r.ri.Provider)
	}
	tmp, err := os.MkdirTemp("", "kiln-runin-")
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
			return fmt.Errorf("kiln: upload %s: %w", dst, err)
		}
	}
	proc, err := box.Start(ctx, sdk.ExecRequest{Cmd: "./" + boxBinary,
		Env: map[string]string{boxEnv: "1", boxDirEnv: boxDir}})
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
			return fmt.Errorf("kiln: agent in sandbox failed to start: %w", err)
		}
	case <-r.conn.Done():
		return errors.New("kiln: agent in sandbox exited during startup (see [box] logs)")
	case <-time.After(60 * time.Second):
		return errors.New("kiln: agent in sandbox did not start within 60s")
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
		return nil, fmt.Errorf("kiln: restarting the sandbox: %w", err)
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
				"tool": call.Name, "call_id": info.CallID, "step": info.Step, "module": "host",
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

func (r *remote) run(ctx context.Context, st *sdk.RunState, run Run) (*Result, error) {
	if st.ID == "" {
		st.ID = sdk.NewRunID()
	}
	r.setRun(st)
	defer r.endRun(st.ID)
	if err := r.host.eng.Root().Snapshot().AgentStart(ctx, st); err != nil {
		r.endHostRun(ctx, st, nil, err)
		return nil, err
	}
	done := r.subscribe(st.ID, run.OnEvent)
	defer done()
	var res Result
	err := r.call(ctx, "agent.run", toWire(st, st.ID, run.OnEvent != nil), &res)
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

func (r *remote) thread(ctx context.Context, thread, prompt string, decisions []sdk.Decision, run Run) (*Result, error) {
	runID := sdk.NewRunID()
	st := &sdk.RunState{ID: runID, Thread: thread, User: run.User, Roles: run.Roles, Model: r.host.model,
		Values: map[string]any{}}
	if run.User == "" {
		st.User, st.Roles = r.host.user, r.host.roles
	}
	if run.Model != "" {
		st.Model = run.Model
	}
	r.setRun(st)
	defer r.endRun(runID)
	if err := r.host.eng.Root().Snapshot().AgentStart(ctx, st); err != nil {
		r.endHostRun(ctx, st, nil, err)
		return nil, err
	}
	done := r.subscribe(runID, run.OnEvent)
	defer done()
	wr := wireRun{RunID: runID, Thread: thread, Prompt: prompt, Decisions: decisions, User: st.User, Roles: st.Roles,
		MaxSteps: run.MaxSteps, Streaming: run.OnEvent != nil}
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

func (r *remote) status() []engine.ModuleStatus {
	rows, err := r.statusErr()
	var out []engine.ModuleStatus
	for _, row := range rows {
		out = append(out, engine.ModuleStatus{ModuleInfo: engine.ModuleInfo{Name: "[box] " + row}, State: "ACTIVE"})
	}
	if err != nil {
		out = append(out, engine.ModuleStatus{ModuleInfo: engine.ModuleInfo{Name: "[box]"}, State: "FAILED", Err: err})
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

// close stops the box and disposes the host modules. The agent that owns
// this remote calls it from its own Close, so it must not call Close again.
func (r *remote) close() {
	r.stop()
	if r.host != nil && r.host.eng != nil {
		r.host.eng.Close()
	}
}

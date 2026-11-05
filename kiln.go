// Package kiln is a statically composed, capability-secured agent SDK.
//
// An agent is assembled from modules: compiled Go packages that declare
// what they require, what they provide and which capabilities they use.
// New validates the composition before anything runs and fails with a
// named error for an unknown module, a missing requirement, a duplicate
// provider or a dependency cycle. Modules reach the network, files,
// processes and databases only through the sdk broker, and only with the
// capabilities the admin policy grants.
//
//	a, _ := kiln.New(kiln.WithModel("openai:gpt-4.1"))
//	defer a.Close()
//	res, _ := a.Generate(ctx, "Hello!")
//	fmt.Println(res.Text)
package kiln

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/modules/std"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Re-exported core types so most users only import kiln.
type (
	Message       = sdk.Message
	ToolCall      = sdk.ToolCall
	ToolDef       = sdk.Tool
	Usage         = sdk.Usage
	Event         = sdk.Event
	Step          = sdk.Step
	Interrupt     = sdk.Interrupt
	Decision      = sdk.Decision
	Image         = sdk.Image
	ModuleSpec    = engine.ModuleSpec
	Module        = engine.Module
	Profile       = engine.Profile
	Policy        = engine.Policy
	ModuleStatus  = engine.ModuleStatus
	ModelRequest  = sdk.ModelRequest
	ModelResponse = sdk.ModelResponse
	ModelHandler  = sdk.ModelHandler
	ToolRequest   = sdk.ToolRequest
	ToolResult    = sdk.ToolResult
	ToolHandler   = sdk.ToolHandler
	ProviderError = sdk.ProviderError
	Checkpointer  = sdk.Checkpointer
	Store         = sdk.Store
)

var (
	ErrNoLoop       = errors.New("kiln: no \"loop\" service; load a loop module (e.g. loop)")
	ErrThreadBusy   = errors.New("kiln: thread was modified concurrently; retry")
	ErrNoCheckpoint = errors.New("kiln: threads need a \"threads\" service; load threads_inmem, threads_file or threads_sql (or use WithPersistence)")
)

// Agent is a started composition plus defaults for runs.
type Agent struct {
	eng          *engine.Engine
	model        string
	instructions string
	maxSteps     int
	user         string
	roles        []string
	closeOnce    sync.Once
	remote       *remote // set when the agent runs inside a sandbox (WithRunIn)
	saveEachStep bool
}

// New composes and starts an agent. Nothing runs until the composition is
// valid. With no options it uses the secure policy, the default loop,
// secrets and thread storage, and the provider implied by the model prefix.
func New(opts ...Option) (*Agent, error) {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.runIn != nil {
		if InBox() {
			return nil, errors.New("kiln: this process is a sandbox box; call kiln.ServeBox instead of WithRunIn")
		}
		return newRemote(cfg)
	}
	return newLocal(cfg)
}

func newLocal(cfg *config) (*Agent, error) {
	for _, f := range cfg.prepare {
		if err := f(); err != nil {
			return nil, fmt.Errorf("kiln: %w", err)
		}
	}
	prof, pol, err := resolve(cfg)
	if err != nil {
		return nil, err
	}
	a := newAgent(cfg, prof)
	plan, err := compose(cfg, prof, pol, a)
	if err != nil {
		return nil, fmt.Errorf("kiln: %w", err)
	}
	eng, err := plan.Start()
	if err != nil {
		return nil, fmt.Errorf("kiln: %w", err)
	}
	a.eng = eng
	return a, nil
}

func newAgent(cfg *config, prof *engine.Profile) *Agent {
	a := &Agent{model: prof.Model, instructions: prof.Instructions, maxSteps: prof.MaxSteps,
		user: cfg.user, roles: cfg.roles, saveEachStep: cfg.saveEachStep}
	if a.instructions == "" {
		a.instructions = "You are a helpful assistant."
	}
	return a
}

// facadeModules are the modules the agent itself contributes: the "agent"
// service (runs started from jobs and webhooks) and inline tools.
func facadeModules(a *Agent, tools []sdk.Tool) []engine.Module {
	ms := []engine.Module{{Name: "agent_service", Provides: []string{"agent"}, Apply: func(m *sdk.Module) error {
		return m.Provide("agent", &sdk.AgentService{Run: func(c context.Context, thread, user, prompt string) (*sdk.RunResult, error) {
			var res *Result
			var err error
			if thread != "" {
				res, err = a.Thread(thread).Generate(c, prompt, AsUser(user))
			} else {
				res, err = a.Generate(c, prompt, AsUser(user))
			}
			if err != nil {
				return nil, err
			}
			return &sdk.RunResult{Text: res.Text, Steps: res.Steps, Usage: res.Usage, Interrupts: res.Interrupts, FinishReason: res.FinishReason, Messages: res.Messages}, nil
		}})
	}}}
	if len(tools) > 0 {
		ms = append(ms, engine.Module{Name: "inline_tools", Apply: func(m *sdk.Module) error {
			for _, t := range tools {
				m.AddTool(t)
			}
			return nil
		}})
	}
	return ms
}

// compose validates the registry and the profile rows against the policy.
// It runs no module code.
func compose(cfg *config, prof *engine.Profile, pol *engine.Policy, a *Agent) (*engine.Plan, error) {
	reg := append(std.All(), cfg.modules...)
	reg = append(reg, facadeModules(a, cfg.tools)...)
	specs := append([]engine.ModuleSpec{{Use: "agent_service"}}, prof.Modules...)
	if len(cfg.tools) > 0 {
		specs = append(specs, engine.ModuleSpec{Use: "inline_tools"})
	}
	specs = autoSpecs(specs, reg, pol, cfg.noDefaults)
	return engine.Compose(engine.Options{Registry: reg, Specs: specs, Policy: pol, Logger: cfg.logger})
}

// Close disposes every module (running their cleanups). It is safe to call
// more than once.
func (a *Agent) Close() error {
	a.closeOnce.Do(func() {
		if a.remote != nil {
			a.remote.close()
			return
		}
		a.eng.Close()
	})
	return nil
}

// Engine exposes the started composition, for applications that read its
// services or status.
func (a *Agent) Engine() *engine.Engine { return a.eng }

// Status lists the running modules in start order.
func (a *Agent) Status() []engine.ModuleStatus {
	if a.remote != nil {
		return a.remote.status()
	}
	return a.eng.Status()
}

func (a *Agent) Model() string { return a.model }

// ---- Runs ----

// Result is what a run returns.
type Result struct {
	Text         string
	Steps        []sdk.Step
	Usage        sdk.Usage
	FinishReason string // stop | max_steps | interrupted | tool:<name>
	Interrupts   []sdk.Interrupt
	Messages     []sdk.Message // full history, feed back with WithHistory
	Thread       string
	Changes      []Change // files written by tool_fs during this run
}

// ToolCalls returns every tool call made during the run.
func (r *Result) ToolCalls() []sdk.ToolCall {
	var out []sdk.ToolCall
	for _, s := range r.Steps {
		out = append(out, s.Response.ToolCalls...)
	}
	return out
}

// Interrupted reports whether the run paused for a human decision.
func (r *Result) Interrupted() bool { return len(r.Interrupts) > 0 }

// Generate runs the agent to completion on a prompt.
func (a *Agent) Generate(ctx context.Context, prompt string, opts ...RunOption) (*Result, error) {
	ro := a.runOpts(opts)
	st := a.newState(ro)
	st.Messages = append(st.Messages, ro.history...)
	if prompt != "" || len(ro.images) > 0 {
		st.Messages = append(st.Messages, sdk.Message{Role: sdk.RoleUser, Content: prompt, Images: ro.images})
	}
	return a.run(ctx, st, ro)
}

// Resume continues a run that stopped with interrupts, without a
// thread storage: the state travels in the previous Result.
func (a *Agent) Resume(ctx context.Context, prev *Result, decisions []sdk.Decision, opts ...RunOption) (*Result, error) {
	ro := a.runOpts(opts)
	st := a.newState(ro)
	st.Messages = append([]sdk.Message{}, prev.Messages...)
	st.Pending = prev.Interrupts
	st.Decisions = decisions
	return a.run(ctx, st, ro)
}

func (a *Agent) runOpts(opts []RunOption) *runOptions {
	ro := &runOptions{user: a.user, roles: a.roles}
	for _, o := range opts {
		o(ro)
	}
	return ro
}

func (a *Agent) newState(ro *runOptions) *sdk.RunState {
	st := &sdk.RunState{
		ID: ro.runID, Model: a.model, System: a.instructions, MaxSteps: a.maxSteps, User: ro.user, Roles: ro.roles,
		StopOnTools: ro.stopOn, ToolChoice: ro.toolChoice, Temperature: ro.temperature, MaxTokens: ro.maxTokens,
		ResponseSchema: ro.schema, ResponseName: ro.schemaName, Values: map[string]any{}, Tools: ro.tools,
	}
	if ro.model != "" {
		st.Model = ro.model
	}
	if ro.system != "" {
		st.System = ro.system
	}
	if ro.maxSteps > 0 {
		st.MaxSteps = ro.maxSteps
	}
	return st
}

func (a *Agent) run(ctx context.Context, st *sdk.RunState, ro *runOptions) (*Result, error) {
	if a.remote != nil {
		return a.remote.run(ctx, st, ro)
	}
	snap := a.eng.Root().Snapshot() // module changes never land mid-run
	loop, ok := snap.Services["loop"].(*sdk.Loop)
	if !ok {
		return nil, ErrNoLoop
	}
	st.Streaming = ro.onEvent != nil
	st.Checkpoint = ro.checkpoint
	st.Emit = func(e sdk.Event) {
		if ro.onEvent != nil {
			ro.onEvent(e)
		}
		if ro.onStep != nil && e.Type == "step_start" {
			ro.onStep(st)
		}
	}
	res, err := loop.Run(ctx, snap, st)
	if err != nil {
		return nil, err
	}
	return &Result{Text: res.Text, Steps: res.Steps, Usage: res.Usage, FinishReason: res.FinishReason,
		Interrupts: res.Interrupts, Messages: res.Messages, Thread: st.Thread, Changes: changesOf(st.Values)}, nil
}

// GenerateText is a one-shot helper: start an agent, run once, close.
func GenerateText(ctx context.Context, model, prompt string, opts ...Option) (*Result, error) {
	a, err := New(append([]Option{WithModel(model)}, opts...)...)
	if err != nil {
		return nil, err
	}
	defer a.Close()
	return a.Generate(ctx, prompt)
}

// ---- Approvals ----

func Approve(id string) sdk.Decision { return sdk.Decision{InterruptID: id, Type: "approve"} }
func Reject(id, why string) sdk.Decision {
	return sdk.Decision{InterruptID: id, Type: "reject", Message: why}
}
func Edit(id string, args map[string]any) sdk.Decision {
	return sdk.Decision{InterruptID: id, Type: "edit", Args: args}
}
func Respond(id, text string) sdk.Decision {
	return sdk.Decision{InterruptID: id, Type: "respond", Message: text}
}

// ---- Model prefix -> provider module ----

type providerPreset struct {
	use    string
	config map[string]any
}

var providerPresets = map[string]providerPreset{
	"openai":     {"model_openai", map[string]any{"name": "openai"}},
	"deepseek":   {"model_openai", map[string]any{"name": "deepseek", "base_url": "https://api.deepseek.com", "api_key": "secret://DEEPSEEK_API_KEY", "structured": "json_object"}},
	"ollama":     {"model_openai", map[string]any{"name": "ollama", "base_url": "http://localhost:11434/v1", "api_key": ""}},
	"openrouter": {"model_openai", map[string]any{"name": "openrouter", "base_url": "https://openrouter.ai/api/v1", "api_key": "secret://OPENROUTER_API_KEY"}},
	"groq":       {"model_openai", map[string]any{"name": "groq", "base_url": "https://api.groq.com/openai/v1", "api_key": "secret://GROQ_API_KEY", "structured": "json_object"}},
	"anthropic":  {"model_anthropic", map[string]any{"name": "anthropic"}},
	"gemini":     {"model_gemini", map[string]any{"name": "gemini"}},
	"google":     {"model_gemini", map[string]any{"name": "google"}},
	"mock":       {"model_mock", map[string]any{"name": "mock"}},
}

// providerSpec returns the row a model string needs, if any. Each provider
// gets its own instance name, so several can be loaded at once.
func providerSpec(model string) (engine.ModuleSpec, bool) {
	prov, _ := sdk.SplitModel(model)
	p, ok := providerPresets[prov]
	if !ok {
		return engine.ModuleSpec{}, false
	}
	cfg := map[string]any{}
	for k, v := range p.config {
		cfg[k] = v
	}
	return engine.ModuleSpec{Use: p.use, As: "model_" + prov, Config: cfg}, true
}

// ---- Resolution ----

// resolve merges the profiles, the options and the defaults into one profile,
// and picks the policy. The result is what compose validates.
func resolve(cfg *config) (*engine.Profile, *engine.Policy, error) {
	prof := &engine.Profile{}
	if !cfg.noDefaults {
		prof.Modules = []engine.ModuleSpec{{Use: "loop"}, {Use: "secrets_env"}, {Use: "threads_inmem"}}
	}
	for _, path := range cfg.profiles {
		var p engine.Profile
		if err := engine.ReadJSON(path, &p); err != nil {
			return nil, nil, err
		}
		prof.Merge(&p)
	}
	prof.Merge(&engine.Profile{Model: cfg.model, Instructions: cfg.instructions, MaxSteps: cfg.maxSteps, Modules: cfg.specs})
	if !cfg.noDefaults {
		prof.Modules = dropOverriddenDefaults(prof.Modules)
	}
	if prof.Model == "" {
		prof.Model = os.Getenv("KILN_MODEL")
	}
	if prof.Model == "" {
		return nil, nil, errors.New("kiln: no model; use WithModel(\"provider:model\") or set KILN_MODEL")
	}
	// The model prefix implies a provider row, unless the application already
	// loads that module: its own configuration then wins.
	if spec, ok := providerSpec(prof.Model); ok && !cfg.noDefaults {
		have := false
		for _, s := range prof.Modules {
			if s.Use == spec.Use {
				have = true
			}
		}
		if !have {
			prof.Modules = append(prof.Modules, spec)
		}
	}
	var pol *engine.Policy
	switch {
	case cfg.policyObj != nil:
		pol = cfg.policyObj
	case cfg.policyPath != "":
		pol = &engine.Policy{}
		if err := engine.ReadJSON(cfg.policyPath, pol); err != nil {
			return nil, nil, err
		}
	default:
		pol = engine.SecurePolicy()
	}
	return prof, pol, nil
}

// Defaults give way to a user module of the same family, so adding
// threads_file replaces threads_inmem instead of clashing with it.
func dropOverriddenDefaults(specs []engine.ModuleSpec) []engine.ModuleSpec {
	families := map[string]string{"loop": "loop", "secrets_env": "secrets_", "threads_inmem": "threads_"}
	var out []engine.ModuleSpec
	for i, s := range specs {
		fam, isDefault := families[s.Name()]
		overridden := false
		if isDefault {
			for _, o := range specs[i+1:] {
				if o.Use != s.Use && strings.HasPrefix(o.Use, fam) {
					overridden = true
				}
			}
		}
		if !overridden {
			out = append(out, s)
		}
	}
	return out
}

// autoModules maps a service that a module requires to the standard module
// that provides it by default. Only used when nothing else provides it.
var autoModules = map[string]string{
	"workspace":     "workspace_local",
	"sandbox/local": "sandbox_local",
	"db":            "db_sql",
	"threads":       "threads_inmem",
	"memory":        "memory_inmem",
	"loop":          "loop",
}

// autoSpecs adds the default module for every required service that no
// enabled module provides, until nothing more is added. It is static: the
// added rows go through compose like any other, and a policy that denies a
// default leaves the requirement unmet, which compose then reports.
func autoSpecs(specs []engine.ModuleSpec, reg []engine.Module, pol *engine.Policy, noDefaults bool) []engine.ModuleSpec {
	if noDefaults {
		return specs
	}
	byName := map[string]engine.Module{}
	for _, m := range reg {
		byName[m.Name] = m
	}
	for round := 0; round < 4; round++ {
		progress := false
		var enabled []engine.Module
		names := map[string]bool{}
		for _, s := range specs {
			if s.Disabled {
				continue
			}
			names[s.Use] = true
			if m, ok := byName[s.Use]; ok {
				enabled = append(enabled, m)
			}
		}
		for _, m := range enabled {
			for _, req := range m.Requires {
				if strings.HasPrefix(req, "?") {
					continue
				}
				if servedBy(enabled, req) {
					continue
				}
				use, ok := autoModules[req]
				if !ok || names[use] || !pol.Allows(use) {
					continue
				}
				specs = append(specs, engine.ModuleSpec{Use: use, Auto: true})
				names[use] = true
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	return specs
}

// servedBy reports whether an enabled module provides service svc.
func servedBy(mods []engine.Module, svc string) bool {
	for _, m := range mods {
		for _, p := range m.Provides {
			if p == svc {
				return true
			}
			if strings.HasSuffix(p, "*") && strings.HasPrefix(svc, strings.TrimSuffix(p, "*")) {
				return true
			}
		}
	}
	return false
}

// AsTool exposes a whole agent (with its own modules, model and policy)
// as a tool another agent can delegate to.
func (a *Agent) AsTool(name, description string) sdk.Tool {
	return sdk.Tool{
		Name:        name,
		Description: description,
		Schema:      sdk.Object(map[string]any{"task": sdk.Str("the task, with all context it needs")}, "task"),
		Meta:        map[string]any{"subagent": true},
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			ri := sdk.RunInfoFrom(ctx)
			res, err := a.Generate(ctx, sdk.ArgString(args, "task"), AsUser(ri.User, ri.Roles...))
			if err != nil {
				return "", err
			}
			if res.Interrupted() {
				return "", fmt.Errorf("%s needs approval for %s; ask the user", name, res.Interrupts[0].ToolCall.Name)
			}
			return res.Text, nil
		},
	}
}

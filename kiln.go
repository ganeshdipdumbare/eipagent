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
//	a, _ := kiln.New(kiln.Config{Model: "openai:gpt-4.1"})
//	defer a.Close()
//	res, _ := a.Generate(ctx, "Hello!")
//	fmt.Println(res.Text)
package kiln

import (
	"context"
	"errors"
	"fmt"
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
	Tool          = sdk.Tool
	Usage         = sdk.Usage
	Event         = sdk.Event
	Step          = sdk.Step
	Interrupt     = sdk.Interrupt
	Decision      = sdk.Decision
	Image         = sdk.Image
	Module        = engine.Module
	ModuleSpec    = engine.ModuleSpec
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
	ErrNoLoop       = errors.New("kiln: no \"loop\" service; add loop.New() to Config.Modules")
	ErrThreadBusy   = errors.New("kiln: thread was modified concurrently; retry")
	ErrNoCheckpoint = errors.New("kiln: threads need a \"threads\" service; add threads_inmem.New(), threads_file.New(dir) or threads_sql.New()")
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
	remote       *remote // set when the agent runs inside a sandbox (Config.RunIn)
	saveEachStep bool
}

// New composes and starts an agent. Nothing runs until the composition is
// valid. The default loop, secrets and thread storage are added unless a
// module of the same family is in Config.Modules.
func New(cfg Config) (*Agent, error) {
	if cfg.RunIn != nil {
		if InBox() {
			return nil, errors.New("kiln: this process is a sandbox box; call kiln.ServeBox instead of Config.RunIn")
		}
		return newRemote(cfg)
	}
	return newLocal(cfg)
}

func newLocal(cfg Config) (*Agent, error) {
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

func newAgent(cfg Config, prof *engine.Profile) *Agent {
	a := &Agent{model: prof.Model, instructions: prof.Instructions, maxSteps: prof.MaxSteps,
		user: cfg.User, roles: cfg.Roles, saveEachStep: cfg.SaveEachStep}
	if a.instructions == "" {
		a.instructions = "You are a helpful assistant."
	}
	return a
}

// facadeModules are the modules the agent itself contributes: the "agent"
// service (runs started from jobs and webhooks) and inline tools.
func facadeModules(a *Agent, tools []sdk.Tool) []engine.Module {
	ms := []engine.Module{{Name: "agent_service", Provides: []string{"agent"}, Apply: func(m *sdk.Scope) error {
		return m.Provide("agent", &sdk.AgentService{Run: func(c context.Context, thread, user, prompt string) (*sdk.RunResult, error) {
			var res *Result
			var err error
			if thread != "" {
				res, err = a.Thread(thread).Generate(c, prompt, Run{User: user})
			} else {
				res, err = a.Generate(c, prompt, Run{User: user})
			}
			if err != nil {
				return nil, err
			}
			return &sdk.RunResult{Text: res.Text, Steps: res.Steps, Usage: res.Usage, Interrupts: res.Interrupts, FinishReason: res.FinishReason, Messages: res.Messages}, nil
		}})
	}}}
	if len(tools) > 0 {
		ms = append(ms, engine.Module{Name: "inline_tools", Apply: func(m *sdk.Scope) error {
			for _, t := range tools {
				if err := m.AddTool(t); err != nil {
					return err
				}
			}
			return nil
		}})
	}
	return ms
}

// compose validates the registry and the resolved profile rows against the
// policy. It runs no module code.
func compose(cfg Config, prof *engine.Profile, pol *engine.Policy, a *Agent) (*engine.Plan, error) {
	reg := append(registry(cfg.Modules), facadeModules(a, cfg.Tools)...)
	specs := append([]engine.ModuleSpec{{Use: "agent_service"}}, prof.Modules...)
	if len(cfg.Tools) > 0 {
		specs = append(specs, engine.ModuleSpec{Use: "inline_tools"})
	}
	specs = autoSpecs(specs, reg, pol)
	return engine.Compose(engine.Options{Registry: reg, Specs: specs, Policy: pol, Logger: cfg.Logger})
}

// registry is the standard catalog with the application's modules layered
// on top: an application descriptor replaces the standard one of the same
// name, so the explicit composition style works for standard
// modules too.
func registry(modules []engine.Module) []engine.Module {
	all := std.All()
	byName := make(map[string]engine.Module, len(all)+len(modules))
	order := make([]string, 0, len(all)+len(modules))
	for _, m := range all {
		if _, ok := byName[m.Name]; !ok {
			order = append(order, m.Name)
		}
		byName[m.Name] = m
	}
	for _, m := range modules {
		if _, ok := byName[m.Name]; !ok {
			order = append(order, m.Name)
		}
		// Configuration belongs to an instance, not the shared descriptor:
		// otherwise one instance's config leaks into another of the same
		// module. resolve copies it into the spec.
		m.Config = nil
		byName[m.Name] = m
	}
	out := make([]engine.Module, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
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
	Messages     []sdk.Message // full history, feed back with Run.History
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
func (a *Agent) Generate(ctx context.Context, prompt string, run ...Run) (*Result, error) {
	r := one(run)
	st := a.newState(r)
	st.Messages = append(st.Messages, r.History...)
	if prompt != "" || len(r.Images) > 0 {
		st.Messages = append(st.Messages, sdk.Message{Role: sdk.RoleUser, Content: prompt, Images: r.Images})
	}
	return a.exec(ctx, st, r)
}

// Resume continues a run that stopped with interrupts, without thread
// storage: the state travels in the previous Result.
func (a *Agent) Resume(ctx context.Context, prev *Result, decisions []sdk.Decision, run ...Run) (*Result, error) {
	r := one(run)
	st := a.newState(r)
	st.Messages = append([]sdk.Message{}, prev.Messages...)
	st.Pending = prev.Interrupts
	st.Decisions = decisions
	return a.exec(ctx, st, r)
}

func (a *Agent) newState(r Run) *sdk.RunState {
	st := &sdk.RunState{
		ID: r.RunID, Model: a.model, System: a.instructions, MaxSteps: a.maxSteps,
		User: a.user, Roles: a.roles,
		StopOnTools: r.StopOnTools, ToolChoice: r.ToolChoice, Temperature: r.Temperature, MaxTokens: r.MaxTokens,
		ResponseSchema: r.Schema, ResponseName: r.SchemaName, Values: map[string]any{}, Tools: r.Tools,
	}
	if r.Model != "" {
		st.Model = r.Model
	}
	if r.Instructions != "" {
		st.System = r.Instructions
	}
	if r.MaxSteps > 0 {
		st.MaxSteps = r.MaxSteps
	}
	if r.User != "" {
		st.User, st.Roles = r.User, r.Roles
	}
	return st
}

func (a *Agent) exec(ctx context.Context, st *sdk.RunState, r Run) (*Result, error) {
	if a.remote != nil {
		return a.remote.run(ctx, st, r)
	}
	snap := a.eng.Root().Snapshot() // module changes never land mid-run
	loop, ok := snap.Services["loop"].(*sdk.Loop)
	if !ok {
		return nil, ErrNoLoop
	}
	st.Streaming = r.OnEvent != nil
	st.Checkpoint = r.checkpoint
	st.Emit = func(e sdk.Event) {
		if r.OnEvent != nil {
			r.OnEvent(e)
		}
		if r.onStep != nil && e.Type == "step_start" {
			r.onStep(st)
		}
	}
	res, err := loop.Run(ctx, snap, st)
	if err != nil {
		return nil, err
	}
	return &Result{Text: res.Text, Steps: res.Steps, Usage: res.Usage, FinishReason: res.FinishReason,
		Interrupts: res.Interrupts, Messages: res.Messages, Thread: st.Thread, Changes: changesOf(st.Values)}, nil
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

// resolve turns Config into the profile Compose validates: the defaults,
// the application's modules, then the provider implied by the model.
func resolve(cfg Config) (*engine.Profile, *engine.Policy, error) {
	if cfg.Model == "" {
		return nil, nil, errors.New("kiln: no model; set Config.Model to \"provider:model\"")
	}
	prof := &engine.Profile{Model: cfg.Model, Instructions: cfg.Instructions, MaxSteps: cfg.MaxSteps}
	// Defaults come first. A module of the same family takes the default's
	// place, so it starts as early as the default it replaces.
	used := make([]bool, len(cfg.Modules))
	for _, d := range []struct{ family, use string }{
		{"loop", "loop"},
		{"secrets_", "secrets_env"},
		{"threads_", "threads_inmem"},
	} {
		found := false
		for i, m := range cfg.Modules {
			if matchesFamily(m.Name, d.family) {
				prof.Modules = append(prof.Modules, specOf(m))
				used[i], found = true, true
				break
			}
		}
		if !found {
			prof.Modules = append(prof.Modules, engine.ModuleSpec{Use: d.use})
		}
	}
	for i, m := range cfg.Modules {
		if !used[i] {
			prof.Modules = append(prof.Modules, specOf(m))
		}
	}
	// The model prefix implies a provider row, unless the application
	// configures that provider itself.
	if spec, ok := providerSpec(cfg.Model); ok && !hasModule(cfg.Modules, spec.Use) {
		prof.Modules = append(prof.Modules, spec)
	}
	pol := cfg.Policy
	if pol == nil {
		pol = engine.SecurePolicy()
	}
	return prof, pol, nil
}

func specOf(m engine.Module) engine.ModuleSpec {
	return engine.ModuleSpec{Use: m.Name, As: m.As, Config: m.Config, Priority: m.Priority}
}

// matchesFamily reports whether a module belongs to a default's family.
// A trailing "_" means prefix; otherwise the name must match exactly.
func matchesFamily(name, family string) bool {
	if strings.HasSuffix(family, "_") {
		return strings.HasPrefix(name, family)
	}
	return name == family
}

func hasModule(modules []engine.Module, name string) bool {
	for _, m := range modules {
		if m.Name == name {
			return true
		}
	}
	return false
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
func autoSpecs(specs []engine.ModuleSpec, reg []engine.Module, pol *engine.Policy) []engine.ModuleSpec {
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
			res, err := a.Generate(ctx, sdk.ArgString(args, "task"), Run{User: ri.User, Roles: ri.Roles})
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

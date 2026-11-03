// Package eipagent is an "everything is a plugin" agent SDK.
//
// The core engine only loads plugins and routes calls between them; the
// agent loop, every LLM provider, tools, memory, middleware, sandboxes
// and policy are plugins. This package is the easy front door:
//
//	a, _ := eipagent.New(eipagent.WithModel("openai:gpt-4.1"))
//	defer a.Close()
//	res, _ := a.Generate(ctx, "Hello!")
//	fmt.Println(res.Text)
package eipagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/plugins"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// Re-exported core types so most users only import eipagent.
type (
	Message      = sdk.Message
	ToolCall     = sdk.ToolCall
	ToolDef      = sdk.Tool
	Usage        = sdk.Usage
	Event        = sdk.Event
	Step         = sdk.Step
	Interrupt    = sdk.Interrupt
	Decision     = sdk.Decision
	Image        = sdk.Image
	PluginSpec   = engine.PluginSpec
	Plugin       = engine.Plugin
	Profile      = engine.Profile
	Policy       = engine.Policy
	PluginStatus = engine.PluginStatus

	// For writing compiled plugins and middleware. A plugin's Apply
	// receives a *sdk.Plugin, in compiled and interpreted plugins alike.
	//
	// Deprecated: use sdk.Plugin (the same type). Kept until v1.0.
	PluginAPI = sdk.Plugin
	// Deprecated: use sdk.Plugin (the same type). Kept until v1.0.
	PluginContext = sdk.Plugin
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
	ErrNoLoop       = errors.New("eipagent: no \"loop\" service; load a loop plugin (e.g. loop)")
	ErrThreadBusy   = errors.New("eipagent: thread was modified concurrently; retry")
	ErrNoCheckpoint = errors.New("eipagent: threads need a \"threads\" service; load threads_inmem, threads_file or threads_sql (or use WithPersistence)")
)

// Agent is a running kernel plus defaults for runs.
type Agent struct {
	eng          *engine.Engine
	model        string
	instructions string
	maxSteps     int
	user         string
	roles        []string
	stop         chan struct{}
	closeOnce    sync.Once
	remote       *remote // set when the agent runs inside a sandbox (WithRunIn)
	saveEachStep bool
}

// New starts an agent. With no options it uses the secure preset, the
// default loop and the provider implied by the model prefix.
func New(opts ...Option) (*Agent, error) {
	cfg := &config{preset: "secure"}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.runIn != nil {
		return newRemote(cfg)
	}
	return newLocal(cfg)
}

func newLocal(cfg *config) (*Agent, error) {
	prof, pol, lock, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	dirs := cfg.dirs
	if cfg.store != nil {
		dirs = append(append([]string{}, dirs...), cfg.store.ActiveDir())
	}
	deny := append([]string{}, cfg.profiles...)
	if cfg.policyPath != "" {
		deny = append(deny, cfg.policyPath)
	}
	if cfg.lockPath != "" {
		deny = append(deny, cfg.lockPath)
	}
	if cfg.store != nil {
		deny = append(deny, cfg.store.Dir())
	}
	eng := engine.New(engine.Options{Dirs: dirs, Embedded: plugins.FS, Policy: pol, Lock: lock, Logger: cfg.logger, WriteDeny: deny})
	for _, p := range cfg.compiled {
		eng.Register(p)
	}
	if len(cfg.tools) > 0 {
		tools := cfg.tools
		eng.Register(engine.Plugin{Name: "inline_tools", Apply: func(ctx *sdk.Plugin) error {
			for _, t := range tools {
				ctx.AddTool(t)
			}
			return nil
		}})
		prof.Plugins = append(prof.Plugins, engine.PluginSpec{Use: "inline_tools"})
	}
	// The Agent is complete before any plugin runs: plugins (jobs,
	// webhooks) may start runs through the "agent" service right away.
	a := &Agent{eng: eng, model: prof.Model, instructions: prof.Instructions, maxSteps: prof.MaxSteps,
		user: cfg.user, roles: cfg.roles, stop: make(chan struct{}), saveEachStep: cfg.saveEachStep}
	if a.instructions == "" {
		a.instructions = "You are a helpful assistant."
	}
	self := a
	eng.Register(engine.Plugin{Name: "agent_service", Apply: func(ctx *sdk.Plugin) error {
		return ctx.Provide("agent", &sdk.AgentService{Run: func(c context.Context, thread, user, prompt string) (*sdk.RunResult, error) {
			var res *Result
			var err error
			if thread != "" {
				res, err = self.Thread(thread).Generate(c, prompt, AsUser(user))
			} else {
				res, err = self.Generate(c, prompt, AsUser(user))
			}
			if err != nil {
				return nil, err
			}
			return &sdk.RunResult{Text: res.Text, Steps: res.Steps, Usage: res.Usage, Interrupts: res.Interrupts, FinishReason: res.FinishReason, Messages: res.Messages}, nil
		}})
	}})
	prof.Plugins = append([]engine.PluginSpec{{Use: "agent_service"}}, prof.Plugins...)
	if cfg.store != nil {
		attachStore(cfg.store, eng, prof)
	}
	if errs := eng.LoadProfile(prof); len(errs) > 0 {
		eng.Close()
		return nil, fmt.Errorf("eipagent: %w", errors.Join(errs...))
	}
	if !cfg.noDefaults {
		autoload(eng)
	}
	if pol.HotReload {
		go eng.Watch(a.stop, func(s string) { eng.Root().Logf("%s", s) })
	}
	return a, nil
}

// Close unloads every plugin (running their cleanups).
func (a *Agent) Close() error {
	a.closeOnce.Do(func() {
		if a.remote != nil {
			a.remote.close()
			return
		}
		close(a.stop)
		a.eng.Close()
	})
	return nil
}

// Engine exposes the core for plugin management (/load, /unload ...).
func (a *Agent) Engine() *engine.Engine { return a.eng }

// Status lists plugins and their lifecycle state.
func (a *Agent) Status() []engine.PluginStatus {
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
		Model: a.model, System: a.instructions, MaxSteps: a.maxSteps, User: ro.user, Roles: ro.roles,
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
	snap := a.eng.Root().Snapshot() // plugin changes never land mid-run
	loop, ok := snap.Services["loop"].(*sdk.Loop)
	if !ok {
		return nil, ErrNoLoop
	}
	st.Streaming = ro.onEvent != nil
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

// ---- Model prefix -> provider plugin ----

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

// providerSpec returns the plugin row a model string needs, if any.
func providerSpec(model string) (engine.PluginSpec, bool) {
	prov, _ := sdk.SplitModel(model)
	p, ok := providerPresets[prov]
	if !ok {
		return engine.PluginSpec{}, false
	}
	cfg := map[string]any{}
	for k, v := range p.config {
		cfg[k] = v
	}
	return engine.PluginSpec{Use: p.use, As: "model_" + prov, Config: cfg}, true
}

// ---- Config resolution ----

func (c *config) resolve() (*engine.Profile, *engine.Policy, *engine.Lock, error) {
	prof := &engine.Profile{}
	if !c.noDefaults {
		prof.Plugins = []engine.PluginSpec{{Use: "loop"}, {Use: "secrets_env"}, {Use: "threads_inmem"}}
	}
	for _, path := range c.profiles {
		var p engine.Profile
		if err := engine.ReadJSON(path, &p); err != nil {
			return nil, nil, nil, err
		}
		prof.Merge(&p)
	}
	prof.Merge(&engine.Profile{Model: c.model, Instructions: c.instructions, MaxSteps: c.maxSteps, Plugins: c.specs})
	if !c.noDefaults {
		prof.Plugins = dropOverriddenDefaults(prof.Plugins)
	}
	if prof.Model == "" {
		prof.Model = os.Getenv("EIPAGENT_MODEL")
	}
	if prof.Model == "" {
		return nil, nil, nil, errors.New("eipagent: no model; use WithModel(\"provider:model\") or set EIPAGENT_MODEL")
	}
	if spec, ok := providerSpec(prof.Model); ok && !c.noDefaults && !c.noProviderAuto {
		have := false
		for _, s := range prof.Plugins {
			// The app may load the provider itself, under its current or its
			// pre-v0.4 name (llm_mock for model_mock).
			if sdk.CanonicalPlugin(s.Name()) == spec.Name() || (sdk.CanonicalPlugin(s.Use) == spec.Use && s.Config["name"] == spec.Config["name"]) {
				have = true
			}
		}
		if !have {
			prof.Plugins = append(prof.Plugins, spec)
		}
	}
	var pol *engine.Policy
	switch {
	case c.policyObj != nil:
		pol = c.policyObj
	case c.policyPath != "":
		pol = &engine.Policy{}
		if err := engine.ReadJSON(c.policyPath, pol); err != nil {
			return nil, nil, nil, err
		}
	case c.preset == "dev":
		pol = engine.DevPolicy()
	case c.preset == "secure" || c.preset == "":
		pol = engine.SecurePolicy()
	default:
		return nil, nil, nil, fmt.Errorf("eipagent: unknown preset %q (secure, dev)", c.preset)
	}
	if c.hotReload != nil {
		pol.HotReload = *c.hotReload
	}
	if c.bridgedSecrets != nil {
		// In a RunIn box no secrets_* plugin loads (the profile's or the
		// policy's): the host's provider answers, with placeholders, and
		// it loads first so required plugins can resolve their refs.
		keep := func(specs []engine.PluginSpec) []engine.PluginSpec {
			var out []engine.PluginSpec
			for _, s := range specs {
				if !strings.HasPrefix(sdk.CanonicalPlugin(s.Use), "secrets_") {
					out = append(out, s)
				}
			}
			return out
		}
		prof.Plugins = keep(prof.Plugins)
		cp := *pol
		cp.Require = append([]engine.PluginSpec{{Use: c.bridgedSecrets.Name}}, keep(pol.Require)...)
		pol = &cp
	}
	lock := &engine.Lock{Plugins: map[string]string{}}
	lockPath := c.lockPath
	if lockPath == "" {
		for _, d := range c.dirs {
			if p := filepath.Join(d, "..", "eip.lock"); fileExists(p) {
				lockPath = p
			} else if p := filepath.Join(d, "eip.lock"); fileExists(p) {
				lockPath = p
			}
		}
	}
	if lockPath != "" {
		c.lockPath = lockPath
		if err := engine.ReadJSON(lockPath, lock); err != nil {
			return nil, nil, nil, err
		}
	}
	return prof, pol, lock, nil
}

// Defaults give way to a user plugin of the same family, so adding
// threads_file replaces threads_inmem instead of clashing with it.
func dropOverriddenDefaults(specs []engine.PluginSpec) []engine.PluginSpec {
	families := map[string]string{"loop": "loop_", "secrets_env": "secrets_", "threads_inmem": "threads_"}
	var out []engine.PluginSpec
	for i, s := range specs {
		fam, isDefault := families[s.Name()]
		overridden := false
		if isDefault {
			for _, o := range specs[i+1:] {
				use := sdk.CanonicalPlugin(o.Use) // old names (checkpoint_file, loop_default) count too
				if use == s.Use || (o.Use != s.Use && strings.HasPrefix(use, fam)) {
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

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// AsTool exposes a whole agent (with its own plugins, model and policy)
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

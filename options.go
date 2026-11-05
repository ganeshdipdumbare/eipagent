package kiln

import (
	"context"

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// config is what New is given. Nothing in it is interpreted at run time:
// every module named here must be in the registry (std or WithModule).
type config struct {
	model, instructions string
	maxSteps            int
	profiles            []string
	policyPath          string
	policyObj           *engine.Policy
	specs               []engine.ModuleSpec
	modules             []engine.Module // application modules, compiled in
	tools               []sdk.Tool
	logger              func(scope, msg string)
	user                string
	roles               []string
	noDefaults          bool
	runIn               *RunIn
	saveEachStep        bool
	// prepare runs before composition: directories that file-backed modules
	// need (modules themselves cannot create host directories).
	prepare []func() error
}

// Option configures New.
type Option func(*config)

// WithSaveEachStep makes threads save their history before every model
// call, not only when a run ends. If the process dies mid-run, the run's
// progress so far is kept: Thread.Continue picks it up from there.
func WithSaveEachStep() Option { return func(c *config) { c.saveEachStep = true } }

// WithModel sets the default model as "provider:model", e.g.
// "openai:gpt-4.1", "anthropic:claude-sonnet-5-5", "deepseek:deepseek-chat".
func WithModel(m string) Option { return func(c *config) { c.model = m } }

// WithInstructions sets the system prompt.
func WithInstructions(s string) Option { return func(c *config) { c.instructions = s } }

// WithMaxSteps caps model calls per run (default 20).
func WithMaxSteps(n int) Option { return func(c *config) { c.maxSteps = n } }

// WithTools registers tools defined in Go (see Tool).
func WithTools(ts ...sdk.Tool) Option { return func(c *config) { c.tools = append(c.tools, ts...) } }

// WithModules runs standard modules by name (see modules/std), with no
// configuration. Use WithModuleConfig to configure one.
func WithModules(names ...string) Option {
	return func(c *config) {
		for _, n := range names {
			c.specs = append(c.specs, engine.ModuleSpec{Use: n})
		}
	}
}

// WithModule compiles an application module into the agent. Build the
// descriptor in your own package and pass it here; it is registered for
// this agent only.
//
//	WithModule(engine.Module{Name: "orders", Requires: []string{"db"}, Permissions: []string{"net"}, Apply: applyOrders})
func WithModule(m engine.Module) Option {
	return func(c *config) {
		c.modules = append(c.modules, m)
		c.specs = append(c.specs, engine.ModuleSpec{Use: m.Name})
	}
}

// WithModuleConfig runs a module (standard or compiled) with configuration.
func WithModuleConfig(name string, cfg map[string]any) Option {
	return func(c *config) { c.specs = append(c.specs, engine.ModuleSpec{Use: name, Config: cfg}) }
}

// WithModuleSpec adds a full row: instance name (As), priority, disabled.
// Use As to load one module twice, e.g. two model providers.
func WithModuleSpec(s engine.ModuleSpec) Option {
	return func(c *config) { c.specs = append(c.specs, s) }
}

// WithProfile layers a profile JSON file (later ones win).
func WithProfile(path string) Option {
	return func(c *config) { c.profiles = append(c.profiles, path) }
}

// WithPolicyFile loads the admin policy; nothing else can override it.
func WithPolicyFile(path string) Option { return func(c *config) { c.policyPath = path } }

// WithPolicy sets the admin policy directly. The default is SecurePolicy:
// every module gets exactly the capabilities it declares.
func WithPolicy(p *engine.Policy) Option { return func(c *config) { c.policyObj = p } }

// WithLogger receives module log lines.
func WithLogger(fn func(scope, msg string)) Option { return func(c *config) { c.logger = fn } }

// WithUser sets the default user identity for runs (audit, policy, memory).
func WithUser(id string, roles ...string) Option {
	return func(c *config) { c.user, c.roles = id, roles }
}

// WithoutDefaults loads only what you ask for: no default loop,
// secrets, thread storage or provider row.
func WithoutDefaults() Option { return func(c *config) { c.noDefaults = true } }

// ---- Per-run options ----

type runOptions struct {
	model, system       string
	runID               string
	maxSteps, maxTokens int
	temperature         *float64
	toolChoice          string
	stopOn              []string
	history             []sdk.Message
	images              []sdk.Image
	onEvent             func(sdk.Event)
	user                string
	roles               []string
	schema              map[string]any
	schemaName          string
	tools               []sdk.Tool
	result              **Result
	onStep              func(*sdk.RunState) // threads: save before each model call (WithSaveEachStep)
	checkpoint          func() error        // threads: save before tools too
}

type RunOption func(*runOptions)

func Model(m string) RunOption { return func(r *runOptions) { r.model = m } }

// withRunID carries the host-assigned run id into the box so trusted
// host hooks can key budgets and audit records to the real run.
func withRunID(id string) RunOption   { return func(r *runOptions) { r.runID = id } }
func System(s string) RunOption       { return func(r *runOptions) { r.system = s } }
func MaxSteps(n int) RunOption        { return func(r *runOptions) { r.maxSteps = n } }
func MaxTokens(n int) RunOption       { return func(r *runOptions) { r.maxTokens = n } }
func Temperature(t float64) RunOption { return func(r *runOptions) { r.temperature = &t } }
func ToolChoice(c string) RunOption   { return func(r *runOptions) { r.toolChoice = c } }
func StopOnTool(n ...string) RunOption {
	return func(r *runOptions) { r.stopOn = append(r.stopOn, n...) }
}
func WithHistory(m ...Message) RunOption {
	return func(r *runOptions) { r.history = append(r.history, m...) }
}
func WithImages(i ...Image) RunOption {
	return func(r *runOptions) { r.images = append(r.images, i...) }
}
func OnEvent(fn func(Event)) RunOption { return func(r *runOptions) { r.onEvent = fn } }
func AsUser(id string, roles ...string) RunOption {
	return func(r *runOptions) { r.user, r.roles = id, roles }
}

// RunTools adds tools for this run only.
func RunTools(ts ...sdk.Tool) RunOption {
	return func(r *runOptions) { r.tools = append(r.tools, ts...) }
}

// CaptureResult stores the run Result (useful with GenerateObject).
func CaptureResult(dst **Result) RunOption { return func(r *runOptions) { r.result = dst } }

// WithApprover decides human-in-the-loop interrupts synchronously (for
// example by prompting in a CLI). Return nil to leave one pending.
func WithApprover(fn func(ctx context.Context, in Interrupt) (*Decision, error)) Option {
	return WithModule(engine.Module{Name: "approver_inline", Provides: []string{"approver"}, Apply: func(m *sdk.Module) error {
		return m.Provide("approver", &sdk.Approver{Decide: fn})
	}})
}

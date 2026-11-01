package eipagent

import (
	"context"

	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/pluginpkg"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

type config struct {
	model, instructions string
	maxSteps            int
	preset              string
	profiles            []string
	policyPath          string
	policyObj           *engine.Policy
	lockPath            string
	dirs                []string
	specs               []engine.PluginSpec
	compiled            []engine.Plugin
	tools               []sdk.Tool
	hotReload           *bool
	logger              func(scope, msg string)
	user                string
	roles               []string
	noDefaults          bool
	noProviderAuto      bool
	runIn               *RunIn
	store               *pluginpkg.Store
}

// Option configures New.
type Option func(*config)

// WithModel sets the default model as "provider:model", e.g.
// "openai:gpt-4.1", "anthropic:claude-sonnet-5-5", "deepseek:deepseek-chat".
func WithModel(m string) Option { return func(c *config) { c.model = m } }

// WithInstructions sets the system prompt.
func WithInstructions(s string) Option { return func(c *config) { c.instructions = s } }

// WithMaxSteps caps model calls per run (default 20).
func WithMaxSteps(n int) Option { return func(c *config) { c.maxSteps = n } }

// WithTools registers tools defined in Go (see Tool).
func WithTools(ts ...sdk.Tool) Option { return func(c *config) { c.tools = append(c.tools, ts...) } }

// WithPlugins loads plugins by name (standard or from WithPluginDir).
func WithPlugins(names ...string) Option {
	return func(c *config) {
		for _, n := range names {
			c.specs = append(c.specs, engine.PluginSpec{Use: n})
		}
	}
}

// WithPlugin loads one plugin with config.
func WithPlugin(use string, cfg map[string]any) Option {
	return func(c *config) { c.specs = append(c.specs, engine.PluginSpec{Use: use, Config: cfg}) }
}

// WithPluginSpec loads a full profile row (instance name, priority...).
func WithPluginSpec(s engine.PluginSpec) Option {
	return func(c *config) { c.specs = append(c.specs, s) }
}

// WithCompiledPlugin registers and loads a native Go plugin.
func WithCompiledPlugin(p engine.Plugin) Option {
	return func(c *config) {
		c.compiled = append(c.compiled, p)
		c.specs = append(c.specs, engine.PluginSpec{Use: p.Name})
	}
}

// WithPluginDir adds a directory of interpreted plugins (<name>.go).
func WithPluginDir(dir string) Option { return func(c *config) { c.dirs = append(c.dirs, dir) } }

// WithProfile layers a profile JSON file (later ones win).
func WithProfile(path string) Option {
	return func(c *config) { c.profiles = append(c.profiles, path) }
}

// WithPolicyFile loads the admin policy; nothing else can override it.
func WithPolicyFile(path string) Option { return func(c *config) { c.policyPath = path } }

// WithPolicy sets the admin policy directly.
func WithPolicy(p *engine.Policy) Option { return func(c *config) { c.policyObj = p } }

// WithLockFile sets the eip.lock with plugin sha256 pins.
func WithLockFile(path string) Option { return func(c *config) { c.lockPath = path } }

// WithPreset picks "secure" (default: pinned plugins, no hot reload) or
// "dev" (no pins, hot reload). Ignored when a policy is given.
func WithPreset(name string) Option { return func(c *config) { c.preset = name } }

// WithHotReload overrides the policy's hot-reload switch.
func WithHotReload(on bool) Option { return func(c *config) { c.hotReload = &on } }

// WithLogger receives plugin log lines.
func WithLogger(fn func(scope, msg string)) Option { return func(c *config) { c.logger = fn } }

// WithUser sets the default user identity for runs (audit, policy, memory).
func WithUser(id string, roles ...string) Option {
	return func(c *config) { c.user, c.roles = id, roles }
}

// WithoutDefaults loads only what you ask for: no default loop,
// secrets, thread storage or provider.
func WithoutDefaults() Option { return func(c *config) { c.noDefaults = true } }

// ---- Per-run options ----

type runOptions struct {
	model, system       string
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
}

type RunOption func(*runOptions)

func Model(m string) RunOption        { return func(r *runOptions) { r.model = m } }
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
	return WithCompiledPlugin(engine.Plugin{Name: "approver_inline", Apply: func(ctx *sdk.Context) error {
		return ctx.Provide("approver", &sdk.Approver{Decide: fn})
	}})
}

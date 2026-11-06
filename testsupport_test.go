package kiln_test

// Helpers that build kiln.Config fragments, so tests read like the old
// option lists while exercising the single Config API.

import (
	"context"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/modules/std"
)

func withModel(m string) kiln.Config        { return kiln.Config{Model: m} }
func withInstructions(s string) kiln.Config { return kiln.Config{Instructions: s} }
func withMaxSteps(n int) kiln.Config        { return kiln.Config{MaxSteps: n} }
func withTools(ts ...kiln.Tool) kiln.Config { return kiln.Config{Tools: ts} }
func withUser(u string, roles ...string) kiln.Config {
	return kiln.Config{User: u, Roles: roles}
}
func withLogger(fn func(scope, msg string)) kiln.Config { return kiln.Config{Logger: fn} }
func withSaveEachStep() kiln.Config                     { return kiln.Config{SaveEachStep: true} }
func withPolicy(p *engine.Policy) kiln.Config           { return kiln.Config{Policy: p} }
func withRunIn(r kiln.RunIn) kiln.Config {
	return kiln.Config{RunIn: &r}
}
func withModule(m engine.Module) kiln.Config { return kiln.Config{Modules: []engine.Module{m}} }

func withModules(names ...string) kiln.Config {
	var c kiln.Config
	for _, n := range names {
		c.Modules = append(c.Modules, module(n))
	}
	return c
}

func withModuleConfig(name string, cfg map[string]any) kiln.Config {
	return kiln.Config{Modules: []engine.Module{module(name, cfg)}}
}

func withModuleSpec(s engine.ModuleSpec) kiln.Config {
	m, ok := std.Get(s.Use)
	if !ok {
		panic("test: unknown module " + s.Use)
	}
	m.Config, m.As, m.Priority = s.Config, s.As, s.Priority
	return kiln.Config{Modules: []engine.Module{m}}
}

func withPersistence(kind, target string) kiln.Config {
	return kiln.Config{Modules: kiln.Persistence(kind, target)}
}
func withApprovals(tools ...string) kiln.Config {
	return kiln.Config{Modules: kiln.Approvals(tools...)}
}
func withApprover(fn func(ctx context.Context, in kiln.Interrupt) (*kiln.Decision, error)) kiln.Config {
	return kiln.Config{Modules: kiln.Approver(fn)}
}
func withSandbox(provider string) kiln.Config {
	return kiln.Config{Modules: kiln.Sandbox(provider)}
}
func withAudit(path string) kiln.Config {
	return kiln.Config{Modules: kiln.Audit(path)}
}
func withTracing(endpoint string) kiln.Config {
	return kiln.Config{Modules: kiln.Tracing(endpoint)}
}
func withMCP(name, target string, args ...string) kiln.Config {
	return kiln.Config{Modules: kiln.MCP(name, target, args...)}
}

// module returns a standard module by name, for tests. String names are
// fine here: the SDK API itself takes module values.
func module(name string, cfg ...map[string]any) engine.Module {
	m, ok := std.Get(name)
	if !ok {
		panic("test: unknown module " + name)
	}
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

// merge layers config fragments; later non-zero values win and lists append.
func merge(cfgs ...kiln.Config) kiln.Config {
	var out kiln.Config
	for _, c := range cfgs {
		if c.Model != "" {
			out.Model = c.Model
		}
		if c.Instructions != "" {
			out.Instructions = c.Instructions
		}
		if c.MaxSteps > 0 {
			out.MaxSteps = c.MaxSteps
		}
		if c.User != "" {
			out.User = c.User
		}
		if len(c.Roles) > 0 {
			out.Roles = c.Roles
		}
		out.Tools = append(out.Tools, c.Tools...)
		out.Modules = append(out.Modules, c.Modules...)
		if c.Policy != nil {
			out.Policy = c.Policy
		}
		if c.Logger != nil {
			out.Logger = c.Logger
		}
		if c.SaveEachStep {
			out.SaveEachStep = true
		}
		if c.RunIn != nil {
			out.RunIn = c.RunIn
		}
	}
	return out
}

func startAgent(cfgs ...kiln.Config) (*kiln.Agent, error) { return kiln.New(merge(cfgs...)) }

// asInstance renames a module instance (two providers of one kind).
func asInstance(m engine.Module, name string) engine.Module {
	m.As = name
	return m
}

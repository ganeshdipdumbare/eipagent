package sdk

import (
	"context"
	"sort"
)

// Hook kinds. Ordering for every kind: priority (high first), then plugin
// load order, then registration order.
//
//	agent/start  serial     once per run, before the first step
//	step/prepare serial     rewrite the next model request (prepareStep / before_model)
//	model/wrap   onion      around every model call (wrap_model_call)
//	model/after  serial     inspect / rewrite the response, may Jump (after_model)
//	tool/wrap    onion      around every tool call (wrap_tool_call)
//	tool/before  serial     legacy: inspect / Veto a tool call
//	tool/after   serial     legacy: inspect / rewrite a tool result
//	agent/end    serial     once per run, after the last step
//	event        emit       observe every streamed Event (logging, tracing)
const (
	HookAgentStart  = "agent/start"
	HookStepPrepare = "step/prepare"
	HookModelWrap   = "model/wrap"
	HookModelAfter  = "model/after"
	HookToolWrap    = "tool/wrap"
	HookToolBefore  = "tool/before"
	HookToolAfter   = "tool/after"
	HookAgentEnd    = "agent/end"
	HookObserve     = "event"
)

// StepContext is what step/prepare and model/after hooks see.
type StepContext struct {
	Run     *RunState
	Step    int
	Request *ModelRequest
	// Jump, set by a model/after hook: "end" stops the run, "model"
	// re-asks the model, "" continues normally.
	Jump string
}

// HookEvent is the legacy tool/before + tool/after payload.
type HookEvent struct {
	Tool   string
	Args   map[string]any
	Result string
	Veto   string // set in tool/before to block the call
}

type (
	AgentStartFn  func(ctx context.Context, st *RunState) error
	StepPrepareFn func(ctx context.Context, sc *StepContext) error
	ModelWrapFn   func(ctx context.Context, req *ModelRequest, next ModelHandler) (*ModelResponse, error)
	ModelAfterFn  func(ctx context.Context, sc *StepContext, resp *ModelResponse) error
	ToolWrapFn    func(ctx context.Context, req *ToolRequest, next ToolHandler) (*ToolResult, error)
	AgentEndFn    func(ctx context.Context, st *RunState, res *RunResult) error
	EventFn       func(Event)
)

func (c *Context) OnAgentStart(fn AgentStartFn)   { c.hook(HookAgentStart, fn) }
func (c *Context) OnStepPrepare(fn StepPrepareFn) { c.hook(HookStepPrepare, fn) }
func (c *Context) WrapModel(fn ModelWrapFn)       { c.hook(HookModelWrap, fn) }
func (c *Context) OnModelAfter(fn ModelAfterFn)   { c.hook(HookModelAfter, fn) }
func (c *Context) WrapTool(fn ToolWrapFn)         { c.hook(HookToolWrap, fn) }
func (c *Context) OnAgentEnd(fn AgentEndFn)       { c.hook(HookAgentEnd, fn) }
func (c *Context) OnEvent(fn EventFn)             { c.hook(HookObserve, fn) }

// On registers a legacy tool hook (HookToolBefore / HookToolAfter).
func (c *Context) On(kind string, fn func(*HookEvent)) { c.hook(kind, fn) }

// ---- Snapshot: an immutable view taken at the start of each run ----

type Snapshot struct {
	Services map[string]any
	Tools    []Tool
	Skills   []Skill
	hooks    map[string][]any
}

func (c *Context) Snapshot() *Snapshot {
	s := &Snapshot{Services: map[string]any{}, hooks: map[string][]any{}}
	// Parent registries first (Isolate), so the child overrides services.
	var chain []*registry
	for r := c.reg; r != nil; r = r.parent {
		chain = append([]*registry{r}, chain...)
	}
	all := map[string][]hookEntry{}
	for i, r := range chain {
		own := i == len(chain)-1
		r.mu.RLock()
		for k, e := range r.services {
			s.Services[k] = e.v
		}
		if own {
			for _, e := range r.tools {
				s.Tools = append(s.Tools, e.v)
			}
			for _, e := range r.skills {
				s.Skills = append(s.Skills, e.v)
			}
		}
		for k, hs := range r.hooks {
			for _, h := range hs {
				if own || h.trusted { // policy/audit hooks follow subagents
					all[k] = append(all[k], h)
				}
			}
		}
		r.mu.RUnlock()
	}
	sort.Slice(s.Tools, func(i, j int) bool { return s.Tools[i].Name < s.Tools[j].Name })
	sort.Slice(s.Skills, func(i, j int) bool { return s.Skills[i].Name < s.Skills[j].Name })
	for k, hs := range all {
		sort.SliceStable(hs, func(i, j int) bool {
			if hs[i].prio != hs[j].prio {
				return hs[i].prio > hs[j].prio
			}
			if hs[i].order != hs[j].order {
				return hs[i].order < hs[j].order
			}
			return hs[i].id < hs[j].id
		})
		for _, h := range hs {
			s.hooks[k] = append(s.hooks[k], h.fn)
		}
	}
	return s
}

// Tool finds a tool by name.
func (s *Snapshot) Tool(name string) *Tool {
	for i := range s.Tools {
		if s.Tools[i].Name == name {
			return &s.Tools[i]
		}
	}
	return nil
}

func (s *Snapshot) AgentStart(ctx context.Context, st *RunState) error {
	for _, f := range s.hooks[HookAgentStart] {
		if err := f.(AgentStartFn)(ctx, st); err != nil {
			return err
		}
	}
	return nil
}

func (s *Snapshot) PrepareStep(ctx context.Context, sc *StepContext) error {
	for _, f := range s.hooks[HookStepPrepare] {
		if err := f.(StepPrepareFn)(ctx, sc); err != nil {
			return err
		}
	}
	return nil
}

func (s *Snapshot) ModelAfter(ctx context.Context, sc *StepContext, resp *ModelResponse) error {
	for _, f := range s.hooks[HookModelAfter] {
		if err := f.(ModelAfterFn)(ctx, sc, resp); err != nil {
			return err
		}
	}
	return nil
}

func (s *Snapshot) AgentEnd(ctx context.Context, st *RunState, res *RunResult) error {
	for _, f := range s.hooks[HookAgentEnd] {
		if err := f.(AgentEndFn)(ctx, st, res); err != nil {
			return err
		}
	}
	return nil
}

// ModelChain wraps base with every model/wrap hook; the highest
// priority hook is the outermost.
func (s *Snapshot) ModelChain(base ModelHandler) ModelHandler {
	h := base
	ws := s.hooks[HookModelWrap]
	for i := len(ws) - 1; i >= 0; i-- {
		w, next := ws[i].(ModelWrapFn), h
		h = func(ctx context.Context, req *ModelRequest) (*ModelResponse, error) { return w(ctx, req, next) }
	}
	return h
}

// ToolChain wraps base with every tool/wrap hook.
func (s *Snapshot) ToolChain(base ToolHandler) ToolHandler {
	h := base
	ws := s.hooks[HookToolWrap]
	for i := len(ws) - 1; i >= 0; i-- {
		w, next := ws[i].(ToolWrapFn), h
		h = func(ctx context.Context, req *ToolRequest) (*ToolResult, error) { return w(ctx, req, next) }
	}
	return h
}

// Emit runs the legacy tool/before|after hooks.
func (s *Snapshot) Emit(kind string, e *HookEvent) {
	for _, f := range s.hooks[kind] {
		f.(func(*HookEvent))(e)
	}
}

// Publish sends an Event to every event observer.
func (s *Snapshot) Publish(e Event) {
	for _, f := range s.hooks[HookObserve] {
		f.(EventFn)(e)
	}
}

// WithTools returns a copy of the snapshot exposing only tools (hooks and
// services are kept). Loops use it to run subagents with a tool subset.
func (s *Snapshot) WithTools(tools []Tool, skills []Skill) *Snapshot {
	cp := *s
	cp.Tools = tools
	cp.Skills = skills
	return &cp
}

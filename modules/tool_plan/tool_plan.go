package tool_plan

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Gives the model a task list it keeps up to date: plan_write sets the
// tasks, plan_update changes one task's status, plan_read shows the list.
// The current plan is shown to the model with the newest tool result
// each step (not in the system prompt, so prompt caching keeps working).
//
// Plans are kept per thread (per run without a thread). With a "memory"
// service loaded they survive restarts.
//
// Config: max_tasks (default 30), reminder (default true: show the plan
// each step).
//
// Example:
//
//	kiln.WithModules("tool_plan")
var Requires = []string{"?memory"}

type task struct {
	Title, Status, Note string
}

func Apply(p *sdk.Module) error {
	maxTasks := p.ConfigInt("max_tasks", 30)
	reminder := p.ConfigBool("reminder", true)

	var mu sync.Mutex
	plans := map[string][]task{}
	ns := []string{"plans"}

	keyOf := func(thread, runID string) string {
		if thread != "" {
			return "thread:" + thread
		}
		return "run:" + runID
	}
	store := func() *sdk.Store { s, _ := p.Service("memory").(*sdk.Store); return s }
	load := func(c context.Context, key string) []task {
		mu.Lock()
		t, ok := plans[key]
		mu.Unlock()
		if ok || !strings.HasPrefix(key, "thread:") || store() == nil {
			return t
		}
		it, err := store().Get(c, ns, key)
		if err != nil || it == nil {
			return nil
		}
		raw, _ := it.Value["tasks"].([]any)
		for _, r := range raw {
			m := map[string]any{}
			if x, ok := r.(map[string]any); ok {
				m = x
			}
			s := func(k string) string { v, _ := m[k].(string); return v }
			t = append(t, task{s("title"), s("status"), s("note")})
		}
		mu.Lock()
		plans[key] = t
		mu.Unlock()
		return t
	}
	save := func(c context.Context, key string, t []task) error {
		mu.Lock()
		plans[key] = t
		mu.Unlock()
		if !strings.HasPrefix(key, "thread:") || store() == nil {
			return nil
		}
		var raw []any
		for _, x := range t {
			raw = append(raw, map[string]any{"title": x.Title, "status": x.Status, "note": x.Note})
		}
		return store().Put(c, ns, key, map[string]any{"tasks": raw})
	}
	render := func(t []task) string {
		if len(t) == 0 {
			return "No plan yet."
		}
		var b strings.Builder
		for i, x := range t {
			fmt.Fprintf(&b, "%d. [%s] %s", i+1, x.Status, x.Title)
			if x.Note != "" {
				fmt.Fprintf(&b, " (%s)", x.Note)
			}
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n")
	}

	// The tools' work happens in the WrapTool below, which knows the thread.
	noRun := func(c context.Context, args map[string]any) (string, error) {
		return "", fmt.Errorf("tool_plan: no run")
	}
	p.AddTool(sdk.Tool{Name: "plan_write", Run: noRun,
		Description: "Write your plan for this task as a list of steps; replaces any earlier plan.",
		Schema:      sdk.Object(map[string]any{"tasks": sdk.Array("the steps, in order", sdk.Str("one step"))}, "tasks")})
	p.AddTool(sdk.Tool{Name: "plan_update", Run: noRun,
		Description: "Change one step's status as you work: pending, in_progress, done or blocked.",
		Schema: sdk.Object(map[string]any{
			"step":   sdk.Int("the step number, from 1"),
			"status": sdk.Enum("the new status", "pending", "in_progress", "done", "blocked"),
			"note":   sdk.Str("why, for blocked steps (optional)"),
		}, "step", "status")})
	p.AddTool(sdk.Tool{Name: "plan_read", Run: noRun, Description: "Show the current plan.",
		Schema: sdk.Object(map[string]any{}), Meta: map[string]any{"readonly": true}})

	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		name := req.Call.Name
		if !strings.HasPrefix(name, "plan_") || req.Run == nil {
			return next(c, req)
		}
		key := keyOf(req.Thread, req.Run.ID)
		cur := load(c, key)
		switch name {
		case "plan_write":
			raw, _ := req.Call.Args["tasks"].([]any)
			if len(raw) == 0 || len(raw) > maxTasks {
				return &sdk.ToolResult{Content: fmt.Sprintf("Give between 1 and %d steps.", maxTasks), IsError: true}, nil
			}
			var t []task
			for _, r := range raw {
				t = append(t, task{Title: fmt.Sprint(r), Status: "pending"})
			}
			if err := save(c, key, t); err != nil {
				return nil, err
			}
			return &sdk.ToolResult{Content: "Plan saved:\n" + render(t)}, nil
		case "plan_update":
			i := sdk.ArgInt(req.Call.Args, "step", 0) - 1
			if i < 0 || i >= len(cur) {
				return &sdk.ToolResult{Content: fmt.Sprintf("No step %d; the plan has %d.", i+1, len(cur)), IsError: true}, nil
			}
			next := append([]task(nil), cur...)
			next[i].Status = sdk.ArgString(req.Call.Args, "status")
			next[i].Note = sdk.ArgString(req.Call.Args, "note")
			if err := save(c, key, next); err != nil {
				return nil, err
			}
			return &sdk.ToolResult{Content: render(next)}, nil
		case "plan_read":
			return &sdk.ToolResult{Content: render(cur)}, nil
		}
		return next(c, req)
	})

	if reminder {
		p.BeforeStep(func(c context.Context, sc *sdk.StepContext) error {
			ms := sc.Request.Messages
			if len(ms) == 0 || ms[len(ms)-1].Role != sdk.RoleTool {
				return nil
			}
			t := load(c, keyOf(sc.Run.Thread, sc.Run.ID))
			if len(t) == 0 {
				return nil
			}
			// Only this request gets the reminder; the stored history stays as it was.
			out := append([]sdk.Message(nil), ms...)
			out[len(out)-1].Content += "\n\n<plan>\n" + render(t) + "\n</plan>"
			sc.Request.Messages = out
			return nil
		})
	}

	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		if st.Thread == "" {
			mu.Lock()
			delete(plans, "run:"+st.ID)
			mu.Unlock()
		}
		return nil
	})
	return nil
}

// Module is the compiled form of "tool_plan". modules/std registers it.
var Module = engine.Module{Name: "tool_plan", Requires: Requires, Provides: nil, Permissions: nil, Apply: Apply}

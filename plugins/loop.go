//go:build ignore

package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"eipagent/sdk"
)

// The default agent loop, provided as the "loop" service. Swap it by
// loading another plugin that provides "loop".
//
// Config: max_steps (default 20), parallel_tools (default false).
//
// Example:
//
//	eipagent.WithPlugin("loop", map[string]any{"max_steps": 30, "parallel_tools": true})
func Apply(p *sdk.Plugin) error {
	defMax := p.ConfigInt("max_steps", 20)
	parallel := p.ConfigBool("parallel_tools", false)
	return p.Provide("loop", &sdk.Loop{
		Name: "default",
		Run: func(c context.Context, snap *sdk.Snapshot, st *sdk.RunState) (*sdk.RunResult, error) {
			return run(c, snap, st, defMax, parallel)
		},
	})
}

func resolveProvider(snap *sdk.Snapshot, model string) (*sdk.Provider, string, string, error) {
	prov, id := sdk.SplitModel(model)
	if prov == "" {
		var only []string
		for k := range snap.Services {
			if strings.HasPrefix(k, "model/") {
				only = append(only, k)
			}
		}
		if len(only) != 1 {
			return nil, "", "", fmt.Errorf("model %q has no provider prefix (use provider:model)", model)
		}
		prov = strings.TrimPrefix(only[0], "model/")
	}
	mp, ok := snap.Services["model/"+prov].(*sdk.Provider)
	if !ok {
		return nil, "", "", fmt.Errorf("no provider %q: load an llm plugin that provides llm/%s", prov, prov)
	}
	return mp, prov, id, nil
}

// run always closes the lifecycle it opened: when agent/start succeeded,
// agent/end runs exactly once, even if the run fails (FinishReason "error").
func run(c context.Context, snap *sdk.Snapshot, st *sdk.RunState, defMax int, parallel bool) (*sdk.RunResult, error) {
	if st.Emit == nil {
		st.Emit = func(sdk.Event) {}
	}
	if st.ID == "" {
		st.ID = sdk.NewRunID()
	}
	if err := snap.AgentStart(c, st); err != nil {
		return nil, err
	}
	ended := false
	res, err := steps(c, snap, st, defMax, parallel, &ended)
	if err != nil {
		e := sdk.Event{Type: "error", Text: err.Error()}
		snap.Publish(e)
		st.Emit(e)
		if !ended {
			snap.AgentEnd(c, st, &sdk.RunResult{FinishReason: "error", Messages: st.Messages})
		}
		return nil, err
	}
	return res, nil
}

func steps(c context.Context, snap *sdk.Snapshot, st *sdk.RunState, defMax int, parallel bool, ended *bool) (*sdk.RunResult, error) {
	emit := func(e sdk.Event) {
		snap.Publish(e)
		st.Emit(e)
	}
	maxSteps := st.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defMax
	}
	res := &sdk.RunResult{}

	tools := append([]sdk.Tool{}, snap.Tools...)
	tools = append(tools, st.Tools...)
	findTool := func(name string) *sdk.Tool {
		for i := range tools {
			if tools[i].Name == name {
				return &tools[i]
			}
		}
		return nil
	}

	baseModel := func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
		mp, prov, id, err := resolveProvider(snap, req.Model)
		if err != nil {
			return nil, err
		}
		req.Provider, req.ModelID = prov, id
		return mp.Generate(c, req)
	}
	callModel := snap.ModelChain(baseModel)

	baseTool := func(c context.Context, req *sdk.ToolRequest) (*sdk.ToolResult, error) {
		if req.Tool == nil {
			return &sdk.ToolResult{Content: "error: unknown tool " + req.Call.Name, IsError: true}, nil
		}
		ev := &sdk.HookEvent{Tool: req.Call.Name, Args: req.Call.Args}
		snap.Emit(sdk.HookToolBefore, ev)
		if ev.Veto != "" {
			return &sdk.ToolResult{Content: "blocked: " + ev.Veto, IsError: true}, nil
		}
		out, err := req.Tool.Run(c, req.Call.Args)
		if err != nil {
			return nil, err
		}
		ev.Result = out
		snap.Emit(sdk.HookToolAfter, ev)
		return &sdk.ToolResult{Content: ev.Result}, nil
	}
	callTool := snap.ToolChain(baseTool)

	var mu sync.Mutex
	var fatalErr error
	runOne := func(step int, call sdk.ToolCall, dec *sdk.Decision) (sdk.Message, *sdk.Interrupt) {
		msg := sdk.Message{Role: sdk.RoleTool, ToolCallID: call.ID, ToolName: call.Name}
		if dec != nil {
			switch dec.Type {
			case "reject":
				msg.Content, msg.IsError = "rejected by reviewer: "+dec.Message, true
				return msg, nil
			case "respond":
				msg.Content = dec.Message
				return msg, nil
			case "edit":
				call.Args = dec.Args
			}
		}
		tc := sdk.WithRunInfo(sdk.WithRunState(c, st), sdk.RunInfo{RunID: st.ID, Thread: st.Thread, User: st.User, Roles: st.Roles, Step: step, CallID: call.ID, Model: st.Model})
		req := &sdk.ToolRequest{Call: call, Tool: findTool(call.Name), Thread: st.Thread, User: st.User, Step: step, Decision: dec, Run: st}
		r, err := callTool(tc, req)
		var fatal *sdk.FatalError
		if errors.As(err, &fatal) {
			mu.Lock()
			if fatalErr == nil {
				fatalErr = err
			}
			mu.Unlock()
		}
		if err != nil {
			msg.Content, msg.IsError = "error: "+err.Error(), true
		} else if r.Interrupt != nil {
			return msg, r.Interrupt
		} else {
			msg.Content, msg.IsError = r.Content, r.IsError
		}
		return msg, nil
	}

	// execute runs a batch of calls and appends results to history.
	execute := func(step int, calls []sdk.ToolCall, decisions map[string]*sdk.Decision) []sdk.Interrupt {
		for i := range calls { // events report the arguments that actually run
			if d := decisions[calls[i].ID]; d != nil && d.Type == "edit" && d.Args != nil {
				calls[i].Args = d.Args
			}
		}
		if st.Checkpoint != nil {
			if err := st.Checkpoint(); err != nil {
				fatalErr = err
				return nil
			}
		}
		out := make([]sdk.Message, len(calls))
		ints := make([]*sdk.Interrupt, len(calls))
		if parallel && len(calls) > 1 {
			var wg sync.WaitGroup
			for i := range calls {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					out[i], ints[i] = runOne(step, calls[i], decisions[calls[i].ID])
				}(i)
			}
			wg.Wait()
		} else {
			for i := range calls {
				out[i], ints[i] = runOne(step, calls[i], decisions[calls[i].ID])
			}
		}
		if fatalErr != nil {
			return nil
		}
		// A synchronous approver (CLI prompt, policy service) can decide now.
		if ap, ok := snap.Services["approver"].(*sdk.Approver); ok {
			for i := range calls {
				if ints[i] == nil {
					continue
				}
				in := *ints[i]
				if in.ToolCall == nil {
					cc := calls[i]
					in.ToolCall = &cc
				}
				dec, err := ap.Decide(c, in)
				if err != nil {
					fatalErr = err
					return nil
				}
				if dec != nil {
					out[i], ints[i] = runOne(step, calls[i], dec)
				}
			}
		}
		var pending []sdk.Interrupt
		var results []sdk.Message
		for i := range calls {
			if ints[i] != nil {
				in := *ints[i]
				if in.ID == "" {
					in.ID = calls[i].ID
				}
				if in.ToolCall == nil {
					cc := calls[i]
					in.ToolCall = &cc
				}
				pending = append(pending, in)
				emit(sdk.Event{Type: "interrupt", Step: step, ToolCall: in.ToolCall, Text: in.Message})
				continue
			}
			results = append(results, out[i])
			st.Messages = append(st.Messages, out[i])
			emit(sdk.Event{Type: "tool_result", Step: step, ToolCall: &calls[i], Result: out[i].Content})
		}
		if n := len(res.Steps); n > 0 {
			res.Steps[n-1].ToolResults = append(res.Steps[n-1].ToolResults, results...)
		}
		return pending
	}

	finish := func(reason string) (*sdk.RunResult, error) {
		res.FinishReason = reason
		res.Text = sdk.LastText(st.Messages)
		res.Messages = st.Messages
		*ended = true
		if err := snap.AgentEnd(c, st, res); err != nil {
			return nil, err
		}
		emit(sdk.Event{Type: "finish", Text: res.Text, Usage: res.Usage, Data: map[string]any{"reason": reason}})
		return res, nil
	}

	// Resume: run the calls that were waiting for a human decision.
	if len(st.Pending) > 0 {
		// Partial resume: decided calls run now; undecided ones stay
		// pending, and the model continues only once every call is answered.
		decisions := map[string]*sdk.Decision{}
		var calls []sdk.ToolCall
		var waiting []sdk.Interrupt
		for _, mp := range st.Pending {
			var dec *sdk.Decision
			for j := range st.Decisions {
				if st.Decisions[j].InterruptID == mp.ID {
					dec = &st.Decisions[j]
				}
			}
			if dec == nil {
				waiting = append(waiting, mp)
				continue
			}
			if mp.ToolCall != nil {
				calls = append(calls, *mp.ToolCall)
				decisions[mp.ToolCall.ID] = dec
			}
		}
		if len(calls) == 0 {
			return nil, fmt.Errorf("resume: no decision matches a pending interrupt")
		}
		st.Pending = nil
		st.Decisions = nil
		pending := append(waiting, execute(0, calls, decisions)...)
		if fatalErr != nil {
			return nil, fatalErr
		}
		if len(pending) > 0 {
			st.Pending = pending
			res.Interrupts = pending
			return finish("interrupted")
		}
	}

	for step := 0; step < maxSteps; step++ {
		var sys strings.Builder
		sys.WriteString(st.System)
		for _, s := range snap.Skills {
			if s.Prompt != "" {
				sys.WriteString("\n\n" + s.Prompt)
			}
		}
		req := &sdk.ModelRequest{
			Model: st.Model, System: strings.TrimSpace(sys.String()), Messages: st.Messages, Tools: tools,
			ToolChoice: st.ToolChoice, ResponseSchema: st.ResponseSchema, ResponseName: st.ResponseName,
			Temperature: st.Temperature, MaxTokens: st.MaxTokens, Thread: st.Thread, User: st.User, Step: step, Run: st,
		}
		sc := &sdk.StepContext{Run: st, Step: step, Request: req}
		if err := snap.PrepareStep(c, sc); err != nil {
			return nil, err
		}
		emit(sdk.Event{Type: "step_start", Step: step})
		stepNo := step
		if st.Streaming {
			req.Stream = func(ch sdk.Chunk) {
				switch ch.Type {
				case "text":
					emit(sdk.Event{Type: "text", Step: stepNo, Text: ch.Text})
				case "reasoning":
					emit(sdk.Event{Type: "reasoning", Step: stepNo, Text: ch.Text})
				}
			}
		}
		resp, err := callModel(c, req)
		if err != nil {
			return nil, err
		}
		if resp == nil {
			return nil, errors.New("model returned no response")
		}
		if err := snap.ModelAfter(c, sc, resp); err != nil {
			return nil, err
		}
		msg := resp.Message
		msg.Role = sdk.RoleAssistant
		for i := range msg.ToolCalls {
			if msg.ToolCalls[i].ID == "" {
				msg.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d_%d", len(st.Messages), step, i)
			}
			if msg.ToolCalls[i].Args == nil {
				msg.ToolCalls[i].Args = map[string]any{}
			}
		}
		st.Messages = append(st.Messages, msg)
		res.Usage = res.Usage.Add(resp.Usage)
		res.Steps = append(res.Steps, sdk.Step{Response: msg, Usage: resp.Usage, Finish: resp.FinishReason})
		emit(sdk.Event{Type: "step_end", Step: step, Usage: resp.Usage})
		if sc.Jump == "model" {
			continue
		}
		if sc.Jump == "end" || len(msg.ToolCalls) == 0 {
			return finish("stop")
		}
		for i := range msg.ToolCalls {
			emit(sdk.Event{Type: "tool_call", Step: step, ToolCall: &msg.ToolCalls[i]})
		}
		pending := execute(step, msg.ToolCalls, map[string]*sdk.Decision{})
		if fatalErr != nil {
			return nil, fatalErr
		}
		if len(pending) > 0 {
			st.Pending = pending
			res.Interrupts = pending
			return finish("interrupted")
		}
		for _, call := range msg.ToolCalls {
			for _, stop := range st.StopOnTools {
				if call.Name == stop {
					return finish("tool:" + stop)
				}
			}
		}
	}
	return finish("max_steps")
}

//go:build ignore

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"eipagent/sdk"
)

// Writes a trajectory trace: one JSON line per span (run, model call,
// tool call) with timing and hierarchy. View it with `eip trace view`.
//
// Config: path (".eipagent/trace.jsonl"), content (include prompts and
// results, default false). Secret values are redacted.
var Permissions = []string{"fs:read", "fs:write"}

func Apply(p *sdk.Plugin) error {
	logPath := p.ConfigString("path", ".eipagent/trace.jsonl")
	content := p.ConfigBool("content", false)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	p.OnUnload(func() { f.Close() })
	var mu sync.Mutex
	seq := 0
	runs := map[*sdk.RunState]string{}
	starts := map[string]time.Time{}
	newID := func() string {
		mu.Lock()
		defer mu.Unlock()
		seq++
		return fmt.Sprintf("%d-%d", time.Now().UnixNano()%1e9, seq)
	}
	write := func(trace, id, parent, kind, name string, start time.Time, attrs map[string]any) {
		attrs = sdk.RedactMap(attrs)
		b, _ := json.Marshal(map[string]any{"trace": trace, "id": id, "parent": parent, "kind": kind, "name": name,
			"start": start.UTC().Format(time.RFC3339Nano), "ms": time.Since(start).Milliseconds(), "attrs": attrs})
		mu.Lock()
		f.Write(append(b, '\n'))
		mu.Unlock()
	}
	runOf := func(st *sdk.RunState) string {
		mu.Lock()
		defer mu.Unlock()
		return runs[st]
	}
	p.OnRunStart(func(c context.Context, st *sdk.RunState) error {
		id := newID()
		mu.Lock()
		runs[st] = id
		starts[id] = time.Now()
		mu.Unlock()
		return nil
	})
	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		id := runs[st]
		start := starts[id]
		delete(runs, st)
		delete(starts, id)
		mu.Unlock()
		attrs := map[string]any{"run": st.ID, "thread": st.Thread, "user": st.User, "model": st.Model, "steps": len(res.Steps),
			"input_tokens": res.Usage.InputTokens, "output_tokens": res.Usage.OutputTokens, "finish": res.FinishReason}
		if content {
			attrs["text"] = res.Text
		}
		write(id, id, "", "run", "run "+st.Thread, start, attrs)
		return nil
	})
	p.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		t := time.Now()
		resp, err := next(c, req)
		run := runOf(req.Run)
		attrs := map[string]any{"step": req.Step, "messages": len(req.Messages), "tools": len(req.Tools)}
		if err != nil {
			attrs["error"] = err.Error()
		} else {
			attrs["input_tokens"], attrs["output_tokens"], attrs["tool_calls"] = resp.Usage.InputTokens, resp.Usage.OutputTokens, len(resp.Message.ToolCalls)
			if content {
				attrs["reply"] = resp.Message.Content
			}
		}
		write(run, newID(), run, "model", req.Model, t, attrs)
		return resp, err
	})
	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		t := time.Now()
		r, err := next(c, req)
		run := runOf(req.Run)
		attrs := map[string]any{"step": req.Step, "call_id": req.Call.ID}
		if req.Tool != nil {
			attrs["plugin"] = req.Tool.Plugin
		}
		if content {
			attrs["args"] = req.Call.Args
		}
		switch {
		case err != nil:
			attrs["error"] = err.Error()
		case r.Interrupt != nil:
			attrs["interrupted"] = true
		case r.IsError:
			attrs["error"] = r.Content
		case content:
			attrs["result"] = r.Content
		}
		write(run, newID(), run, "tool", req.Call.Name, t, attrs)
		return r, err
	})
	return nil
}

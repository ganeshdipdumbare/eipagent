//go:build ignore

package plugin

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"eipagent/sdk"
)

// Append-only, hash-chained audit log (JSON lines), provided as "audit".
// Each line is {"body": {...}, "prev": "<hash>", "hash": sha256(prev + body)},
// so removing or editing any line breaks the chain (eip audit verify).
//
// Records: plugin lifecycle (from the core), every model call (model,
// tokens, user, thread), every tool call (name, args, outcome, approval
// decision, duration), interrupts and policy denials.
//
// Config: path (".eipagent/audit.jsonl"), args (log tool args, true),
// results (log tool results, false).
var Permissions = []string{"fs:read", "fs:write"}

func Apply(ctx *sdk.Context) error {
	ctx.SetPriority(sdk.PriorityReserved + 1) // outermost when locked: sees everything
	p := ctx.ConfigString("path", ".eipagent/audit.jsonl")
	logArgs := ctx.ConfigBool("args", true)
	logResults := ctx.ConfigBool("results", false)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	prev := ""
	seq := 0
	if f, err := os.Open(p); err == nil { // continue an existing chain
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		for sc.Scan() {
			var line struct {
				Hash string `json:"hash"`
			}
			if json.Unmarshal(sc.Bytes(), &line) == nil {
				prev = line.Hash
				seq++
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ctx.OnDispose(func() { f.Close() })
	var mu sync.Mutex
	record := func(kind string, data map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		seq++
		body, _ := json.Marshal(map[string]any{"seq": seq, "ts": time.Now().UTC().Format(time.RFC3339Nano), "type": kind, "data": data})
		h := sha256.Sum256(append([]byte(prev), body...))
		hash := hex.EncodeToString(h[:])
		line := `{"body":` + string(body) + `,"prev":"` + prev + `","hash":"` + hash + `"}` + "\n"
		f.WriteString(line)
		f.Sync()
		prev = hash
	}

	ctx.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		t := time.Now()
		resp, err := next(c, req)
		d := map[string]any{"model": req.Model, "user": req.User, "thread": req.Thread, "step": req.Step, "ms": time.Since(t).Milliseconds()}
		if err != nil {
			d["error"] = err.Error()
		} else {
			d["input_tokens"], d["output_tokens"], d["tool_calls"] = resp.Usage.InputTokens, resp.Usage.OutputTokens, len(resp.Message.ToolCalls)
		}
		record("model.call", d)
		return resp, err
	})
	ctx.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		t := time.Now()
		r, err := next(c, req)
		d := map[string]any{"tool": req.Call.Name, "call_id": req.Call.ID, "user": req.User, "thread": req.Thread, "ms": time.Since(t).Milliseconds()}
		if logArgs {
			d["args"] = req.Call.Args
		}
		if req.Decision != nil {
			d["decision"] = req.Decision.Type
			d["decided_by"] = req.Decision.By
		}
		switch {
		case err != nil:
			d["outcome"], d["error"] = "error", err.Error()
		case r.Interrupt != nil:
			d["outcome"] = "interrupted"
		case r.IsError:
			d["outcome"] = "failed"
			d["result"] = r.Content
		default:
			d["outcome"] = "ok"
			if logResults {
				d["result"] = r.Content
			}
		}
		record("tool.call", d)
		return r, err
	})
	return ctx.Provide("audit", &sdk.Audit{Record: record})
}

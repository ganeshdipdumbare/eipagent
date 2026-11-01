//go:build ignore

package plugin

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// Records: plugin lifecycle (from the core), secret access (from the
// core), every model call (model, tokens, user, thread), every tool call
// (name, args, outcome, approval decision, duration), interrupts and
// policy denials. Every record has the standard fields of an audit
// event (see SECURITY.md): run, thread, user, plugin, tool, provider...
// Secret values are redacted from everything written.
//
// Each record is also handed to every "audit/sink/*" service (for
// example audit_http), so a control plane gets the same chained stream.
//
// The chain detects edits, but whoever can rewrite the whole file can
// recompute it. With key (a secret://), each line also carries an
// HMAC-SHA256 "mac" of its hash, checked by audit.VerifyKeyed; without
// the key a rewritten log can't produce valid macs.
//
// Config: path (".eipagent/audit.jsonl"; "-" keeps no local file),
// args (log tool args, true), results (log tool results, false),
// key (secret://NAME, optional).
var Permissions = []string{"fs:read", "fs:write"}

var Requires = []string{"?secrets"} // only for key

func Apply(p *sdk.Plugin) error {
	p.SetPriority(sdk.PriorityReserved + 1) // outermost when locked: sees everything
	logPath := p.ConfigString("path", ".eipagent/audit.jsonl")
	logArgs := p.ConfigBool("args", true)
	logResults := p.ConfigBool("results", false)
	var chainKey []byte
	if ref := p.ConfigString("key", ""); ref != "" {
		k, err := p.Secret(ref)
		if err != nil || k == "" {
			return fmt.Errorf("audit_file: key: %v", err)
		}
		chainKey = []byte(k)
	}
	prev := ""
	seq := 0
	var f *os.File
	if logPath != "-" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			return err
		}
		if old, err := os.Open(logPath); err == nil { // continue an existing chain
			sc := bufio.NewScanner(old)
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
			old.Close()
		}
		nf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		f = nf
		p.OnUnload(func() { f.Close() })
	}
	var mu sync.Mutex
	lastErr := ""
	record := func(kind string, data map[string]any) {
		data = sdk.RedactMap(data)
		mu.Lock()
		seq++
		body, _ := json.Marshal(map[string]any{"seq": seq, "ts": time.Now().UTC().Format(time.RFC3339Nano), "type": kind, "data": data})
		h := sha256.Sum256(append([]byte(prev), body...))
		hash := hex.EncodeToString(h[:])
		mac := ""
		if len(chainKey) > 0 {
			m := hmac.New(sha256.New, chainKey)
			m.Write([]byte(hash))
			mac = `,"mac":"` + hex.EncodeToString(m.Sum(nil)) + `"`
		}
		line := []byte(`{"body":` + string(body) + `,"prev":"` + prev + `","hash":"` + hash + `"` + mac + `}` + "\n")
		if f != nil {
			_, werr := f.Write(line)
			if werr == nil {
				werr = f.Sync()
			}
			if werr != nil {
				// Never silent: the record still goes to the sinks, and the
				// failure is logged (once per distinct error).
				if msg := werr.Error(); msg != lastErr {
					lastErr = msg
					p.Logf("audit_file: cannot write %s: %v (records are not reaching the local log)", logPath, werr)
				}
			} else {
				lastErr = ""
			}
		}
		prev = hash
		for _, name := range p.Services("audit/sink/") { // in chain order; sinks only enqueue
			if s, ok := p.Service(name).(*sdk.AuditSink); ok && s.Send != nil {
				s.Send(line)
			}
		}
		mu.Unlock()
	}
	// runFields are the standard fields every model / tool record carries.
	runFields := func(st *sdk.RunState, thread, user string) map[string]any {
		d := map[string]any{"thread": thread, "user": user, "writer": "audit_file"}
		if st != nil {
			d["run"] = st.ID
		}
		return d
	}

	p.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		t := time.Now()
		resp, err := next(c, req)
		d := runFields(req.Run, req.Thread, req.User)
		provider, _ := sdk.SplitModel(req.Model)
		d["model"], d["provider"], d["step"], d["ms"] = req.Model, provider, req.Step, time.Since(t).Milliseconds()
		if err != nil {
			d["error"] = err.Error()
		} else {
			d["input_tokens"], d["output_tokens"], d["tool_calls"] = resp.Usage.InputTokens, resp.Usage.OutputTokens, len(resp.Message.ToolCalls)
		}
		record("model.call", d)
		return resp, err
	})
	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		t := time.Now()
		r, err := next(c, req)
		d := runFields(req.Run, req.Thread, req.User)
		d["tool"], d["call_id"], d["step"], d["ms"] = req.Call.Name, req.Call.ID, req.Step, time.Since(t).Milliseconds()
		if req.Tool != nil {
			d["plugin"] = req.Tool.Plugin
		}
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
	return p.Provide("audit", &sdk.Audit{Record: record})
}

package audit_file

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Append-only, hash-chained audit log (JSON lines), provided as "audit".
// Each line is {"body": {...}, "prev": "<hash>", "hash": sha256(prev + body)},
// so removing or editing any line breaks the chain (kiln audit verify).
//
// Records: module lifecycle (from the core), secret access (from the
// core), every model call (model, tokens, user, thread), every tool call
// (name, args, outcome, approval decision, duration), interrupts and
// policy denials. Every record has the standard fields of an audit
// event (see SECURITY.md): run, thread, user, module, tool, provider...
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
// Config: path (".kiln/audit.jsonl"; "-" keeps no local file),
// args (log tool args, true), results (log tool results, false),
// key (secret://NAME, optional).
//
// The log lives in the directory of path (granted through the fs:read and
// fs:write capabilities). That directory must already exist: the module
// does not create it.
//
// Example:
//
//	audit_file.New(map[string]any{
//		"path":    ".kiln/audit.jsonl",
//		"results": true,                      // also record tool results
//		"key":     "secret://AUDIT_HMAC_KEY", // sign each line
//	})
//	// Shortcut: kiln.Audit(".kiln/audit.jsonl")
var Permissions = []string{"fs:read", "fs:write"}

var Requires = []string{"?secrets"} // only for key

func Apply(p *sdk.Scope) error {
	p.SetPriority(sdk.PriorityReserved + 1) // outermost when locked: sees everything
	logPath := p.ConfigString("path", ".kiln/audit.jsonl")
	logArgs := p.ConfigBool("args", true)
	logResults := p.ConfigBool("results", false)
	var chainKey []byte
	if ref := p.ConfigString("key", ""); ref != "" {
		k, err := p.SecretValue(ref)
		if err != nil || k == "" {
			return fmt.Errorf("audit_file: key: %v", err)
		}
		chainKey = []byte(k)
	}
	prev := ""
	seq := 0
	var f sdk.AppendFile
	var d *sdk.Dir
	base := filepath.Base(logPath)
	if logPath != "-" {
		var err error
		if d, err = p.Dir(filepath.Dir(logPath), true); err != nil {
			return err
		}
		p.OnClose(func() { d.Close() })
		if old, err := d.Open(base); err == nil { // continue an existing chain
			// A partial trailing line (a crash mid-write) can't be
			// continued safely: appending would fuse it with the next
			// record. Refuse rather than silently truncate an audit log.
			if st, serr := old.Stat(); serr == nil && st.Size() > 0 {
				last := make([]byte, 1)
				if _, rerr := old.ReadAt(last, st.Size()-1); rerr != nil {
					old.Close()
					return fmt.Errorf("audit_file: reading %s: %v", logPath, rerr)
				}
				if last[0] != '\n' {
					old.Close()
					return fmt.Errorf("audit_file: %s ends in a partial line; refusing to continue the chain", logPath)
				}
			}
			sc := bufio.NewScanner(old)
			sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
			line := 0
			for sc.Scan() {
				line++
				var rec struct {
					Hash string `json:"hash"`
					Body struct {
						Seq int `json:"seq"`
					} `json:"body"`
				}
				if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
					old.Close()
					return fmt.Errorf("audit_file: %s line %d is not valid JSON; refusing to continue the chain", logPath, line)
				}
				if rec.Body.Seq != line {
					old.Close()
					return fmt.Errorf("audit_file: %s line %d has seq %d; refusing to continue a broken chain", logPath, line, rec.Body.Seq)
				}
				prev = rec.Hash
				seq = line
			}
			if err := sc.Err(); err != nil {
				old.Close()
				return fmt.Errorf("audit_file: reading %s: %v", logPath, err)
			}
			old.Close()
		}
		nf, err := d.AppendFile(base, 0o600)
		if err != nil {
			return err
		}
		f = nf
		p.OnClose(func() { f.Close() })
	}
	var mu sync.Mutex
	lastErr := ""
	headMAC := func(seq int, hash string) string {
		m := hmac.New(sha256.New, chainKey)
		m.Write([]byte(strconv.Itoa(seq)))
		m.Write([]byte("|"))
		m.Write([]byte(hash))
		return hex.EncodeToString(m.Sum(nil))
	}
	// writeHead keeps <log>.head in step with the last line, so a
	// truncated tail is detectable (kiln audit verify / audit.VerifyKeyed).
	writeHead := func(seq int, hash string) {
		if f == nil {
			return
		}
		mac := ""
		if len(chainKey) > 0 {
			mac = headMAC(seq, hash)
		}
		b, _ := json.Marshal(struct {
			Seq  int    `json:"seq"`
			Hash string `json:"hash"`
			Mac  string `json:"mac"`
		}{seq, hash, mac})
		b = append(b, '\n')
		// writeHead runs under mu, so a time-based name is unique enough.
		name := fmt.Sprintf(".head-%d", time.Now().UnixNano())
		if err := d.WriteFile(name, b, 0o600); err != nil {
			d.Remove(name)
			p.Logf("audit_file: cannot write the head checkpoint: %v", err)
			return
		}
		if err := d.Rename(name, base+".head"); err != nil {
			d.Remove(name)
			p.Logf("audit_file: cannot write the head checkpoint: %v", err)
		}
	}
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
			size, haveSize := int64(0), false
			if st, serr := f.Stat(); serr == nil {
				size, haveSize = st.Size(), true
			}
			n, werr := f.Write(line)
			if werr == nil && n != len(line) {
				werr = fmt.Errorf("short write: %d of %d bytes", n, len(line))
			}
			if werr == nil {
				werr = f.Sync()
			}
			if werr != nil {
				// Roll back any partial bytes and keep the in-memory chain on
				// the last persisted record: a transient disk error must not
				// leave a seq/prev gap that blocks the next startup.
				if haveSize {
					if terr := f.Truncate(size); terr != nil {
						p.Logf("audit_file: cannot roll back %s: %v", logPath, terr)
					}
				}
				seq--
				if msg := werr.Error(); msg != lastErr {
					lastErr = msg
					p.Logf("audit_file: cannot write %s: %v (records are not reaching the local log)", logPath, werr)
				}
				mu.Unlock()
				return
			}
			lastErr = ""
			writeHead(seq, hash)
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
			d["module"] = req.Tool.Module
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

// Module is the compiled form of "audit_file". modules/std registers it.
var Module = engine.Module{Name: "audit_file", Requires: Requires, Provides: []string{"audit"}, Permissions: Permissions, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

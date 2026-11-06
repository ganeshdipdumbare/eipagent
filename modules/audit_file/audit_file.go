package audit_file

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
		old, oerr := d.Open(base)
		switch {
		case oerr == nil: // continue an existing chain
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
					Body json.RawMessage `json:"body"`
					Prev string          `json:"prev"`
					Hash string          `json:"hash"`
				}
				if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
					old.Close()
					return fmt.Errorf("audit_file: %s line %d is not valid JSON; refusing to continue the chain", logPath, line)
				}
				var body struct {
					Seq int `json:"seq"`
				}
				if err := json.Unmarshal(rec.Body, &body); err != nil {
					old.Close()
					return fmt.Errorf("audit_file: %s line %d has no sequence; refusing to continue the chain", logPath, line)
				}
				if body.Seq != line {
					old.Close()
					return fmt.Errorf("audit_file: %s line %d has seq %d; refusing to continue a broken chain", logPath, line, body.Seq)
				}
				if rec.Prev != prev {
					old.Close()
					return fmt.Errorf("audit_file: %s line %d does not follow the previous hash; refusing to continue a broken chain", logPath, line)
				}
				if !isHash(rec.Hash) {
					old.Close()
					return fmt.Errorf("audit_file: %s line %d has an invalid hash; refusing to continue the chain", logPath, line)
				}
				sum := sha256.Sum256(append([]byte(prev), rec.Body...))
				if hex.EncodeToString(sum[:]) != rec.Hash {
					old.Close()
					return fmt.Errorf("audit_file: %s line %d fails its hash; refusing to continue a forged chain", logPath, line)
				}
				prev = rec.Hash
				seq = line
			}
			if err := sc.Err(); err != nil {
				old.Close()
				return fmt.Errorf("audit_file: reading %s: %v", logPath, err)
			}
			old.Close()
			// A keyed log must match its signed head before we append:
			// otherwise a truncated tail is silently re-signed below.
			if len(chainKey) > 0 && seq > 0 {
				hb, herr := d.ReadFile(base + ".head")
				if errors.Is(herr, fs.ErrNotExist) {
					return fmt.Errorf("audit_file: %s has no signed head; refusing to append to a keyed log whose tail cannot be checked", logPath)
				}
				if herr != nil {
					return fmt.Errorf("audit_file: reading %s.head: %v", logPath, herr)
				}
				var head struct {
					Seq  int    `json:"seq"`
					Hash string `json:"hash"`
					MAC  string `json:"mac"`
				}
				if err := json.Unmarshal(hb, &head); err != nil {
					return fmt.Errorf("audit_file: %s.head: %v", logPath, err)
				}
				if head.Seq != seq || head.Hash != prev || !hmac.Equal([]byte(head.MAC), []byte(headMAC(chainKey, head.Seq, head.Hash))) {
					return fmt.Errorf("audit_file: %s does not match %s.head; refusing to append to a truncated or forged log", logPath, logPath)
				}
			}
		case errors.Is(oerr, fs.ErrNotExist):
			// A new log. Nothing to continue.
		default:
			return fmt.Errorf("audit_file: opening %s: %w", logPath, oerr)
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
	// writeHead keeps <log>.head in step with the last line, so a
	// truncated tail is detectable (kiln audit verify / audit.VerifyKeyed).
	writeHead := func(seq int, hash string) {
		if f == nil {
			return
		}
		mac := ""
		if len(chainKey) > 0 {
			mac = headMAC(chainKey, seq, hash)
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
		// Snapshot the arguments before next runs: a tool receives the same
		// map and could otherwise rewrite what the audit record says it was
		// asked to do.
		var args map[string]any
		if logArgs {
			args = cloneMap(req.Call.Args)
		}
		decisionType, decidedBy := "", ""
		if req.Decision != nil {
			decisionType, decidedBy = req.Decision.Type, req.Decision.By
		}
		r, err := next(c, req)
		d := runFields(req.Run, req.Thread, req.User)
		d["tool"], d["call_id"], d["step"], d["ms"] = req.Call.Name, req.Call.ID, req.Step, time.Since(t).Milliseconds()
		if req.Tool != nil {
			d["module"] = req.Tool.Module
		}
		if logArgs {
			d["args"] = args
		}
		if decisionType != "" {
			d["decision"], d["decided_by"] = decisionType, decidedBy
		}
		switch {
		case err != nil:
			d["outcome"], d["error"] = "error", err.Error()
		case r != nil && r.Interrupt != nil:
			d["outcome"] = "interrupted"
		case r != nil && r.IsError:
			d["outcome"] = "failed"
			if logResults {
				d["result"] = r.Content
			}
		default:
			d["outcome"] = "ok"
			if logResults && r != nil {
				d["result"] = r.Content
			}
		}
		record("tool.call", d)
		return r, err
	})
	return p.Provide("audit", &sdk.Audit{Record: record})
}

// headMAC must match audit.HeadMAC: HMAC-SHA256 of "seq|hash".
func headMAC(key []byte, seq int, hash string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strconv.Itoa(seq) + "|" + hash))
	return hex.EncodeToString(m.Sum(nil))
}

func isHash(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// cloneMap snapshots tool arguments through their JSON form. A tool gets
// the live map and must not be able to change the audit record after the
// fact.
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return m
	}
	return out
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

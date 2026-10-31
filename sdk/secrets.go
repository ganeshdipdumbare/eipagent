package sdk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version is the eipagent SDK version plugins are checked against
// (the "eipagent" field of a plugin manifest).
const Version = "0.2.0"

// Redaction: every secret value resolved through the "secrets" service
// is remembered here, and audit / trace / log plugins pass what they
// write through Redact, so a secret that ends up in a tool argument,
// model output or error message never reaches those sinks.

const redacted = "[REDACTED]"

var (
	redactMu   sync.RWMutex
	redactVals []string // longest first, so overlapping values redact fully
)

// AddRedaction registers a value that must never appear in logs,
// traces or audit output. Values shorter than 4 bytes are ignored.
func AddRedaction(v string) {
	if len(v) < 4 {
		return
	}
	redactMu.Lock()
	defer redactMu.Unlock()
	for _, x := range redactVals {
		if x == v {
			return
		}
	}
	redactVals = append(redactVals, v)
	sort.Slice(redactVals, func(i, j int) bool { return len(redactVals[i]) > len(redactVals[j]) })
}

// Redact replaces every registered secret value in s.
func Redact(s string) string {
	redactMu.RLock()
	defer redactMu.RUnlock()
	for _, v := range redactVals {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	return s
}

// RedactMap returns a deep copy of m with every string redacted.
func RedactMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = RedactValue(v)
	}
	return out
}

// RedactValue redacts strings inside v (maps, slices, errors).
func RedactValue(v any) any {
	switch x := v.(type) {
	case string:
		return Redact(x)
	case error:
		return Redact(x.Error())
	case map[string]any:
		return RedactMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = RedactValue(e)
		}
		return out
	case []string:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = Redact(e)
		}
		return out
	}
	return v
}

// Granted reads are audited once per plugin and secret every
// grantAuditEvery, or right away when the value changes (a rotation).
// Plugins that resolve secrets per call (as they should) would otherwise
// flood the log, and an audit sink that resolves its own key would feed
// itself. Denials are always recorded.
const grantAuditEvery = 10 * time.Minute

type grantMark struct {
	at  time.Time
	sum [32]byte
}

// auditGrant reports whether a granted read should be recorded now. The
// marks live on the agent's root registry, so two agents in one process
// each audit their own reads.
func (c *Context) auditGrant(caller, name, v string) bool {
	r := c.reg
	for r.parent != nil {
		r = r.parent
	}
	k := caller + "\x00" + name
	sum := sha256.Sum256([]byte(v))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.grants == nil {
		r.grants = map[string]grantMark{}
	}
	m, ok := r.grants[k]
	if ok && m.sum == sum && time.Since(m.at) < grantAuditEvery {
		return false
	}
	r.grants[k] = grantMark{time.Now(), sum}
	return true
}

// boundSecrets is what Service("secrets") hands a plugin: the caller is
// fixed to the plugin's identity (a plugin can't ask as someone else),
// values are registered for redaction and every access is audited.
func (c *Context) boundSecrets(s *Secrets) *Secrets {
	caller := c.PluginName()
	return &Secrets{Get: func(ctx context.Context, _ string, name string) (string, error) {
		v, err := s.Get(ctx, caller, name)
		if err == nil {
			AddRedaction(v)
		}
		if err == nil && !c.auditGrant(caller, name, v) {
			return v, nil // already audited recently
		}
		if a, ok := c.Service("audit").(*Audit); ok && a.Record != nil {
			d := map[string]any{"plugin": caller, "secret": name, "outcome": "granted"}
			if err != nil {
				d["outcome"], d["error"] = "denied", Redact(err.Error())
			}
			a.Record("secret.access", d)
		}
		return v, err
	}}
}

// NewRunID returns a random run id (16 hex chars).
func NewRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

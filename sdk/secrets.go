package sdk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version is the eipagent SDK version plugins are checked against
// (the "eipagent" field of a plugin manifest).
const Version = "0.4.4"

// Redaction: every secret value resolved through the "secrets" service
// is remembered here, and audit / trace / log plugins pass what they
// write through Redact, so a secret that ends up in a tool argument,
// model output or error message never reaches those sinks.

const redacted = "[REDACTED]"

var (
	redactMu          sync.RWMutex
	redactSet         = map[string]struct{}{}
	redactOrder       []string // insertion order, oldest first
	redactSorted      []string // length-desc, rebuilt when the set changes
	redactEvictLogged bool
)

// MinRedactLen is the shortest value that can be redacted: shorter ones
// would blank out ordinary text. Secrets this short are reported as
// unredactable in their secret.access audit record.
const MinRedactLen = 4

// maxRedactions bounds the registry (about 10 forms per secret value).
const maxRedactions = 20000

// AddRedaction registers a value that must never appear in logs,
// traces or audit output, together with its common encodings (JSON
// string escaping, URL query escaping, base64), so a secret that was
// serialized on its way to a sink is caught too. It reports false for
// values shorter than MinRedactLen, which are not registered. Plugins
// can't call it: only the core and compiled code register values.
func AddRedaction(v string) bool {
	if len(v) < MinRedactLen {
		return false
	}
	forms := []string{v}
	if b, err := json.Marshal(v); err == nil {
		forms = append(forms, string(b[1:len(b)-1]))
	}
	forms = append(forms, url.QueryEscape(v), url.PathEscape(v),
		base64.StdEncoding.EncodeToString([]byte(v)), base64.RawStdEncoding.EncodeToString([]byte(v)),
		base64.URLEncoding.EncodeToString([]byte(v)), base64.RawURLEncoding.EncodeToString([]byte(v)),
		hex.EncodeToString([]byte(v)), strings.ToUpper(hex.EncodeToString([]byte(v))))
	redactMu.Lock()
	defer redactMu.Unlock()
	changed := false
	for _, f := range forms {
		if _, ok := redactSet[f]; ok {
			continue
		}
		redactSet[f] = struct{}{}
		redactOrder = append(redactOrder, f)
		changed = true
	}
	if len(redactOrder) > maxRedactions {
		// Bounded: a long-running agent whose secrets keep rotating would
		// otherwise grow this forever. The oldest values go first.
		over := len(redactOrder) - maxRedactions
		if !redactEvictLogged {
			redactEvictLogged = true
			log.Printf("sdk: redaction registry full (%d forms); dropping the oldest secrets", maxRedactions)
		}
		for _, f := range redactOrder[:over] {
			delete(redactSet, f)
		}
		redactOrder = append([]string(nil), redactOrder[over:]...)
		changed = true
	}
	if changed {
		redactSorted = make([]string, len(redactOrder))
		copy(redactSorted, redactOrder)
		sort.SliceStable(redactSorted, func(i, j int) bool { return len(redactSorted[i]) > len(redactSorted[j]) })
	}
	return true
}

// Redact replaces every registered secret value in s.
func Redact(s string) string {
	redactMu.RLock()
	defer redactMu.RUnlock()
	for _, v := range redactSorted {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	return s
}

// RedactMap returns a deep copy of m with every string redacted,
// including keys.
func RedactMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[Redact(k)] = RedactValue(v)
	}
	return out
}

// RedactValue returns a redacted deep copy of v. Strings, errors and
// fmt.Stringers are redacted; maps with string keys and slices of any
// element type are copied element by element (as map[string]any and
// []any); structs are copied through their JSON form. Numbers, bools
// and nil come back unchanged.
func RedactValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
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
	case []byte:
		return Redact(string(x))
	case fmt.Stringer:
		return Redact(x.String())
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.String:
		return Redact(rv.String())
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			k := Redact(fmt.Sprint(it.Key().Interface()))
			out[k] = RedactValue(it.Value().Interface())
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = RedactValue(rv.Index(i).Interface())
		}
		return out
	case reflect.Struct:
		b, err := json.Marshal(rv.Interface())
		if err != nil {
			return Redact(fmt.Sprint(rv.Interface()))
		}
		var m any
		json.Unmarshal(b, &m)
		return RedactValue(m)
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return v
	}
	return Redact(fmt.Sprint(v))
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
func (c *Plugin) auditGrant(caller, name, v string) bool {
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

// rawService looks a service up without the per-plugin binding (for
// records the core itself writes).
func (c *Plugin) rawService(name string) any {
	v, _ := c.reg.service(name)
	return v
}

// boundAudit is what Service("audit") hands a plugin: every record it
// submits is stamped writer="plugin:<name>" (overwriting any writer the
// plugin set), so a plugin can't pass its own records off as the core's
// or another plugin's. Records the core writes carry writer="core".
func (c *Plugin) boundAudit(a *Audit) *Audit {
	writer := "plugin:" + c.PluginName()
	return &Audit{Record: func(kind string, data map[string]any) {
		d := make(map[string]any, len(data)+1)
		for k, v := range data {
			d[k] = v
		}
		d["writer"] = writer
		a.Record(kind, d)
	}}
}

// boundSecrets is what Service("secrets") hands a plugin: the caller is
// fixed to the plugin's identity (a plugin can't ask as someone else),
// values are registered for redaction and every access is audited.
func (c *Plugin) boundSecrets(s *Secrets) *Secrets {
	caller := c.PluginName()
	return &Secrets{Get: func(ctx context.Context, _ string, name string) (string, error) {
		v, err := s.Get(ctx, caller, name)
		redactable := true
		if err == nil {
			redactable = AddRedaction(v)
		}
		if err == nil && !c.auditGrant(caller, name, v) {
			return v, nil // already audited recently
		}
		if a, ok := c.rawService("audit").(*Audit); ok && a.Record != nil {
			d := map[string]any{"plugin": caller, "secret": name, "outcome": "granted", "writer": "core"}
			if !redactable {
				d["redaction"] = "not possible: value shorter than 4 bytes"
			}
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

// SecretLike reports whether an environment variable name looks like a
// credential (*_API_KEY, *_TOKEN, *PASSWORD* ...).
func SecretLike(name string) bool {
	n := strings.ToUpper(name)
	for _, s := range []string{"_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_PASS", "_PASSWD", "_DSN", "_CREDENTIALS", "_CREDENTIAL", "_PAT"} {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	for _, s := range []string{"PASSWORD", "SECRET", "TOKEN", "APIKEY", "API_KEY", "PRIVATE_KEY", "DATABASE_URL"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// Placeholders: inside a WithRunIn sandbox the "secrets" service hands
// out fake tokens of this form. Only the host maps them back to real
// values, and only on requests to the hosts allowed for that secret.

// PlaceholderPrefix starts every placeholder: "eipsec_" + 32 hex chars.
const PlaceholderPrefix = "eipsec_"

const placeholderLen = len(PlaceholderPrefix) + 32

// NewPlaceholder returns a fresh random placeholder.
func NewPlaceholder() string {
	b := make([]byte, 16)
	rand.Read(b)
	return PlaceholderPrefix + hex.EncodeToString(b)
}

// FindPlaceholders returns the distinct placeholders in s, in order.
func FindPlaceholders(s string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; ; {
		j := strings.Index(s[i:], PlaceholderPrefix)
		if j < 0 {
			return out
		}
		i += j
		if i+placeholderLen <= len(s) && isHex(s[i+len(PlaceholderPrefix):i+placeholderLen]) {
			if p := s[i : i+placeholderLen]; !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			i += placeholderLen
		} else {
			i += len(PlaceholderPrefix)
		}
	}
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// ContainsSecret reports whether s holds a value resolved through the
// "secrets" service (in any form Redact catches). Placeholders don't
// count, even where they were registered for redaction.
func ContainsSecret(s string) bool {
	for _, p := range FindPlaceholders(s) {
		s = strings.ReplaceAll(s, p, "")
	}
	return Redact(s) != s
}

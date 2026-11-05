package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ---- Run info carried on context.Context (tools read who/what is running) ----

type runInfoKey struct{}
type runStateKey struct{}

type RunInfo struct {
	RunID  string
	Thread string
	User   string
	Roles  []string
	Step   int
	CallID string
	Model  string
}

func WithRunInfo(ctx context.Context, ri RunInfo) context.Context {
	return context.WithValue(ctx, runInfoKey{}, ri)
}

func RunInfoFrom(ctx context.Context) RunInfo {
	ri, _ := ctx.Value(runInfoKey{}).(RunInfo)
	return ri
}

// WithRunState carries the active RunState on a tool context (the loop
// sets it). Subagents use it to share the parent's budgets and limits.
func WithRunState(ctx context.Context, st *RunState) context.Context {
	return context.WithValue(ctx, runStateKey{}, st)
}

func RunStateFrom(ctx context.Context) *RunState {
	st, _ := ctx.Value(runStateKey{}).(*RunState)
	return st
}

func UserFrom(ctx context.Context) string   { return RunInfoFrom(ctx).User }
func ThreadFrom(ctx context.Context) string { return RunInfoFrom(ctx).Thread }

// ---- JSON Schema shorthands for module authors ----

func Object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		s["required"] = req
	}
	return s
}

func Str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func Int(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func Num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
func Bool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func Enum(desc string, vals ...string) map[string]any {
	e := make([]any, len(vals))
	for i, v := range vals {
		e[i] = v
	}
	return map[string]any{"type": "string", "description": desc, "enum": e}
}
func Array(desc string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": items}
}

// ---- Misc ----

func ToJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// ArgString reads a string argument.
func ArgString(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func ArgInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil {
			return n
		}
	}
	return def
}

// SplitModel splits "provider:model" (no provider = "").
func SplitModel(m string) (provider, model string) {
	if i := strings.Index(m, ":"); i > 0 {
		return m[:i], m[i+1:]
	}
	return "", m
}

// Text returns the concatenated text of the last assistant message.
func LastText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant {
			return msgs[i].Content
		}
	}
	return ""
}

// Secret resolves a config value: "secret://NAME" goes through the
// "secrets" service; anything else is returned as is.
func (c *Module) Secret(ref string) (string, error) {
	if !strings.HasPrefix(ref, "secret://") {
		return ref, nil
	}
	s, ok := c.Service("secrets").(*Secrets)
	if !ok {
		return "", fmt.Errorf("%s: no \"secrets\" service to resolve %s (load secrets_env)", c.Name, ref)
	}
	return s.Get(context.Background(), c.PluginName(), strings.TrimPrefix(ref, "secret://"))
}

// SecretValue is Secret for a value that is known to be a credential.
// Unlike Secret it also registers a literal (non-secret://) value for
// redaction, so a key written directly in config can't appear in logs,
// traces or audit records.
func (c *Module) SecretValue(ref string) (string, error) {
	v, err := c.Secret(ref)
	if err == nil && v != "" {
		AddRedaction(v)
	}
	return v, err
}

// ReadSSE parses a server-sent-events stream, calling fn per event.
func ReadSSE(r io.Reader, fn func(event, data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var event string
	var data []string
	flush := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		d := strings.Join(data, "\n")
		e := event
		event, data = "", nil
		return fn(e, d)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

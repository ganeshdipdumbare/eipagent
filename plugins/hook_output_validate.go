//go:build ignore

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"eipagent/sdk"
)

// Checks structured output (runs with a ResponseSchema) against the
// schema. When the final reply is not valid JSON or does not match, the
// model is asked again with the errors, up to max_retries times.
//
// Validates type, required, properties, additionalProperties: false,
// items, enum, minItems/maxItems and minLength/maxLength.
//
// Config: max_retries (2), on_fail (error | accept, default error).
func Apply(p *sdk.Plugin) error {
	maxRetries := p.ConfigInt("max_retries", 2)
	onFail := p.ConfigString("on_fail", "error")

	// Per-run state: retries used and the correction for the next step.
	var mu sync.Mutex
	tries := map[string]int{}
	fixes := map[string]string{}

	// Hand the correction to the model on the next step (the loop appends
	// the rejected reply after AfterModel, so it can't be added there).
	p.BeforeStep(func(c context.Context, sc *sdk.StepContext) error {
		mu.Lock()
		fix := fixes[sc.Run.ID]
		delete(fixes, sc.Run.ID)
		mu.Unlock()
		if fix == "" {
			return nil
		}
		sc.Run.Messages = append(sc.Run.Messages, sdk.Message{Role: sdk.RoleUser, Content: fix})
		sc.Request.Messages = sc.Run.Messages
		return nil
	})

	p.AfterModel(func(c context.Context, sc *sdk.StepContext, resp *sdk.ModelResponse) error {
		schema := sc.Request.ResponseSchema
		if schema == nil || len(resp.Message.ToolCalls) > 0 {
			return nil
		}
		var v any
		var problems []string
		if err := json.Unmarshal([]byte(stripFence(resp.Message.Content)), &v); err != nil {
			problems = []string{"the reply is not valid JSON: " + err.Error()}
		} else {
			problems = validate(schema, v, "$")
		}
		if len(problems) == 0 {
			return nil
		}
		msg := strings.Join(problems, "; ")
		mu.Lock()
		n := tries[sc.Run.ID]
		if n < maxRetries {
			tries[sc.Run.ID] = n + 1
			fixes[sc.Run.ID] = "Your reply did not match the required JSON Schema:\n- " + strings.Join(problems, "\n- ") +
				"\nReply again with only the corrected JSON."
		}
		mu.Unlock()
		if n >= maxRetries {
			if onFail == "accept" {
				p.Logf("output still invalid after %d retries; accepting: %s", n, msg)
				return nil
			}
			return fmt.Errorf("hook_output_validate: output does not match the schema after %d retries: %s", n, msg)
		}
		sc.Jump = "model"
		p.Logf("invalid structured output, retry %d/%d: %s", n+1, maxRetries, msg)
		return nil
	})

	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		delete(tries, st.ID)
		delete(fixes, st.ID)
		mu.Unlock()
		return nil
	})
	return nil
}

func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// validate checks v against a JSON Schema subset and lists the problems.
func validate(schema map[string]any, v any, at string) []string {
	var out []string
	bad := func(format string, args ...any) { out = append(out, at+": "+fmt.Sprintf(format, args...)) }
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				found = true
			}
		}
		if !found {
			bad("must be one of %v", enum)
		}
	}
	if !typeOK(schema["type"], v) {
		bad("expected %v, got %s", schema["type"], jsonType(v))
		return out
	}
	switch x := v.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		for _, r := range anyStrings(schema["required"]) {
			if _, ok := x[r]; !ok {
				bad("missing required property %q", r)
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k].(map[string]any); ok {
				out = append(out, validate(ps, x[k], at+"."+k)...)
			} else if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
				bad("unexpected property %q", k)
			}
		}
	case []any:
		if n, ok := num(schema["minItems"]); ok && float64(len(x)) < n {
			bad("needs at least %v items", n)
		}
		if n, ok := num(schema["maxItems"]); ok && float64(len(x)) > n {
			bad("allows at most %v items", n)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, e := range x {
				out = append(out, validate(items, e, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	case string:
		if n, ok := num(schema["minLength"]); ok && float64(len([]rune(x))) < n {
			bad("must be at least %v characters", n)
		}
		if n, ok := num(schema["maxLength"]); ok && float64(len([]rune(x))) > n {
			bad("must be at most %v characters", n)
		}
	}
	return out
}

func typeOK(t any, v any) bool {
	switch t := t.(type) {
	case string:
		return t == jsonType(v) || (t == "number" && jsonType(v) == "integer")
	case []any:
		for _, x := range t {
			if typeOK(x, v) {
				return true
			}
		}
		return false
	}
	return true // no type constraint
}

func jsonType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if x == float64(int64(x)) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func anyStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

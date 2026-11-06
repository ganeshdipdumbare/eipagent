package kiln

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

var ErrNoObject = errors.New("kiln: model did not produce a valid object")

// GenerateObject runs the agent and decodes its final answer into T,
// using the provider's native structured output where available. One
// repair round is attempted when the reply doesn't parse.
//
//	type Recipe struct {
//		Name        string   `json:"name"`
//		Ingredients []string `json:"ingredients"`
//	}
//	r, err := kiln.GenerateObject[Recipe](ctx, agent, "A pancake recipe")
func GenerateObject[T any](ctx context.Context, a *Agent, prompt string, run ...Run) (T, error) {
	var zero T
	name := strings.ToLower(reflect.TypeOf(zero).Name())
	if name == "" {
		name = "response"
	}
	schema := SchemaOf(zero)
	wrapped := schema["type"] != "object" // providers want an object at the top
	if wrapped {
		schema = map[string]any{"type": "object", "properties": map[string]any{"value": schema}, "required": []any{"value"}, "additionalProperties": false}
	}
	r := one(run)
	r.Schema, r.SchemaName = schema, name
	capture := r.CaptureResult

	res, err := a.Generate(ctx, prompt, r)
	for attempt := 0; ; attempt++ {
		if err != nil {
			return zero, err
		}
		if capture != nil {
			*capture = res
		}
		out, perr := decodeObject[T](res.Text, wrapped)
		if perr == nil {
			return out, nil
		}
		if attempt >= 1 {
			return zero, fmt.Errorf("%w: %v", ErrNoObject, perr)
		}
		fix := fmt.Sprintf("Your reply was not valid JSON for the required schema (%v). Reply with only the JSON object.", perr)
		next := r
		next.History = append(append([]sdk.Message{}, r.History...), res.Messages...)
		res, err = a.Generate(ctx, fix, next)
	}
}

func decodeObject[T any](text string, wrapped bool) (T, error) {
	var out T
	s := strings.TrimSpace(text)
	if i := strings.Index(s, "```"); i >= 0 { // tolerate fenced replies
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	if i := strings.IndexAny(s, "{["); i > 0 {
		s = s[i:]
	}
	s = strings.TrimSpace(s)
	if wrapped {
		var w struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal([]byte(s), &w); err != nil {
			return out, err
		}
		s = string(w.Value)
	}
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

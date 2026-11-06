package kiln

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Object builds a JSON Schema object from field schemas (sdk.Object, so a
// module can import only kiln).
func Object(props map[string]any, required ...string) map[string]any {
	return sdk.Object(props, required...)
}

// Str is a string field schema with a description.
func Str(desc string) map[string]any { return sdk.Str(desc) }

// ArgString reads a string tool argument.
func ArgString(args map[string]any, key string) string { return sdk.ArgString(args, key) }

// Tool defines a tool from a typed Go function. The argument schema is
// derived from In (see SchemaOf); a string Out is returned as is, any
// other Out is sent to the model as JSON.
//
//	type WeatherIn struct {
//		City string `json:"city" desc:"city name"`
//	}
//	weather := kiln.NewTool("weather", "Get the weather", func(ctx context.Context, in WeatherIn) (string, error) {
//		return "sunny in " + in.City, nil
//	})
func NewTool[In, Out any](name, description string, fn func(ctx context.Context, in In) (Out, error)) sdk.Tool {
	var zero In
	return sdk.Tool{
		Name:        name,
		Description: description,
		Schema:      SchemaOf(zero),
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			var in In
			raw, _ := json.Marshal(args)
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("invalid arguments for %s: %w", name, err)
			}
			out, err := fn(ctx, in)
			if err != nil {
				return "", err
			}
			if s, ok := any(out).(string); ok {
				return s, nil
			}
			b, err := json.Marshal(out)
			return string(b), err
		},
	}
}

// ReadOnly marks a tool as side-effect free (policy, approval and
// parallel execution use it).
func ReadOnly(t sdk.Tool) sdk.Tool { return WithMeta(t, "readonly", true) }

// WithMeta returns t with a metadata key set (e.g. "risk": "high").
func WithMeta(t sdk.Tool, key string, v any) sdk.Tool {
	m := map[string]any{}
	for k, x := range t.Meta {
		m[k] = x
	}
	m[key] = v
	t.Meta = m
	return t
}

package eipagent

import (
	"reflect"
	"strings"
	"time"
)

// SchemaOf derives a JSON Schema from a Go type. Field tags:
//
//	json:"name,omitempty"   name; omitempty or a pointer makes it optional
//	desc:"..."              description
//	enum:"a,b,c"            allowed values
func SchemaOf(v any) map[string]any {
	t := reflect.TypeOf(v)
	if t == nil {
		return map[string]any{}
	}
	return schemaFor(t, map[reflect.Type]bool{})
}

var timeType = reflect.TypeOf(time.Time{})

func schemaFor(t reflect.Type, seen map[reflect.Type]bool) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": schemaFor(t.Elem(), seen)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem(), seen)}
	case reflect.Struct:
		if seen[t] {
			return map[string]any{"type": "object"}
		}
		seen[t] = true
		defer delete(seen, t)
		props := map[string]any{}
		required := []any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if f.Anonymous && name == "" {
				sub := schemaFor(f.Type, seen)
				if p, ok := sub["properties"].(map[string]any); ok {
					for k, v := range p {
						props[k] = v
					}
					if r, ok := sub["required"].([]any); ok {
						required = append(required, r...)
					}
				}
				continue
			}
			if name == "" {
				name = f.Name
			}
			s := schemaFor(f.Type, seen)
			if d := f.Tag.Get("desc"); d != "" {
				s["description"] = d
			}
			if e := f.Tag.Get("enum"); e != "" {
				var vals []any
				for _, v := range strings.Split(e, ",") {
					vals = append(vals, strings.TrimSpace(v))
				}
				s["enum"] = vals
			}
			props[name] = s
			if !strings.Contains(opts, "omitempty") && f.Type.Kind() != reflect.Pointer {
				required = append(required, name)
			}
		}
		out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	}
	return map[string]any{}
}

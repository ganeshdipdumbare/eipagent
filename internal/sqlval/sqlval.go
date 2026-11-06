// Package sqlval converts driver column values and checks SQL identifiers.
package sqlval

// AsString accepts what a driver returns for a text column (string or []byte).
func AsString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	}
	return "", false
}

// AsBytes accepts what a driver returns for a JSONB column ([]byte or string).
func AsBytes(v any) ([]byte, bool) {
	switch x := v.(type) {
	case []byte:
		return x, true
	case string:
		return []byte(x), true
	}
	return nil, false
}

// ValidIdent reports whether s is a safe lowercase SQL table name.
func ValidIdent(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Policy is the admin layer. It is applied last; nothing below it (a
// profile, a module) can override it.
type Policy struct {
	// Allow lists the modules that may run (empty = any registered module).
	Allow []string `json:"allow,omitempty"`
	// Deny lists modules that may not run. Deny wins over Allow.
	Deny []string `json:"deny,omitempty"`
	// Grants limits capabilities per module name, or "*" for all. A module
	// holds the intersection of what it declares and what is granted here.
	// nil: every declared capability is granted. Non-nil: anything not
	// listed is denied.
	Grants map[string][]string `json:"grants,omitempty"`
	// Locked modules cannot be disabled by a profile. They get the reserved
	// priority band, so their hooks run outermost.
	Locked []string `json:"locked,omitempty"`
	// Require rows are always enabled. They replace profile rows with the
	// same name.
	Require []ModuleSpec `json:"require,omitempty"`
	// Rules is passed to the "policy" module as its config.
	Rules map[string]any `json:"rules,omitempty"`
}

// SecurePolicy is the default. Modules get exactly the capabilities they
// declare; nothing is denied or locked until the application says so.
func SecurePolicy() *Policy { return &Policy{} }

func contains(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

func (p *Policy) allowed(name string) bool {
	if contains(p.Deny, name) {
		return false
	}
	return len(p.Allow) == 0 || contains(p.Allow, name)
}

func (p *Policy) locked(name string) bool { return contains(p.Locked, name) }

// granted returns the capabilities a module holds: declared, narrowed by
// Grants when the policy sets it.
func (p *Policy) granted(name string, declared []string) []string {
	if p.Grants == nil {
		return append([]string(nil), declared...)
	}
	allow := append(append([]string(nil), p.Grants[name]...), p.Grants["*"]...)
	var out []string
	for _, d := range declared {
		if contains(allow, d) {
			out = append(out, d)
		}
	}
	return out
}

// ReadJSON decodes a JSON file into v.
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolate expands ${ENV} in config strings. Expanding needs the "env"
// capability; secret-looking names also need "env:secrets". Secrets
// normally arrive through secret:// references instead.
func interpolate(v any, granted []string) (any, error) {
	envOK, secretsOK := false, false
	for _, p := range granted {
		envOK = envOK || p == sdk.PermEnv || p == sdk.PermEnvSecrets
		secretsOK = secretsOK || p == sdk.PermEnvSecrets
	}
	var bad string
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return envRef.ReplaceAllStringFunc(x, func(m string) string {
				name := m[2 : len(m)-1]
				if !envOK || (!secretsOK && sdk.SecretLike(name)) {
					bad = name
					return ""
				}
				return os.Getenv(name)
			})
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, e := range x {
				out[k] = walk(e)
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = walk(e)
			}
			return out
		}
		return v
	}
	out := walk(v)
	if bad != "" {
		return nil, fmt.Errorf("config ${%s}: expanding environment variables needs the %q capability", bad, sdk.PermEnv)
	}
	return out, nil
}

var secretRef = regexp.MustCompile(`secret://([A-Za-z0-9_.-]+)`)

// configSecretRefs lists the secret names written as secret://NAME anywhere
// in a module's configuration.
func configSecretRefs(v any) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			for _, m := range secretRef.FindAllStringSubmatch(x, -1) {
				out = append(out, m[1])
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// Allows reports whether the policy lets module name run at all.
func (p *Policy) Allows(name string) bool { return p.allowed(name) }

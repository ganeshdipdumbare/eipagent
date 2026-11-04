package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"

	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// PluginSpec is one row of a profile.
type PluginSpec struct {
	Use      string         `json:"use"`          // source plugin name
	As       string         `json:"as,omitempty"` // instance name (load one source twice)
	Config   map[string]any `json:"config,omitempty"`
	Disabled bool           `json:"disabled,omitempty"`
	Priority *int           `json:"priority,omitempty"` // default: profile order
	// Auto marks a plugin the SDK loaded because something needed it
	// (shown as "auto" in Status); never set it in a profile.
	Auto bool `json:"-"`
}

func (s PluginSpec) Name() string {
	if s.As != "" {
		return s.As
	}
	return s.Use
}

// Profile says which plugins to run (like cordis.yml). Profiles layer:
// later layers replace rows with the same instance name and append new ones.
type Profile struct {
	Model        string       `json:"model,omitempty"`
	Instructions string       `json:"instructions,omitempty"`
	MaxSteps     int          `json:"max_steps,omitempty"`
	Plugins      []PluginSpec `json:"plugins"`
}

func (p *Profile) Merge(o *Profile) {
	if o == nil {
		return
	}
	if o.Model != "" {
		p.Model = o.Model
	}
	if o.Instructions != "" {
		p.Instructions = o.Instructions
	}
	if o.MaxSteps != 0 {
		p.MaxSteps = o.MaxSteps
	}
	for _, s := range o.Plugins {
		replaced := false
		for i := range p.Plugins {
			if p.Plugins[i].Name() == s.Name() {
				p.Plugins[i], replaced = s, true
			}
		}
		if !replaced {
			p.Plugins = append(p.Plugins, s)
		}
	}
}

// Policy is the admin layer. It is applied last and nothing below it
// (profile, chat, plugins) can override it.
type Policy struct {
	// RequirePins refuses file plugins whose sha256 is not in the lock.
	RequirePins bool `json:"require_pins"`
	// Allow lists permitted plugin sources (empty = all); Deny wins.
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
	// Grants maps plugin instance (or "*") to permissions it may hold.
	// nil = every plugin gets what it declares.
	Grants map[string][]string `json:"grants,omitempty"`
	// Locked plugins are trusted: they can't be unloaded or reloaded at
	// runtime and may use the reserved priority band.
	Locked []string `json:"locked,omitempty"`
	// Require are plugins always loaded (appended to any profile).
	Require []PluginSpec `json:"require,omitempty"`
	// HotReload enables the file watcher.
	HotReload bool `json:"hot_reload"`
	// Rules is passed through to the policy plugin as its config.
	Rules map[string]any `json:"rules,omitempty"`
}

// named reports whether list contains name under its current or
// pre-v0.4 name (policies written before the renames keep working).
func named(list []string, name string) bool {
	for _, n := range sdk.PluginAliases(name) {
		for _, l := range list {
			if l == n {
				return true
			}
		}
	}
	return false
}

func (p *Policy) allowed(src string) bool {
	if named(p.Deny, src) {
		return false
	}
	return len(p.Allow) == 0 || named(p.Allow, src)
}

func (p *Policy) locked(name string) bool { return named(p.Locked, name) }

func (p *Policy) grant(name string, declared []string) []string {
	if p.Grants == nil {
		return declared
	}
	for _, n := range sdk.PluginAliases(name) {
		if g, ok := p.Grants[n]; ok {
			return intersect(declared, g)
		}
	}
	return intersect(declared, p.Grants["*"])
}

// Presets. Secure is the default for New().
func SecurePolicy() *Policy {
	return &Policy{RequirePins: true, HotReload: false}
}

func DevPolicy() *Policy {
	return &Policy{RequirePins: false, HotReload: true}
}

// Lock pins plugin sources by sha256 (eip.lock).
type Lock struct {
	Plugins map[string]string `json:"plugins"`
}

func Hash(src []byte) string {
	h := sha256.Sum256(src)
	return hex.EncodeToString(h[:])
}

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

func WriteLock(path string, l *Lock) error {
	names := make([]string, 0, len(l.Plugins))
	for n := range l.Plugins {
		names = append(names, n)
	}
	sort.Strings(names)
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolate expands ${ENV} in config strings. Secret-looking names
// (SecretLike) expand only for plugins holding env:secrets; others get
// their credentials through secret://NAME references and grants.
func interpolate(v any, perms []string) (any, error) {
	secrets, envOK := false, false
	for _, p := range perms {
		secrets = secrets || p == PermEnvSecrets
		envOK = envOK || p == PermEnv || p == PermEnvSecrets
	}
	var bad string
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return envRef.ReplaceAllStringFunc(x, func(m string) string {
				name := m[2 : len(m)-1]
				if !envOK {
					bad = name
					return ""
				}
				if !secrets && SecretLike(name) {
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
		return nil, fmt.Errorf("config ${%s}: expanding environment variables needs the env permission", bad)
	}
	return out, nil
}

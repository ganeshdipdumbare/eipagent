// Package pluginpkg is the SDK side of shipping plugins: the plugin.json
// manifest, a linter, signed and hashed packages, and a store that
// installs, approves, enables, updates, rolls back and uninstalls them.
//
// A plugin source directory holds one plugin:
//
//	weather/
//	  weather.go      // package plugin, func Apply(*sdk.Plugin) error
//	  plugin.json     // the manifest
//	  README.md       // optional
//
// `eip plugin build` turns it into weather-0.1.0.eip, a gzipped tar with
// the manifest (carrying every file's sha256), the files, and an
// optional ed25519 signature over the manifest.
package pluginpkg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// ManifestFile is the manifest's file name in a source dir or package.
const ManifestFile = "plugin.json"

// Manifest describes a plugin package. Permissions and Requires must match
// the `var Permissions` / `var Requires` in the source; the linter checks
// that the rest matches what the code does.
type Manifest struct {
	Schema      int      `json:"schema"`                // manifest format, 1
	Name        string   `json:"name"`                  // = file name of the .go source
	Version     string   `json:"version"`               // semver, e.g. 0.1.0
	Description string   `json:"description"`           //
	Permissions []string `json:"permissions,omitempty"` // net, exec, fs:read, fs:write, env, db
	Requires    []string `json:"requires,omitempty"`    // services needed first ("?x" optional, "x/*" any)
	Inject      []string `json:"inject,omitempty"`      // Deprecated: pre-v0.4 name of requires (still read)
	Provides    []string `json:"provides,omitempty"`    // services it registers
	Secrets     []string `json:"secrets,omitempty"`     // secret names it resolves (secret://NAME)
	Tools       []string `json:"tools,omitempty"`       // tools it exports
	EIPAgent    string   `json:"eipagent,omitempty"`    // compatible SDK versions, e.g. ">=0.2.0, <1.0.0"
	Dynamic     bool     `json:"dynamic,omitempty"`     // tools/services are registered with runtime-decided names
	Author      string   `json:"author,omitempty"`
	License     string   `json:"license,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	// Files maps every file in the package to its sha256. Set by build.
	Files map[string]string `json:"files,omitempty"`
}

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	secretRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	semverRe = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?$`)
)

// ReadManifest reads plugin.json from a file path.
func ReadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseManifest(b)
}

// ParseManifest decodes a manifest, rejecting unknown fields.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("plugin.json: %w", err)
	}
	return &m, nil
}

// Needs is what the plugin requires (requires, plus the pre-v0.4 inject).
func (m *Manifest) Needs() []string {
	return append(append([]string(nil), m.Requires...), m.Inject...)
}

// Encode returns the canonical manifest bytes (what gets signed).
func (m *Manifest) Encode() []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(m)
	return buf.Bytes()
}

// Check validates the manifest on its own (not against the source).
func (m *Manifest) Check() []error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if m.Schema != 1 {
		bad("schema must be 1, got %d", m.Schema)
	}
	if !nameRe.MatchString(m.Name) {
		bad("name %q must match %s", m.Name, nameRe)
	}
	if !semverRe.MatchString(m.Version) {
		bad("version %q is not semver (MAJOR.MINOR.PATCH)", m.Version)
	}
	if strings.TrimSpace(m.Description) == "" {
		bad("description is empty")
	}
	seenPerm := map[string]bool{}
	for _, p := range m.Permissions {
		if seenPerm[p] {
			bad("duplicate permission %q", p)
		}
		seenPerm[p] = true
		if !contains(engine.AllPerms, p) {
			bad("unknown permission %q (known: %s)", p, strings.Join(engine.AllPerms, ", "))
		}
	}
	seenNeed := map[string]bool{}
	for _, n := range m.Needs() {
		if seenNeed[n] {
			bad("duplicate requires entry %q", n)
		}
		seenNeed[n] = true
	}
	for _, s := range m.Secrets {
		if !secretRe.MatchString(s) {
			bad("secret name %q is invalid (list names, not secret:// refs or values)", s)
		}
	}
	for _, t := range m.Tools {
		if t == "" {
			bad("empty tool name")
		}
	}
	if m.EIPAgent != "" {
		if _, err := Compatible(m.EIPAgent, sdk.Version); err != nil {
			bad("eipagent: %v", err)
		}
	}
	return errs
}

// Compatible reports whether version satisfies a constraint such as
// ">=0.2.0, <1.0.0" (comparisons: = == != > >= < <=, and ^0.2.0 / ~0.2.0).
func Compatible(constraint, version string) (bool, error) {
	v, err := parseSemver(version)
	if err != nil {
		return false, err
	}
	for _, part := range strings.Split(constraint, ",") {
		part = strings.TrimSpace(part)
		if part == "" || part == "*" {
			continue
		}
		op := strings.TrimRightFunc(part, func(r rune) bool { return r == '.' || (r >= '0' && r <= '9') || r == '-' || (r >= 'a' && r <= 'z') })
		want, err := parseSemver(strings.TrimSpace(part[len(op):]))
		if err != nil {
			return false, fmt.Errorf("constraint %q: %w", part, err)
		}
		c := cmpSemver(v, want)
		var ok bool
		switch strings.TrimSpace(op) {
		case "", "=", "==":
			ok = c == 0
		case "!=":
			ok = c != 0
		case ">":
			ok = c > 0
		case ">=":
			ok = c >= 0
		case "<":
			ok = c < 0
		case "<=":
			ok = c <= 0
		case "^": // same major (same minor while major is 0)
			ok = c >= 0 && v[0] == want[0] && (want[0] != 0 || v[1] == want[1])
		case "~": // same minor
			ok = c >= 0 && v[0] == want[0] && v[1] == want[1]
		default:
			return false, fmt.Errorf("constraint %q: unknown operator %q", part, op)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func parseSemver(s string) ([3]int, error) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, fmt.Errorf("%q is not semver", s)
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, nil
}

func cmpSemver(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

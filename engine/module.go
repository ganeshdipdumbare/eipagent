// Package engine composes and runs kiln modules. A module is a
// compiled Go package (modules/<name>) that exports a Module descriptor.
// Compose validates a set of modules before anything runs; Start applies
// them in dependency order. The engine knows nothing about models, tools,
// loops or memory: those are modules.
package engine

import (
	"fmt"
	"strings"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Module is a compiled module descriptor. Each module package exports
// `var Module = engine.Module{...}`.
type Module struct {
	// Name identifies the module: letters, digits, '_' and '-'. Standard
	// modules use their package directory name.
	Name string
	// Requires lists services Apply needs before it runs. "?svc" is
	// optional. "svc/*" matches any service with that prefix.
	Requires []string
	// Provides lists the services Apply will register. A name ending in
	// "*" declares a prefix (for dynamic names such as "sandbox/<name>").
	// Provide of any other name is refused at run time.
	Provides []string
	// Permissions lists the capabilities the module uses (sdk.Perm*). The
	// module holds only the ones the policy also grants.
	Permissions []string
	// Secrets lists the secret names the module may resolve.
	Secrets []string
	// Apply configures the module. It runs once, in dependency order.
	Apply func(*sdk.Module) error
}

// ModuleSpec is one row of a profile or option: which module runs and
// with what configuration.
type ModuleSpec struct {
	Use      string         `json:"use"`          // Module.Name
	As       string         `json:"as,omitempty"` // instance name, to load one module twice (default: Use)
	Config   map[string]any `json:"config,omitempty"`
	Disabled bool           `json:"disabled,omitempty"`
	Priority *int           `json:"priority,omitempty"` // default: profile order
	// Auto marks a row the facade added because a required service had no
	// provider. It is shown in status and never written to a profile.
	Auto bool `json:"-"`
}

// Name is the instance name: As if set, otherwise Use.
func (s ModuleSpec) Name() string {
	if s.As != "" {
		return s.As
	}
	return s.Use
}

// Profile is a list of module rows plus the run defaults they share.
// Later profiles replace rows with the same name and append new ones.
type Profile struct {
	Model        string       `json:"model,omitempty"`
	Instructions string       `json:"instructions,omitempty"`
	MaxSteps     int          `json:"max_steps,omitempty"`
	Modules      []ModuleSpec `json:"modules"`
}

// Merge applies o on top of p.
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
	for _, s := range o.Modules {
		replaced := false
		for i := range p.Modules {
			if p.Modules[i].Name() == s.Name() {
				p.Modules[i], replaced = s, true
			}
		}
		if !replaced {
			p.Modules = append(p.Modules, s)
		}
	}
}

// validName accepts module names: letters, digits, '_' and '-'.
func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// ValidName reports whether s is a valid module name.
func ValidName(s string) bool { return validName(s) }

// providesMatch reports whether a declared provide entry covers the
// concrete service name: an exact match, or a "prefix*" pattern.
func providesMatch(declared, service string) bool {
	if strings.HasSuffix(declared, "*") {
		return strings.HasPrefix(service, strings.TrimSuffix(declared, "*"))
	}
	return declared == service
}

// overlaps reports whether two declared provide entries can name the same
// service. Two patterns may overlap in principle (model/* from two
// providers), so only the run-time Provide decides that; a concrete name
// is compared exactly or against a pattern.
func overlaps(a, b string) bool {
	pa, pb := strings.HasSuffix(a, "*"), strings.HasSuffix(b, "*")
	switch {
	case !pa && !pb:
		return a == b
	case pa && pb:
		return false
	case pa:
		return strings.HasPrefix(b, strings.TrimSuffix(a, "*"))
	default:
		return strings.HasPrefix(a, strings.TrimSuffix(b, "*"))
	}
}

// checkPermissions returns an error for any capability name that is not known.
func checkPermissions(module string, perms []string) error {
	for _, p := range perms {
		if !sdk.IsKnownPermission(p) {
			return fmt.Errorf("module %s: unknown permission %q (known: %s)", module, p, strings.Join(sdk.KnownPermissions, ", "))
		}
	}
	return nil
}

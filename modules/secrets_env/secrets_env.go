package secrets_env

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"fmt"
	"path"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Resolves secret://NAME from environment variables, provided as
// "secrets". Only names matching allow (glob patterns) are readable, so
// other modules can't read arbitrary env through it.
//
// Config:
//
//	allow:  glob patterns (default ["*_API_KEY", "*_TOKEN"])
//	grants: {module instance: [names]} — when set, a module may read only
//	        the names granted to it (the caller is set by the core).
//
// Example:
//
//	kiln.WithModuleConfig("secrets_env", map[string]any{
//		"allow":  []any{"*_API_KEY", "GITHUB_TOKEN"},
//		"grants": map[string]any{"tool_mcp": []any{"GITHUB_TOKEN"}},
//	})
var Permissions = []string{"env", "env:secrets"}

func Apply(p *sdk.Module) error {
	allow := p.ConfigStrings("allow")
	if len(allow) == 0 {
		allow = []string{"*_API_KEY", "*_TOKEN"}
	}
	grants := p.ConfigMap("grants")
	return p.Provide("secrets", &sdk.Secrets{
		Get: func(c context.Context, caller, name string) (string, error) {
			if grants != nil {
				granted := false
				var list []any
				for _, n := range []string{caller} {
					if l, ok := grants[n].([]any); ok {
						list = l
						break
					}
				}
				for _, g := range list {
					if g == name {
						granted = true
					}
				}
				if !granted {
					return "", fmt.Errorf("secret %q is not granted to %s", name, caller)
				}
			}
			ok := false
			for _, pat := range allow {
				if m, _ := path.Match(pat, name); m {
					ok = true
				}
			}
			if !ok {
				return "", fmt.Errorf("secret %q is not in the secrets_env allow list", name)
			}
			// p.Env cannot tell an unset variable from an empty one, so an
			// empty value counts as not set.
			v, err := p.Env(name)
			if err != nil {
				return "", err
			}
			if v == "" {
				return "", fmt.Errorf("secret %q is not set", name)
			}
			return v, nil
		},
	})
}

// Module is the compiled form of "secrets_env". modules/std registers it.
var Module = engine.Module{Name: "secrets_env", Requires: nil, Provides: []string{"secrets"}, Permissions: Permissions, Apply: Apply}

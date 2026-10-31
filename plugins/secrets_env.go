//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"os"
	"path"

	"eipagent/sdk"
)

// Resolves secret://NAME from environment variables, provided as
// "secrets". Only names matching allow (glob patterns) are readable, so
// other plugins can't read arbitrary env through it.
//
// Config:
//
//	allow:  glob patterns (default ["*_API_KEY", "*_TOKEN"])
//	grants: {plugin instance: [names]} — when set, a plugin may read only
//	        the names granted to it (the caller is set by the core).
var Permissions = []string{"env", "env:secrets"}

func Apply(ctx *sdk.Context) error {
	allow := ctx.ConfigStrings("allow")
	if len(allow) == 0 {
		allow = []string{"*_API_KEY", "*_TOKEN"}
	}
	grants := ctx.ConfigMap("grants")
	return ctx.Provide("secrets", &sdk.Secrets{
		Get: func(c context.Context, caller, name string) (string, error) {
			if grants != nil {
				granted := false
				list, _ := grants[caller].([]any)
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
			v, found := os.LookupEnv(name)
			if !found {
				return "", fmt.Errorf("secret %q is not set", name)
			}
			return v, nil
		},
	})
}

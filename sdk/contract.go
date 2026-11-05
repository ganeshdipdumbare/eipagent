package sdk

import (
	"fmt"
	"strings"
)

// Capability names a module may declare in its Permissions list. A
// module holds a capability only if it declared it AND the admin policy
// grants it (see engine.Policy). Every broker method below checks the
// grant and fails closed.
const (
	PermNet        = "net"         // HTTP clients (Module.HTTP) and listeners (Module.Listen)
	PermExec       = "exec"        // child processes, without a shell (Module.Exec)
	PermFSRead     = "fs:read"     // directories, read-only (Module.Dir)
	PermFSWrite    = "fs:write"    // directories, writable (Module.Dir with write=true)
	PermEnv        = "env"         // environment variables, minus secret-looking names (Module.Env)
	PermEnvSecrets = "env:secrets" // also secret-looking names (Module.Env)
	PermDB         = "db"          // the database provided as the "db" service (Module.DB)
)

// KnownPermissions lists every capability name, in a fixed order.
var KnownPermissions = []string{PermNet, PermExec, PermFSRead, PermFSWrite, PermEnv, PermEnvSecrets, PermDB}

// IsKnownPermission reports whether name is one of KnownPermissions.
func IsKnownPermission(name string) bool {
	for _, p := range KnownPermissions {
		if p == name {
			return true
		}
	}
	return false
}

// contract is what the engine granted a module: the services it may
// provide and the capabilities it may use. Set once, before Apply.
type contract struct {
	provides []string
	grants   map[string]bool
}

// SetContract is called by the engine before Apply. It records the
// module's declared Provides, its granted capabilities and the secret
// names it may resolve. Without the engine's key it does nothing, so a
// module can't widen its own contract.
func (c *Module) SetContract(key *TrustKey, provides, grants, secrets []string) {
	if key == nil || key != engineKey {
		return
	}
	k := &contract{provides: append([]string(nil), provides...), grants: map[string]bool{}}
	for _, g := range grants {
		k.grants[g] = true
	}
	c.contract = k
	c.SetSecretPolicy(key, secrets, true)
	c.SetBuiltin(key, true)
}

// Grants returns the capabilities this module holds, in KnownPermissions
// order. Nil for a context the engine did not start (the root).
func (c *Module) Grants() []string {
	if c.contract == nil {
		return nil
	}
	var out []string
	for _, p := range KnownPermissions {
		if c.contract.grants[p] {
			out = append(out, p)
		}
	}
	return out
}

// Granted reports whether the module holds the capability.
func (c *Module) Granted(perm string) bool {
	return c.contract != nil && c.contract.grants[perm]
}

// requireGrant returns an error naming the capability when it is missing.
func (c *Module) requireGrant(perm string) error {
	if c.Granted(perm) {
		return nil
	}
	return fmt.Errorf("module %s: capability %q is not granted (declare it in Permissions and allow it in the policy)", c.PluginName(), perm)
}

// mayProvide reports whether the module may register service name. A
// context without a contract (the root, or code the engine did not start)
// is unrestricted.
func (c *Module) mayProvide(name string) bool {
	if c.contract == nil {
		return true
	}
	for _, p := range c.contract.provides {
		if p == name {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

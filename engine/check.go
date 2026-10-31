package engine

import (
	"fmt"

	"github.com/traefik/yaegi/interp"

	"eipagent/sdk"
)

// ImportPermission says which permission importing pkg needs: "" for
// packages every plugin may use, "-" for packages never available to
// plugins.
func ImportPermission(pkg string) string { return importPerm(pkg) }

// OSPermissions lists the permissions that each allow using os.<sym>
// (any one is enough). nil means the symbol needs no permission, and
// ok is false when no permission makes it available.
func OSPermissions(sym string) (perms []string, ok bool) {
	if osAlwaysOK(sym) {
		return nil, true
	}
	for _, perm := range AllPerms {
		for _, n := range osSyms[perm] {
			if n == sym {
				perms = append(perms, perm)
			}
		}
	}
	return perms, len(perms) > 0
}

// Check compiles a plugin source the way Load would (declared
// permissions only, nothing run) and reports the first problem.
func Check(name string, src []byte) error {
	m, err := parseMeta(name, src)
	if err != nil {
		return err
	}
	for _, p := range m.permissions {
		if !knownPerm(p) {
			return fmt.Errorf("%s: unknown permission %q", name, p)
		}
	}
	if err := checkImports(name, m.imports, m.permissions); err != nil {
		return err
	}
	i := interp.New(interp.Options{Env: []string{}})
	if err := i.Use(buildSymbols(m.permissions)); err != nil {
		return err
	}
	if _, err := i.Eval(string(src)); err != nil {
		return fmt.Errorf("compile %s: %w", name, err)
	}
	v, err := i.Eval("plugin.Apply")
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if _, ok := v.Interface().(func(*sdk.Context) error); !ok {
		return fmt.Errorf("%s: Apply must be func(*sdk.Context) error", name)
	}
	return nil
}

func knownPerm(p string) bool {
	for _, a := range AllPerms {
		if a == p {
			return true
		}
	}
	return false
}

// Pin records a trusted sha256 for a plugin source, as if it were in
// eip.lock. Plugin stores use it when they activate a version.
func (e *Engine) Pin(use, hash string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.opts.Lock.Plugins == nil {
		e.opts.Lock.Plugins = map[string]string{}
	}
	if hash == "" {
		delete(e.opts.Lock.Plugins, use)
		return
	}
	e.opts.Lock.Plugins[use] = hash
}

// Loaded reports whether a plugin instance is loaded (in any state).
func (e *Engine) Loaded(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.recs[name]
	return ok
}

// Missing lists a loaded plugin's required dependencies that no plugin
// provides (why it is PENDING).
func (e *Engine) Missing(name string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r, ok := e.recs[name]; ok {
		return e.missing(r)
	}
	return nil
}

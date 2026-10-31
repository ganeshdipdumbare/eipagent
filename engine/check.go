package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/traefik/yaegi/interp"
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

// Check compiles a plugin source the way Load would for a plugin from a
// plugin directory or store (declared permissions only) and reports the
// first problem. Nothing in the plugin runs: the source is compiled, not
// executed, so even top-level var initializers don't run (validating a
// malicious plugin must not execute it).
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
	if !hasApply(src) {
		return fmt.Errorf("%s: missing func Apply(ctx *sdk.Context) error", name)
	}
	i := newInterp([]string{})
	if err := i.Use(buildSymbols(m.permissions, false)); err != nil {
		return err
	}
	if _, err := i.Compile(string(src)); err != nil {
		return fmt.Errorf("compile %s: %w", name, err)
	}
	return nil
}

// hasApply reports whether src declares func Apply(<x> *sdk.Context) error.
func hasApply(src []byte) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, 0)
	if err != nil {
		return false
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name.Name != "Apply" {
			continue
		}
		t := fd.Type
		if t.Params == nil || len(t.Params.List) != 1 || len(t.Params.List[0].Names) > 1 || t.Results == nil || len(t.Results.List) != 1 {
			return false
		}
		star, ok := t.Params.List[0].Type.(*ast.StarExpr)
		if !ok {
			return false
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Context" {
			return false
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "sdk" {
			return false
		}
		res, ok := t.Results.List[0].Type.(*ast.Ident)
		return ok && res.Name == "error"
	}
	return false
}

// newInterp is an interpreter for one plugin: env is what it may see,
// and stdin is never the process's (in `eip serve --stdio` that carries
// the RPC protocol).
func newInterp(env []string) *interp.Interpreter {
	return interp.New(interp.Options{Env: env, Stdin: strings.NewReader("")})
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

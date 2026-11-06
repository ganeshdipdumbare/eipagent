// Package modulecheck statically checks module packages (modules/<name>).
// It is syntactic: it reads the source, not types. It enforces that a
// module reaches the outside world only through the sdk broker, that the
// capabilities it uses are declared, and that its descriptor matches its
// directory. It is a CI gate, not a sandbox: code that bypasses it is not
// contained by it.
package modulecheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Finding is one problem (or warning) in one module.
type Finding struct {
	File    string // relative to the root given to Check
	Line    int
	Rule    string // forbidden-import, missing-permission, unknown-permission, undeclared-provide, descriptor, init-func
	Module  string
	Message string
	Warning bool
}

// knownPermissions mirrors sdk.KnownPermissions.
var knownPermissions = map[string]bool{"net": true, "exec": true, "fs:read": true, "fs:write": true, "env": true, "env:secrets": true, "db": true}

// forbiddenImports may not be imported by a module at all.
var forbiddenImports = map[string]string{
	"os":             "use the sdk broker: p.Dir for files, p.Env for variables",
	"os/exec":        "use p.Exec, which allows named programs only and never runs a shell",
	"syscall":        "process and OS control belongs to the sdk",
	"unsafe":         "not allowed in modules",
	"plugin":         "runtime plugin loading is not part of kiln",
	"net/rpc":        "use p.HTTP or p.Listen",
	"runtime/cgo":    "not allowed in modules",
	"embed":          "modules may not embed files",
	"io/ioutil":      "use p.Dir (io/ioutil is deprecated and opens files directly)",
	"os/signal":      "signal handling belongs to the application",
	"net/http/cgi":   "not allowed in modules",
	"net/http/pprof": "not allowed in modules",
}

// forbiddenSelectors are calls that bypass the broker in packages that
// are otherwise allowed (for their types or pure helpers).
var forbiddenSelectors = map[string]map[string]string{
	"net": {
		"DefaultResolver": "use p.HTTP; the broker checks addresses when connecting",
		"Dialer":          "use p.HTTP", "ListenConfig": "use p.Listen", "Resolver": "use p.HTTP",
	},
	"net/http": {
		"Get": "use p.HTTP(...).Do", "Post": "use p.HTTP(...).Do", "PostForm": "use p.HTTP(...).Do", "Head": "use p.HTTP(...).Do",
		"DefaultClient": "use p.HTTP", "DefaultTransport": "use p.HTTP", "Client": "use p.HTTP", "Transport": "use p.HTTP",
		"ListenAndServe": "use p.Listen", "ListenAndServeTLS": "use p.Listen",
	},
}

// brokerCapability maps a broker method to the capability it needs. Dir is
// handled separately (its write mode needs fs:write).
var brokerCapability = map[string][]string{
	"HTTP":   {"net"},
	"Listen": {"net"},
	"Exec":   {"exec"},
	"Env":    {"env"}, // env:secrets also satisfies this, see check
	"DB":     {"db"},
	"Dir":    {"fs:read"},
}

// dbDriverModules may import database/sql: the one provider that opens
// connections. Everything else uses p.DB().
var dbDriverModules = map[string]bool{"db_sql": true}

// ModuleInfo is what a module declares and uses, for manifests.
type ModuleInfo struct {
	Name        string
	Requires    []string
	Provides    []string
	Permissions []string
	Secrets     []string
	BrokerCalls []string // capabilities the Apply code needs, sorted
}

// Check analyzes every module directory directly under root.
func Check(root string) ([]Finding, error) {
	var out []Finding
	err := eachModule(root, func(dir, name string, m *moduleSrc) {
		out = append(out, m.check(root, name)...)
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

// Manifest describes every module under root, sorted by name.
func Manifest(root string) ([]ModuleInfo, error) {
	var out []ModuleInfo
	err := eachModule(root, func(dir, name string, m *moduleSrc) {
		info := ModuleInfo{Name: name}
		info.Requires, _, _ = m.list("Requires")
		info.Provides, _, _ = m.list("Provides")
		info.Permissions, _, _ = m.list("Permissions")
		info.Secrets, _, _ = m.list("Secrets")
		calls := map[string]bool{}
		for _, c := range m.brokerUses() {
			for _, need := range c.needed {
				calls[need] = true
			}
		}
		for c := range calls {
			info.BrokerCalls = append(info.BrokerCalls, c)
		}
		sort.Strings(info.BrokerCalls)
		out = append(out, info)
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Format renders findings one per line, sorted as given.
func Format(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		prefix := ""
		if f.Warning {
			prefix = "warning: "
		}
		fmt.Fprintf(&b, "%s%s:%d: [%s] %s: %s\n", prefix, f.File, f.Line, f.Rule, f.Module, f.Message)
	}
	return b.String()
}

// HasErrors reports whether any finding is an error (not a warning).
func HasErrors(fs []Finding) bool {
	for _, f := range fs {
		if !f.Warning {
			return true
		}
	}
	return false
}

// ---- module source ----

type moduleSrc struct {
	dir   string
	files []*fileSrc
	vars  map[string]ast.Expr // package-level vars by name (first value)
	desc  *ast.CompositeLit   // the Module descriptor literal, if any
	descF *fileSrc
	descN *ast.ValueSpec
}

type fileSrc struct {
	rel     string
	fset    *token.FileSet
	file    *ast.File
	imports map[string]string // local name -> import path
}

func eachModule(root string, fn func(dir, name string, m *moduleSrc)) error {
	ents, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		if !e.IsDir() || name == "std" || name == "testdata" || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(root, name)
		m, err := load(root, dir)
		if err != nil {
			return err
		}
		if m == nil {
			continue
		}
		fn(dir, name, m)
	}
	return nil
}

func load(root, dir string) (*moduleSrc, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := &moduleSrc{dir: dir, vars: map[string]ast.Expr{}}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		path := filepath.Join(dir, n)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		fs := &fileSrc{rel: rel, fset: fset, file: f, imports: map[string]string{}}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			local := filepath.Base(p)
			if imp.Name != nil {
				local = imp.Name.Name
			}
			fs.imports[local] = p
		}
		m.files = append(m.files, fs)
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i < len(vs.Values) {
						m.vars[id.Name] = vs.Values[i]
					}
					if id.Name == "Module" && i < len(vs.Values) {
						if cl, ok := vs.Values[i].(*ast.CompositeLit); ok {
							m.desc, m.descF, m.descN = cl, fs, vs
						}
					}
				}
			}
		}
	}
	if len(m.files) == 0 {
		return nil, nil
	}
	return m, nil
}

// list resolves a []string field of the descriptor (or a package var of the
// same name). ok is false when the value is not a literal; then warn.
func (m *moduleSrc) list(field string) (vals []string, literal bool, line int) {
	if m.desc == nil {
		return nil, false, 0
	}
	expr := m.field(field)
	if expr == nil {
		return nil, true, 0
	}
	return m.strings(expr)
}

func (m *moduleSrc) field(name string) ast.Expr {
	if m.desc == nil {
		return nil
	}
	for _, el := range m.desc.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == name {
			return kv.Value
		}
	}
	return nil
}

// strings reads a composite literal of string constants, or an identifier
// naming a package-level var with one. nil and empty are both "no values".
func (m *moduleSrc) strings(e ast.Expr) ([]string, bool, int) {
	switch x := e.(type) {
	case *ast.Ident:
		if x.Name == "nil" {
			return nil, true, 0
		}
		if v, ok := m.vars[x.Name]; ok {
			return m.strings(v)
		}
		return nil, false, 0
	case *ast.CompositeLit:
		var out []string
		for _, el := range x.Elts {
			bl, ok := el.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return nil, false, 0
			}
			s, _ := strconv.Unquote(bl.Value)
			out = append(out, s)
		}
		return out, true, 0
	}
	return nil, false, 0
}

// receivers are the names of parameters of type *sdk.Scope, per file.
func receivers(f *fileSrc) map[string]bool {
	out := map[string]bool{}
	sdkName := ""
	for local, p := range f.imports {
		if strings.HasSuffix(p, "/sdk") {
			sdkName = local
		}
	}
	if sdkName == "" {
		return out
	}
	ast.Inspect(f.file, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Type.Params == nil {
			return true
		}
		for _, p := range fd.Type.Params.List {
			star, ok := p.Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			sel, ok := star.X.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Scope" {
				continue
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != sdkName {
				continue
			}
			for _, nm := range p.Names {
				out[nm.Name] = true
			}
		}
		return true
	})
	return out
}

type use struct {
	line   int
	file   string
	method string
	needed []string
}

// brokerUses lists the broker calls in the module, with what each needs.
func (m *moduleSrc) brokerUses() []use {
	var out []use
	for _, f := range m.files {
		recv := receivers(f)
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || !recv[id.Name] {
				return true
			}
			needs, known := brokerCapability[sel.Sel.Name]
			if !known {
				return true
			}
			if sel.Sel.Name == "Dir" && len(call.Args) == 2 && isTrue(call.Args[1]) {
				needs = []string{"fs:read", "fs:write"}
			}
			out = append(out, use{line: f.fset.Position(call.Pos()).Line, file: f.rel, method: sel.Sel.Name, needed: needs})
			return true
		})
	}
	return out
}

func isTrue(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "true"
}

// ---- rules ----

func (m *moduleSrc) check(root, name string) []Finding {
	var out []Finding
	add := func(f *fileSrc, pos token.Pos, rule, msg string, warn bool) {
		line := 0
		file := ""
		if f != nil {
			line = f.fset.Position(pos).Line
			file = f.rel
		}
		out = append(out, Finding{File: file, Line: line, Rule: rule, Module: name, Message: msg, Warning: warn})
	}

	// R5 descriptor.
	if m.desc == nil {
		add(nil, 0, "descriptor", "no `var Module = engine.Module{...}` in this package", false)
	} else {
		nameExpr := m.field("Name")
		if bl, ok := nameExpr.(*ast.BasicLit); !ok || bl.Kind != token.STRING {
			add(m.descF, m.descN.Pos(), "descriptor", "Module.Name must be a string literal", false)
		} else if got, _ := strconv.Unquote(bl.Value); got != name {
			add(m.descF, bl.Pos(), "descriptor", fmt.Sprintf("Module.Name %q does not match directory %q", got, name), false)
		}
		if m.field("Apply") == nil {
			add(m.descF, m.descN.Pos(), "descriptor", "Module has no Apply", false)
		}
	}

	// R3 permissions, and the literal list used for R2.
	perms, literal, _ := m.list("Permissions")
	if m.desc != nil && !literal {
		add(m.descF, m.descN.Pos(), "missing-permission", "Permissions is not a literal; cannot check broker use", true)
	}
	for _, p := range perms {
		if !knownPermissions[p] {
			add(m.descF, m.descN.Pos(), "unknown-permission", fmt.Sprintf("%q is not a known capability", p), false)
		}
	}
	declared := map[string]bool{}
	for _, p := range perms {
		declared[p] = true
	}

	// R1 forbidden imports and selectors. R2 broker calls.
	provides, provLiteral, _ := m.list("Provides")
	for _, f := range m.files {
		for _, imp := range f.file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if why, bad := forbiddenImports[p]; bad {
				add(f, imp.Pos(), "forbidden-import", fmt.Sprintf("import %q: %s", p, why), false)
			}
			if p == "database/sql" && !dbDriverModules[name] {
				add(f, imp.Pos(), "forbidden-import", `import "database/sql": use p.DB() (only the db_sql provider opens connections)`, false)
			}
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			pkg, ok := f.imports[id.Name]
			if !ok {
				return true
			}
			if why, bad := forbiddenSelectors[pkg][sel.Sel.Name]; bad {
				add(f, sel.Pos(), "forbidden-import", fmt.Sprintf("%s.%s: %s", id.Name, sel.Sel.Name, why), false)
			}
			return true
		})
	}
	for _, c := range m.brokerUses() {
		var missing []string
		for _, need := range c.needed {
			if declared[need] || (need == "env" && declared["env:secrets"]) {
				continue
			}
			missing = append(missing, need)
		}
		if len(missing) == 0 {
			continue
		}
		msg := fmt.Sprintf("p.%s needs capability %q; add it to Permissions", c.method, strings.Join(missing, "\" and \""))
		if !literal {
			add(m.fileByRel(c.file), tokenPos(m, c.file, c.line), "missing-permission", msg, true)
			continue
		}
		add(m.fileByRel(c.file), tokenPos(m, c.file, c.line), "missing-permission", msg, false)
	}

	// R4 provides.
	for _, f := range m.files {
		recv := receivers(f)
		ast.Inspect(f.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Provide" || len(call.Args) == 0 {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || !recv[id.Name] {
				return true
			}
			bl, ok := call.Args[0].(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				add(f, call.Pos(), "undeclared-provide", "Provide name is not a literal; cannot check against Provides", true)
				return true
			}
			svc, _ := strconv.Unquote(bl.Value)
			if !provLiteral || !covered(provides, svc) {
				add(f, call.Pos(), "undeclared-provide", fmt.Sprintf("Provide(%q) is not in Provides", svc), false)
			}
			return true
		})
	}

	// R6 init functions.
	for _, f := range m.files {
		for _, d := range f.file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "init" {
				add(f, fd.Pos(), "init-func", "init functions are not allowed; modules are explicit", false)
			}
		}
	}
	return out
}

func (m *moduleSrc) fileByRel(rel string) *fileSrc {
	for _, f := range m.files {
		if f.rel == rel {
			return f
		}
	}
	return nil
}

func tokenPos(m *moduleSrc, rel string, line int) token.Pos {
	f := m.fileByRel(rel)
	if f == nil {
		return token.NoPos
	}
	tf := f.fset.File(f.file.Pos())
	if tf == nil || line <= 0 || line > tf.LineCount() {
		return f.file.Pos()
	}
	return tf.LineStart(line)
}

// covered reports whether a declared Provides list covers the service name.
func covered(declared []string, svc string) bool {
	for _, d := range declared {
		if d == svc {
			return true
		}
		if strings.HasSuffix(d, "*") && strings.HasPrefix(svc, strings.TrimSuffix(d, "*")) {
			return true
		}
	}
	return false
}

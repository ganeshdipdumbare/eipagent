package pluginpkg

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

	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// Facts is what static analysis learns from a plugin source.
type Facts struct {
	Package         string
	HasApply        bool     // func Apply(p *sdk.Plugin) error (or the pre-v0.4 *sdk.Context)
	Doc             string   // first comment paragraph, used as a default description
	Imports         []string //
	Inject          []string // var Requires (or the pre-v0.4 var Inject)
	Permissions     []string // var Permissions
	OSSymbols       []string // os.X used
	Services        []string // literal names passed to Service / HasService
	Provides        []string // literal names passed to Provide
	Tools           []string // literal Name of tools passed to AddTool
	SecretRefs      []string // "secret://NAME" literals
	Hooks           []string // hook methods called (WrapModel, WrapTool, OnRunStart ...)
	Background      bool     // calls Effect (timers, listeners, connections)
	RuntimeTools    bool     // calls AddTool with names only known at runtime
	RuntimeProvides bool     // calls Provide with a name only known at runtime
	OldNames        []string // pre-v0.4 names used (see eip plugin fix)
}

// Analyze parses a plugin source.
func Analyze(filename string, src []byte) (*Facts, error) {
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	fa := &Facts{Package: f.Name.Name}
	osName := ""
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		fa.Imports = append(fa.Imports, p)
		if p == "os" {
			osName = "os"
			if imp.Name != nil {
				osName = imp.Name.Name
			}
		}
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == "Apply" && isApplySig(d.Type) {
				fa.HasApply = true
				if star, ok := d.Type.Params.List[0].Type.(*ast.StarExpr); ok {
					if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Context" {
						fa.OldNames = append(fa.OldNames, "*sdk.Context (now *sdk.Plugin)")
					}
				}
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, sp := range d.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					switch id.Name {
					case "Requires", "Inject": // Inject: the pre-v0.4 name
						fa.Inject = append(fa.Inject, stringList(vs.Values[i])...)
						if id.Name == "Inject" {
							fa.OldNames = append(fa.OldNames, "var Inject (now var Requires)")
						}
					case "Permissions":
						fa.Permissions = stringList(vs.Values[i])
					}
				}
			}
		}
	}
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			fa.Doc = firstParagraph(cg.Text())
			break
		}
	}
	add := func(list *[]string, s string) {
		if !contains(*list, s) {
			*list = append(*list, s)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok && osName != "" && id.Name == osName {
				add(&fa.OSSymbols, n.Sel.Name)
			}
		case *ast.BasicLit:
			if s, ok := strLit(n); ok && strings.HasPrefix(s, "secret://") {
				add(&fa.SecretRefs, strings.TrimPrefix(s, "secret://"))
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || len(n.Args) == 0 {
				return true
			}
			switch sel.Sel.Name {
			case "Service", "HasService":
				if s, ok := strLit(n.Args[0]); ok {
					add(&fa.Services, s)
				}
			case "Provide":
				if s, ok := strLit(n.Args[0]); ok {
					add(&fa.Provides, s)
				} else if pre := literalPrefix(n.Args[0]); pre != "" {
					add(&fa.Provides, pre+"<name>") // "model/"+name
				} else {
					fa.RuntimeProvides = true
				}
			case "AddTool":
				if name := toolName(n.Args[0]); name != "" {
					add(&fa.Tools, name)
				} else {
					fa.RuntimeTools = true
				}
			case "Effect":
				fa.Background = true
			case "WrapModel", "WrapTool", "OnRunStart", "OnRunEnd", "BeforeStep", "AfterModel", "OnEvent":
				add(&fa.Hooks, sel.Sel.Name)
			case "On": // the older before/after tool hooks (HookToolBefore / HookToolAfter)
				add(&fa.Hooks, "WrapTool")
			case "OnAgentStart", "OnAgentEnd", "OnStepPrepare", "OnModelAfter":
				add(&fa.Hooks, newHookName[sel.Sel.Name])
				add(&fa.OldNames, sel.Sel.Name+" (now "+newHookName[sel.Sel.Name]+")")
			case "OnDispose":
				add(&fa.OldNames, "OnDispose (now OnUnload)")
			}
		}
		return true
	})
	for _, l := range [][]string{fa.OSSymbols, fa.SecretRefs, fa.Services, fa.Provides, fa.Tools} {
		sort.Strings(l)
	}
	return fa, nil
}

// newHookName maps pre-v0.4 hook methods to their names now.
var newHookName = map[string]string{"OnAgentStart": "OnRunStart", "OnAgentEnd": "OnRunEnd", "OnStepPrepare": "BeforeStep", "OnModelAfter": "AfterModel"}

// literalPrefix returns "model/" for "model/"+name (a string literal
// followed by something computed), else "".
func literalPrefix(e ast.Expr) string {
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		if s, ok := strLit(b.X); ok {
			return s
		}
	}
	return ""
}

func isApplySig(t *ast.FuncType) bool {
	if t.Params == nil || len(t.Params.List) != 1 || t.Results == nil || len(t.Results.List) != 1 {
		return false
	}
	star, ok := t.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Plugin" && sel.Sel.Name != "Context") {
		return false
	}
	res, ok := t.Results.List[0].Type.(*ast.Ident)
	return ok && res.Name == "error"
}

func stringList(e ast.Expr) []string {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	var out []string
	for _, el := range lit.Elts {
		if s, ok := strLit(el); ok {
			out = append(out, s)
		}
	}
	return out
}

func strLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

// toolName finds Name: "x" in sdk.Tool{...} or &sdk.Tool{...}.
func toolName(e ast.Expr) string {
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Name" {
			if s, ok := strLit(kv.Value); ok {
				return s
			}
		}
	}
	return ""
}

func firstParagraph(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	return strings.Join(strings.Fields(s), " ")
}

// ---- Lint ----

// Level of a lint finding: errors block build and install.
type Level string

const (
	Error   Level = "error"
	Warning Level = "warning"
)

type Issue struct {
	Level Level  `json:"level"`
	Rule  string `json:"rule"`
	Msg   string `json:"message"`
}

func (i Issue) String() string { return fmt.Sprintf("%s [%s] %s", i.Level, i.Rule, i.Msg) }

// Report is the result of validating a plugin.
type Report struct {
	Name     string    `json:"name"`
	State    State     `json:"state"` // DRAFT or VALIDATED
	Manifest *Manifest `json:"manifest,omitempty"`
	Facts    *Facts    `json:"facts"`
	Issues   []Issue   `json:"issues"`
}

// OK is true when there are no errors.
func (r *Report) OK() bool {
	for _, i := range r.Issues {
		if i.Level == Error {
			return false
		}
	}
	return true
}

// Err summarizes the errors, or nil.
func (r *Report) Err() error {
	var msgs []string
	for _, i := range r.Issues {
		if i.Level == Error {
			msgs = append(msgs, i.Rule+": "+i.Msg)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("plugin %s is invalid: %s", r.Name, strings.Join(msgs, "; "))
}

// Source is a plugin's files in memory (a source dir or an opened package).
type Source struct {
	Name     string
	Manifest *Manifest // nil when there is no plugin.json
	Files    map[string][]byte
}

// Code is the plugin's Go source.
func (s *Source) Code() []byte { return s.Files[s.Name+".go"] }

// ReadDir reads a plugin source directory, or a single .go file.
func ReadDir(path string) (*Source, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(filepath.Base(path), ".go")
		src := &Source{Name: name, Files: map[string][]byte{name + ".go": b}}
		if m, err := ReadManifest(filepath.Join(filepath.Dir(path), ManifestFile)); err == nil && m.Name == name {
			src.Manifest = m
		}
		return src, nil
	}
	src := &Source{Files: map[string][]byte{}}
	ents, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), "_test.go") || strings.HasSuffix(e.Name(), ".eip") || e.Name() == SignatureFile {
			continue
		}
		b, err := os.ReadFile(filepath.Join(path, e.Name()))
		if err != nil {
			return nil, err
		}
		if e.Name() == ManifestFile {
			m, err := ParseManifest(b)
			if err != nil {
				return nil, err
			}
			src.Manifest = m
			continue
		}
		src.Files[e.Name()] = b
	}
	var gos []string
	for n := range src.Files {
		if strings.HasSuffix(n, ".go") {
			gos = append(gos, n)
		}
	}
	switch {
	case src.Manifest != nil:
		src.Name = src.Manifest.Name
	case len(gos) == 1:
		src.Name = strings.TrimSuffix(gos[0], ".go")
	default:
		return nil, fmt.Errorf("%s: no %s and %d .go files; a plugin dir holds one plugin", path, ManifestFile, len(gos))
	}
	return src, nil
}

// Lint validates a plugin: the manifest, the source, and that the two
// agree. It also compiles the source under its declared permissions.
func Lint(src *Source) *Report {
	r := &Report{Name: src.Name, State: StateDraft, Manifest: src.Manifest}
	add := func(l Level, rule, f string, a ...any) {
		r.Issues = append(r.Issues, Issue{l, rule, fmt.Sprintf(f, a...)})
	}
	code := src.Code()
	if code == nil {
		add(Error, "source", "missing %s.go", src.Name)
		return r
	}
	for n := range src.Files {
		if strings.HasSuffix(n, ".go") && n != src.Name+".go" {
			add(Error, "source", "extra Go file %s: a plugin is one file named after it", n)
		}
	}
	fa, err := Analyze(src.Name+".go", code)
	if err != nil {
		add(Error, "source", "%v", err)
		return r
	}
	r.Facts = fa
	if fa.Package != "plugin" {
		add(Error, "source", "package must be \"plugin\", got %q", fa.Package)
	}
	if !fa.HasApply {
		add(Error, "source", "missing func Apply(p *sdk.Plugin) error")
	}
	old := append([]string(nil), fa.OldNames...)
	for _, s := range append(append(append([]string(nil), fa.Inject...), fa.Services...), fa.Provides...) {
		s = strings.TrimPrefix(s, "?")
		if c := sdk.CanonicalService(s); c != s {
			if n := fmt.Sprintf("service %q (now %q)", s, c); !contains(old, n) {
				old = append(old, n)
			}
		}
	}
	if len(old) > 0 {
		add(Warning, "old-name", "uses names renamed in v0.4: %s; they work until v1.0 (eip plugin fix rewrites them)", strings.Join(old, ", "))
	}
	for _, line := range taglessMultiCases(code) {
		add(Warning, "interpreter", "line %d: a case with several conditions in a switch with no tag only checks the first one under Yaegi; join them with ||", line)
	}
	for _, line := range loopAsserts(code) {
		add(Warning, "interpreter", "line %d: `x, _ := v.(T)` in a loop body keeps the previous iteration's value when the assertion fails (a Yaegi quirk); check ok: if y, ok := v.(T); ok { x = y }", line)
	}

	// Imports and permissions.
	needed := map[string]bool{}
	for _, imp := range fa.Imports {
		switch p := engine.ImportPermission(imp); p {
		case "":
		case "-":
			add(Error, "forbidden-import", "%s is not available to plugins", imp)
		default:
			needed[p] = true
			if !contains(fa.Permissions, p) {
				add(Error, "permission-missing", "import %s needs permission %q", imp, p)
			}
		}
	}
	for _, sym := range fa.OSSymbols {
		perms, ok := engine.OSPermissions(sym)
		if !ok {
			add(Error, "forbidden-import", "os.%s is not available to plugins", sym)
			continue
		}
		if perms == nil {
			continue
		}
		have := false
		for _, p := range perms {
			if contains(fa.Permissions, p) {
				have, needed[p] = true, true
			}
		}
		if !have {
			add(Error, "permission-missing", "os.%s needs one of %v", sym, perms)
		}
	}
	for _, p := range fa.Permissions {
		if !contains(engine.AllPerms, p) {
			add(Error, "permission-unknown", "unknown permission %q", p)
		} else if !needed[p] {
			add(Warning, "permission-unused", "permission %q is declared but nothing in the source needs it", p)
		}
	}

	// Services: everything looked up must be injected or self-provided.
	for _, s := range fa.Services {
		if contains(fa.Provides, s) || covered(fa.Inject, s) {
			continue
		}
		if s == "secrets" || s == "audit" {
			add(Warning, "undeclared-service", "uses service %q without listing it in Requires (add \"?%s\" so the plugin restarts when it appears)", s, s)
			continue
		}
		add(Error, "undeclared-service", "uses service %q but Requires doesn't list it (add %q, or \"?%s\" if optional)", s, s, s)
	}
	for _, ref := range fa.SecretRefs {
		if !secretRe.MatchString(ref) {
			add(Error, "secret-ref", "invalid secret reference %q", "secret://"+ref)
		}
	}

	// Compile under the declared permissions.
	if err := engine.Check(src.Name, code); err != nil {
		add(Error, "compile", "%v", err)
	}

	m := src.Manifest
	if m == nil {
		add(Warning, "manifest", "no %s; run `eip plugin init` or write one (it is required to build a package)", ManifestFile)
		return r
	}
	for _, err := range m.Check() {
		add(Error, "manifest", "%v", err)
	}
	if m.Name != src.Name {
		add(Error, "manifest", "name %q does not match source %s.go", m.Name, src.Name)
	}
	if m.EIPAgent != "" {
		if ok, err := Compatible(m.EIPAgent, sdkVersion()); err == nil && !ok {
			add(Error, "compat", "requires eipagent %s, this SDK is %s", m.EIPAgent, sdkVersion())
		}
	}
	if !sameSet(m.Permissions, fa.Permissions) {
		add(Error, "permission-mismatch", "manifest permissions %v != source var Permissions %v", m.Permissions, fa.Permissions)
	}
	if !sameSet(canonical(m.Needs()), canonical(fa.Inject)) {
		add(Error, "requires-mismatch", "manifest requires %v != source var Requires %v", m.Needs(), fa.Inject)
	}
	for _, s := range fa.Provides {
		if strings.HasSuffix(s, "<name>") {
			continue // computed at runtime ("model/"+name): nothing to list
		}
		if !contains(canonical(m.Provides), sdk.CanonicalService(s)) {
			add(Error, "provides", "source provides %q but the manifest doesn't list it", s)
		}
	}
	for _, t := range fa.Tools {
		if !contains(m.Tools, t) {
			add(Error, "tools", "source registers tool %q but the manifest doesn't list it", t)
		}
	}
	for _, t := range m.Tools {
		if !contains(fa.Tools, t) {
			add(Warning, "tools", "manifest lists tool %q but no literal AddTool for it was found", t)
		}
	}
	for _, ref := range fa.SecretRefs {
		if secretRe.MatchString(ref) && !contains(m.Secrets, ref) {
			add(Error, "secret-ref", "source references secret://%s but the manifest doesn't list it in secrets", ref)
		}
	}
	if len(m.Secrets) > 0 && !covered(fa.Inject, "secrets") {
		add(Warning, "secret-ref", "declares secrets but Requires has no \"secrets\" or \"?secrets\"")
	}
	for name, sum := range m.Files {
		b, ok := src.Files[name]
		if !ok {
			add(Error, "files", "manifest lists %s but it is missing", name)
		} else if engine.Hash(b) != sum {
			add(Error, "files", "%s sha256 does not match the manifest", name)
		}
	}
	if r.OK() {
		r.State = StateValidated
	}
	return r
}

// covered reports whether a Requires list declares service s (old and
// new service names count as the same).
func covered(inject []string, s string) bool {
	s = sdk.CanonicalService(s)
	for _, d := range inject {
		d = sdk.CanonicalService(strings.TrimPrefix(d, "?"))
		if d == s || (strings.HasSuffix(d, "*") && strings.HasPrefix(s, strings.TrimSuffix(d, "*"))) {
			return true
		}
	}
	return false
}

// canonical maps service names (keeping a "?" prefix) to current names.
func canonical(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		if strings.HasPrefix(s, "?") {
			out[i] = "?" + sdk.CanonicalService(s[1:])
		} else {
			out[i] = sdk.CanonicalService(s)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}

// Infer drafts a manifest from the source (used by init and as a hint
// when plugin.json is missing).
func Infer(name string, fa *Facts) *Manifest {
	desc := fa.Doc
	if desc == "" {
		desc = name + " plugin"
	}
	return &Manifest{Schema: 1, Name: name, Version: "0.1.0", Description: desc,
		Permissions: fa.Permissions, Requires: canonical(fa.Inject), Provides: literalOnly(fa.Provides), Tools: fa.Tools,
		Secrets: validSecrets(fa.SecretRefs), EIPAgent: ">=" + sdkVersion()}
}

// literalOnly drops names computed at runtime ("model/<name>").
func literalOnly(names []string) []string {
	var out []string
	for _, n := range names {
		if !strings.HasSuffix(n, "<name>") {
			out = append(out, n)
		}
	}
	return out
}

func validSecrets(refs []string) []string {
	var out []string
	for _, r := range refs {
		if secretRe.MatchString(r) {
			out = append(out, r)
		}
	}
	return out
}

// loopAsserts finds `x, _ := v.(T)` written directly in a loop body (not
// in a function literal inside it). Under Yaegi a failed assertion there
// leaves x holding the previous iteration's value instead of the zero
// value; `ok` itself is reported correctly.
func loopAsserts(src []byte) []int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		return nil
	}
	var lines []int
	var walk func(root ast.Node, inLoop bool)
	walk = func(root ast.Node, inLoop bool) {
		ast.Inspect(root, func(n ast.Node) bool {
			if n == root {
				return true
			}
			switch n := n.(type) {
			case *ast.FuncLit:
				walk(n.Body, false)
				return false
			case *ast.ForStmt:
				walk(n.Body, true)
				return false
			case *ast.RangeStmt:
				walk(n.Body, true)
				return false
			case *ast.AssignStmt:
				if !inLoop || len(n.Lhs) != 2 || len(n.Rhs) != 1 {
					return true
				}
				id, isIdent := n.Lhs[1].(*ast.Ident)
				_, isAssert := ast.Unparen(n.Rhs[0]).(*ast.TypeAssertExpr)
				if isIdent && id.Name == "_" && isAssert {
					lines = append(lines, fset.Position(n.Pos()).Line)
				}
			}
			return true
		})
	}
	walk(f, false)
	return lines
}

// taglessMultiCases finds `case a, b:` in a switch with no tag. Under
// Yaegi only the first condition is checked.
func taglessMultiCases(src []byte) []int {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		return nil
	}
	var lines []int
	ast.Inspect(f, func(n ast.Node) bool {
		if sw, ok := n.(*ast.SwitchStmt); ok && sw.Tag == nil {
			for _, st := range sw.Body.List {
				if cc, ok := st.(*ast.CaseClause); ok && len(cc.List) > 1 {
					lines = append(lines, fset.Position(cc.Pos()).Line)
				}
			}
		}
		return true
	})
	return lines
}

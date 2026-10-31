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
)

// Facts is what static analysis learns from a plugin source.
type Facts struct {
	Package     string
	HasApply    bool     // func Apply(ctx *sdk.Context) error
	Doc         string   // first comment paragraph, used as a default description
	Imports     []string //
	Inject      []string // var Inject
	Permissions []string // var Permissions
	OSSymbols   []string // os.X used
	Services    []string // literal names passed to Service / HasService
	Provides    []string // literal names passed to Provide
	Tools       []string // literal Name of tools passed to AddTool
	SecretRefs  []string // "secret://NAME" literals
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
					case "Inject":
						fa.Inject = stringList(vs.Values[i])
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
				}
			case "AddTool":
				if name := toolName(n.Args[0]); name != "" {
					add(&fa.Tools, name)
				}
			}
		}
		return true
	})
	for _, l := range [][]string{fa.OSSymbols, fa.SecretRefs, fa.Services, fa.Provides, fa.Tools} {
		sort.Strings(l)
	}
	return fa, nil
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
	if !ok || sel.Sel.Name != "Context" {
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
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), "_test.go") || strings.HasSuffix(e.Name(), ".eip") {
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
		add(Error, "source", "missing func Apply(ctx *sdk.Context) error")
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
			add(Warning, "undeclared-service", "uses service %q without listing it in Inject (add \"?%s\" so the plugin restarts when it appears)", s, s)
			continue
		}
		add(Error, "undeclared-service", "uses service %q but Inject doesn't list it (add %q, or \"?%s\" if optional)", s, s, s)
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
	if !sameSet(m.Inject, fa.Inject) {
		add(Error, "inject-mismatch", "manifest inject %v != source var Inject %v", m.Inject, fa.Inject)
	}
	for _, s := range fa.Provides {
		if !contains(m.Provides, s) {
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
		add(Warning, "secret-ref", "declares secrets but Inject has no \"secrets\" or \"?secrets\"")
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

// covered reports whether an Inject list declares service s.
func covered(inject []string, s string) bool {
	for _, d := range inject {
		d = strings.TrimPrefix(d, "?")
		if d == s || (strings.HasSuffix(d, "*") && strings.HasPrefix(s, strings.TrimSuffix(d, "*"))) {
			return true
		}
	}
	return false
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
		Permissions: fa.Permissions, Inject: fa.Inject, Provides: fa.Provides, Tools: fa.Tools,
		Secrets: validSecrets(fa.SecretRefs), EIPAgent: ">=" + sdkVersion()}
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

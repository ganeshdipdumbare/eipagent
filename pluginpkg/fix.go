package pluginpkg

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"

	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// Fix rewrites the pre-v0.4 names in a plugin source and reports what it
// changed. It uses the syntax tree, so comments and unrelated strings
// stay as they are:
//
//   - *sdk.Context -> *sdk.Plugin, var Inject -> var Requires
//   - OnAgentStart/OnAgentEnd/OnStepPrepare/OnModelAfter/OnDispose ->
//     OnRunStart/OnRunEnd/BeforeStep/AfterModel/OnUnload
//   - service names in Requires and in Service/HasService/Provide/Services
//     calls ("llm/x" -> "model/x", "checkpointer" -> "threads",
//     "store" -> "memory", "exec" -> "workspace")
//
// The parameter's variable name (often ctx) is left alone.
func Fix(src []byte) ([]byte, []string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "plugin.go", src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	type edit struct {
		off, end int
		text     string
	}
	var edits []edit
	var notes []string
	seen := map[string]bool{}
	note := func(s string) {
		if !seen[s] {
			seen[s] = true
			notes = append(notes, s)
		}
	}
	at := func(n ast.Node, text string) {
		edits = append(edits, edit{fset.Position(n.Pos()).Offset, fset.Position(n.End()).Offset, text})
	}
	service := func(e ast.Expr) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return
		}
		opt := ""
		if len(s) > 0 && s[0] == '?' {
			opt, s = "?", s[1:]
		}
		if c := sdk.CanonicalService(s); c != s {
			at(lit, strconv.Quote(opt+c))
			note("service \"" + s + "\" -> \"" + c + "\"")
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok && x.Name == "sdk" && n.Sel.Name == "Context" {
				at(n.Sel, "Plugin")
				note("sdk.Context -> sdk.Plugin")
			}
			if to, ok := renamedMethods[n.Sel.Name]; ok {
				at(n.Sel, to)
				note(n.Sel.Name + " -> " + to)
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if id.Name != "Inject" && id.Name != "Requires" {
					continue
				}
				if id.Name == "Inject" {
					at(id, "Requires")
					note("var Inject -> var Requires")
				}
				if i < len(n.Values) {
					if lit, ok := n.Values[i].(*ast.CompositeLit); ok {
						for _, el := range lit.Elts {
							service(el)
						}
					}
				}
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && len(n.Args) > 0 {
				switch sel.Sel.Name {
				case "Service", "HasService", "Provide", "Services":
					service(n.Args[0])
				}
			}
		}
		return true
	})
	sort.Slice(edits, func(i, j int) bool { return edits[i].off > edits[j].off })
	out := append([]byte(nil), src...)
	for _, e := range edits {
		out = append(out[:e.off], append([]byte(e.text), out[e.end:]...)...)
	}
	if bytes.Equal(out, src) {
		return src, nil, nil
	}
	return out, notes, nil
}

var renamedMethods = map[string]string{
	"OnAgentStart": "OnRunStart", "OnAgentEnd": "OnRunEnd", "OnStepPrepare": "BeforeStep",
	"OnModelAfter": "AfterModel", "OnDispose": "OnUnload",
}

// FixManifest moves the pre-v0.4 "inject" into "requires" with current
// service names. It reports whether anything changed.
func FixManifest(m *Manifest) bool {
	changed := false
	if len(m.Inject) > 0 {
		m.Requires = append(m.Requires, m.Inject...)
		m.Inject = nil
		changed = true
	}
	for i, r := range m.Requires {
		if c := canonical([]string{r})[0]; c != r {
			m.Requires[i], changed = c, true
		}
	}
	for i, r := range m.Provides {
		if c := sdk.CanonicalService(r); c != r {
			m.Provides[i], changed = c, true
		}
	}
	return changed
}

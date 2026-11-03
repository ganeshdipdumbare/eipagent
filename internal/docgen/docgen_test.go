package docgen

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/eipagent/plugins"
)

// The docs' plugin reference must match the code: run `go run ./docs/gen`.
func TestSiteIsUpToDate(t *testing.T) {
	site, err := os.ReadFile("../../docs/site.html")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Generate(site, plugins.FS)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, site) {
		t.Fatal("docs/site.html is out of date with the plugins; run: go run ./docs/gen")
	}
	for _, k := range Kinds {
		if !bytes.Contains(site, []byte("<!-- gen:builtins "+k.ID+" -->")) {
			t.Errorf("docs/site.html has no built-ins block for %s", k.ID)
		}
	}
}

// Every plugin's doc comment has an Example that parses as Go, names the
// plugin, and only uses config keys the plugin reads.
func TestPluginExamples(t *testing.T) {
	ents, err := fs.ReadDir(plugins.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".go")
		if !ok || name == "embed" {
			continue
		}
		src, _ := fs.ReadFile(plugins.FS, e.Name())
		_, ex := SplitExample(leadingDoc(src))
		// String literals in the plugin's code (not its comments).
		lits := map[string]bool{}
		if pf, err := parser.ParseFile(token.NewFileSet(), e.Name(), src, 0); err == nil {
			ast.Inspect(pf, func(n ast.Node) bool {
				if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					lits[bl.Value] = true
				}
				return true
			})
		}
		if ex == "" {
			t.Errorf("%s: no Example: section in its doc comment", name)
			continue
		}
		if !strings.Contains(ex, name) {
			t.Errorf("%s: the example doesn't mention %s", name, name)
		}
		f, err := parser.ParseFile(token.NewFileSet(), "ex.go", "package p\nfunc _() {\n"+ex+"\n}\n", 0)
		if err != nil {
			t.Errorf("%s: example doesn't parse: %v", name, err)
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "WithPlugin" {
				return true
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); !ok || lit.Value != strconv.Quote(name) {
				return true
			}
			cfg, ok := call.Args[1].(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, el := range cfg.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.BasicLit); ok && !lits[k.Value] {
					t.Errorf("%s: example uses config key %s, which the plugin never reads", name, k.Value)
				}
			}
			return true
		})
	}
}

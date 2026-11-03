// Package docgen fills the generated parts of docs/site.html from the
// code, so the plugin reference can't drift from it:
//
//	<!-- gen:builtins KIND -->...<!-- /gen:builtins -->   every built-in plugin of a kind:
//	                                                     what its code does, its doc comment and config
//	<!-- gen:template KIND NAME -->...<!-- /gen:template --> the source `eip plugin init` writes
//
// Run it with `go run ./docs/gen` (docs/build.sh does); a test fails when
// site.html is out of date.
package docgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/ganeshdipdumbare/eipagent/pluginpkg"
)

// Kind groups built-in plugins on one docs page.
type Kind struct {
	ID       string   // used in markers and page ids (kind-ID)
	Match    []string // name prefixes ("tool_") or exact names ("db_sql")
	Template string   // eip plugin init template, if any
}

// Kinds lists every kind; each built-in plugin must match exactly one.
var Kinds = []Kind{
	{"tools", []string{"tool_"}, "tool"},
	{"hooks", []string{"hook_"}, "hook"},
	{"models", []string{"model_"}, "model"},
	{"threads-memory", []string{"threads_", "memory_", "db_sql"}, "memory"},
	{"sandboxes", []string{"sandbox_", "workspace_"}, "sandbox"},
	{"triggers", []string{"trigger_"}, "trigger"},
	{"telemetry", []string{"trace_", "audit_"}, ""},
	{"secrets", []string{"secrets_"}, ""},
	{"policy", []string{"policy", "loop"}, ""},
}

func (k Kind) matches(name string) bool {
	for _, m := range k.Match {
		if name == m || (strings.HasSuffix(m, "_") && strings.HasPrefix(name, m)) {
			return true
		}
	}
	return false
}

// KindOf returns the kind a built-in plugin belongs to.
func KindOf(name string) (Kind, bool) {
	for _, k := range Kinds {
		if k.matches(name) {
			return k, true
		}
	}
	return Kind{}, false
}

var (
	builtinsRe = regexp.MustCompile(`(?s)(<!-- gen:builtins ([a-z-]+) -->).*?(<!-- /gen:builtins -->)`)
	templateRe = regexp.MustCompile(`(?s)(<!-- gen:template ([a-z]+) ([a-z_]+) -->).*?(<!-- /gen:template -->)`)
)

// Generate returns site with every generated block rebuilt from the
// built-in plugin sources in plugins (one NAME.go per plugin).
func Generate(site []byte, plugins fs.FS) ([]byte, error) {
	docs, err := readPlugins(plugins)
	if err != nil {
		return nil, err
	}
	var genErr error
	out := builtinsRe.ReplaceAllFunc(site, func(m []byte) []byte {
		sm := builtinsRe.FindSubmatch(m)
		block, err := renderBuiltins(string(sm[2]), docs)
		if err != nil {
			genErr = err
			return m
		}
		return []byte(string(sm[1]) + "\n" + block + string(sm[3]))
	})
	out = templateRe.ReplaceAllFunc(out, func(m []byte) []byte {
		sm := templateRe.FindSubmatch(m)
		src, err := pluginpkg.RenderTemplate(string(sm[2]), string(sm[3]))
		if err != nil {
			genErr = err
			return m
		}
		block := `<div class="code" data-title="` + string(sm[3]) + `/` + string(sm[3]) + `.go"><pre><code class="language-go">` +
			html.EscapeString(strings.TrimRight(string(src), "\n")) + "</code></pre></div>\n"
		return []byte(string(sm[1]) + "\n" + block + string(sm[4]))
	})
	return out, genErr
}

type pluginDoc struct {
	name, summary, lead, doc, example string
}

// SplitExample separates a plugin doc comment's "Example:" section (the
// code shown on the site) from the rest of it.
func SplitExample(doc string) (rest, example string) {
	i := strings.Index(doc, "\nExample:\n")
	if i < 0 {
		return doc, ""
	}
	var lines []string
	for _, l := range strings.Split(doc[i+len("\nExample:\n"):], "\n") {
		lines = append(lines, strings.TrimPrefix(l, "\t"))
	}
	return strings.TrimSpace(doc[:i]), strings.TrimSpace(strings.Join(lines, "\n"))
}

func readPlugins(plugins fs.FS) (map[string]pluginDoc, error) {
	ents, err := fs.ReadDir(plugins, ".")
	if err != nil {
		return nil, err
	}
	out := map[string]pluginDoc{}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".go")
		if !ok || name == "embed" {
			continue
		}
		if _, ok := KindOf(name); !ok {
			return nil, fmt.Errorf("built-in plugin %s matches no docs kind (add it to docgen.Kinds)", name)
		}
		src, err := fs.ReadFile(plugins, e.Name())
		if err != nil {
			return nil, err
		}
		fa, err := pluginpkg.Analyze(e.Name(), src)
		if err != nil {
			return nil, err
		}
		doc, example := SplitExample(leadingDoc(src))
		lead := doc
		if i := strings.Index(lead, "\n\n"); i >= 0 {
			lead = lead[:i]
		}
		summary := strings.TrimPrefix(pluginpkg.Summary(name, fa), name+": ")
		out[name] = pluginDoc{name: name, summary: summary, lead: strings.Join(strings.Fields(lead), " "), doc: strings.TrimSpace(doc), example: example}
	}
	return out, nil
}

// leadingDoc is the plugin's description: the doc comment of its first
// declaration after the imports.
func leadingDoc(src []byte) string {
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.ParseComments)
	if err != nil {
		return ""
	}
	for _, d := range f.Decls {
		var cg *ast.CommentGroup
		switch d := d.(type) {
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			cg = d.Doc
		case *ast.FuncDecl:
			cg = d.Doc
		}
		if cg != nil {
			return cg.Text()
		}
	}
	return ""
}

func renderBuiltins(kindID string, docs map[string]pluginDoc) (string, error) {
	var kind *Kind
	for i := range Kinds {
		if Kinds[i].ID == kindID {
			kind = &Kinds[i]
		}
	}
	if kind == nil {
		return "", fmt.Errorf("unknown docs kind %q", kindID)
	}
	var names []string
	for n := range docs {
		if kind.matches(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteString(`<div class="builtins">` + "\n")
	for _, n := range names {
		d := docs[n]
		fmt.Fprintf(&b, `<div class="builtin" id="plugin-%s"><div class="bhead"><code>%s</code><span class="bsum">%s</span></div>`,
			n, n, html.EscapeString(d.summary))
		fmt.Fprintf(&b, "<p>%s</p>", html.EscapeString(d.lead))
		if d.example != "" {
			fmt.Fprintf(&b, `<div class="code" data-title="example"><pre><code class="language-go">%s</code></pre></div>`, html.EscapeString(d.example))
		}
		if d.doc != "" && d.doc != d.lead {
			fmt.Fprintf(&b, `<details><summary>Config and details</summary><pre>%s</pre></details>`, html.EscapeString(d.doc))
		}
		b.WriteString("</div>\n")
	}
	b.WriteString("</div>\n")
	return b.String(), nil
}

package kiln

import (
	"go/format"
	"go/parser"
	"go/token"
	"html"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The docs site is hand-written HTML. These checks keep it from drifting
// from the API and the navigation the same way the README examples are
// kept honest by examples/readme.
func TestDocsUseCurrentAPI(t *testing.T) {
	b, err := os.ReadFile("docs/site.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	removed := []string{
		"WithModel", "WithInstructions", "WithMaxSteps", "WithUser",
		"WithTools", "WithLogger", "WithPolicy", "WithPolicyFile",
		"WithRunIn", "WithSaveEachStep", "WithoutDefaults",
		"WithModule", "WithModules", "WithModuleConfig", "WithModuleSpec",
		"WithProfile", "WithPersistence", "WithApprovals", "WithApprover",
		"WithSandbox", "WithAudit", "WithTracing", "WithMCP",
		"WithWebSearch", "WithThinking", "GenerateText", "ToolDef",
	}
	for _, name := range removed {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(page) {
			t.Errorf("docs/site.html still documents removed API %q", name)
		}
	}
}

func TestDocsNavigationCoversEveryPage(t *testing.T) {
	html, err := os.ReadFile("docs/site.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("docs/assets/site.js")
	if err != nil {
		t.Fatal(err)
	}
	section := regexp.MustCompile(`<section id="([^"]+)" class="page"`)
	var pages []string
	for _, m := range section.FindAllStringSubmatch(string(html), -1) {
		pages = append(pages, m[1])
	}
	nav := string(js)
	for _, id := range pages {
		if !strings.Contains(nav, `"`+id+`"`) {
			t.Errorf("docs page %q is missing from the sidebar NAV in docs/assets/site.js", id)
		}
	}
}

func TestDocsHeadingOrder(t *testing.T) {
	b, err := os.ReadFile("docs/site.html")
	if err != nil {
		t.Fatal(err)
	}
	section := regexp.MustCompile(`(?s)<section id="([^"]+)" class="page">(.*?)</section>`)
	heading := regexp.MustCompile(`<h([1-6])[ >]`)
	for _, m := range section.FindAllStringSubmatch(string(b), -1) {
		id, body := m[1], m[2]
		prev := 0
		for _, h := range heading.FindAllStringSubmatch(body, -1) {
			level := int(h[1][0] - '0')
			if prev > 0 && level > prev+1 {
				t.Errorf("docs page %q jumps from h%d to h%d", id, prev, level)
				break
			}
			prev = level
		}
	}
}

// Complete Go programs in the docs must be gofmt-clean. Fragments are
// skipped: only a full file can be parsed.
func TestDocsGoCodeIsFormatted(t *testing.T) {
	b, err := os.ReadFile("docs/site.html")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)<code class="language-go">(.*?)</code>`)
	for _, m := range block.FindAllStringSubmatch(string(b), -1) {
		src := strings.TrimSpace(html.UnescapeString(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(m[1], ""))) + "\n"
		if _, err := parser.ParseFile(token.NewFileSet(), "docs", src, parser.ParseComments); err != nil {
			continue // a fragment, not a complete file
		}
		formatted, err := format.Source([]byte(src))
		if err != nil {
			continue
		}
		if string(formatted) != src {
			t.Errorf("docs/site.html has a Go block that is not gofmt-clean")
		}
	}
}

package docgen

import (
	"bytes"
	"os"
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

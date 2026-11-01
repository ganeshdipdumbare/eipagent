// Command gen rebuilds the generated parts of docs/site.html (built-in
// plugin reference, plugin templates) from the code. Run from the repo
// root: go run ./docs/gen
package main

import (
	"log"
	"os"

	"github.com/ganeshdipdumbare/eipagent/internal/docgen"
	"github.com/ganeshdipdumbare/eipagent/plugins"
)

func main() {
	const path = "docs/site.html"
	site, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	out, err := docgen.Generate(site, plugins.FS)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		log.Fatal(err)
	}
}

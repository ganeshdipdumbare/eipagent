package pluginpkg

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/eipagent/plugins"
)

func TestLoopAsserts(t *testing.T) {
	src := []byte(`package plugin

func Apply(m []map[string]any) {
	a, _ := m[0]["x"].(string) // not in a loop
	for _, v := range m {
		b, _ := v["x"].(string) // flagged
		if c, ok := v["x"].(string); ok { _ = c } // checks ok
		f := func() { d, _ := v["x"].(string); _ = d } // inside a function literal
		_, _, _ = a, b, f
	}
	for i := 0; i < 2; i++ {
		e, _ := m[i]["y"].(int) // flagged
		_ = e
	}
}
`)
	if got := loopAsserts(src); !reflect.DeepEqual(got, []int{6, 12}) {
		t.Fatalf("got lines %v, want [6 12]", got)
	}
}

func TestTaglessMultiCases(t *testing.T) {
	src := []byte(`package plugin

func f(a string, b bool) {
	switch {
	case a == "x", b:
	case a == "y":
	}
	switch a {
	case "p", "q":
	}
}
`)
	if got := taglessMultiCases(src); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("got lines %v, want [5]", got)
	}
}

// The standard plugins must not use the patterns Yaegi gets wrong.
func TestStandardPluginsAvoidInterpreterQuirks(t *testing.T) {
	ents, err := fs.ReadDir(plugins.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".go") || e.Name() == "embed.go" {
			continue
		}
		src, _ := fs.ReadFile(plugins.FS, e.Name())
		for _, l := range loopAsserts(src) {
			t.Errorf("%s:%d: `x, _ := v.(T)` in a loop body; check ok instead", e.Name(), l)
		}
		for _, l := range taglessMultiCases(src) {
			t.Errorf("%s:%d: several conditions in one case of a switch with no tag; join them with ||", e.Name(), l)
		}
	}
}

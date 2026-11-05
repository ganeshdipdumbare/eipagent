package modulecheck

import (
	"sort"
	"strings"
	"testing"
)

// pairs returns "module:rule" for every error (not warning), sorted and
// de-duplicated.
func pairs(fs []Finding) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range fs {
		if f.Warning {
			continue
		}
		k := f.Module + ":" + f.Rule
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func TestGoodTreeHasNoErrors(t *testing.T) {
	fs, err := Check("testdata/good")
	if err != nil {
		t.Fatal(err)
	}
	if HasErrors(fs) {
		t.Fatalf("good tree has errors:\n%s", Format(fs))
	}
}

func TestBadTreeReportsEachRule(t *testing.T) {
	fs, err := Check("testdata/bad")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"dirnoread:missing-permission",
		"initfunc:init-func",
		"netpkg:forbidden-import",
		"osexec:forbidden-import",
		"undeclared:undeclared-provide",
		"unknownperm:unknown-permission",
		"wrongname:descriptor",
	}
	if got := strings.Join(pairs(fs), "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("findings:\n%s\nwant:\n%s\nraw:\n%s", got, strings.Join(want, "\n"), Format(fs))
	}
}

func TestFormatIsStable(t *testing.T) {
	a, _ := Check("testdata/bad")
	b, _ := Check("testdata/bad")
	if Format(a) != Format(b) || !strings.Contains(Format(a), "[descriptor] wrongname:") {
		t.Fatal("format is not stable or misses the descriptor finding")
	}
}

func TestManifestOfGoodTree(t *testing.T) {
	ms, err := Manifest("testdata/good")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ModuleInfo{}
	for _, m := range ms {
		got[m.Name] = m
	}
	if strings.Join(got["alpha"].Provides, ",") != "alpha.svc" {
		t.Fatalf("alpha provides %v", got["alpha"].Provides)
	}
	if strings.Join(got["beta"].Permissions, ",") != "net" || strings.Join(got["beta"].BrokerCalls, ",") != "net" {
		t.Fatalf("beta %+v", got["beta"])
	}
	if _, ok := got["std"]; ok {
		t.Fatal("std must be skipped")
	}
}

func TestRealModulesTreeLogsOnly(t *testing.T) {
	fs, err := Check("../../modules")
	if err != nil {
		t.Skipf("modules tree not available: %v", err)
	}
	t.Logf("modules/ findings: %d errors", countErrors(fs))
	for _, p := range pairs(fs) {
		t.Logf("  %s", p)
	}
}

func countErrors(fs []Finding) int {
	n := 0
	for _, f := range fs {
		if !f.Warning {
			n++
		}
	}
	return n
}

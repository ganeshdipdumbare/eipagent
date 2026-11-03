package pluginpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ganeshdipdumbare/eipagent/engine"
)

func TestTemplatesLintClean(t *testing.T) {
	for _, kind := range TemplateNames() {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "my_"+kind)
			if _, err := Init(dir, "my_"+kind, kind); err != nil {
				t.Fatal(err)
			}
			src, err := ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			r := Lint(src)
			for _, i := range r.Issues {
				t.Log(i)
			}
			if !r.OK() || r.State != StateValidated {
				t.Fatalf("template %s does not validate: %v", kind, r.Err())
			}
			if _, _, err := Build(src, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, "a", "tool"); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir, "a", "tool"); err == nil {
		t.Fatal("expected refusal to overwrite")
	}
}

const goodSrc = `//go:build ignore

package plugin

import (
	"context"

	"eipagent/sdk"
)

// Greets people.
var Inject = []string{"?secrets"}

func Apply(ctx *sdk.Context) error {
	ctx.AddTool(sdk.Tool{Name: "greet", Run: func(c context.Context, a map[string]any) (string, error) { return "hi", nil }})
	return nil
}
`

func source(name, code string, m *Manifest) *Source {
	return &Source{Name: name, Manifest: m, Files: map[string][]byte{name + ".go": []byte(code)}}
}

func manifest(name string) *Manifest {
	return &Manifest{Schema: 1, Name: name, Version: "1.0.0", Description: "test", Inject: []string{"?secrets"}, Tools: []string{"greet"}}
}

func rules(r *Report) map[string]Level {
	out := map[string]Level{}
	for _, i := range r.Issues {
		if out[i.Rule] != Error {
			out[i.Rule] = i.Level
		}
	}
	return out
}

func TestLintRules(t *testing.T) {
	cases := []struct {
		name, code string
		edit       func(*Manifest)
		rule       string
	}{
		{"forbidden import", strings.Replace(goodSrc, `"context"`, "\"context\"\n\t\"unsafe\"", 1) + "\nvar _ = unsafe.Sizeof(0)\n", nil, "forbidden-import"},
		{"missing net", strings.Replace(goodSrc, `"context"`, "\"context\"\n\t\"net/http\"", 1) + "\nvar _ = http.Get\n", nil, "permission-missing"},
		{"os env without permission", strings.Replace(goodSrc, `"context"`, "\"context\"\n\t\"os\"", 1) + "\nvar home = os.Getenv(\"HOME\")\n", nil, "permission-missing"},
		{"manifest permission mismatch", goodSrc, func(m *Manifest) { m.Permissions = []string{"net"} }, "permission-mismatch"},
		{"undeclared service", strings.Replace(goodSrc, "return nil\n}", "_ = ctx.Service(\"store\")\n\treturn nil\n}", 1), nil, "undeclared-service"},
		{"bad secret ref", strings.Replace(goodSrc, "return nil\n}", "_, _ = ctx.Secret(\"secret://bad name\")\n\treturn nil\n}", 1), nil, "secret-ref"},
		{"undeclared secret", strings.Replace(goodSrc, "return nil\n}", "_, _ = ctx.Secret(\"secret://API_KEY\")\n\treturn nil\n}", 1), nil, "secret-ref"},
		{"bad version", goodSrc, func(m *Manifest) { m.Version = "v1" }, "manifest"},
		{"incompatible", goodSrc, func(m *Manifest) { m.EIPAgent = ">=99.0.0" }, "compat"},
		{"tool not in manifest", goodSrc, func(m *Manifest) { m.Tools = nil }, "tools"},
		{"requires mismatch", goodSrc, func(m *Manifest) { m.Inject = nil }, "requires-mismatch"},
		{"no Apply", strings.Replace(goodSrc, "func Apply", "func apply", 1), nil, "source"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := manifest("p")
			if c.edit != nil {
				c.edit(m)
			}
			r := Lint(source("p", c.code, m))
			if rules(r)[c.rule] != Error {
				t.Fatalf("want error %s, got %v", c.rule, r.Issues)
			}
			if r.State != StateDraft {
				t.Fatalf("invalid plugin should stay DRAFT, got %s", r.State)
			}
		})
	}
	if r := Lint(source("p", goodSrc, manifest("p"))); !r.OK() {
		t.Fatalf("good plugin: %v", r.Issues)
	}
}

func TestCompatible(t *testing.T) {
	for c, want := range map[string]bool{">=0.2.0": true, ">=0.2.0, <1.0.0": true, "<0.2.0": false, "^0.2.0": true, "^0.1.0": false, "~0.2.0": true, "0.2.5": true, "0.2.0": false, "*": true} {
		got, err := Compatible(c, "0.2.5")
		if err != nil || got != want {
			t.Errorf("%q: got %v %v, want %v", c, got, err, want)
		}
	}
}

func buildPkg(t *testing.T, version, code string, signWith string) []byte {
	t.Helper()
	m := manifest("greet")
	m.Version = version
	var key []byte
	_ = key
	src := source("greet", code, m)
	if signWith == "" {
		b, _, err := Build(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	sk, err := ParsePrivateKey(signWith)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := Build(src, sk)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPackageBuildOpenVerify(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := TrustedKeys(pub)
	signed := buildPkg(t, "1.0.0", goodSrc, priv)
	if again := buildPkg(t, "1.0.0", goodSrc, priv); !bytes.Equal(signed, again) {
		t.Fatal("build is not reproducible")
	}
	pkg, err := Open(signed)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Verify(pkg, VerifyOptions{TrustedKeys: keys, RequireSignature: true, SHA256: pkg.SHA256})
	if err != nil || v.Signer == "" {
		t.Fatalf("verify: %v %+v", err, v)
	}
	// Wrong pin.
	if _, err := Verify(pkg, VerifyOptions{SHA256: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("pin mismatch accepted")
	}
	// Unsigned when signatures are required.
	unsigned, _ := Open(buildPkg(t, "1.0.0", goodSrc, ""))
	if _, err := Verify(unsigned, VerifyOptions{TrustedKeys: keys, RequireSignature: true}); err == nil {
		t.Fatal("unsigned package accepted")
	}
	// Signed by someone else.
	_, other, _ := GenerateKey()
	foreign, _ := Open(buildPkg(t, "1.0.0", goodSrc, other))
	if _, err := Verify(foreign, VerifyOptions{TrustedKeys: keys}); err == nil {
		t.Fatal("untrusted signature accepted")
	}
	// Tampered source inside the archive.
	if _, err := Open(rewrite(t, signed, "greet.go", []byte(strings.Replace(goodSrc, `"hi"`, `"pwned"`, 1)))); err == nil {
		t.Fatal("tampered file accepted")
	}
	// Tampered manifest (escalating permissions) breaks the signature.
	pm := *pkg.Manifest
	pm.Permissions = []string{"exec"}
	tm, err := Open(rewrite(t, signed, ManifestFile, pm.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(tm, VerifyOptions{TrustedKeys: keys}); err == nil {
		t.Fatal("tampered manifest accepted")
	}
}

func TestOpenRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "../evil.go", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	if _, err := Open(buf.Bytes()); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("want unsafe name error, got %v", err)
	}
}

// rewrite replaces one file in a package archive, keeping the rest.
func rewrite(t *testing.T, data []byte, name string, content []byte) []byte {
	t.Helper()
	gz, _ := gzip.NewReader(bytes.NewReader(data))
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = b
	}
	files[name] = content
	out, err := writeArchive(files)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type fakeRuntime struct {
	active map[string]string
	fail   bool
}

func (f *fakeRuntime) Activate(name, hash string) error {
	if f.fail {
		return errors.New("boom")
	}
	f.active[name] = hash
	return nil
}
func (f *fakeRuntime) Deactivate(name string) error { delete(f.active, name); return nil }

func TestStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	rt := &fakeRuntime{active: map[string]string{}}
	st, err := OpenStore(dir, StoreOptions{Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	v1, err := st.Install(buildPkg(t, "1.0.0", goodSrc, ""), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if v1.State != StateValidated {
		t.Fatalf("install should hold the version: %s", v1.State)
	}
	if err := st.Enable("greet", "1.0.0"); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("enable before approval: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.ActiveDir(), "greet.go")); err == nil {
		t.Fatal("unapproved plugin is in the active dir")
	}
	must(t, st.Approve("greet", "1.0.0", "bob"))
	must(t, st.Enable("greet", ""))
	if rt.active["greet"] != v1.Source || st.Pins()["greet"] != v1.Source {
		t.Fatal("not activated / pinned")
	}
	// Update: approved v2 replaces v1, v1 becomes the rollback target.
	st.opts.AutoApprove = true
	v2code := strings.Replace(goodSrc, `"hi"`, `"hello"`, 1)
	v2, err := st.Update(buildPkg(t, "1.1.0", v2code, ""), "alice")
	if err != nil || v2.State != StateActive {
		t.Fatalf("update: %v %+v", err, v2)
	}
	p, _ := st.Get("greet")
	if p.Active != "1.1.0" || p.Previous != "1.0.0" {
		t.Fatalf("after update: %+v", p)
	}
	b, _ := os.ReadFile(filepath.Join(st.ActiveDir(), "greet.go"))
	if !strings.Contains(string(b), "hello") {
		t.Fatal("active source not updated")
	}
	// A failing update keeps the running version.
	rt.fail = true
	if _, err := st.Update(buildPkg(t, "1.2.0", strings.Replace(goodSrc, `"hi"`, `"bad"`, 1), ""), "alice"); err == nil {
		t.Fatal("expected activation failure")
	}
	rt.fail = false
	p, _ = st.Get("greet")
	if p.Active != "1.1.0" || p.Versions["1.2.0"].State != StateFailed {
		t.Fatalf("failed update changed state: active=%s 1.2.0=%s", p.Active, p.Versions["1.2.0"].State)
	}
	b, _ = os.ReadFile(filepath.Join(st.ActiveDir(), "greet.go"))
	if !strings.Contains(string(b), "hello") {
		t.Fatal("failed update left its source in the active dir")
	}
	// Rollback.
	must(t, st.Rollback("greet"))
	p, _ = st.Get("greet")
	if p.Active != "1.0.0" || p.Versions["1.1.0"].State != StateApproved {
		t.Fatalf("rollback: %+v", p)
	}
	// Quarantine the active version: it stops and can't be re-enabled.
	must(t, st.Quarantine("greet", "1.0.0", "CVE-1234"))
	if _, ok := rt.active["greet"]; ok {
		t.Fatal("quarantined plugin still active")
	}
	if err := st.Enable("greet", "1.0.0"); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("enable quarantined: %v", err)
	}
	// State survives reopening.
	st2, _ := OpenStore(dir, StoreOptions{})
	p, _ = st2.Get("greet")
	if p.Versions["1.0.0"].State != StateQuarantined || p.Versions["1.0.0"].Reason != "CVE-1234" {
		t.Fatalf("reopened: %+v", p.Versions["1.0.0"])
	}
	// Tampering at rest is caught on enable.
	must(t, st.Approve("greet", "1.1.0", "bob"))
	pkgPath := filepath.Join(dir, "packages", "greet", "1.1.0.eip")
	raw, _ := os.ReadFile(pkgPath)
	os.WriteFile(pkgPath, rewrite(t, raw, "README.md", []byte("x")), 0o600)
	if err := st.Enable("greet", "1.1.0"); err == nil {
		t.Fatal("tampered stored package enabled")
	}
	p, _ = st.Get("greet")
	if p.Versions["1.1.0"].State != StateQuarantined {
		t.Fatalf("tampered version should be quarantined: %s", p.Versions["1.1.0"].State)
	}
	must(t, st.Uninstall("greet"))
	if _, err := st.Get("greet"); !errors.Is(err, ErrNotFound) {
		t.Fatal("uninstall left the plugin")
	}
}

func TestStoreRefusesReservedAndUnsigned(t *testing.T) {
	pub, _, _ := GenerateKey()
	keys, _ := TrustedKeys(pub)
	st, _ := OpenStore(t.TempDir(), StoreOptions{Reserved: []string{"greet"}})
	if _, err := st.Install(buildPkg(t, "1.0.0", goodSrc, ""), "x"); err == nil {
		t.Fatal("reserved name installed")
	}
	st, _ = OpenStore(t.TempDir(), StoreOptions{Verify: VerifyOptions{TrustedKeys: keys, RequireSignature: true}})
	if _, err := st.Install(buildPkg(t, "1.0.0", goodSrc, ""), "x"); err == nil {
		t.Fatal("unsigned installed in secure mode")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestExamplePluginsValidate(t *testing.T) {
	dirs, _ := filepath.Glob("../examples/plugins/*")
	if len(dirs) < 7 {
		t.Fatalf("expected the example plugins, found %v", dirs)
	}
	for _, d := range dirs {
		src, err := ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		if r := Lint(src); !r.OK() {
			t.Errorf("%s: %v", d, r.Err())
		}
	}
}

// Someone who can write to the store directory (but has no trusted key)
// swaps in their own package and edits state.json to match: enable must
// refuse it because the signature is checked again.
func TestEnableRechecksSignature(t *testing.T) {
	pub, priv, _ := GenerateKey()
	keys, _ := TrustedKeys(pub)
	opts := StoreOptions{Verify: VerifyOptions{TrustedKeys: keys, RequireSignature: true}}
	dir := t.TempDir()
	st, _ := OpenStore(dir, opts)
	v, err := st.Install(buildPkg(t, "1.0.0", goodSrc, priv), "alice")
	must(t, err)
	must(t, st.Approve("greet", "1.0.0", "bob"))

	_, evil, _ := GenerateKey()
	bad := buildPkg(t, "1.0.0", strings.Replace(goodSrc, `"hi"`, `"pwned"`, 1), evil)
	pkg, _ := Open(bad)
	os.WriteFile(filepath.Join(dir, "packages", "greet", "1.0.0.eip"), bad, 0o600)
	state, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	state = bytes.ReplaceAll(state, []byte(v.SHA256), []byte(pkg.SHA256))
	os.WriteFile(filepath.Join(dir, "state.json"), state, 0o600)

	st2, _ := OpenStore(dir, opts)
	if err := st2.Enable("greet", "1.0.0"); err == nil {
		t.Fatal("a package re-signed by an untrusted key was enabled")
	}
	p, _ := st2.Get("greet")
	if p.Versions["1.0.0"].State != StateQuarantined {
		t.Fatalf("want QUARANTINED, got %s", p.Versions["1.0.0"].State)
	}
	if _, err := os.Stat(filepath.Join(st2.ActiveDir(), "greet.go")); err == nil {
		t.Fatal("tampered source reached the active dir")
	}
}

func TestGetReturnsACopy(t *testing.T) {
	st, _ := OpenStore(t.TempDir(), StoreOptions{})
	_, err := st.Install(buildPkg(t, "1.0.0", goodSrc, ""), "a")
	must(t, err)
	p, _ := st.Get("greet")
	p.Versions["1.0.0"].State = StateApproved // try to skip approval
	if err := st.Enable("greet", "1.0.0"); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("state changed through a returned copy: %v", err)
	}
}

func TestFixRewritesOldNames(t *testing.T) {
	old := `//go:build ignore

package plugin

import (
	"context"

	"eipagent/sdk"
)

// Uses "store" in a comment, which stays.
var Inject = []string{"?store", "llm/*"}

func Apply(ctx *sdk.Context) error {
	ctx.OnAgentStart(func(c context.Context, st *sdk.RunState) error { return nil })
	ctx.OnDispose(func() {})
	_ = ctx.Service("store")
	label := "store" // not a service name: stays
	_ = label
	return nil
}
`
	out, notes, err := Fix([]byte(old))
	must(t, err)
	s := string(out)
	for _, want := range []string{`var Requires = []string{"?memory", "model/*"}`, `func Apply(ctx *sdk.Plugin) error`, `ctx.OnRunStart(`, `ctx.OnUnload(`, `ctx.Service("memory")`, `label := "store"`, `Uses "store" in a comment`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if len(notes) < 5 {
		t.Fatalf("notes: %v", notes)
	}
	if err := engine.Check("p", out); err != nil {
		t.Fatalf("fixed source doesn't compile: %v", err)
	}
	fa, _ := Analyze("p.go", out)
	if got := Summary("p", fa); !strings.Contains(got, "runs at run start") || !strings.Contains(got, "requires ?memory, model/*") {
		t.Fatalf("summary: %s", got)
	}
}

func TestInstallSourceVersionsAndReports(t *testing.T) {
	pub, priv, _ := GenerateKey()
	keys, _ := TrustedKeys(pub)
	sk, _ := ParsePrivateKey(priv)
	st, _ := OpenStore(t.TempDir(), StoreOptions{Verify: VerifyOptions{TrustedKeys: keys, RequireSignature: true}})
	v1, rep, err := st.InstallSource("greet", []byte(goodSrc), sk, "sam")
	if err != nil || v1.Version != "0.1.0" || v1.State != StateValidated || !rep.OK() {
		t.Fatalf("first: %v %+v", err, v1)
	}
	v2, _, err := st.InstallSource("greet", []byte(strings.Replace(goodSrc, `"hi"`, `"hello"`, 1)), sk, "sam")
	if err != nil || v2.Version != "0.1.1" {
		t.Fatalf("update: %v %+v", err, v2)
	}
	_, rep, err = st.InstallSource("greet", []byte("package plugin\n\nimport \"os/exec\"\n\nvar _ = exec.Command\n"), sk, "sam")
	if err == nil || rep == nil || rep.OK() {
		t.Fatal("bad source installed, or no report for the UI")
	}
}

func TestTemplatesCompile(t *testing.T) {
	for _, kind := range TemplateNames() {
		t.Run(kind, func(t *testing.T) {
			src, err := RenderTemplate(kind, "my_"+kind)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Check("my_"+kind, src); err != nil {
				t.Fatalf("template %s does not compile: %v", kind, err)
			}
		})
	}
}

func TestQuarantineRequiresVersion(t *testing.T) {
	st, _ := OpenStore(t.TempDir(), StoreOptions{})
	must(t, func() error { _, err := st.Install(buildPkg(t, "1.0.0", goodSrc, ""), "a"); return err }())
	if err := st.Quarantine("greet", "", "because"); err == nil {
		t.Fatal("an empty version was quarantined")
	}
}

func TestEnableEmptyDoesNotDowngrade(t *testing.T) {
	st, _ := OpenStore(t.TempDir(), StoreOptions{})
	must(t, func() error { _, err := st.Install(buildPkg(t, "1.0.0", goodSrc, ""), "a"); return err }())
	must(t, st.Approve("greet", "1.0.0", "a"))
	must(t, st.Enable("greet", "1.0.0"))
	must(t, func() error {
		_, err := st.Install(buildPkg(t, "1.1.0", strings.Replace(goodSrc, `"hi"`, `"hello"`, 1), ""), "a")
		return err
	}())
	must(t, st.Approve("greet", "1.1.0", "a"))
	must(t, st.Enable("greet", "1.1.0"))
	must(t, st.Enable("greet", ""))
	p, _ := st.Get("greet")
	if p.Active != "1.1.0" {
		t.Fatalf("enable with no version downgraded to %s", p.Active)
	}
}

func TestSemverPrecedenceIsDeterministic(t *testing.T) {
	if !semverLess("1.0.0-rc.2", "1.0.0-rc.10") {
		t.Fatal("rc.2 should sort before rc.10")
	}
	if !semverLess("1.0.0-rc.1", "1.0.0") {
		t.Fatal("a prerelease should sort before the release")
	}
	p := &Plugin{Versions: map[string]*Version{
		"1.0.0":      {Version: "1.0.0", State: StateApproved},
		"1.0.0-rc.1": {Version: "1.0.0-rc.1", State: StateApproved},
	}}
	for i := 0; i < 20; i++ {
		if got := newest(p, StateApproved); got != "1.0.0" {
			t.Fatalf("newest = %q", got)
		}
	}
}

func TestBuildRejectsReservedSourceNames(t *testing.T) {
	src := &Source{Name: "greet", Manifest: &Manifest{Name: "greet", Version: "1.0.0"},
		Files: map[string][]byte{ManifestFile: []byte("x")}}
	if _, _, err := Build(src, nil); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved manifest name accepted: %v", err)
	}
	src.Files = map[string][]byte{SignatureFile: []byte("x")}
	if _, _, err := Build(src, nil); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved signature name accepted: %v", err)
	}
}

func TestReadDirSkipsSignature(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "greet.go"), []byte(goodSrc), 0o600)
	os.WriteFile(filepath.Join(dir, SignatureFile), []byte("{}"), 0o600)
	src, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.Files[SignatureFile]; ok {
		t.Fatal("ReadDir kept plugin.sig")
	}
}

func TestVerifySignedWithoutTrustedKeys(t *testing.T) {
	_, priv, _ := GenerateKey()
	pkg, _ := Open(buildPkg(t, "1.0.0", goodSrc, priv))
	if _, err := Verify(pkg, VerifyOptions{}); err == nil {
		t.Fatal("a signed package with no trusted keys was accepted")
	}
	if _, err := Verify(pkg, VerifyOptions{AllowUntrustedSignature: true}); err != nil {
		t.Fatalf("explicit opt-in refused: %v", err)
	}
}

func TestInstallSourceConcurrent(t *testing.T) {
	st, _ := OpenStore(t.TempDir(), StoreOptions{})
	codes := [][]byte{[]byte(goodSrc), []byte(strings.Replace(goodSrc, `"hi"`, `"hello"`, 1))}
	var wg sync.WaitGroup
	errs := make(chan error, len(codes))
	for _, code := range codes {
		wg.Add(1)
		go func(c []byte) {
			defer wg.Done()
			_, _, err := st.InstallSource("greet", c, nil, "sam")
			errs <- err
		}(code)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, _ := st.Get("greet")
	if len(p.Versions) != 2 {
		t.Fatalf("versions: %v", p.Versions)
	}
}

type failingRuntime struct{}

func (failingRuntime) Activate(string, string) error { return errors.New("start failed") }
func (failingRuntime) Deactivate(string) error       { return nil }

func TestEnableRestoresStateWhenStartFails(t *testing.T) {
	dir := t.TempDir()
	st, _ := OpenStore(dir, StoreOptions{Runtime: failingRuntime{}})
	must(t, func() error { _, err := st.Install(buildPkg(t, "1.0.0", goodSrc, ""), "a"); return err }())
	must(t, st.Approve("greet", "1.0.0", "a"))
	if err := st.Enable("greet", "1.0.0"); err == nil {
		t.Fatal("a failing runtime was reported as success")
	}
	p, _ := st.Get("greet")
	if p.Active != "" || p.Versions["1.0.0"].State != StateFailed {
		t.Fatalf("state not restored: active=%q state=%s", p.Active, p.Versions["1.0.0"].State)
	}
	if _, err := os.Stat(filepath.Join(st.ActiveDir(), "greet.go")); err == nil {
		t.Fatal("failed source was left in the active dir")
	}
	if b, err := os.ReadFile(st.LockPath()); err == nil && strings.Contains(string(b), "greet") {
		t.Fatalf("failed version stayed pinned: %s", b)
	}
}

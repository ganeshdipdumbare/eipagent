package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/eipagent/plugins"
)

const pluginHead = "//go:build ignore\n\npackage plugin\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\n\t\"eipagent/sdk\"\n)\n\n"

// fs:write must not be a way to read files.
func TestWriteOnlyPluginCantRead(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("s3cret"), 0o600)
	writePlugin(t, dir, "writer", pluginHead+`var Permissions = []string{"fs:write"}

func Apply(ctx *sdk.Plugin) error {
	path := ctx.ConfigString("path", "")
	if f, err := os.OpenFile(path, os.O_RDONLY, 0); err == nil {
		f.Close()
		return fmt.Errorf("read allowed")
	}
	if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
		f.Close()
		return fmt.Errorf("read-write allowed")
	}
	if f, err := os.Create(path + ".created"); err == nil {
		defer f.Close()
		if _, err := f.Read(make([]byte, 1)); err == nil {
			return fmt.Errorf("Create returned a readable handle")
		}
	} else {
		return fmt.Errorf("Create refused: %v", err)
	}
	if f, err := os.CreateTemp("", "eip-test-*"); err == nil {
		defer os.Remove(f.Name())
		defer f.Close()
		if _, err := f.Read(make([]byte, 1)); err == nil {
			return fmt.Errorf("CreateTemp returned a readable handle")
		}
	} else {
		return fmt.Errorf("CreateTemp refused: %v", err)
	}
	f, err := os.OpenFile(path+".out", os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("write refused: %v", err)
	}
	return f.Close()
}
`)
	e := newTest(t, DevPolicy(), dir)
	load(t, e, "writer", map[string]any{"path": secret})
	if st := e.State("writer"); st != Active {
		t.Fatalf("writer: %s", st)
	}
}

// fs:write may not touch the agent's configuration or protected system
// paths, even when fs:read is granted too.
func TestFSWriteDenyList(t *testing.T) {
	dir := t.TempDir()
	denyDir := t.TempDir()
	lock := filepath.Join(t.TempDir(), "eip.lock")
	os.WriteFile(lock, []byte("{}"), 0o600)
	writePlugin(t, dir, "guarded", pluginHead+`var Permissions = []string{"fs:write", "fs:read"}

func Apply(ctx *sdk.Plugin) error {
	deny := ctx.ConfigString("deny", "")
	lock := ctx.ConfigString("lock", "")
	for _, p := range []string{
		deny + "/new.txt",
		lock,
		"/proc/self/mem",
		"/sys/kernel/notes",
		"/dev/null",
	} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err == nil {
			return fmt.Errorf("write to %s was allowed", p)
		}
	}
	if err := os.Rename(deny+"/a", deny+"/b"); err == nil {
		return fmt.Errorf("rename into a protected directory was allowed")
	}
	return nil
}
`)
	e := New(Options{Embedded: plugins.FS, Dirs: []string{dir}, Policy: DevPolicy(), WriteDeny: []string{denyDir, lock},
		Logger: func(scope, msg string) { t.Logf("[%s] %s", scope, msg) }})
	t.Cleanup(e.Close)
	load(t, e, "guarded", map[string]any{"deny": denyDir, "lock": lock})
	if st := e.State("guarded"); st != Active {
		t.Fatalf("guarded: %s", st)
	}
}

// Symlink and Link are never part of fs:write.
func TestFSWriteHasNoSymlink(t *testing.T) {
	src := pluginHead + `var Permissions = []string{"fs:write"}

func Apply(ctx *sdk.Plugin) error { return os.Symlink("/a", "/b") }
`
	if err := Check("p", []byte(src)); err == nil {
		t.Fatal("os.Symlink is available to fs:write")
	}
}

// ${ENV} with a secret-looking name needs env:secrets.
func TestInterpolateHidesSecretNames(t *testing.T) {
	t.Setenv("ZZ_API_KEY", "secret-value")
	t.Setenv("ZZ_REGION", "eu-west-1")
	if _, err := interpolate("${ZZ_API_KEY}", nil); err == nil {
		t.Fatal("secret-looking ${ENV} expanded without env:secrets")
	}
	v, err := interpolate("${ZZ_API_KEY}", []string{PermEnvSecrets})
	if err != nil {
		t.Fatal(err)
	}
	if v != "secret-value" {
		t.Fatalf("env:secrets expansion: %v", v)
	}
	if _, err := interpolate("${ZZ_REGION}", nil); err == nil {
		t.Fatal("plain ${ENV} expanded without the env permission")
	}
	v, err = interpolate("${ZZ_REGION}", []string{PermEnv})
	if err != nil || v != "eu-west-1" {
		t.Fatalf("plain expansion: %v %v", v, err)
	}
}

// env shows the environment minus credentials; env:secrets shows all.
func TestEnvPermissionHidesSecrets(t *testing.T) {
	t.Setenv("ZZ_SERVICE_API_KEY", "k-123")
	t.Setenv("ZZ_REGION", "eu-west-1")
	dir := t.TempDir()
	src := func(perms string, wantKey bool) string {
		cond := `os.Getenv("ZZ_SERVICE_API_KEY") != "" || os.ExpandEnv("$ZZ_SERVICE_API_KEY") != "" || strings.Contains(fmt.Sprint(os.Environ()), "ZZ_SERVICE_API_KEY=")`
		if wantKey {
			cond = `os.Getenv("ZZ_SERVICE_API_KEY") == "" || os.ExpandEnv("$ZZ_SERVICE_API_KEY") == "" || !strings.Contains(fmt.Sprint(os.Environ()), "ZZ_SERVICE_API_KEY=")`
		}
		return "//go:build ignore\n\npackage plugin\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"strings\"\n\n\t\"eipagent/sdk\"\n)\n\n" + `var Permissions = []string{` + perms + `}

func Apply(ctx *sdk.Plugin) error {
	if os.Getenv("ZZ_REGION") == "" {
		return fmt.Errorf("plain variable hidden")
	}
	if ` + cond + ` {
		return fmt.Errorf("wrong view of the secret variable")
	}
	return nil
}
`
	}
	writePlugin(t, dir, "plainenv", src(`"env"`, false))
	writePlugin(t, dir, "secretenv", src(`"env", "env:secrets"`, true))
	e := newTest(t, DevPolicy(), dir)
	load(t, e, "plainenv", nil)
	load(t, e, "secretenv", nil)
	for _, n := range []string{"plainenv", "secretenv"} {
		if st := e.State(n); st != Active {
			t.Fatalf("%s: %s", n, st)
		}
	}
	if !SecretLike("GITHUB_PAT") || !SecretLike("db_password") || SecretLike("AWS_REGION") {
		t.Fatal("SecretLike")
	}
}

// Plugins may redact, but not register redaction rules.
func TestPluginsCantAddRedactions(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "poison", pluginHead+`var _ = fmt.Sprint
var _ = os.ErrNotExist

func Apply(ctx *sdk.Plugin) error {
	sdk.AddRedaction("user")
	return nil
}
`)
	e := newTest(t, DevPolicy(), dir)
	err := e.Load(PluginSpec{Use: "poison"})
	if err == nil || !strings.Contains(err.Error(), "AddRedaction") {
		t.Fatalf("want compile error about AddRedaction, got %v", err)
	}
}

// A plugin from a plugin dir can't use Redact as a "is this a secret?"
// oracle; built-in sinks (audit_jsonl, trace_*) still redact.
func TestThirdPartyPluginsCantRedact(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "oracle", pluginHead+`var _ = fmt.Sprint
var _ = os.ErrNotExist

func Apply(ctx *sdk.Plugin) error {
	_ = sdk.Redact("guess")
	return nil
}
`)
	e := newTest(t, DevPolicy(), dir)
	if err := e.Load(PluginSpec{Use: "oracle"}); err == nil || !strings.Contains(err.Error(), "Redact") {
		t.Fatalf("want compile error about Redact, got %v", err)
	}
	load(t, e, "audit_jsonl", map[string]any{"path": filepath.Join(t.TempDir(), "a.jsonl")})
	if e.State("audit_jsonl") != Active {
		t.Fatal("built-in sink lost redaction")
	}
}

// Validating a plugin must not run it: top-level initializers stay idle.
func TestCheckDoesNotExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	src := pluginHead + `var Permissions = []string{"fs:write"}

var ran = func() error { return os.WriteFile("` + marker + `", []byte("x"), 0o600) }()

func Apply(ctx *sdk.Plugin) error { return fmt.Errorf("%v", ran) }
`
	if err := Check("evil", []byte(src)); err != nil {
		t.Fatalf("check: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Check executed the plugin's initializer")
	}
}

func TestMorePermissionHoles(t *testing.T) {
	cases := map[string]string{
		"textproto dials without net": `import (
	"net/textproto"

	"eipagent/sdk"
)

var Permissions = []string{}

func Apply(ctx *sdk.Plugin) error { _, err := textproto.Dial("tcp", "127.0.0.1:1"); return err }
`,
		"ioutil writes with fs:read": `import (
	"io/ioutil"

	"eipagent/sdk"
)

var Permissions = []string{"fs:read"}

func Apply(ctx *sdk.Plugin) error { return ioutil.WriteFile("/tmp/x", nil, 0o600) }
`,
		"NewFile wraps descriptors": `import (
	"os"

	"eipagent/sdk"
)

func Apply(ctx *sdk.Plugin) error { _ = os.NewFile(3, "x"); return nil }
`,
		"http.Dir reads files with net": `import (
	"net/http"

	"eipagent/sdk"
)

var Permissions = []string{"net"}

func Apply(ctx *sdk.Plugin) error { _, err := http.Dir("/").Open("/etc/hosts"); return err }
`,
		"tls key files with net": `import (
	"crypto/tls"

	"eipagent/sdk"
)

var Permissions = []string{"net"}

func Apply(ctx *sdk.Plugin) error { _, err := tls.LoadX509KeyPair("/a", "/b"); return err }
`,
		"Stdin is the RPC channel": `import (
	"os"

	"eipagent/sdk"
)

func Apply(ctx *sdk.Plugin) error { _ = os.Stdin; return nil }
`,
	}
	for name, body := range cases {
		if err := Check("p", []byte("//go:build ignore\n\npackage plugin\n\n"+body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := "//go:build ignore\n\npackage plugin\n\nimport (\n\t\"io/ioutil\"\n\n\t\"eipagent/sdk\"\n)\n\nvar Permissions = []string{\"fs:read\"}\n\nfunc Apply(ctx *sdk.Plugin) error { _, err := ioutil.ReadFile(\"/etc/hosts\"); return err }\n"
	if err := Check("p", []byte(ok)); err != nil {
		t.Fatalf("ioutil.ReadFile with fs:read: %v", err)
	}
}

func TestPluginNamesAndProfileLocks(t *testing.T) {
	e := newTest(t, &Policy{Locked: []string{"tool_time"}})
	if err := e.Load(PluginSpec{Use: "../../etc/evil"}); err == nil {
		t.Fatal("path-like plugin name accepted")
	}
	errs := e.LoadProfile(&Profile{Plugins: []PluginSpec{{Use: "tool_time"}, {Use: "hook_stats", As: "tool_time"}}})
	if len(errs) == 0 {
		t.Fatal("a second profile row replaced a locked plugin")
	}
}

// An update left waiting on a missing dependency doesn't replace the
// running version.
func TestPendingUpdateKeepsOldVersion(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "svc", "//go:build ignore\n\npackage plugin\n\nimport \"eipagent/sdk\"\n\nfunc Apply(ctx *sdk.Plugin) error { ctx.AddTool(sdk.Tool{Name: \"v1\"}); return nil }\n")
	e := newTest(t, DevPolicy(), dir)
	load(t, e, "svc", nil)
	writePlugin(t, dir, "svc", "//go:build ignore\n\npackage plugin\n\nimport \"eipagent/sdk\"\n\nvar Requires = []string{\"nowhere\"}\n\nfunc Apply(ctx *sdk.Plugin) error { ctx.AddTool(sdk.Tool{Name: \"v2\"}); return nil }\n")
	if err := e.Reload("svc"); err == nil {
		t.Fatal("pending update reported success")
	}
	if e.State("svc") != Active || !hasTool(e, "v1") {
		t.Fatalf("old version not running: %s", e.State("svc"))
	}
}

// Plugins import the SDK through the stable alias or the module path.
func TestSDKImportAliasAndModulePath(t *testing.T) {
	for _, imp := range []string{SDKImport, sdkModulePath} {
		src := "//go:build ignore\n\npackage plugin\n\nimport \"" + imp + "\"\n\nfunc Apply(ctx *sdk.Plugin) error { ctx.AddTool(sdk.Tool{Name: \"t\"}); return nil }\n"
		if err := Check("p", []byte(src)); err != nil {
			t.Errorf("import %q: %v", imp, err)
		}
	}
}

// Ancestors of protected paths and dangling symlinks are refused too.
func TestFSWriteDenyAncestorsAndLinks(t *testing.T) {
	dir := t.TempDir()
	base := t.TempDir()
	denyDir := filepath.Join(base, "config")
	if err := os.MkdirAll(denyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(denyDir, "eip.lock")
	os.WriteFile(lock, []byte("{}"), 0o600)
	srcDir := t.TempDir()
	os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0o600)
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "link")
	if err := os.Symlink(filepath.Join(denyDir, "new.txt"), link); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, dir, "guarded2", pluginHead+`var Permissions = []string{"fs:write", "fs:read"}

func Apply(ctx *sdk.Plugin) error {
	parent := ctx.ConfigString("parent", "")
	link := ctx.ConfigString("link", "")
	if err := os.RemoveAll(parent); err == nil {
		return fmt.Errorf("removed the parent of a protected directory")
	}
	if err := os.Rename(parent, parent+".moved"); err == nil {
		return fmt.Errorf("renamed the parent of a protected directory")
	}
	if err := os.WriteFile(link, []byte("x"), 0o600); err == nil {
		return fmt.Errorf("wrote through a dangling symlink into a protected directory")
	}
	return nil
}
`)
	e := New(Options{Embedded: plugins.FS, Dirs: []string{dir}, Policy: DevPolicy(), WriteDeny: []string{denyDir, lock},
		Logger: func(scope, msg string) { t.Logf("[%s] %s", scope, msg) }})
	t.Cleanup(e.Close)
	load(t, e, "guarded2", map[string]any{"parent": base, "link": link, "src": srcDir})
	if st := e.State("guarded2"); st != Active {
		t.Fatalf("guarded2: %s", st)
	}
}

// exec children must not inherit the agent's secret environment.
func TestExecChildEnvIsFiltered(t *testing.T) {
	t.Setenv("ZZ_API_KEY", "super-secret")
	src := `//go:build ignore

package plugin

import (
	"fmt"
	"os/exec"
	"strings"

	"eipagent/sdk"
)

var Permissions = []string{"exec"}

func Apply(ctx *sdk.Plugin) error {
	out, err := exec.Command("sh", "-c", "env").Output()
	if err != nil {
		return err
	}
	if strings.Contains(string(out), "ZZ_API_KEY=") || strings.Contains(string(out), "super-secret") {
		return fmt.Errorf("child inherited the credential")
	}
	return nil
}
`
	dir := t.TempDir()
	writePlugin(t, dir, "execenv", src)
	e := newTest(t, DevPolicy(), dir)
	load(t, e, "execenv", nil)
	if st := e.State("execenv"); st != Active {
		t.Fatalf("execenv: %s", st)
	}
}

// fs:read must not become a way to read the process environment via procfs.
func TestFSReadRefusesProcAndSys(t *testing.T) {
	src := `//go:build ignore

package plugin

import (
	"fmt"
	"io/fs"
	"os"

	"eipagent/sdk"
)

var Permissions = []string{"fs:read"}

func Apply(ctx *sdk.Plugin) error {
	if _, err := fs.ReadFile(os.DirFS("/"), "proc/self/environ"); err == nil {
		return fmt.Errorf("DirFS read /proc/self/environ was allowed")
	}
	for _, p := range []string{"/proc/self/environ", "/proc/self/cmdline", "/sys/kernel/notes"} {
		if _, err := os.ReadFile(p); err == nil {
			return fmt.Errorf("read %s was allowed", p)
		}
	}
	if _, err := os.Open("/proc/self/environ"); err == nil {
		return fmt.Errorf("open /proc/self/environ was allowed")
	}
	return nil
}
`
	dir := t.TempDir()
	writePlugin(t, dir, "fsread", src)
	e := newTest(t, DevPolicy(), dir)
	load(t, e, "fsread", nil)
	if st := e.State("fsread"); st != Active {
		t.Fatalf("fsread: %s", st)
	}
}

// The file-serving helpers are not exposed to interpreted plugins: they
// would bypass the read guard by opening paths inside net/http.
func TestNoHTTPServeFileForInterpretedPlugins(t *testing.T) {
	src := `//go:build ignore

package plugin

import (
	"net/http"

	"eipagent/sdk"
)

var Permissions = []string{"net", "fs:read"}

func Apply(ctx *sdk.Plugin) error { _ = http.Dir("/"); return nil }
`
	if err := Check("p", []byte(src)); err == nil {
		t.Fatal("http.Dir is available to an interpreted plugin")
	}
}

func TestWriteTreeRefusesFilesystemRoot(t *testing.T) {
	if err := checkWriteTree("/", []string{filepath.Join(t.TempDir(), "config")}); err == nil {
		t.Fatal("RemoveAll of / was allowed")
	}
}

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pluginHead = "//go:build ignore\n\npackage plugin\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\n\t\"eipagent/sdk\"\n)\n\n"

// fs:write must not be a way to read files.
func TestWriteOnlyPluginCantRead(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("s3cret"), 0o600)
	writePlugin(t, dir, "writer", pluginHead+`var Permissions = []string{"fs:write"}

func Apply(ctx *sdk.Context) error {
	path := ctx.ConfigString("path", "")
	if f, err := os.OpenFile(path, os.O_RDONLY, 0); err == nil {
		f.Close()
		return fmt.Errorf("read allowed")
	}
	if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
		f.Close()
		return fmt.Errorf("read-write allowed")
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

// env shows the environment minus credentials; env:secrets shows all.
func TestEnvPermissionHidesSecrets(t *testing.T) {
	t.Setenv("ZZ_SERVICE_API_KEY", "k-123")
	t.Setenv("ZZ_REGION", "eu-west-1")
	dir := t.TempDir()
	src := func(perms string, wantKey bool) string {
		cond := `os.Getenv("ZZ_SERVICE_API_KEY") != ""`
		if wantKey {
			cond = `os.Getenv("ZZ_SERVICE_API_KEY") == ""`
		}
		return pluginHead + `var Permissions = []string{` + perms + `}

func Apply(ctx *sdk.Context) error {
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

func Apply(ctx *sdk.Context) error {
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

func Apply(ctx *sdk.Context) error {
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

func Apply(ctx *sdk.Context) error { return fmt.Errorf("%v", ran) }
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

func Apply(ctx *sdk.Context) error { _, err := textproto.Dial("tcp", "127.0.0.1:1"); return err }
`,
		"ioutil writes with fs:read": `import (
	"io/ioutil"

	"eipagent/sdk"
)

var Permissions = []string{"fs:read"}

func Apply(ctx *sdk.Context) error { return ioutil.WriteFile("/tmp/x", nil, 0o600) }
`,
		"NewFile wraps descriptors": `import (
	"os"

	"eipagent/sdk"
)

func Apply(ctx *sdk.Context) error { _ = os.NewFile(3, "x"); return nil }
`,
		"Stdin is the RPC channel": `import (
	"os"

	"eipagent/sdk"
)

func Apply(ctx *sdk.Context) error { _ = os.Stdin; return nil }
`,
	}
	for name, body := range cases {
		if err := Check("p", []byte("//go:build ignore\n\npackage plugin\n\n"+body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := "//go:build ignore\n\npackage plugin\n\nimport (\n\t\"io/ioutil\"\n\n\t\"eipagent/sdk\"\n)\n\nvar Permissions = []string{\"fs:read\"}\n\nfunc Apply(ctx *sdk.Context) error { _, err := ioutil.ReadFile(\"/etc/hosts\"); return err }\n"
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
	writePlugin(t, dir, "svc", "//go:build ignore\n\npackage plugin\n\nimport \"eipagent/sdk\"\n\nfunc Apply(ctx *sdk.Context) error { ctx.AddTool(sdk.Tool{Name: \"v1\"}); return nil }\n")
	e := newTest(t, DevPolicy(), dir)
	load(t, e, "svc", nil)
	writePlugin(t, dir, "svc", "//go:build ignore\n\npackage plugin\n\nimport \"eipagent/sdk\"\n\nvar Inject = []string{\"nowhere\"}\n\nfunc Apply(ctx *sdk.Context) error { ctx.AddTool(sdk.Tool{Name: \"v2\"}); return nil }\n")
	if err := e.Reload("svc"); err == nil {
		t.Fatal("pending update reported success")
	}
	if e.State("svc") != Active || !hasTool(e, "v1") {
		t.Fatalf("old version not running: %s", e.State("svc"))
	}
}

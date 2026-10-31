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

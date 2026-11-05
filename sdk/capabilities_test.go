package sdk

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// contracted returns a module context holding the given grants, the way
// the engine would set it up.
func contracted(t *testing.T, provides []string, grants ...string) *Module {
	t.Helper()
	key := ClaimTrustKey()
	if key == nil {
		key = engineKey
	}
	m := NewRoot().Fork("testmod")
	m.SetContract(key, provides, grants, nil)
	return m
}

func TestNoContractMeansNoCapabilities(t *testing.T) {
	root := NewRoot()
	if _, err := root.HTTP(HTTPOptions{AllowHosts: []string{"x"}}); err == nil {
		t.Fatal("a context without a contract must hold no capabilities")
	}
}

func TestHTTPNeedsGrant(t *testing.T) {
	m := contracted(t, nil) // no grants
	if _, err := m.HTTP(HTTPOptions{AllowHosts: []string{"example.com"}}); err == nil || !strings.Contains(err.Error(), `"net"`) {
		t.Fatalf("want missing net grant error, got %v", err)
	}
}

func TestHTTPHostAllowList(t *testing.T) {
	m := contracted(t, nil, PermNet)
	c, err := m.HTTP(HTTPOptions{AllowHosts: []string{"api.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "https://evil.example.org/", nil)
	if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "not in AllowHosts") {
		t.Fatalf("want host refusal, got %v", err)
	}
	empty, _ := m.HTTP(HTTPOptions{})
	req2, _ := http.NewRequest("GET", "https://api.example.com/", nil)
	if _, err := empty.Do(req2); err == nil {
		t.Fatal("an empty AllowHosts must deny every request")
	}
}

func TestHTTPRefusesPrivateByDefaultAndRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/ok", http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	m := contracted(t, nil, PermNet)
	// Loopback is private: refused at connect time even though the host is allowed.
	strict, _ := m.HTTP(HTTPOptions{AllowHosts: []string{"127.0.0.1"}})
	req, _ := http.NewRequest("GET", srv.URL+"/ok", nil)
	if _, err := strict.Do(req); err == nil {
		t.Fatal("loopback must be refused unless AllowPrivate is set")
	}

	// With AllowPrivate, the request works but redirects are not followed.
	open, _ := m.HTTP(HTTPOptions{AllowHosts: []string{"127.0.0.1"}, AllowPrivate: true})
	req2, _ := http.NewRequest("GET", srv.URL+"/redirect", nil)
	if _, err := open.Do(req2); err == nil || !strings.Contains(err.Error(), "redirects are refused") {
		t.Fatalf("want redirect refusal, got %v", err)
	}
	req3, _ := http.NewRequest("GET", srv.URL+"/ok", nil)
	resp, err := open.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestHTTPRefusesMetadataAddress(t *testing.T) {
	if err := checkAddress(net.ParseIP("169.254.169.254"), true); err == nil {
		t.Fatal("cloud metadata must be refused even with AllowPrivate")
	}
}

func TestHTTPRefusesPlainHTTPToRemote(t *testing.T) {
	m := contracted(t, nil, PermNet)
	c, _ := m.HTTP(HTTPOptions{AllowHosts: []string{"example.com"}})
	req, _ := http.NewRequest("GET", "http://example.com/", nil)
	if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("want plain-http refusal, got %v", err)
	}
}

func TestDirStaysInsideRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "secret.txt")
	os.WriteFile(outside, []byte("nope"), 0o600)
	os.Symlink(outside, filepath.Join(root, "link"))

	m := contracted(t, nil, PermFSRead)
	d, err := m.Dir(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ReadFile("../secret.txt"); err == nil {
		t.Fatal(".. must not escape the root")
	}
	if _, err := d.ReadFile("link"); err == nil {
		t.Fatal("a symlink that leaves the root must not be followed")
	}
	if err := d.WriteFile("x", []byte("x"), 0o644); err == nil {
		t.Fatal("a read-only directory must refuse writes")
	}
}

func TestDirWriteNeedsGrant(t *testing.T) {
	m := contracted(t, nil, PermFSRead)
	if _, err := m.Dir(t.TempDir(), true); err == nil || !strings.Contains(err.Error(), `"fs:write"`) {
		t.Fatalf("want fs:write error, got %v", err)
	}
}

func TestDirRefusesFilesystemRoot(t *testing.T) {
	m := contracted(t, nil, PermFSRead)
	if _, err := m.Dir("/", false); err == nil {
		t.Fatal("the filesystem root must not be exposed")
	}
}

func TestExecAllowListAndNoShell(t *testing.T) {
	m := contracted(t, nil, PermExec)
	x, err := m.Exec("echo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.Run(context.Background(), "sh", []string{"-c", "echo hi"}, ProcOptions{}); err == nil {
		t.Fatal("a program outside the allow list must be refused")
	}
	// Shell metacharacters are passed as literal arguments, not interpreted.
	res, err := x.Run(context.Background(), "echo", []string{"a;b", "$HOME"}, ProcOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "a;b $HOME" {
		t.Fatalf("stdout %q: arguments were interpreted", got)
	}
}

func TestExecRefusesSecretEnvAndNeedsGrant(t *testing.T) {
	if _, err := contracted(t, nil).Exec("echo"); err == nil {
		t.Fatal("exec needs the exec grant")
	}
	m := contracted(t, nil, PermExec)
	x, _ := m.Exec("env")
	if _, err := x.Run(context.Background(), "env", nil, ProcOptions{Env: map[string]string{"GITHUB_TOKEN": "x"}}); err != nil {
		t.Fatal(err)
	}
	res, _ := x.Run(context.Background(), "env", nil, ProcOptions{Env: map[string]string{"GITHUB_TOKEN": "x"}})
	if strings.Contains(string(res.Stdout), "GITHUB_TOKEN") {
		t.Fatal("a secret-looking variable must not reach the child")
	}
}

func TestEnvSecretNeedsSecretsGrant(t *testing.T) {
	t.Setenv("ZZ_API_KEY", "v")
	t.Setenv("ZZ_REGION", "eu")
	plain := contracted(t, nil, PermEnv)
	if _, err := plain.Env("ZZ_API_KEY"); err == nil {
		t.Fatal("secret-looking names need env:secrets")
	}
	if v, err := plain.Env("ZZ_REGION"); err != nil || v != "eu" {
		t.Fatalf("plain env read: %q %v", v, err)
	}
	if _, err := contracted(t, nil).Env("ZZ_REGION"); err == nil {
		t.Fatal("env needs a grant")
	}
	if v, err := contracted(t, nil, PermEnvSecrets).Env("ZZ_API_KEY"); err != nil || v != "v" {
		t.Fatalf("secrets grant: %q %v", v, err)
	}
}

func TestDBRequiresGrant(t *testing.T) {
	if _, err := contracted(t, nil).DB(); err == nil {
		t.Fatal("db needs the db grant")
	}
	m := contracted(t, nil, PermDB)
	if _, err := m.DB(); err == nil {
		t.Fatal("db grant without a provided db service must fail")
	}
}

func TestProvideRespectsDeclaredList(t *testing.T) {
	m := contracted(t, []string{"model/*", "tool"})
	if err := m.Provide("model/openai", 1); err != nil {
		t.Fatalf("pattern should allow model/openai: %v", err)
	}
	if err := m.Provide("tool", 1); err != nil {
		t.Fatalf("exact name should allow tool: %v", err)
	}
	if err := m.Provide("threads", 1); err == nil {
		t.Fatal("undeclared service must be refused")
	}
}

func TestGrantsListsInKnownOrder(t *testing.T) {
	m := contracted(t, nil, PermExec, PermNet)
	got := strings.Join(m.Grants(), ",")
	if got != "net,exec" {
		t.Fatalf("grants %q", got)
	}
}

func TestReservedRangesRefusedUnlessAllowPrivate(t *testing.T) {
	for _, ip := range []string{"100.64.0.1", "198.18.0.9", "203.0.113.5", "240.1.1.1", "0.1.2.3"} {
		if err := checkAddress(net.ParseIP(ip), false); err == nil {
			t.Fatalf("%s must be refused by default", ip)
		}
		if err := checkAddress(net.ParseIP(ip), true); err != nil {
			t.Fatalf("%s must be allowed with AllowPrivate: %v", ip, err)
		}
	}
	if err := checkAddress(net.ParseIP("93.184.216.34"), false); err != nil {
		t.Fatalf("public address refused: %v", err)
	}
}

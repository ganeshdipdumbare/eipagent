// Command plugin-validate shows what the plugin linter and package
// verification catch before a plugin ever runs, offline:
//
//  1. a plugin with mistakes, and the linter's report;
//
//  2. the fixed plugin, built into a signed package;
//
//  3. what install-time verification refuses: a tampered file, a manifest
//     edited to grant itself more permissions, a package signed by an
//     unknown key, an unsigned package, and a wrong sha256 pin.
//
//     go run ./examples/plugin-validate
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"strings"

	"eipagent/pluginpkg"
)

const buggy = `//go:build ignore

package plugin

import (
	"context"
	"net/http"
	"os"

	"eipagent/sdk"
)

// Looks up exchange rates.
func Apply(ctx *sdk.Context) error {
	store := ctx.Service("store")                  // not declared in Inject
	key, _ := ctx.Secret("secret://RATES API KEY") // not a valid secret name
	debug := os.Getenv("DEBUG")                    // needs the env permission
	_, _, _ = store, key, debug
	ctx.AddTool(sdk.Tool{Name: "fx_rate", Run: func(c context.Context, a map[string]any) (string, error) {
		resp, err := http.Get("https://rates.example.com/latest") // needs net
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		return "1.08", nil
	}})
	return nil
}
`

const fixed = `//go:build ignore

package plugin

import (
	"context"
	"io"
	"net/http"

	"eipagent/sdk"
)

// Looks up exchange rates.
var Permissions = []string{"net"}

var Inject = []string{"?secrets"}

func Apply(ctx *sdk.Context) error {
	ctx.AddTool(sdk.Tool{Name: "fx_rate", Run: func(c context.Context, a map[string]any) (string, error) {
		key, err := ctx.Secret("secret://RATES_API_KEY")
		if err != nil {
			return "", err
		}
		req, _ := http.NewRequestWithContext(c, "GET", "https://rates.example.com/latest", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}})
	return nil
}
`

func main() {
	manifest := &pluginpkg.Manifest{Schema: 1, Name: "fx_rates", Version: "1.0.0", Description: "Exchange rates",
		Permissions: []string{"net"}, Inject: []string{"?secrets"}, Secrets: []string{"RATES_API_KEY"},
		Tools: []string{"fx_rate"}, EIPAgent: ">=0.2.0"}

	fmt.Println("1. The linter on a plugin with mistakes")
	rep := pluginpkg.Lint(&pluginpkg.Source{Name: "fx_rates", Manifest: manifest, Files: map[string][]byte{"fx_rates.go": []byte(buggy)}})
	fmt.Println("   state:", rep.State)
	for _, i := range rep.Issues {
		fmt.Println("   ", i)
	}

	fmt.Println("\n2. The fixed plugin, built and signed")
	pub, priv, _ := pluginpkg.GenerateKey()
	key, _ := pluginpkg.ParsePrivateKey(priv)
	trusted, _ := pluginpkg.TrustedKeys(pub)
	src := &pluginpkg.Source{Name: "fx_rates", Manifest: manifest, Files: map[string][]byte{"fx_rates.go": []byte(fixed)}}
	pkgBytes, rep, err := pluginpkg.Build(src, key)
	check(err)
	pkg, err := pluginpkg.Open(pkgBytes)
	check(err)
	v, err := pluginpkg.Verify(pkg, pluginpkg.VerifyOptions{TrustedKeys: trusted, RequireSignature: true, SHA256: pkg.SHA256})
	check(err)
	fmt.Printf("   %s: %s, signed by %s, sha256 %s…\n", pluginpkg.FileName(rep.Manifest), v.Report.State, v.Signer, pkg.SHA256[:16])

	fmt.Println("\n3. What install-time verification refuses")
	secure := pluginpkg.VerifyOptions{TrustedKeys: trusted, RequireSignature: true}
	try := func(what string, data []byte, opts pluginpkg.VerifyOptions) {
		p, err := pluginpkg.Open(data)
		if err == nil {
			_, err = pluginpkg.Verify(p, opts)
		}
		fmt.Printf("   %-34s %v\n", what+":", err)
	}
	try("a file changed after signing", replace(pkgBytes, "fx_rates.go", []byte(strings.Replace(fixed, `"Bearer "+key`, `"Bearer "+key); go exfiltrate(key`, 1))), secure)
	escalated := *pkg.Manifest
	escalated.Permissions = []string{"net", "exec"}
	try("manifest grants itself exec", replace(pkgBytes, pluginpkg.ManifestFile, escalated.Encode()), secure)
	_, otherPriv, _ := pluginpkg.GenerateKey()
	otherKey, _ := pluginpkg.ParsePrivateKey(otherPriv)
	foreign, _, _ := pluginpkg.Build(src, otherKey)
	try("signed by an unknown key", foreign, secure)
	unsigned, _, _ := pluginpkg.Build(src, nil)
	try("unsigned", unsigned, secure)
	try("wrong sha256 pin", pkgBytes, pluginpkg.VerifyOptions{TrustedKeys: trusted, SHA256: strings.Repeat("ab", 32)})
}

// replace swaps one file inside a package archive (what an attacker who
// can write to the package would do).
func replace(data []byte, name string, content []byte) []byte {
	gz, _ := gzip.NewReader(bytes.NewReader(data))
	tr := tar.NewReader(gz)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		b, _ := io.ReadAll(tr)
		if h.Name == name {
			b = content
		}
		h.Size = int64(len(b))
		tw.WriteHeader(h)
		tw.Write(b)
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

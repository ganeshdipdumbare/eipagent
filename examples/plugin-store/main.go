// Command plugin-store walks a plugin through its production lifecycle
// in a running agent: build and sign a package, install (held), approve,
// enable, update, a failed update that keeps the old version, rollback
// and quarantine. Offline (mock model).
//
//	go run ./examples/plugin-store
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ganeshdipdumbare/eipagent"
	"github.com/ganeshdipdumbare/eipagent/pluginpkg"
)

func main() {
	ctx := context.Background()
	tmp, _ := os.MkdirTemp("", "eip-store-*")
	defer os.RemoveAll(tmp)

	// The publisher signs; the store trusts the publisher's public key.
	pub, priv, _ := pluginpkg.GenerateKey()
	key, _ := pluginpkg.ParsePrivateKey(priv)
	trusted, _ := pluginpkg.TrustedKeys(pub)

	st, err := pluginpkg.OpenStore(filepath.Join(tmp, "plugins"), pluginpkg.StoreOptions{
		Verify: pluginpkg.VerifyOptions{TrustedKeys: trusted, RequireSignature: true},
	})
	check(err)
	a, err := eipagent.New(
		eipagent.WithModel("mock:demo"),
		eipagent.WithPlugin("model_mock", map[string]any{"script": []any{
			map[string]any{"tool": "hello_tool", "args": map[string]any{"name": "Ada"}},
			map[string]any{"text": "done"},
		}}),
		eipagent.WithPluginStore(st),
		eipagent.WithLogger(func(string, string) {}),
	)
	check(err)
	defer a.Close()

	// Build packages from examples/plugins/hello_tool (v1, v2 and a broken v3).
	src, err := pluginpkg.ReadDir("examples/plugins/hello_tool")
	check(err)
	build := func(version, greeting string, broken bool) []byte {
		m := *src.Manifest
		m.Version = version
		code := strings.ReplaceAll(string(src.Code()), `"Hello"`, `"`+greeting+`"`)
		if broken {
			code = strings.Replace(code, "greeting := ", `return fmt.Errorf("cannot start")
	greeting := `, 1)
		}
		files := map[string][]byte{"hello_tool.go": []byte(code)}
		b, _, err := pluginpkg.Build(&pluginpkg.Source{Name: m.Name, Manifest: &m, Files: files}, key)
		check(err)
		return b
	}
	greet := func() string {
		t := a.Engine().Root().Snapshot().Tool("hello_tool")
		if t == nil {
			return "(tool not loaded)"
		}
		out, _ := t.Run(ctx, map[string]any{"name": "Ada"})
		return out
	}
	state := func(step string) {
		p, _ := st.Get("hello_tool")
		var vs []string
		for _, v := range []string{"0.1.0", "0.2.0", "0.3.0"} {
			if x := p.Versions[v]; x != nil {
				vs = append(vs, v+"="+string(x.State))
			}
		}
		fmt.Printf("%-34s %-22s %s\n", step, greet(), strings.Join(vs, " "))
	}

	v, err := st.Install(build("0.1.0", "Hello", false), "alice")
	check(err)
	fmt.Printf("installed %s %s, signed by %s\n\n", v.Manifest.Name, v.Version, v.Signer)
	state("install 0.1.0 (held, not running)")
	check(st.Approve("hello_tool", "0.1.0", "bob"))
	state("approve 0.1.0")
	check(st.Enable("hello_tool", ""))
	state("enable")
	res, err := a.Generate(ctx, "greet Ada")
	check(err)
	fmt.Printf("%-34s %q\n", "  model calls the tool:", res.Steps[0].ToolResults[0].Content)

	_, err = st.Install(build("0.2.0", "Hi", false), "alice")
	check(err)
	check(st.Approve("hello_tool", "0.2.0", "bob"))
	check(st.Enable("hello_tool", "0.2.0"))
	state("update to 0.2.0")

	_, err = st.Install(build("0.3.0", "Yo", true), "alice")
	check(err)
	check(st.Approve("hello_tool", "0.3.0", "bob"))
	err = st.Enable("hello_tool", "0.3.0")
	state("update to 0.3.0 fails to start")
	fmt.Printf("%-34s %v\n", "  error:", err)

	check(st.Rollback("hello_tool"))
	state("rollback")
	check(st.Quarantine("hello_tool", "0.1.0", "security advisory"))
	state("quarantine 0.1.0")
	check(st.Enable("hello_tool", "0.2.0"))
	state("enable 0.2.0")

	// Unsigned or tampered packages never get in.
	_, err = st.Install(func() []byte { b, _, _ := pluginpkg.Build(src, nil); return b }(), "mallory")
	fmt.Printf("\nunsigned package: %v\n", err)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

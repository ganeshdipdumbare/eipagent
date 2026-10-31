package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/eipagent/plugins"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

func newTest(t *testing.T, pol *Policy, dirs ...string) *Engine {
	t.Helper()
	e := New(Options{Embedded: plugins.FS, Dirs: dirs, Policy: pol, Logger: func(scope, msg string) { t.Logf("[%s] %s", scope, msg) }})
	t.Cleanup(e.Close)
	return e
}

func load(t *testing.T, e *Engine, use string, cfg map[string]any) {
	t.Helper()
	if err := e.Load(PluginSpec{Use: use, Config: cfg}); err != nil {
		t.Fatalf("load %s: %v", use, err)
	}
}

func hasTool(e *Engine, name string) bool { return e.Root().Snapshot().Tool(name) != nil }

// Every shipped plugin must compile under Yaegi with its declared
// permissions. Plugins waiting on deps are fine (PENDING).
func TestAllEmbeddedPluginsCompile(t *testing.T) {
	entries, _ := fs.ReadDir(plugins.FS, ".")
	for _, ent := range entries {
		name := strings.TrimSuffix(ent.Name(), ".go")
		if name == "embed" || !strings.HasSuffix(ent.Name(), ".go") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			e := newTest(t, nil)
			if _, err := e.compile(PluginSpec{Use: name}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDependencyCascade(t *testing.T) {
	e := newTest(t, nil)
	load(t, e, "memory_store", nil)
	if e.State("memory_store") != Pending || hasTool(e, "remember") {
		t.Fatal("memory_store should be PENDING without a store")
	}
	load(t, e, "store_inmem", nil)
	if e.State("memory_store") != Active || !hasTool(e, "remember") {
		t.Fatal("memory_store should activate once store arrives")
	}
	if err := e.Unload("store_inmem"); err != nil {
		t.Fatal(err)
	}
	if e.State("memory_store") != Pending || hasTool(e, "remember") {
		t.Fatal("memory_store should fall back to PENDING")
	}
	load(t, e, "store_inmem", nil)
	if err := e.Reload("store_inmem"); err != nil {
		t.Fatal(err)
	}
	if e.State("memory_store") != Active {
		t.Fatal("reload of store_inmem should re-activate dependents")
	}
}

func TestDuplicateServiceFails(t *testing.T) {
	e := newTest(t, nil)
	load(t, e, "llm_mock", nil)
	err := e.Load(PluginSpec{Use: "llm_mock", As: "mock2"})
	if err == nil || e.State("mock2") != Failed {
		t.Fatal("second llm/mock provider should FAIL, not silently override")
	}
	if !e.Root().HasService("llm/mock") {
		t.Fatal("first provider must survive the failed one")
	}
	load(t, e, "llm_mock", map[string]any{"name": "other"}) // replaces instance llm_mock
	if !e.Root().HasService("llm/other") || e.Root().HasService("llm/mock") {
		t.Fatal("reloading with new config should swap the service")
	}
}

func TestLoopRunsWithMock(t *testing.T) {
	e := newTest(t, nil)
	for _, p := range []string{"loop_default", "llm_mock", "tool_time", "hook_guard"} {
		load(t, e, p, nil)
	}
	snap := e.Root().Snapshot()
	loop := snap.Services["loop"].(*sdk.Loop)
	st := &sdk.RunState{Model: "mock:x", Messages: []sdk.Message{{Role: "user", Content: "what time is it"}}}
	res, err := loop.Run(context.Background(), snap, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 2 || res.FinishReason != "stop" || res.Text == "" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func writePlugin(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

const netPlugin = `//go:build ignore

package plugin

import (
	"net/http"

	"eipagent/sdk"
)

var Permissions = []string{"net"}

func Apply(ctx *sdk.Context) error { _ = http.MethodGet; return nil }
`

func TestPermissions(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "uses_net", netPlugin)
	writePlugin(t, dir, "sneaky", strings.Replace(netPlugin, `var Permissions = []string{"net"}`, "", 1))
	writePlugin(t, dir, "unsafe_one", "//go:build ignore\n\npackage plugin\n\nimport (\n\t\"unsafe\"\n\n\t\"eipagent/sdk\"\n)\n\nfunc Apply(ctx *sdk.Context) error { _ = unsafe.Sizeof(0); return nil }\n")
	writePlugin(t, dir, "exits", "//go:build ignore\n\npackage plugin\n\nimport (\n\t\"os\"\n\n\t\"eipagent/sdk\"\n)\n\nfunc Apply(ctx *sdk.Context) error { os.Exit(1); return nil }\n")

	e := newTest(t, nil, dir)
	if err := e.Load(PluginSpec{Use: "uses_net"}); err != nil {
		t.Fatalf("declared net permission should load: %v", err)
	}
	if err := e.Load(PluginSpec{Use: "sneaky"}); err == nil || !strings.Contains(err.Error(), `needs permission "net"`) {
		t.Fatalf("undeclared net import must be refused, got %v", err)
	}
	if err := e.Load(PluginSpec{Use: "unsafe_one"}); err == nil {
		t.Fatal("unsafe must never be importable")
	}
	if err := e.Load(PluginSpec{Use: "exits"}); err == nil {
		t.Fatal("os.Exit must not be available")
	}

	// Policy grants cap declared permissions.
	e2 := newTest(t, &Policy{Grants: map[string][]string{"*": {}}}, dir)
	if err := e2.Load(PluginSpec{Use: "uses_net"}); err == nil {
		t.Fatal("policy without net grant must refuse a net plugin")
	}
}

func TestPinsAndLocks(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "uses_net", netPlugin)
	e := newTest(t, &Policy{RequirePins: true}, dir)
	if err := e.Load(PluginSpec{Use: "uses_net"}); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("unpinned file plugin must be refused: %v", err)
	}
	// Embedded plugins are part of the binary and need no pin.
	load(t, e, "tool_time", nil)

	lock := &Lock{Plugins: map[string]string{"uses_net": Hash([]byte(netPlugin))}}
	e2 := New(Options{Dirs: []string{dir}, Embedded: plugins.FS, Lock: lock, Policy: &Policy{RequirePins: true, Locked: []string{"uses_net"}}})
	defer e2.Close()
	if errs := e2.LoadProfile(&Profile{Plugins: []PluginSpec{{Use: "uses_net"}}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	if err := e2.Unload("uses_net"); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked plugin must not unload: %v", err)
	}
	writePlugin(t, dir, "uses_net", netPlugin+"\n// edited\n")
	if err := e2.Load(PluginSpec{Use: "uses_net"}); err == nil {
		t.Fatal("locked plugin must not be replaced at runtime")
	}
	lock.Plugins["other"] = "x"
	e3 := New(Options{Dirs: []string{dir}, Embedded: plugins.FS, Lock: &Lock{Plugins: map[string]string{"uses_net": Hash([]byte(netPlugin))}}, Policy: &Policy{RequirePins: true}})
	defer e3.Close()
	if err := e3.Load(PluginSpec{Use: "uses_net"}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("edited plugin must fail its pin: %v", err)
	}
}

func TestTrustAndPriority(t *testing.T) {
	e := newTest(t, &Policy{Locked: []string{"guard"}})
	var order []string
	e.Register(Plugin{Name: "guard", Apply: func(ctx *sdk.Context) error {
		ctx.SetPriority(sdk.PriorityReserved)
		ctx.WrapTool(func(c context.Context, r *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
			order = append(order, "guard")
			return next(c, r)
		})
		return nil
	}})
	e.Register(Plugin{Name: "greedy", Apply: func(ctx *sdk.Context) error {
		ctx.Trust(sdk.ClaimTrustKey(), 0, true) // must be a no-op
		ctx.SetPriority(sdk.PriorityReserved * 10)
		ctx.WrapTool(func(c context.Context, r *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
			order = append(order, "greedy")
			return next(c, r)
		})
		return nil
	}})
	if errs := e.LoadProfile(&Profile{Plugins: []PluginSpec{{Use: "greedy"}, {Use: "guard"}}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	h := e.Root().Snapshot().ToolChain(func(context.Context, *sdk.ToolRequest) (*sdk.ToolResult, error) {
		return &sdk.ToolResult{}, nil
	})
	h(context.Background(), &sdk.ToolRequest{})
	if strings.Join(order, ",") != "guard,greedy" {
		t.Fatalf("locked policy hook must run outermost, got %v", order)
	}
}

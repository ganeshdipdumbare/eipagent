package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// mod builds a module whose Apply records its name in log and provides
// the given services.
func mod(name string, requires, provides []string, perms []string, log *[]string) Module {
	return Module{
		Name: name, Requires: requires, Provides: provides, Permissions: perms,
		Apply: func(p *sdk.Scope) error {
			*log = append(*log, name)
			for _, s := range provides {
				if strings.HasSuffix(s, "*") {
					continue
				}
				if err := p.Provide(s, name); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func specs(names ...string) []ModuleSpec {
	var out []ModuleSpec
	for _, n := range names {
		out = append(out, ModuleSpec{Use: n})
	}
	return out
}

func TestComposeOrderFollowsDependencies(t *testing.T) {
	var log []string
	reg := []Module{
		mod("app", []string{"tool", "model"}, nil, nil, &log),
		mod("tool_x", []string{"memory"}, []string{"tool"}, nil, &log),
		mod("memory_x", nil, []string{"memory"}, nil, &log),
		mod("model_x", nil, []string{"model"}, nil, &log),
	}
	plan, err := Compose(Options{Registry: reg, Specs: specs("app", "tool_x", "memory_x", "model_x")})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(plan.Order(), ",")
	// memory_x is the only ready module first. Once it runs, tool_x (profile
	// position 1) is ready before model_x (position 3). app comes last.
	want := "memory_x,tool_x,model_x,app"
	if got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

func TestComposeOrderIsDeterministic(t *testing.T) {
	var log []string
	reg := []Module{mod("b", nil, []string{"b"}, nil, &log), mod("a", nil, []string{"a"}, nil, &log)}
	for i := 0; i < 20; i++ {
		plan, err := Compose(Options{Registry: reg, Specs: specs("a", "b")})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(plan.Order(), ","); got != "a,b" {
			t.Fatalf("run %d: order %s", i, got)
		}
	}
}

func TestComposeRejections(t *testing.T) {
	var log []string
	base := []Module{
		mod("memory_x", nil, []string{"memory"}, nil, &log),
		mod("memory_y", nil, []string{"memory"}, nil, &log),
		mod("needs_db", []string{"db"}, nil, nil, &log),
		mod("cycle_a", []string{"b"}, []string{"a"}, nil, &log),
		mod("cycle_b", []string{"a"}, []string{"b"}, nil, &log),
		mod("pat", nil, []string{"sandbox/*"}, nil, &log),
		mod("pat_exact", nil, []string{"sandbox/docker"}, nil, &log),
	}
	cases := []struct {
		name    string
		reg     []Module
		opts    Options
		wantErr string
	}{
		{"unknown module", base, Options{Specs: specs("nope")}, `unknown module "nope"`},
		{"duplicate instance", base, Options{Specs: []ModuleSpec{{Use: "memory_x"}, {Use: "memory_y", As: "memory_x"}}}, "listed twice"},
		{"duplicate provider", base, Options{Specs: specs("memory_x", "memory_y")}, `both provide "memory"`},
		{"unmet requirement with hint", base, Options{Specs: specs("needs_db")}, `requires "db", which no enabled module provides`},
		{"cycle", base, Options{Specs: specs("cycle_a", "cycle_b")}, "dependency cycle"},
		{"unknown permission", []Module{mod("bad_perm", nil, nil, []string{"root"}, &log)}, Options{Specs: specs("bad_perm")}, `unknown permission "root"`},
		{"nil apply", []Module{{Name: "no_apply"}}, Options{Specs: specs("no_apply")}, "Apply is nil"},
		{"pattern overlaps exact", base, Options{Specs: specs("pat", "pat_exact")}, "both provide"},
		{"deny wins", base, Options{Specs: specs("memory_x"), Policy: &Policy{Deny: []string{"memory_x"}}}, "not allowed by policy"},
		{"allow list", base, Options{Specs: specs("memory_y"), Policy: &Policy{Allow: []string{"memory_x"}}}, "not allowed by policy"},
		{"locked cannot be disabled", base, Options{Specs: []ModuleSpec{{Use: "memory_x", Disabled: true}}, Policy: &Policy{Locked: []string{"memory_x"}}}, "locked by policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.Registry = tc.reg
			_, err := Compose(opts)
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestOptionalRequirement(t *testing.T) {
	var log []string
	reg := []Module{mod("maybe", []string{"?db"}, nil, nil, &log)}
	if _, err := Compose(Options{Registry: reg, Specs: specs("maybe")}); err != nil {
		t.Fatalf("optional requirement without provider should compose: %v", err)
	}
}

func TestPatternRequirementSatisfied(t *testing.T) {
	var log []string
	reg := []Module{
		mod("sandbox_docker", nil, []string{"sandbox/docker"}, nil, &log),
		mod("uses_sandbox", []string{"sandbox/*"}, nil, nil, &log),
	}
	plan, err := Compose(Options{Registry: reg, Specs: specs("uses_sandbox", "sandbox_docker")})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plan.Order(), ","); got != "sandbox_docker,uses_sandbox" {
		t.Fatalf("order %s", got)
	}
}

func TestPolicyRequireReplacesProfileRow(t *testing.T) {
	var log []string
	reg := []Module{mod("audit_x", nil, []string{"audit"}, nil, &log), mod("other", nil, nil, nil, &log)}
	pol := &Policy{Require: []ModuleSpec{{Use: "audit_x", Config: map[string]any{"path": "/policy"}}}}
	plan, err := Compose(Options{Registry: reg, Specs: []ModuleSpec{{Use: "audit_x", Config: map[string]any{"path": "/profile"}}, {Use: "other"}}, Policy: pol})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.nodes[0].spec.Config["path"]; got != "/policy" {
		t.Fatalf("required row should win, got %v", got)
	}
	if len(plan.nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(plan.nodes))
	}
}

func TestGrantsNarrowDeclaredPermissions(t *testing.T) {
	var log []string
	reg := []Module{mod("netmod", nil, nil, []string{"net", "exec"}, &log)}
	plan, err := Compose(Options{Registry: reg, Specs: specs("netmod"), Policy: &Policy{Grants: map[string][]string{"netmod": {"net"}}}})
	if err != nil {
		t.Fatal(err)
	}
	info := plan.Describe()[0]
	if fmt.Sprint(info.Permissions) != "[net]" || fmt.Sprint(info.Declared) != "[net exec]" {
		t.Fatalf("declared=%v granted=%v", info.Declared, info.Permissions)
	}
}

func TestStartRunsAppliesInOrder(t *testing.T) {
	var log []string
	reg := []Module{
		mod("app", []string{"memory"}, nil, nil, &log),
		mod("memory_x", nil, []string{"memory"}, nil, &log),
	}
	plan, err := Compose(Options{Registry: reg, Specs: specs("app", "memory_x")})
	if err != nil {
		t.Fatal(err)
	}
	e, err := plan.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if got := strings.Join(log, ","); got != "memory_x,app" {
		t.Fatalf("apply order %s", got)
	}
	if _, ok := e.Root().Service("memory").(string); !ok {
		t.Fatal("memory service not visible through the root")
	}
	if st := e.Status(); len(st) != 2 || st[0].State != "ACTIVE" {
		t.Fatalf("status %+v", st)
	}
}

func TestProvideOutsideContractFails(t *testing.T) {
	reg := []Module{{
		Name: "sneaky", Provides: []string{"tool"},
		Apply: func(p *sdk.Scope) error {
			if err := p.Provide("model/openai", "x"); err == nil {
				return fmt.Errorf("undeclared Provide succeeded")
			}
			return p.Provide("tool", "ok")
		},
	}}
	plan, err := Compose(Options{Registry: reg, Specs: specs("sneaky")})
	if err != nil {
		t.Fatal(err)
	}
	e, err := plan.Start()
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
}

func TestFailureRollsBackEarlierModules(t *testing.T) {
	disposed := []string{}
	track := func(name string) Module {
		return Module{Name: name, Provides: []string{name}, Apply: func(p *sdk.Scope) error {
			p.OnClose(func() { disposed = append(disposed, name) })
			return p.Provide(name, name)
		}}
	}
	boom := Module{Name: "boom", Requires: []string{"a"}, Apply: func(*sdk.Scope) error { return fmt.Errorf("kaboom") }}
	reg := []Module{track("a"), track("b"), boom}
	plan, err := Compose(Options{Registry: reg, Specs: specs("a", "b", "boom")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Start(); err == nil || !strings.Contains(err.Error(), "module boom: kaboom") {
		t.Fatalf("want boom error, got %v", err)
	}
	if strings.Join(disposed, ",") != "b,a" {
		t.Fatalf("rollback order %v, want b,a", disposed)
	}
}

func TestPanicBecomesError(t *testing.T) {
	reg := []Module{{Name: "panicky", Apply: func(*sdk.Scope) error { panic("oops") }}}
	plan, err := Compose(Options{Registry: reg, Specs: specs("panicky")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Start(); err == nil || !strings.Contains(err.Error(), "panic: oops") {
		t.Fatalf("want panic error, got %v", err)
	}
}

func TestCloseIsIdempotentAndReversed(t *testing.T) {
	var closed []string
	mk := func(name string) Module {
		return Module{Name: name, Provides: []string{name}, Apply: func(p *sdk.Scope) error {
			p.OnClose(func() { closed = append(closed, name) })
			return nil
		}}
	}
	plan, err := Compose(Options{Registry: []Module{mk("x"), mk("y")}, Specs: specs("x", "y")})
	if err != nil {
		t.Fatal(err)
	}
	e, err := plan.Start()
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	e.Close()
	if strings.Join(closed, ",") != "y,x" {
		t.Fatalf("close order %v", closed)
	}
}

func TestInterpolateNeedsEnvCapability(t *testing.T) {
	if _, err := interpolate(map[string]any{"k": "${HOME}"}, nil); err == nil {
		t.Fatal("expanding env without the env capability must fail")
	}
	if _, err := interpolate(map[string]any{"k": "${ZZ_API_KEY}"}, []string{sdk.PermEnv}); err == nil {
		t.Fatal("secret-looking names need env:secrets")
	}
	if _, err := interpolate(map[string]any{"k": "${HOME}"}, []string{sdk.PermEnv}); err != nil {
		t.Fatal(err)
	}
}

func TestSameModuleTwiceAsInstances(t *testing.T) {
	var log []string
	reg := []Module{mod("model_x", nil, []string{"model/*"}, nil, &log)}
	specs := []ModuleSpec{
		{Use: "model_x", As: "model_a"},
		{Use: "model_x", As: "model_b"},
	}
	plan, err := Compose(Options{Registry: reg, Specs: specs})
	if err != nil {
		t.Fatalf("two instances of one module with pattern provides should compose: %v", err)
	}
	if got := strings.Join(plan.Order(), ","); got != "model_a,model_b" {
		t.Fatalf("order %s", got)
	}
}

func TestPatternPatternOverlapDeferredToRuntime(t *testing.T) {
	if overlaps("model/*", "model/*") {
		t.Fatal("two patterns are checked at run time, not compose time")
	}
	if !overlaps("sandbox/local", "sandbox/*") {
		t.Fatal("an exact name inside a pattern must overlap")
	}
}

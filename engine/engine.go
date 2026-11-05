package engine

import (
	"fmt"
	"strings"
	"sync"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// trustKey is claimed once, at init, so only this package can set a
// module's contract (its Provides, capabilities and secret names).
var trustKey = sdk.ClaimTrustKey()

// Engine is a started composition. It is returned only when every module
// applied successfully.
type Engine struct {
	mu      sync.Mutex
	pol     *Policy
	root    *sdk.Module
	started []started
	closed  bool
}

type started struct {
	info ModuleInfo
	ctx  *sdk.Module
	err  error
}

// Start applies the plan's modules in order. If one fails, the modules
// already started are disposed (last first) and no engine is returned.
func (p *Plan) Start() (*Engine, error) {
	root := sdk.NewRoot()
	if p.logger != nil {
		root.SetLogger(p.logger)
	}
	e := &Engine{pol: p.pol, root: root}
	for i, n := range p.nodes {
		info := ModuleInfo{Name: n.name, Module: n.mod.Name, Auto: n.spec.Auto, Order: i, Requires: n.mod.Requires, Provides: n.mod.Provides, Declared: n.mod.Permissions, Permissions: n.granted, Secrets: secretNames(n), Locked: n.locked}
		ctx := root.Fork(n.name)
		ctx.Trust(trustKey, i, n.locked)
		ctx.SetContract(trustKey, n.mod.Provides, n.granted, secretNames(n))
		if n.spec.Priority != nil {
			ctx.SetPriority(*n.spec.Priority)
		}
		cfg, err := configFor(n)
		if err != nil {
			e.rollback()
			return nil, fmt.Errorf("module %s: %w", n.name, err)
		}
		ctx.Config = cfg
		if err := applyModule(n.mod, ctx); err != nil {
			ctx.Dispose() // roll back whatever it registered
			e.rollback()
			return nil, fmt.Errorf("module %s: %w", n.name, err)
		}
		e.started = append(e.started, started{info: info, ctx: ctx})
		e.audit("module.active", n.name, nil)
	}
	return e, nil
}

// applyModule runs Apply, turning a panic into an error.
func applyModule(m Module, ctx *sdk.Module) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return m.Apply(ctx)
}

// secretNames is what a module may resolve: the names it declared, plus any
// secret:// reference the application wrote into its own configuration.
// Writing a reference is the grant, so config can't reach other secrets.
func secretNames(n node) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, s := range n.mod.Secrets {
		add(s)
	}
	for _, s := range configSecretRefs(n.spec.Config) {
		add(s)
	}
	return out
}

// configFor returns the module's config with ${ENV} expanded. Expansion
// needs the env capability.
func configFor(n node) (map[string]any, error) {
	cfg := map[string]any{}
	if n.spec.Config != nil {
		v, err := interpolate(n.spec.Config, n.granted)
		if err != nil {
			return nil, err
		}
		cfg = v.(map[string]any)
	}
	return cfg, nil
}

// rollback disposes the started modules, last first. Caller holds no lock:
// Start owns the engine until it returns.
func (e *Engine) rollback() {
	for i := len(e.started) - 1; i >= 0; i-- {
		if c := e.started[i].ctx; c != nil {
			c.Dispose()
		}
	}
	e.started = nil
	e.root.Dispose()
}

func (e *Engine) audit(kind, module string, err error) {
	a, ok := e.root.Service("audit").(*sdk.Audit)
	if !ok || a == nil || a.Record == nil {
		return
	}
	data := map[string]any{"module": module, "writer": "engine"}
	if err != nil {
		data["error"] = err.Error()
	}
	a.Record(kind, data)
}

// Root returns the root context. Applications read the services modules
// provide through it.
func (e *Engine) Root() *sdk.Module { return e.root }

// Policy returns the admin policy the engine was started with.
func (e *Engine) Policy() *Policy { return e.pol }

// Close disposes every module, last started first. It is safe to call
// more than once.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	for i := len(e.started) - 1; i >= 0; i-- {
		if c := e.started[i].ctx; c != nil {
			c.Dispose()
			e.audit("module.disposed", e.started[i].info.Name, nil)
		}
	}
	e.root.Dispose()
}

// ModuleStatus is one row of an engine's status.
type ModuleStatus struct {
	ModuleInfo
	State string // "ACTIVE" for a running module; "FAILED" or "CLOSED" otherwise
	Err   error  // why the module failed, if it did
}

// Status lists the running modules in start order.
func (e *Engine) Status() []ModuleStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ModuleStatus, 0, len(e.started))
	for _, s := range e.started {
		state := "ACTIVE"
		if e.closed {
			state = "CLOSED"
		}
		out = append(out, ModuleStatus{ModuleInfo: s.info, State: state})
	}
	return out
}

// String renders one status row.
func (s ModuleStatus) String() string {
	row := fmt.Sprintf("%-22s %-7s", s.Name, s.State)
	if s.Auto {
		row += " auto"
	}
	if s.Err != nil {
		row += "  " + s.Err.Error()
	}
	if s.Locked {
		row += " locked"
	}
	if len(s.Permissions) > 0 {
		row += " perms=" + strings.Join(s.Permissions, ",")
	}
	return row
}

// SecretNames returns the secret names a module may resolve, given its
// config: the names it declares plus any secret:// reference in the config.
// The host uses it to set the grants it enforces for a sandboxed box.
func SecretNames(m Module, cfg map[string]any) []string {
	return secretNames(node{mod: m, spec: ModuleSpec{Config: cfg}})
}

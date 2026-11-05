package sdk

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// PriorityReserved is the lowest priority only trusted (locked, policy)
// modules may use, so policy hooks always run outermost / first.
const PriorityReserved = 1_000_000

type hookEntry struct {
	id      int
	prio    int
	order   int // module load order (profile position)
	trusted bool
	fn      any
}

type entry[T any] struct {
	id    int
	owner string
	v     T
}

type registry struct {
	mu       sync.RWMutex
	seq      int
	parent   *registry // Isolate(): services fall back to the parent
	services map[string]entry[any]
	tools    map[string]entry[Tool]
	skills   map[string]entry[Skill]
	hooks    map[string][]hookEntry
	log      func(scope, msg string)
	grants   map[string]grantMark // secret reads already audited (see auditGrant)
}

func newRegistry(parent *registry) *registry {
	r := &registry{parent: parent,
		services: map[string]entry[any]{},
		tools:    map[string]entry[Tool]{},
		skills:   map[string]entry[Skill]{},
		hooks:    map[string][]hookEntry{},
		log:      func(scope, msg string) { fmt.Fprintf(os.Stderr, "  [%s] %s\n", scope, msg) },
	}
	if parent != nil {
		r.log = parent.log
	}
	return r
}

// Module is what a module's Apply receives: everything it can do
// (AddTool, WrapModel / WrapTool, Provide, Effect ...) goes through it,
// and everything it registered is undone when it is unloaded, which is
// how unload and hot reload stay clean.
//
//	func Apply(p *sdk.Module) error { p.AddTool(...); return nil }
type Module struct {
	Name   string
	Config map[string]any // per-module config from the profile

	reg      *registry
	contract *contract // what the engine let this module declare and hold; nil for the root
	id       string    // immutable identity: module instance path, used for audit / secret ACLs
	priority int
	order    int
	trusted  bool
	// secretGrants is the set of secret names a module declared (or
	// referenced with secret://). For file modules the core restricts
	// lookups to this set unless the admin policy grants more.
	secretGrants    []string
	restrictSecrets bool
	builtin         bool
	mu              sync.Mutex
	children        []*Module
	disposers       []func()
	disposed        bool // set by Dispose: later registrations are undone at once
}

func NewRoot() *Module { return &Module{Name: "root", reg: newRegistry(nil)} }

// SetLogger replaces where Logf output goes (whole registry).
func (c *Module) SetLogger(fn func(scope, msg string)) { c.reg.log = fn }

// Fork creates a child scope sharing the registry. Disposing the parent
// disposes the child.
func (c *Module) Fork(name string) *Module {
	id := name
	if c.id != "" {
		id = c.id + "/" + name
	}
	child := &Module{Name: name, id: id, reg: c.reg, contract: c.contract, priority: c.priority, order: c.order, trusted: c.trusted}
	c.mu.Lock()
	if c.disposed {
		child.disposed = true
	} else {
		c.children = append(c.children, child)
	}
	c.mu.Unlock()
	return child
}

// Isolate creates a child kernel: it sees the parent's services and its
// trusted hooks (policy, audit), but has its own tools, skills and hooks.
// Used for subagents.
func (c *Module) Isolate(name string) *Module {
	child := &Module{Name: name, id: c.id + "/" + name, reg: newRegistry(c.reg), contract: c.contract, trusted: c.trusted}
	c.onDispose(child.Dispose)
	return child
}

// TrustKey proves the caller is the engine. Exactly one exists per
// process: the engine claims it at init, before any module can run, so
// interpreted modules calling ClaimTrustKey get nil.
type TrustKey struct{ _ int }

var (
	engineKey *TrustKey
	keyOnce   sync.Once
)

func ClaimTrustKey() *TrustKey {
	var k *TrustKey
	keyOnce.Do(func() { engineKey = &TrustKey{}; k = engineKey })
	return k
}

// Trust sets load order and trust on a scope. Without the engine's key
// it does nothing.
func (c *Module) Trust(key *TrustKey, order int, trusted bool) {
	if key == nil || key != engineKey {
		return
	}
	c.order, c.trusted = order, trusted
}

func (c *Module) Trusted() bool { return c.trusted }

// SetSecretPolicy is called by the engine before Apply. Without the
// engine's key it does nothing, so a module can't widen its own grant.
func (c *Module) SetSecretPolicy(key *TrustKey, grants []string, restrict bool) {
	if key == nil || key != engineKey {
		return
	}
	c.secretGrants = append([]string(nil), grants...)
	c.restrictSecrets = restrict
}

// SetBuiltin is called by the engine. Only built-in modules may register
// extra values for redaction.
func (c *Module) SetBuiltin(key *TrustKey, builtin bool) {
	if key == nil || key != engineKey {
		return
	}
	c.builtin = builtin
}

// RegisterSecret puts a value the module obtained outside the secrets
// service (for example a Vault token read from a file) into the
// process-wide redaction set. It is a no-op for non-built-in modules, so
// a third-party module can't blank out words in everyone's audit log.
func (c *Module) RegisterSecret(v string) bool {
	if !c.builtin || v == "" {
		return false
	}
	return AddRedaction(v)
}

// ID is the scope's immutable identity ("module" or "module/child").
func (c *Module) ID() string { return c.id }

// PluginName is the top-level module instance that owns this scope.
func (c *Module) PluginName() string {
	if i := strings.Index(c.id, "/"); i >= 0 {
		return c.id[:i]
	}
	return c.id
}

// SetPriority sets the priority of hooks registered after this call.
// Higher runs earlier / further out. Values >= PriorityReserved are
// clamped unless the module is trusted.
func (c *Module) SetPriority(p int) {
	if p >= PriorityReserved && !c.trusted {
		p = PriorityReserved - 1
	}
	c.priority = p
}

// Priority is the priority hooks registered now will get.
func (c *Module) Priority() int { return c.priority }

func (c *Module) onDispose(f func()) {
	c.mu.Lock()
	if c.disposed {
		// A goroutine registered something after the module was unloaded:
		// undo it now instead of leaking it into the registry.
		c.mu.Unlock()
		f()
		return
	}
	c.disposers = append(c.disposers, f)
	c.mu.Unlock()
}

// Dispose runs all disposers in reverse order (children first).
func (c *Module) Dispose() {
	c.mu.Lock()
	kids, ds := c.children, c.disposers
	c.children, c.disposers = nil, nil
	c.disposed = true
	c.mu.Unlock()
	for i := len(kids) - 1; i >= 0; i-- {
		kids[i].Dispose()
	}
	for i := len(ds) - 1; i >= 0; i-- {
		ds[i]()
	}
}

func (c *Module) nextID() int { c.reg.seq++; return c.reg.seq }

// Effect runs fn now; the cleanup it returns runs when the scope is
// disposed. Use it for timers, goroutines, connections, processes.
func (c *Module) Effect(fn func() func()) {
	if cleanup := fn(); cleanup != nil {
		c.onDispose(cleanup)
	}
}

// OnUnload registers a cleanup that runs when the module is unloaded,
// reloaded or the agent closes.
func (c *Module) OnUnload(fn func()) { c.onDispose(fn) }

// ---- Services ----

// Provide registers a service. The name must be in the module's declared
// Provides list (a pattern ending in "*" matches a prefix); duplicate names
// are an error so two modules can't silently fight over one name.
func (c *Module) Provide(name string, svc any) error {
	if !c.mayProvide(name) {
		return fmt.Errorf("module %s may not provide %q: declare it in Provides", c.PluginName(), name)
	}
	r := c.reg
	r.mu.Lock()
	if _, taken := r.services[name]; taken {
		r.mu.Unlock()
		return fmt.Errorf("service %q already provided", name)
	}
	id := c.nextID()
	r.services[name] = entry[any]{id, c.Name, svc}
	r.mu.Unlock()
	c.onDispose(func() {
		r.mu.Lock()
		if e, ok := r.services[name]; ok && e.id == id {
			delete(r.services, name)
		}
		r.mu.Unlock()
	})
	return nil
}

func (r *registry) service(name string) (any, bool) {
	for ; r != nil; r = r.parent {
		r.mu.RLock()
		e, ok := r.services[name]
		r.mu.RUnlock()
		if ok {
			return e.v, true
		}
	}
	return nil, false
}

func (c *Module) Service(name string) any {
	v, _ := c.reg.service(name)
	if s, ok := v.(*Secrets); ok && name == "secrets" && s != nil {
		return c.boundSecrets(s)
	}
	if a, ok := v.(*Audit); ok && name == "audit" && a != nil && c.id != "" {
		return c.boundAudit(a)
	}
	if p, ok := v.(*SandboxProvider); ok && strings.HasPrefix(name, "sandbox/") && p != nil && c.id != "" {
		return moduleSandboxes(p, c.builtin)
	}
	return v
}

func (c *Module) HasService(name string) bool {
	_, ok := c.reg.service(name)
	return ok
}

// Services lists service names with the given prefix ("" = all).
func (c *Module) Services(prefix string) []string {
	seen := map[string]bool{}
	var out []string
	for r := c.reg; r != nil; r = r.parent {
		r.mu.RLock()
		for k := range r.services {
			if strings.HasPrefix(k, prefix) && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
		r.mu.RUnlock()
	}
	sortStrings(out)
	return out
}

// ---- Tools and skills ----

// AddTool registers a tool. A name another module already registered
// is refused (logged, first one wins), so a later module can't take over
// a tool such as issue_refund; a module may re-register its own names.
// Tool.Module is always set to the registering module.
func (c *Module) AddTool(t Tool) {
	t.Module = c.PluginName()
	r := c.reg
	r.mu.Lock()
	if e, ok := r.tools[t.Name]; ok && e.v.Module != t.Module {
		r.mu.Unlock()
		c.Logf("tool %q is already registered by %s; not replacing it", t.Name, e.v.Module)
		return
	}
	id := c.nextID()
	r.tools[t.Name] = entry[Tool]{id, c.PluginName(), t}
	r.mu.Unlock()
	c.onDispose(func() {
		r.mu.Lock()
		if e, ok := r.tools[t.Name]; ok && e.id == id {
			delete(r.tools, t.Name)
		}
		r.mu.Unlock()
	})
}

// AddSkill registers a skill; like tools, another module's name is refused.
func (c *Module) AddSkill(s Skill) {
	r := c.reg
	r.mu.Lock()
	if e, ok := r.skills[s.Name]; ok && e.owner != c.PluginName() {
		r.mu.Unlock()
		c.Logf("skill %q is already registered by %s; not replacing it", s.Name, e.owner)
		return
	}
	id := c.nextID()
	r.skills[s.Name] = entry[Skill]{id, c.PluginName(), s}
	r.mu.Unlock()
	c.onDispose(func() {
		r.mu.Lock()
		if e, ok := r.skills[s.Name]; ok && e.id == id {
			delete(r.skills, s.Name)
		}
		r.mu.Unlock()
	})
}

// ---- Hooks ----

func (c *Module) hook(kind string, fn any) {
	r := c.reg
	r.mu.Lock()
	id := c.nextID()
	r.hooks[kind] = append(r.hooks[kind], hookEntry{id: id, prio: c.priority, order: c.order, trusted: c.trusted, fn: fn})
	r.mu.Unlock()
	c.onDispose(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		hs := r.hooks[kind]
		for i, h := range hs {
			if h.id == id {
				r.hooks[kind] = append(hs[:i:i], hs[i+1:]...)
				return
			}
		}
	})
}

func (c *Module) Logf(format string, args ...any) {
	c.reg.log(c.Name, Redact(fmt.Sprintf(format, args...)))
}

// ConfigString / ConfigInt / ConfigBool read module config with defaults.
func (c *Module) ConfigString(key, def string) string {
	if v, ok := c.Config[key].(string); ok && v != "" {
		return v
	}
	return def
}

func (c *Module) ConfigInt(key string, def int) int {
	switch v := c.Config[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return def
}

func (c *Module) ConfigFloat(key string, def float64) float64 {
	switch v := c.Config[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return def
}

func (c *Module) ConfigBool(key string, def bool) bool {
	if v, ok := c.Config[key].(bool); ok {
		return v
	}
	return def
}

func (c *Module) ConfigStrings(key string) []string {
	switch v := c.Config[key].(type) {
	case []string:
		return v
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (c *Module) ConfigMap(key string) map[string]any {
	if v, ok := c.Config[key].(map[string]any); ok {
		return v
	}
	return nil
}

func (c *Module) ConfigList(key string) []map[string]any {
	raw, _ := c.Config[key].([]any)
	var out []map[string]any
	for _, x := range raw {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

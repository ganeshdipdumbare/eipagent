package sdk

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// PriorityReserved is the lowest priority only trusted (locked, policy)
// plugins may use, so policy hooks always run outermost / first.
const PriorityReserved = 1_000_000

type hookEntry struct {
	id      int
	prio    int
	order   int // plugin load order (profile position)
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

// Context is a scope. Everything registered through it is undone when
// it is disposed, which is how unload and hot reload stay clean.
type Context struct {
	Name   string
	Config map[string]any // per-plugin config from the profile

	reg       *registry
	id        string // immutable identity: plugin instance path, used for audit / secret ACLs
	priority  int
	order     int
	trusted   bool
	mu        sync.Mutex
	children  []*Context
	disposers []func()
}

func NewRoot() *Context { return &Context{Name: "root", reg: newRegistry(nil)} }

// SetLogger replaces where Logf output goes (whole registry).
func (c *Context) SetLogger(fn func(scope, msg string)) { c.reg.log = fn }

// Fork creates a child scope sharing the registry. Disposing the parent
// disposes the child.
func (c *Context) Fork(name string) *Context {
	id := name
	if c.id != "" {
		id = c.id + "/" + name
	}
	child := &Context{Name: name, id: id, reg: c.reg, priority: c.priority, order: c.order, trusted: c.trusted}
	c.mu.Lock()
	c.children = append(c.children, child)
	c.mu.Unlock()
	return child
}

// Isolate creates a child kernel: it sees the parent's services and its
// trusted hooks (policy, audit), but has its own tools, skills and hooks.
// Used for subagents.
func (c *Context) Isolate(name string) *Context {
	child := &Context{Name: name, id: c.id + "/" + name, reg: newRegistry(c.reg), trusted: c.trusted}
	c.onDispose(child.Dispose)
	return child
}

// TrustKey proves the caller is the engine. Exactly one exists per
// process: the engine claims it at init, before any plugin can run, so
// interpreted plugins calling ClaimTrustKey get nil.
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
func (c *Context) Trust(key *TrustKey, order int, trusted bool) {
	if key == nil || key != engineKey {
		return
	}
	c.order, c.trusted = order, trusted
}

func (c *Context) Trusted() bool { return c.trusted }

// ID is the scope's immutable identity ("plugin" or "plugin/child").
func (c *Context) ID() string { return c.id }

// PluginName is the top-level plugin instance that owns this scope.
func (c *Context) PluginName() string {
	if i := strings.Index(c.id, "/"); i >= 0 {
		return c.id[:i]
	}
	return c.id
}

// SetPriority sets the priority of hooks registered after this call.
// Higher runs earlier / further out. Values >= PriorityReserved are
// clamped unless the plugin is trusted.
func (c *Context) SetPriority(p int) {
	if p >= PriorityReserved && !c.trusted {
		p = PriorityReserved - 1
	}
	c.priority = p
}

// Priority is the priority hooks registered now will get.
func (c *Context) Priority() int { return c.priority }

func (c *Context) onDispose(f func()) {
	c.mu.Lock()
	c.disposers = append(c.disposers, f)
	c.mu.Unlock()
}

// Dispose runs all disposers in reverse order (children first).
func (c *Context) Dispose() {
	c.mu.Lock()
	kids, ds := c.children, c.disposers
	c.children, c.disposers = nil, nil
	c.mu.Unlock()
	for i := len(kids) - 1; i >= 0; i-- {
		kids[i].Dispose()
	}
	for i := len(ds) - 1; i >= 0; i-- {
		ds[i]()
	}
}

func (c *Context) nextID() int { c.reg.seq++; return c.reg.seq }

// Effect runs fn now; the cleanup it returns runs when the scope is
// disposed. Use it for timers, goroutines, connections, processes.
func (c *Context) Effect(fn func() func()) {
	if cleanup := fn(); cleanup != nil {
		c.onDispose(cleanup)
	}
}

// OnDispose registers a cleanup without a setup step.
func (c *Context) OnDispose(fn func()) { c.onDispose(fn) }

// ---- Services ----

// Provide registers a service. Duplicate names are an error so two
// plugins can't silently fight over one name; unload one to swap.
func (c *Context) Provide(name string, svc any) error {
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

func (c *Context) Service(name string) any {
	v, _ := c.reg.service(name)
	if s, ok := v.(*Secrets); ok && name == "secrets" && s != nil {
		return c.boundSecrets(s)
	}
	return v
}

func (c *Context) HasService(name string) bool {
	_, ok := c.reg.service(name)
	return ok
}

// Services lists service names with the given prefix ("" = all).
func (c *Context) Services(prefix string) []string {
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

func (c *Context) AddTool(t Tool) {
	if t.Plugin == "" {
		t.Plugin = c.PluginName()
	}
	r := c.reg
	r.mu.Lock()
	id := c.nextID()
	r.tools[t.Name] = entry[Tool]{id, c.Name, t}
	r.mu.Unlock()
	c.onDispose(func() {
		r.mu.Lock()
		if e, ok := r.tools[t.Name]; ok && e.id == id {
			delete(r.tools, t.Name)
		}
		r.mu.Unlock()
	})
}

func (c *Context) AddSkill(s Skill) {
	r := c.reg
	r.mu.Lock()
	id := c.nextID()
	r.skills[s.Name] = entry[Skill]{id, c.Name, s}
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

func (c *Context) hook(kind string, fn any) {
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

func (c *Context) Logf(format string, args ...any) {
	c.reg.log(c.Name, Redact(fmt.Sprintf(format, args...)))
}

// ConfigString / ConfigInt / ConfigBool read plugin config with defaults.
func (c *Context) ConfigString(key, def string) string {
	if v, ok := c.Config[key].(string); ok && v != "" {
		return v
	}
	return def
}

func (c *Context) ConfigInt(key string, def int) int {
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

func (c *Context) ConfigFloat(key string, def float64) float64 {
	switch v := c.Config[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return def
}

func (c *Context) ConfigBool(key string, def bool) bool {
	if v, ok := c.Config[key].(bool); ok {
		return v
	}
	return def
}

func (c *Context) ConfigStrings(key string) []string {
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

func (c *Context) ConfigMap(key string) map[string]any {
	if v, ok := c.Config[key].(map[string]any); ok {
		return v
	}
	return nil
}

func (c *Context) ConfigList(key string) []map[string]any {
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

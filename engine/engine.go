// Package engine is the eipagent core: plugin sources, the trust gate,
// lifecycle and dependency reconcile, hot reload. It knows nothing about
// models, tools, loops or memory; those are all plugins.
package engine

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// Plugin lifecycle (Cordis):
//
//	PENDING -> LOADING -> ACTIVE -> (dependency lost) -> PENDING
//	                  \-> FAILED
type State string

const (
	Pending State = "PENDING"
	Loading State = "LOADING"
	Active  State = "ACTIVE"
	Failed  State = "FAILED"
)

var trustKey = sdk.ClaimTrustKey()

// Plugin is a compiled (native Go) plugin. It goes through the same
// lifecycle as interpreted ones.
type Plugin struct {
	Name        string
	Inject      []string
	Permissions []string // informational for compiled plugins
	Apply       func(*sdk.Context) error
}

type record struct {
	spec    PluginSpec
	name    string
	kind    string // compiled | file | embedded
	path    string
	hash    string
	mod     time.Time
	inject  []string
	perms   []string
	config  map[string]any
	apply   func(*sdk.Context) error
	ctx     *sdk.Context
	state   State
	err     error
	order   int
	locked  bool
	optSeen string
}

type Options struct {
	Dirs     []string // user plugin directories, searched in order
	Embedded fs.FS    // stdlib plugins (plugins.FS)
	Policy   *Policy  // nil = DevPolicy
	Lock     *Lock
	Logger   func(scope, msg string)
}

type Engine struct {
	mu       sync.Mutex
	opts     Options
	pol      *Policy
	root     *sdk.Context
	compiled map[string]Plugin
	recs     map[string]*record
	nextOrd  int
}

func New(opts Options) *Engine {
	pol := opts.Policy
	if pol == nil {
		pol = DevPolicy()
	}
	if opts.Lock == nil {
		opts.Lock = &Lock{Plugins: map[string]string{}}
	}
	root := sdk.NewRoot()
	if opts.Logger != nil {
		root.SetLogger(opts.Logger)
	}
	return &Engine{opts: opts, pol: pol, root: root, compiled: map[string]Plugin{}, recs: map[string]*record{}}
}

func (e *Engine) Root() *sdk.Context { return e.root }
func (e *Engine) Policy() *Policy    { return e.pol }

func (e *Engine) logf(format string, args ...any) {
	e.root.Logf(format, args...)
}

// audit reports lifecycle events to the "audit" service, if any.
func (e *Engine) audit(kind string, r *record, err error) {
	a, ok := e.root.Service("audit").(*sdk.Audit)
	if !ok || r == nil {
		return
	}
	data := map[string]any{"plugin": r.name, "source": r.spec.Use, "kind": r.kind, "sha256": r.hash, "perms": r.perms, "locked": r.locked, "writer": "core"}
	if err != nil {
		data["error"] = err.Error()
	}
	a.Record(kind, data)
}

// Register adds a compiled plugin source. It wins over files of the same name.
func (e *Engine) Register(p Plugin) {
	e.mu.Lock()
	e.compiled[p.Name] = p
	e.mu.Unlock()
}

// ---- Sources and the trust gate ----

type source struct {
	kind string
	path string
	src  []byte
	mod  time.Time
}

// validName accepts plugin names and sources: letters, digits, '_' and
// '-', so a name can never point outside the plugin directories.
func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (e *Engine) find(use string) (*source, error) {
	if !validName(use) {
		return nil, fmt.Errorf("plugin %q: invalid name (letters, digits, '_' and '-' only)", use)
	}
	for _, dir := range e.opts.Dirs {
		path := filepath.Join(dir, use+".go")
		if b, err := os.ReadFile(path); err == nil {
			st, _ := os.Stat(path)
			return &source{kind: "file", path: path, src: b, mod: st.ModTime()}, nil
		}
	}
	if e.opts.Embedded != nil {
		if b, err := fs.ReadFile(e.opts.Embedded, use+".go"); err == nil {
			return &source{kind: "embedded", path: "embedded:" + use, src: b}, nil
		}
	}
	return nil, fmt.Errorf("plugin %q not found", use)
}

// meta is what the engine learns from a plugin's source before running it.
type meta struct {
	imports     []string
	inject      []string
	permissions []string
}

func parseMeta(name string, src []byte) (*meta, error) {
	f, err := parser.ParseFile(token.NewFileSet(), name+".go", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	if f.Name.Name != "plugin" {
		return nil, fmt.Errorf("%s: package must be \"plugin\", got %q", name, f.Name.Name)
	}
	m := &meta{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		m.imports = append(m.imports, p)
	}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if i >= len(vs.Values) || (id.Name != "Inject" && id.Name != "Permissions") {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					return nil, fmt.Errorf("%s: %s must be a []string literal", name, id.Name)
				}
				var vals []string
				for _, el := range lit.Elts {
					bl, ok := el.(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						return nil, fmt.Errorf("%s: %s must contain string literals", name, id.Name)
					}
					s, _ := strconv.Unquote(bl.Value)
					vals = append(vals, s)
				}
				if id.Name == "Inject" {
					m.inject = vals
				} else {
					m.permissions = vals
				}
			}
		}
	}
	return m, nil
}

func (e *Engine) compile(spec PluginSpec) (*record, error) {
	name, use := spec.Name(), spec.Use
	if !validName(name) {
		return nil, fmt.Errorf("plugin %q: invalid name (letters, digits, '_' and '-' only)", name)
	}
	if !e.pol.allowed(use) {
		return nil, fmt.Errorf("plugin %s: not allowed by policy", use)
	}
	cfg := map[string]any{}
	if spec.Config != nil {
		cfg = interpolate(spec.Config).(map[string]any)
	}
	if e.pol.locked(name) {
		if rules := e.pol.Rules; rules != nil && use == "policy" {
			cfg["rules"] = rules
		}
	}
	rec := &record{spec: spec, name: name, config: cfg, state: Pending, locked: e.pol.locked(name)}
	if p, ok := e.compiled[use]; ok {
		rec.kind, rec.path, rec.inject, rec.perms, rec.apply = "compiled", "compiled:"+use, p.Inject, p.Permissions, p.Apply
		return rec, nil
	}
	src, err := e.find(use)
	if err != nil {
		return nil, err
	}
	rec.kind, rec.path, rec.mod, rec.hash = src.kind, src.path, src.mod, Hash(src.src)
	// Embedded plugins ship inside the binary, so they're trusted like code.
	if src.kind == "file" && e.pol.RequirePins {
		want, ok := e.opts.Lock.Plugins[use]
		if !ok {
			return nil, fmt.Errorf("plugin %s: not pinned in eip.lock (policy requires pins)", use)
		}
		if want != rec.hash {
			return nil, fmt.Errorf("plugin %s: sha256 %s does not match eip.lock pin %s", use, rec.hash[:12], short(want))
		}
	}
	m, err := parseMeta(use, src.src)
	if err != nil {
		return nil, err
	}
	rec.inject = m.inject
	rec.perms = e.pol.grant(name, m.permissions)
	if err := checkImports(use, m.imports, rec.perms); err != nil {
		if len(rec.perms) < len(m.permissions) {
			err = fmt.Errorf("%w (policy granted %v of declared %v)", err, rec.perms, m.permissions)
		}
		return nil, err
	}
	i := newInterp(pluginEnv(rec.perms))
	if err := i.Use(buildSymbols(rec.perms, src.kind == "embedded")); err != nil {
		return nil, err
	}
	if _, err := i.Eval(string(src.src)); err != nil {
		return nil, fmt.Errorf("compile %s: %w", use, err)
	}
	v, err := i.Eval("plugin.Apply")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", use, err)
	}
	apply, ok := v.Interface().(func(*sdk.Context) error)
	if !ok {
		return nil, fmt.Errorf("%s: Apply must be func(*sdk.Context) error", use)
	}
	rec.apply = apply
	return rec, nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// ---- Lifecycle ----

var ErrLocked = errors.New("plugin is locked by policy")

// Load loads (or replaces) a plugin instance. Dependents cascade.
func (e *Engine) Load(spec PluginSpec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.loadLocked(spec, false)
}

func (e *Engine) loadLocked(spec PluginSpec, admin bool) error {
	name := spec.Name()
	old := e.recs[name]
	if old != nil && old.locked && !admin {
		return fmt.Errorf("%s: %w", name, ErrLocked)
	}
	rec, err := e.compile(spec)
	if err != nil {
		e.audit("plugin.refused", &record{name: name, spec: spec}, err)
		if old != nil && old.kind == "file" {
			if st, e2 := os.Stat(old.path); e2 == nil {
				old.mod = st.ModTime() // don't retry a broken file until it changes
			}
		}
		return err // a broken edit never takes down the running version
	}
	wasActive := old != nil && old.state == Active
	if old != nil {
		rec.order = old.order
		e.disposeLocked(old)
	} else {
		rec.order = e.nextOrd
		e.nextOrd++
	}
	e.recs[name] = rec
	e.reconcileLocked()
	if rec.state != Active && wasActive {
		// Safe replacement: a version that fails to start, or is left
		// waiting on a missing dependency, never takes down the one that
		// was running. Bring the old one back.
		failed := rec.err
		if failed == nil {
			failed = fmt.Errorf("left %s: missing %v", rec.state, e.missing(rec))
		}
		e.disposeLocked(rec)
		old.state, old.err = Pending, nil
		e.recs[name] = old
		e.reconcileLocked()
		e.logf("%s: new version failed, previous version restored", name)
		return fmt.Errorf("%s: new version failed (previous version kept): %w", name, failed)
	}
	return rec.err
}

// Reload recompiles a loaded plugin from its source with the same config.
func (e *Engine) Reload(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	old := e.recs[name]
	if old == nil {
		return fmt.Errorf("plugin %q not loaded", name)
	}
	return e.loadLocked(old.spec, false)
}

func (e *Engine) Unload(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	old, ok := e.recs[name]
	if !ok {
		return fmt.Errorf("plugin %q not loaded", name)
	}
	if old.locked {
		return fmt.Errorf("%s: %w", name, ErrLocked)
	}
	e.disposeLocked(old)
	delete(e.recs, name)
	e.reconcileLocked()
	return nil
}

// Close disposes every plugin, last loaded first.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	recs := e.sortedLocked()
	for i := len(recs) - 1; i >= 0; i-- {
		e.disposeLocked(recs[i])
	}
	e.recs = map[string]*record{}
	e.root.Dispose()
}

func (e *Engine) disposeLocked(r *record) {
	if r != nil && r.ctx != nil {
		e.audit("plugin.disposed", r, nil)
		r.ctx.Dispose()
		r.ctx = nil
	}
}

// missing returns required dependencies that are not provided.
func (e *Engine) missing(r *record) []string {
	var m []string
	for _, dep := range r.inject {
		if strings.HasPrefix(dep, "?") {
			continue
		}
		if strings.HasSuffix(dep, "*") {
			if len(e.root.Services(strings.TrimSuffix(dep, "*"))) == 0 {
				m = append(m, dep)
			}
			continue
		}
		if !e.root.HasService(dep) {
			m = append(m, dep)
		}
	}
	return m
}

// optional fingerprints which optional deps currently exist.
func (e *Engine) optional(r *record) string {
	var b strings.Builder
	for _, dep := range r.inject {
		if strings.HasPrefix(dep, "?") {
			fmt.Fprintf(&b, "%s=%v;", dep, e.root.HasService(dep[1:]))
		}
	}
	return b.String()
}

// reconcileLocked moves every plugin toward its desired state until
// stable: ACTIVE plugins that lost a dependency go back to PENDING,
// PENDING plugins whose dependencies exist are applied, ACTIVE plugins
// whose optional deps changed restart.
func (e *Engine) reconcileLocked() {
	for guard := 0; guard < 100; guard++ {
		changed := false
		for _, r := range e.sortedLocked() {
			switch {
			case r.state == Active && len(e.missing(r)) > 0:
				e.disposeLocked(r)
				r.state = Pending
				e.logf("%s: dependency lost -> PENDING", r.name)
				changed = true
			case r.state == Active && r.optSeen != e.optional(r):
				e.disposeLocked(r)
				r.state = Pending
				changed = true
			case r.state == Pending && len(e.missing(r)) == 0:
				e.startLocked(r)
				changed = true
			}
		}
		if !changed {
			return
		}
	}
	e.logf("reconcile did not settle (dependency cycle?)")
}

func (e *Engine) startLocked(r *record) {
	r.state, r.err = Loading, nil
	r.optSeen = e.optional(r)
	ctx := e.root.Fork(r.name)
	ctx.Trust(trustKey, r.order, r.locked)
	ctx.Config = r.config
	if r.spec.Priority != nil {
		ctx.SetPriority(*r.spec.Priority)
	}
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
			}
		}()
		return r.apply(ctx)
	}()
	if err != nil {
		ctx.Dispose() // roll back whatever it registered
		r.state, r.err = Failed, err
		e.logf("%s: FAILED: %v", r.name, err)
		e.audit("plugin.failed", r, err)
		return
	}
	r.ctx, r.state = ctx, Active
	e.audit("plugin.active", r, nil)
}

func (e *Engine) sortedLocked() []*record {
	out := make([]*record, 0, len(e.recs))
	for _, r := range e.recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].order < out[j].order })
	return out
}

// ---- Profiles ----

// LoadProfile loads every enabled row plus the policy's required
// plugins. It returns one error per plugin that failed to load.
func (e *Engine) LoadProfile(p *Profile) []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	// Policy-required plugins load first (audit sees everything after them)
	// and replace any profile row with the same name.
	specs := append([]PluginSpec{}, e.pol.Require...)
	required := len(specs) // only these act with admin rights (may replace locked plugins)
	for _, s := range p.Plugins {
		dup := false
		for _, req := range e.pol.Require {
			if req.Name() == s.Name() {
				dup = true
			}
		}
		if !dup {
			specs = append(specs, s)
		}
	}
	for i, s := range specs {
		if s.Disabled && !e.pol.locked(s.Name()) {
			continue
		}
		if err := e.loadLocked(s, i < required); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// ---- Introspection ----

type PluginStatus struct {
	Name, Source, Kind, Hash string
	State                    State
	Missing                  []string
	Perms                    []string
	Locked                   bool
	Err                      error
}

func (e *Engine) Status() []PluginStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []PluginStatus
	for _, r := range e.sortedLocked() {
		st := PluginStatus{Name: r.name, Source: r.spec.Use, Kind: r.kind, Hash: r.hash, State: r.state, Perms: r.perms, Locked: r.locked, Err: r.err}
		if r.state == Pending {
			st.Missing = e.missing(r)
		}
		out = append(out, st)
	}
	return out
}

func (s PluginStatus) String() string {
	row := fmt.Sprintf("%-18s %-8s %-8s", s.Name, s.State, s.Kind)
	if s.Locked {
		row += " locked"
	}
	if len(s.Perms) > 0 {
		row += " perms=" + strings.Join(s.Perms, ",")
	}
	if len(s.Missing) > 0 {
		row += "  waiting for: " + strings.Join(s.Missing, ", ")
	}
	if s.Err != nil {
		row += "  " + s.Err.Error()
	}
	return row
}

func (e *Engine) State(name string) State {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r := e.recs[name]; r != nil {
		return r.state
	}
	return ""
}

// Pin computes sha256 pins for every loaded file/embedded plugin.
func (e *Engine) Pins() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	for _, r := range e.recs {
		if r.hash != "" {
			out[r.spec.Use] = r.hash
		}
	}
	return out
}

// Watch hot-reloads file plugins whose source changed. It does nothing
// unless the policy enables hot reload; locked plugins never reload.
func (e *Engine) Watch(stop <-chan struct{}, say func(string)) {
	if !e.pol.HotReload {
		return
	}
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			e.mu.Lock()
			var changed []string
			for _, r := range e.recs {
				if r.kind != "file" || r.locked {
					continue
				}
				if st, err := os.Stat(r.path); err == nil && !st.ModTime().Equal(r.mod) {
					changed = append(changed, r.name)
				}
			}
			e.mu.Unlock()
			for _, n := range changed {
				if err := e.Reload(n); err != nil {
					say(fmt.Sprintf("hot-reload %s failed (old version kept): %v", n, err))
				} else {
					say("hot-reloaded " + n)
				}
			}
		}
	}
}

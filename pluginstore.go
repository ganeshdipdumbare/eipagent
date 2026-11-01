package eipagent

import (
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/pluginpkg"
	"github.com/ganeshdipdumbare/eipagent/plugins"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// WithPluginStore loads the plugins enabled in an installed-plugin store
// and lets the store manage them at runtime: Install, Approve, Enable,
// Update, Rollback, Quarantine and Uninstall act on the running agent.
// Only the store's pinned sources load from its directory.
//
//	st, _ := pluginpkg.OpenStore(".eipagent/plugins", pluginpkg.StoreOptions{
//		Verify: pluginpkg.VerifyOptions{TrustedKeys: keys, RequireSignature: true},
//	})
//	a, _ := eipagent.New(eipagent.WithPluginStore(st))
//	st.Install(pkgBytes, "alice")   // VALIDATED: held, nothing runs
//	st.Approve("weather", "0.1.0", "bob")
//	st.Enable("weather", "")        // ACTIVE in the running agent
func WithPluginStore(st *pluginpkg.Store) Option { return func(c *config) { c.store = st } }

// builtinNames lists the embedded plugins, under their current and
// pre-v0.4 names (a store can't shadow either).
func builtinNames() []string {
	var out []string
	for old := range sdk.PluginRenames {
		out = append(out, old)
	}
	ents, _ := fs.ReadDir(plugins.FS, ".")
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".go"); ok {
			out = append(out, n)
		}
	}
	return out
}

// storeRuntime activates store plugins in a live engine.
type storeRuntime struct {
	eng *engine.Engine
	mu  sync.Mutex
	pin map[string]string // what is pinned now, to restore on failure
}

func (r *storeRuntime) Activate(name, hash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.pin[name]
	r.eng.Pin(name, hash)
	var err error
	loaded := r.eng.Loaded(name)
	if loaded {
		err = r.eng.Reload(name) // keeps the running version if the new one fails
	} else {
		err = r.eng.Load(engine.PluginSpec{Use: name})
	}
	if err == nil {
		switch st := r.eng.State(name); st {
		case engine.Active:
		case engine.Pending: // dependency readiness
			err = fmt.Errorf("%s: missing dependencies %v", name, r.eng.Missing(name))
		default:
			err = fmt.Errorf("%s: %s after activation", name, st)
		}
	}
	if err != nil {
		r.eng.Pin(name, old)
		if !loaded && r.eng.Loaded(name) {
			r.eng.Unload(name)
		}
		return err
	}
	r.pin[name] = hash
	return nil
}

func (r *storeRuntime) Deactivate(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eng.Loaded(name) {
		if err := r.eng.Unload(name); err != nil {
			return err
		}
	}
	r.eng.Pin(name, "")
	delete(r.pin, name)
	return nil
}

// attachStore wires a store into a new engine: pins, the plugin dir,
// the runtime adapter and audit.
func attachStore(st *pluginpkg.Store, eng *engine.Engine, prof *engine.Profile) {
	rt := &storeRuntime{eng: eng, pin: st.Pins()}
	for n, h := range rt.pin {
		eng.Pin(n, h)
	}
	st.Reserve(builtinNames()...)
	st.SetRuntime(rt)
	st.SetAudit(func(kind string, data map[string]any) {
		if a, ok := eng.Root().Service("audit").(*sdk.Audit); ok && a.Record != nil {
			data["writer"] = "core"
			a.Record(kind, data)
		}
	})
	listed := map[string]bool{}
	for _, s := range prof.Plugins {
		listed[s.Use] = true
	}
	for _, n := range st.ActiveNames() {
		if !listed[n] {
			prof.Plugins = append(prof.Plugins, engine.PluginSpec{Use: n})
		}
	}
}

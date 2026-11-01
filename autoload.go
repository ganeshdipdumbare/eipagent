package eipagent

import (
	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// autoPlugins are loaded when a plugin waits on a service nobody
// provides, so apps don't have to know about plumbing: shell and file
// tools get a workspace on the host, *_sql plugins get the database
// connection, and so on. Each shows as "auto" in Status. Loading the
// service yourself (or a policy that denies the default) always wins.
var autoPlugins = map[string]string{
	"workspace":     "workspace_local",
	"sandbox/local": "sandbox_local",
	"db":            "db_sql",
	"threads":       "threads_inmem",
	"memory":        "memory_inmem",
	"loop":          "loop",
}

// autoload loads the default provider of every missing service, until
// nothing more can be resolved.
func autoload(eng *engine.Engine) {
	tried := map[string]bool{}
	for round := 0; round < 4; round++ {
		progress := false
		for _, st := range eng.Status() {
			if st.State != engine.Pending {
				continue
			}
			for _, dep := range st.Missing {
				use, ok := autoPlugins[sdk.CanonicalService(dep)]
				if !ok || tried[use] || eng.Loaded(use) {
					continue
				}
				tried[use] = true
				if err := eng.Load(engine.PluginSpec{Use: use, Auto: true}); err == nil {
					progress = true
				} else {
					eng.Root().Logf("could not load %s for %s automatically: %v", use, st.Name, err)
				}
			}
		}
		if !progress {
			return
		}
	}
}

func canonicalUse(use string) string { return sdk.CanonicalPlugin(use) }

//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"eipagent/sdk"
)

// Shell and file tools act in a sandbox, one per thread, created lazily
// on first use and re-attached when a thread resumes in a new process.
// Provided as "workspace".
//
// Config: provider ("docker"), image, memory_mb, cpus, network ("none"),
// idle_ttl_seconds (1800).
//
// Example:
//
//	eipagent.WithPlugin("workspace_sandbox", map[string]any{
//		"provider":         "docker",
//		"image":            "python:3.12-slim",
//		"idle_ttl_seconds": 900,
//	})
//	eipagent.WithPlugins("sandbox_docker", "tool_shell", "tool_fs")
var Requires = []string{"sandbox/*"}

type entry struct {
	box  *sdk.Sandbox
	used time.Time
}

func Apply(p *sdk.Plugin) error {
	provName := p.ConfigString("provider", "docker")
	spec := sdk.SandboxSpec{
		Image:    p.ConfigString("image", ""),
		MemoryMB: p.ConfigInt("memory_mb", 0),
		CPUs:     p.ConfigFloat("cpus", 0),
		Network:  sdk.NetworkPolicy{Mode: p.ConfigString("network", "none"), Allow: p.ConfigStrings("allow_hosts")},
	}
	ttl := time.Duration(p.ConfigInt("idle_ttl_seconds", 1800)) * time.Second
	var mu sync.Mutex
	boxes := map[string]*entry{}

	provider := func() (*sdk.SandboxProvider, error) {
		sp, ok := p.Service("sandbox/" + provName).(*sdk.SandboxProvider)
		if !ok {
			return nil, fmt.Errorf("workspace_sandbox: no sandbox/%s (load sandbox_%s)", provName, provName)
		}
		return sp, nil
	}
	key := func(thread string) string {
		if thread == "" {
			return "_default"
		}
		return thread
	}

	p.OnRunStart(func(c context.Context, st *sdk.RunState) error {
		id, _ := st.Values["exec_sandbox.id"].(string)
		k := key(st.Thread)
		mu.Lock()
		_, have := boxes[k]
		mu.Unlock()
		if id == "" || have {
			return nil
		}
		sp, err := provider()
		if err != nil || sp.Attach == nil {
			return nil
		}
		if box, err := sp.Attach(c, id); err == nil {
			mu.Lock()
			boxes[k] = &entry{box, time.Now()}
			mu.Unlock()
		}
		return nil
	})
	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		e := boxes[key(st.Thread)]
		mu.Unlock()
		if e != nil && st.Values != nil {
			st.Values["exec_sandbox.id"] = e.box.ID
		}
		return nil
	})
	p.Effect(func() func() {
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					mu.Lock()
					for k, e := range boxes {
						if time.Since(e.used) > ttl {
							e.box.Close(context.Background())
							delete(boxes, k)
						}
					}
					mu.Unlock()
				}
			}
		}()
		return func() {
			close(stop)
			mu.Lock()
			for k, e := range boxes {
				e.box.Close(context.Background())
				delete(boxes, k)
			}
			mu.Unlock()
		}
	})
	return p.Provide("workspace", &sdk.ExecWorld{
		Name: "sandbox:" + provName,
		For: func(c context.Context) (*sdk.Sandbox, error) {
			k := key(sdk.ThreadFrom(c))
			mu.Lock()
			defer mu.Unlock()
			if e := boxes[k]; e != nil {
				e.used = time.Now()
				return e.box, nil
			}
			sp, err := provider()
			if err != nil {
				return nil, err
			}
			s := spec
			s.Labels = map[string]string{"eipagent.thread": k}
			box, err := sp.Create(c, s)
			if err != nil {
				return nil, err
			}
			p.Logf("created %s sandbox %s for %s", provName, box.ID, k)
			boxes[k] = &entry{box, time.Now()}
			return box, nil
		},
	})
}

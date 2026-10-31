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
// Provided as "exec".
//
// Config: provider ("docker"), image, memory_mb, cpus, network ("none"),
// idle_ttl_seconds (1800).
var Inject = []string{"sandbox/*"}

type entry struct {
	box  *sdk.Sandbox
	used time.Time
}

func Apply(ctx *sdk.Context) error {
	provName := ctx.ConfigString("provider", "docker")
	spec := sdk.SandboxSpec{
		Image:    ctx.ConfigString("image", ""),
		MemoryMB: ctx.ConfigInt("memory_mb", 0),
		CPUs:     ctx.ConfigFloat("cpus", 0),
		Network:  sdk.NetworkPolicy{Mode: ctx.ConfigString("network", "none"), Allow: ctx.ConfigStrings("allow_hosts")},
	}
	ttl := time.Duration(ctx.ConfigInt("idle_ttl_seconds", 1800)) * time.Second
	var mu sync.Mutex
	boxes := map[string]*entry{}

	provider := func() (*sdk.SandboxProvider, error) {
		p, ok := ctx.Service("sandbox/" + provName).(*sdk.SandboxProvider)
		if !ok {
			return nil, fmt.Errorf("exec_sandbox: no sandbox/%s (load sandbox_%s)", provName, provName)
		}
		return p, nil
	}
	key := func(thread string) string {
		if thread == "" {
			return "_default"
		}
		return thread
	}

	ctx.OnAgentStart(func(c context.Context, st *sdk.RunState) error {
		id, _ := st.Values["exec_sandbox.id"].(string)
		k := key(st.Thread)
		mu.Lock()
		_, have := boxes[k]
		mu.Unlock()
		if id == "" || have {
			return nil
		}
		p, err := provider()
		if err != nil || p.Attach == nil {
			return nil
		}
		if box, err := p.Attach(c, id); err == nil {
			mu.Lock()
			boxes[k] = &entry{box, time.Now()}
			mu.Unlock()
		}
		return nil
	})
	ctx.OnAgentEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		e := boxes[key(st.Thread)]
		mu.Unlock()
		if e != nil && st.Values != nil {
			st.Values["exec_sandbox.id"] = e.box.ID
		}
		return nil
	})
	ctx.Effect(func() func() {
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
	return ctx.Provide("exec", &sdk.ExecWorld{
		Name: "sandbox:" + provName,
		For: func(c context.Context) (*sdk.Sandbox, error) {
			k := key(sdk.ThreadFrom(c))
			mu.Lock()
			defer mu.Unlock()
			if e := boxes[k]; e != nil {
				e.used = time.Now()
				return e.box, nil
			}
			p, err := provider()
			if err != nil {
				return nil, err
			}
			s := spec
			s.Labels = map[string]string{"eipagent.thread": k}
			box, err := p.Create(c, s)
			if err != nil {
				return nil, err
			}
			ctx.Logf("created %s sandbox %s for %s", provName, box.ID, k)
			boxes[k] = &entry{box, time.Now()}
			return box, nil
		},
	})
}

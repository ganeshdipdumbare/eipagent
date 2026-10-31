//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"eipagent/sdk"
)

// Lets the model start and use sandboxes on demand: sandbox_create,
// sandbox_exec, sandbox_write_file, sandbox_read_file, sandbox_list,
// sandbox_destroy. Boxes belong to the thread that made them and are
// destroyed when the plugin unloads.
//
// Config: provider ("docker"), image, network ("none"), memory_mb, cpus,
// max_boxes (3 per thread), images (allowed images; [] = any).
var Inject = []string{"sandbox/*"}

func Apply(ctx *sdk.Context) error {
	provName := ctx.ConfigString("provider", "docker")
	defImage := ctx.ConfigString("image", "")
	maxBoxes := ctx.ConfigInt("max_boxes", 3)
	allowedImages := ctx.ConfigStrings("images")
	base := sdk.SandboxSpec{
		MemoryMB: ctx.ConfigInt("memory_mb", 0),
		CPUs:     ctx.ConfigFloat("cpus", 0),
		Network:  sdk.NetworkPolicy{Mode: ctx.ConfigString("network", "none")},
	}
	var mu sync.Mutex
	boxes := map[string]map[string]*sdk.Sandbox{} // thread -> id -> box
	ctx.OnDispose(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range boxes {
			for _, b := range m {
				b.Close(context.Background())
			}
		}
	})
	thread := func(c context.Context) string {
		if t := sdk.ThreadFrom(c); t != "" {
			return t
		}
		return "_default"
	}
	get := func(c context.Context, id string) (*sdk.Sandbox, error) {
		mu.Lock()
		defer mu.Unlock()
		b := boxes[thread(c)][id]
		if b == nil {
			return nil, fmt.Errorf("no sandbox %q in this conversation (use sandbox_list)", id)
		}
		return b, nil
	}
	idArg := sdk.Str("sandbox id from sandbox_create")

	ctx.AddTool(sdk.Tool{
		Name: "sandbox_create", Description: "Start a new isolated sandbox (" + provName + ") and return its id",
		Schema: sdk.Object(map[string]any{"image": sdk.Str("container image (optional)")}),
		Meta:   map[string]any{"risk": "medium"},
		Run: func(c context.Context, a map[string]any) (string, error) {
			p, ok := ctx.Service("sandbox/" + provName).(*sdk.SandboxProvider)
			if !ok {
				return "", fmt.Errorf("no sandbox/%s provider", provName)
			}
			spec := base
			spec.Image = sdk.ArgString(a, "image")
			if spec.Image == "" {
				spec.Image = defImage
			}
			if len(allowedImages) > 0 && spec.Image != "" {
				ok := false
				for _, im := range allowedImages {
					if im == spec.Image {
						ok = true
					}
				}
				if !ok {
					return "", fmt.Errorf("image %q is not allowed; allowed: %s", spec.Image, strings.Join(allowedImages, ", "))
				}
			}
			t := thread(c)
			mu.Lock()
			n := len(boxes[t])
			mu.Unlock()
			if n >= maxBoxes {
				return "", fmt.Errorf("limit of %d sandboxes reached; destroy one first", maxBoxes)
			}
			spec.Labels = map[string]string{"eipagent.thread": t}
			b, err := p.Create(c, spec)
			if err != nil {
				return "", err
			}
			mu.Lock()
			if boxes[t] == nil {
				boxes[t] = map[string]*sdk.Sandbox{}
			}
			boxes[t][b.ID] = b
			mu.Unlock()
			return fmt.Sprintf("created sandbox %s (isolation: %s)", b.ID, b.Isolation), nil
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "sandbox_exec", Description: "Run a shell command inside a sandbox",
		Schema: sdk.Object(map[string]any{"id": idArg, "command": sdk.Str("shell command"), "timeout_seconds": sdk.Int("timeout")}, "id", "command"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			b, err := get(c, sdk.ArgString(a, "id"))
			if err != nil {
				return "", err
			}
			res, err := b.Exec(c, sdk.ExecRequest{Shell: sdk.ArgString(a, "command"), TimeoutSeconds: sdk.ArgInt(a, "timeout_seconds", 120)})
			if err != nil {
				return "", err
			}
			out := res.Stdout
			if res.Stderr != "" {
				out += "\n[stderr]\n" + res.Stderr
			}
			if len(out) > 30000 {
				out = out[:30000] + "\n...[truncated]"
			}
			return fmt.Sprintf("%s\n[exit code %d]", strings.TrimSpace(out), res.ExitCode), nil
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "sandbox_write_file", Description: "Write a file inside a sandbox",
		Schema: sdk.Object(map[string]any{"id": idArg, "path": sdk.Str("path"), "content": sdk.Str("content")}, "id", "path", "content"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			b, err := get(c, sdk.ArgString(a, "id"))
			if err != nil {
				return "", err
			}
			return "written", b.WriteFile(c, sdk.ArgString(a, "path"), []byte(sdk.ArgString(a, "content")))
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "sandbox_read_file", Description: "Read a file from a sandbox",
		Schema: sdk.Object(map[string]any{"id": idArg, "path": sdk.Str("path")}, "id", "path"),
		Meta:   map[string]any{"readonly": true},
		Run: func(c context.Context, a map[string]any) (string, error) {
			b, err := get(c, sdk.ArgString(a, "id"))
			if err != nil {
				return "", err
			}
			data, err := b.ReadFile(c, sdk.ArgString(a, "path"))
			if len(data) > 200000 {
				data = data[:200000]
			}
			return string(data), err
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "sandbox_list", Description: "List this conversation's sandboxes",
		Schema: sdk.Object(map[string]any{}), Meta: map[string]any{"readonly": true},
		Run: func(c context.Context, a map[string]any) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			var ids []string
			for id := range boxes[thread(c)] {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			if len(ids) == 0 {
				return "no sandboxes", nil
			}
			return strings.Join(ids, "\n"), nil
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "sandbox_destroy", Description: "Destroy a sandbox and everything in it",
		Schema: sdk.Object(map[string]any{"id": idArg}, "id"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			id := sdk.ArgString(a, "id")
			b, err := get(c, id)
			if err != nil {
				return "", err
			}
			mu.Lock()
			delete(boxes[thread(c)], id)
			mu.Unlock()
			return "destroyed " + id, b.Close(c)
		},
	})
	return nil
}

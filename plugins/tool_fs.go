//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"strings"

	"eipagent/sdk"
)

// File tools in the "exec" world: read_file, write_file, edit_file,
// list_dir, find_files, grep. Writes are recorded per run (Result.Changes)
// so hosts can show a review diff.
//
// Config: max_read (200000 bytes), readonly (true = only read tools).
var Inject = []string{"exec"}

func Apply(ctx *sdk.Context) error {
	world := ctx.Service("exec").(*sdk.ExecWorld)
	maxRead := ctx.ConfigInt("max_read", 200000)
	readonly := ctx.ConfigBool("readonly", false)
	ro := map[string]any{"readonly": true, "sandbox": world.Name}
	rw := map[string]any{"risk": "medium", "sandbox": world.Name}

	box := func(c context.Context) (*sdk.Sandbox, error) { return world.For(c) }
	sh := func(c context.Context, script string, params ...string) (string, error) {
		b, err := box(c)
		if err != nil {
			return "", err
		}
		res, err := b.Exec(c, sdk.ExecRequest{Cmd: "sh", Args: append([]string{"-c", script, "_"}, params...), TimeoutSeconds: 60})
		if err != nil {
			return "", err
		}
		if res.ExitCode > 1 {
			return "", fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
		}
		return res.Stdout, nil
	}

	ctx.AddTool(sdk.Tool{
		Name: "read_file", Description: "Read a text file. Optional offset/limit are line numbers.", Meta: ro,
		Schema: sdk.Object(map[string]any{"path": sdk.Str("file path"), "offset": sdk.Int("first line (1-based)"), "limit": sdk.Int("max lines")}, "path"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			b, err := box(c)
			if err != nil {
				return "", err
			}
			data, err := b.ReadFile(c, sdk.ArgString(a, "path"))
			if err != nil {
				return "", err
			}
			if len(data) > maxRead {
				data = data[:maxRead]
			}
			lines := strings.Split(string(data), "\n")
			off := sdk.ArgInt(a, "offset", 1)
			if off < 1 {
				off = 1
			}
			lim := sdk.ArgInt(a, "limit", len(lines))
			var out strings.Builder
			for i := off - 1; i < len(lines) && i < off-1+lim; i++ {
				fmt.Fprintf(&out, "%6d\t%s\n", i+1, lines[i])
			}
			return out.String(), nil
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "list_dir", Description: "List a directory", Meta: ro,
		Schema: sdk.Object(map[string]any{"path": sdk.Str("directory (default .)")}),
		Run: func(c context.Context, a map[string]any) (string, error) {
			b, err := box(c)
			if err != nil {
				return "", err
			}
			p := sdk.ArgString(a, "path")
			if p == "" {
				p = "."
			}
			fis, err := b.ListDir(c, p)
			if err != nil {
				return "", err
			}
			var out []string
			for _, f := range fis {
				if f.IsDir {
					out = append(out, f.Name+"/")
				} else {
					out = append(out, f.Name)
				}
			}
			return strings.Join(out, "\n"), nil
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "find_files", Description: "Find files by name pattern (shell glob, e.g. *.go)", Meta: ro,
		Schema: sdk.Object(map[string]any{"pattern": sdk.Str("name glob"), "path": sdk.Str("where to look (default .)")}, "pattern"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			p := sdk.ArgString(a, "path")
			if p == "" {
				p = "."
			}
			return sh(c, `find "$2" -type f -name "$1" -not -path "*/.git/*" | head -n 200`, sdk.ArgString(a, "pattern"), p)
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "grep", Description: "Search file contents with a regular expression", Meta: ro,
		Schema: sdk.Object(map[string]any{"pattern": sdk.Str("regex"), "path": sdk.Str("file or directory (default .)")}, "pattern"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			p := sdk.ArgString(a, "path")
			if p == "" {
				p = "."
			}
			out, err := sh(c, `grep -rnIE --exclude-dir=.git -e "$1" -- "$2" | head -n 200`, sdk.ArgString(a, "pattern"), p)
			if err == nil && out == "" {
				out = "no matches"
			}
			return out, err
		},
	})
	if readonly {
		return nil
	}
	ctx.AddTool(sdk.Tool{
		Name: "write_file", Description: "Create or overwrite a file", Meta: rw,
		Schema: sdk.Object(map[string]any{"path": sdk.Str("file path"), "content": sdk.Str("full new content")}, "path", "content"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			b, err := box(c)
			if err != nil {
				return "", err
			}
			content := sdk.ArgString(a, "content")
			if err := b.WriteFile(c, sdk.ArgString(a, "path"), []byte(content)); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(content), sdk.ArgString(a, "path")), nil
		},
	})
	ctx.AddTool(sdk.Tool{
		Name: "edit_file", Description: "Replace exact text in a file. old_text must match exactly once unless replace_all.", Meta: rw,
		Schema: sdk.Object(map[string]any{"path": sdk.Str("file path"), "old_text": sdk.Str("text to find"), "new_text": sdk.Str("replacement"), "replace_all": sdk.Bool("replace every match")}, "path", "old_text", "new_text"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			b, err := box(c)
			if err != nil {
				return "", err
			}
			path := sdk.ArgString(a, "path")
			data, err := b.ReadFile(c, path)
			if err != nil {
				return "", err
			}
			old, repl := sdk.ArgString(a, "old_text"), sdk.ArgString(a, "new_text")
			n := strings.Count(string(data), old)
			all, _ := a["replace_all"].(bool)
			switch {
			case old == "" || n == 0:
				return "", fmt.Errorf("old_text not found in %s", path)
			case n > 1 && !all:
				return "", fmt.Errorf("old_text matches %d times in %s; add context or set replace_all", n, path)
			}
			out := strings.Replace(string(data), old, repl, -1)
			if err := b.WriteFile(c, path, []byte(out)); err != nil {
				return "", err
			}
			return fmt.Sprintf("replaced %d occurrence(s) in %s", n, path), nil
		},
	})

	// Record before/after of writes for review.
	ctx.OnAgentStart(func(c context.Context, st *sdk.RunState) error {
		if st.Values != nil {
			delete(st.Values, "fs.changes")
		}
		return nil
	})
	ctx.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		if (req.Call.Name != "write_file" && req.Call.Name != "edit_file") || req.Run == nil {
			return next(c, req)
		}
		path := sdk.ArgString(req.Call.Args, "path")
		before := ""
		if b, err := box(c); err == nil {
			if data, err := b.ReadFile(c, path); err == nil {
				before = string(data)
			}
		}
		r, err := next(c, req)
		if err != nil || r == nil || r.IsError || r.Interrupt != nil {
			return r, err
		}
		after := before
		if b, err := box(c); err == nil {
			if data, err := b.ReadFile(c, path); err == nil {
				after = string(data)
			}
		}
		if req.Run.Values == nil {
			req.Run.Values = map[string]any{}
		}
		changes, _ := req.Run.Values["fs.changes"].([]map[string]any)
		req.Run.Values["fs.changes"] = append(changes, map[string]any{"path": path, "before": before, "after": after})
		return r, nil
	})
	return nil
}

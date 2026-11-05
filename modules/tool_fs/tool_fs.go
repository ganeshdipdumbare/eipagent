package tool_fs

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

var errStopWalk = errors.New("stop walk")

// File tools in the "workspace" world: read_file, write_file, edit_file,
// list_dir, find_files, grep. Writes are recorded per run (Result.Changes)
// so hosts can show a review diff.
//
// Config: max_read (200000 bytes), readonly (true = only read tools).
//
// Example:
//
//	kiln.WithModuleConfig("tool_fs", map[string]any{"readonly": true}) // only read_file, list_dir, find_files, grep
var Requires = []string{"workspace"}

func Apply(p *sdk.Module) error {
	world := p.Service("workspace").(*sdk.ExecWorld)
	maxRead := p.ConfigInt("max_read", 200000)
	readonly := p.ConfigBool("readonly", false)
	ro := map[string]any{"readonly": true, "sandbox": world.Name}
	rw := map[string]any{"risk": "medium", "sandbox": world.Name}

	box := func(c context.Context) (*sdk.Sandbox, error) { return world.For(c) }
	// walkFiles walks the sandbox through its own file API instead of
	// running find/grep in a shell. That keeps the workspace confinement
	// working for sandbox_local and avoids passing model-controlled paths
	// to a shell.
	walkFiles := func(c context.Context, b *sdk.Sandbox, root string, fn func(string) error) error {
		const maxDepth, maxEntries = 32, 20000
		entries := 0
		var walk func(string, int) error
		walk = func(dir string, depth int) error {
			if depth > maxDepth || entries > maxEntries {
				return nil
			}
			fis, err := b.ListDir(c, dir)
			if err != nil {
				if depth > 0 {
					return nil // skip an unreadable subdirectory, like find/grep
				}
				return err
			}
			for _, fi := range fis {
				entries++
				if entries > maxEntries {
					return nil
				}
				p := path.Join(dir, fi.Name)
				if fi.IsDir {
					if fi.Name == ".git" {
						continue
					}
					if err := walk(p, depth+1); err != nil {
						return err
					}
					continue
				}
				if err := fn(p); err != nil {
					return err
				}
			}
			return nil
		}
		return walk(root, 0)
	}

	p.AddTool(sdk.Tool{
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
	p.AddTool(sdk.Tool{
		Name: "list_dir", Description: "List a directory", Meta: ro,
		Schema: sdk.Object(map[string]any{"path": sdk.Str("directory (default .)")}),
		Run: func(c context.Context, a map[string]any) (string, error) {
			b, err := box(c)
			if err != nil {
				return "", err
			}
			fpath := sdk.ArgString(a, "path")
			if fpath == "" {
				fpath = "."
			}
			fis, err := b.ListDir(c, fpath)
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
	p.AddTool(sdk.Tool{
		Name: "find_files", Description: "Find files by name pattern (shell glob, e.g. *.go)", Meta: ro,
		Schema: sdk.Object(map[string]any{"pattern": sdk.Str("name glob"), "path": sdk.Str("where to look (default .)")}, "pattern"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			b, err := box(c)
			if err != nil {
				return "", err
			}
			fpath := sdk.ArgString(a, "path")
			if fpath == "" {
				fpath = "."
			}
			pattern := sdk.ArgString(a, "pattern")
			if _, err := path.Match(pattern, ""); err != nil {
				return "", fmt.Errorf("invalid pattern: %v", err)
			}
			var out []string
			add := func(p string) error {
				if ok, _ := path.Match(pattern, path.Base(p)); ok {
					out = append(out, p)
				}
				if len(out) >= 200 {
					return errStopWalk
				}
				return nil
			}
			if _, err := b.ReadFile(c, fpath); err == nil {
				if err := add(fpath); err != nil && err != errStopWalk {
					return "", err
				}
				return strings.Join(out, "\n"), nil
			}
			err = walkFiles(c, b, fpath, add)
			if err != nil && err != errStopWalk {
				return "", err
			}
			return strings.Join(out, "\n"), nil
		},
	})
	p.AddTool(sdk.Tool{
		Name: "grep", Description: "Search file contents with a regular expression", Meta: ro,
		Schema: sdk.Object(map[string]any{"pattern": sdk.Str("regex"), "path": sdk.Str("file or directory (default .)")}, "pattern"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			b, err := box(c)
			if err != nil {
				return "", err
			}
			fpath := sdk.ArgString(a, "path")
			if fpath == "" {
				fpath = "."
			}
			re, err := regexp.Compile(sdk.ArgString(a, "pattern"))
			if err != nil {
				return "", fmt.Errorf("invalid pattern: %v", err)
			}
			var out []string
			matchData := func(p string, data []byte) error {
				if len(data) > maxRead {
					data = data[:maxRead]
				}
				if strings.IndexByte(string(data), 0) >= 0 {
					return nil
				}
				for i, line := range strings.Split(string(data), "\n") {
					if re.MatchString(line) {
						out = append(out, fmt.Sprintf("%s:%d:%s", p, i+1, line))
						if len(out) >= 200 {
							return errStopWalk
						}
					}
				}
				return nil
			}
			if data, err := b.ReadFile(c, fpath); err == nil {
				if err := matchData(fpath, data); err != nil && err != errStopWalk {
					return "", err
				}
			} else {
				err = walkFiles(c, b, fpath, func(p string) error {
					data, err := b.ReadFile(c, p)
					if err != nil {
						return nil // an unreadable file is skipped, like grep -I
					}
					return matchData(p, data)
				})
				if err != nil && err != errStopWalk {
					return "", err
				}
			}
			if len(out) == 0 {
				return "no matches", nil
			}
			return strings.Join(out, "\n"), nil
		},
	})
	if readonly {
		return nil
	}
	p.AddTool(sdk.Tool{
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
	p.AddTool(sdk.Tool{
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
	p.OnRunStart(func(c context.Context, st *sdk.RunState) error {
		if st.Values != nil {
			delete(st.Values, "fs.changes")
		}
		return nil
	})
	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
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

// Module is the compiled form of "tool_fs". modules/std registers it.
var Module = engine.Module{Name: "tool_fs", Requires: Requires, Provides: nil, Permissions: nil, Apply: Apply}

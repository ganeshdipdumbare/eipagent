//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"eipagent/sdk"
)

// Agent skills: folders containing SKILL.md (YAML-ish front matter with
// name and description, then instructions). Only names and descriptions
// go into the system prompt; the model loads a skill's body (and files
// next to it) on demand, which keeps prompts small.
//
// Config: dirs (default ["skills"]).
var Permissions = []string{"fs:read"}

type skill struct {
	name, desc, dir, body string
}

func Apply(ctx *sdk.Context) error {
	dirs := ctx.ConfigStrings("dirs")
	if len(dirs) == 0 {
		dirs = []string{"skills"}
	}
	skills := map[string]*skill{}
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			return fmt.Errorf("skills_dir: %w", err)
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(d, e.Name())
			raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
			if err != nil {
				continue
			}
			s := parse(string(raw), dir)
			if s.name == "" {
				s.name = e.Name()
			}
			skills[s.name] = s
		}
	}
	if len(skills) == 0 {
		return nil
	}
	var names []string
	for n := range skills {
		names = append(names, n)
	}
	sort.Strings(names)
	var idx strings.Builder
	idx.WriteString("You have skills: packaged instructions for specific tasks. When a task matches one, call load_skill first and follow it.\n")
	for _, n := range names {
		fmt.Fprintf(&idx, "- %s: %s\n", n, skills[n].desc)
	}
	ctx.AddSkill(sdk.Skill{Name: "skills_index", Description: "index of available skills", Prompt: strings.TrimSpace(idx.String())})

	ctx.AddTool(sdk.Tool{
		Name:        "load_skill",
		Description: "Load the full instructions of a skill by name",
		Schema:      sdk.Object(map[string]any{"name": sdk.Enum("skill name", names...)}, "name"),
		Meta:        map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			s := skills[sdk.ArgString(args, "name")]
			if s == nil {
				return "", fmt.Errorf("no skill %q", sdk.ArgString(args, "name"))
			}
			return s.body, nil
		},
	})
	ctx.AddTool(sdk.Tool{
		Name:        "read_skill_file",
		Description: "Read a file that belongs to a skill (path relative to the skill folder)",
		Schema:      sdk.Object(map[string]any{"name": sdk.Enum("skill name", names...), "path": sdk.Str("relative path")}, "name", "path"),
		Meta:        map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			s := skills[sdk.ArgString(args, "name")]
			if s == nil {
				return "", fmt.Errorf("no skill %q", sdk.ArgString(args, "name"))
			}
			p := filepath.Clean(filepath.Join(s.dir, sdk.ArgString(args, "path")))
			root, _ := filepath.Abs(s.dir)
			abs, _ := filepath.Abs(p)
			if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
				return "", fmt.Errorf("path escapes the skill folder")
			}
			b, err := os.ReadFile(abs)
			if err != nil {
				return "", err
			}
			if len(b) > 200*1024 {
				b = b[:200*1024]
			}
			return string(b), nil
		},
	})
	return nil
}

func parse(raw, dir string) *skill {
	s := &skill{dir: dir, body: raw}
	if !strings.HasPrefix(raw, "---") {
		return s
	}
	rest := raw[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return s
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "name":
			s.name = v
		case "description":
			s.desc = v
		}
	}
	s.body = strings.TrimSpace(rest[end+4:])
	return s
}

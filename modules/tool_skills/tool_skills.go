package tool_skills

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Agent skills: folders containing SKILL.md (YAML-ish front matter with
// name and description, then instructions). Only names and descriptions
// go into the system prompt; the model loads a skill's body (and files
// next to it) on demand, which keeps prompts small.
//
// Config: dirs (default ["skills"]). Each directory is granted read-only
// through fs:read and must already exist; a skill folder may not reach
// outside itself, including through symlinks.
//
// Example:
//
//	kiln.WithModuleConfig("tool_skills", map[string]any{"dirs": []any{"./skills"}}) // skills/<name>/SKILL.md
var Permissions = []string{"fs:read"}

type skill struct {
	name, desc, dir, body string
	root                  *sdk.Dir // the skill folder, read-only
}

func Apply(p *sdk.Module) error {
	dirs := p.ConfigStrings("dirs")
	if len(dirs) == 0 {
		dirs = []string{"skills"}
	}
	var roots []*sdk.Dir // every skill folder opened; closed on unload
	p.OnUnload(func() {
		for _, r := range roots {
			r.Close()
		}
	})
	skills := map[string]*skill{}
	for _, d := range dirs {
		top, err := p.Dir(d, false)
		if err != nil {
			return fmt.Errorf("tool_skills: %w", err)
		}
		ents, err := top.ReadDir(".")
		top.Close()
		if err != nil {
			return fmt.Errorf("tool_skills: %w", err)
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(d, e.Name())
			root, err := p.Dir(dir, false)
			if err != nil {
				p.Logf("tool_skills: skipping %s: %v", dir, err)
				continue
			}
			roots = append(roots, root)
			raw, err := root.ReadFile("SKILL.md")
			if err != nil {
				p.Logf("tool_skills: skipping %s: %v", dir, err)
				continue
			}
			s := parse(string(raw), dir)
			s.root = root
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
	p.AddSkill(sdk.Skill{Name: "skills_index", Description: "index of available skills", Prompt: strings.TrimSpace(idx.String())})

	p.AddTool(sdk.Tool{
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
	p.AddTool(sdk.Tool{
		Name:        "read_skill_file",
		Description: "Read a file that belongs to a skill (path relative to the skill folder)",
		Schema:      sdk.Object(map[string]any{"name": sdk.Enum("skill name", names...), "path": sdk.Str("relative path")}, "name", "path"),
		Meta:        map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			s := skills[sdk.ArgString(args, "name")]
			if s == nil {
				return "", fmt.Errorf("no skill %q", sdk.ArgString(args, "name"))
			}
			rel := sdk.ArgString(args, "path")
			if filepath.IsAbs(rel) {
				// An absolute path is accepted when it is inside the skill folder.
				base, err := filepath.Abs(s.dir)
				if err != nil {
					return "", err
				}
				if rel, err = filepath.Rel(base, rel); err != nil {
					return "", fmt.Errorf("path escapes the skill folder")
				}
			}
			b, err := s.root.ReadFile(rel)
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

// Module is the compiled form of "tool_skills". modules/std registers it.
var Module = engine.Module{Name: "tool_skills", Requires: nil, Provides: nil, Permissions: Permissions, Apply: Apply}

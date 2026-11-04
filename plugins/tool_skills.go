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
//
// Example:
//
//	eipagent.WithPlugin("tool_skills", map[string]any{"dirs": []any{"./skills"}}) // skills/<name>/SKILL.md
var Permissions = []string{"fs:read"}

type skill struct {
	name, desc, dir, body string
}

func Apply(p *sdk.Plugin) error {
	dirs := p.ConfigStrings("dirs")
	if len(dirs) == 0 {
		dirs = []string{"skills"}
	}
	skills := map[string]*skill{}
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			return fmt.Errorf("tool_skills: %w", err)
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(d, e.Name())
			md, err := resolveSkillPath(dir, "SKILL.md")
			if err != nil {
				p.Logf("tool_skills: skipping %s: %v", dir, err)
				continue
			}
			raw, err := os.ReadFile(md)
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
			abs, err := resolveSkillPath(s.dir, sdk.ArgString(args, "path"))
			if err != nil {
				return "", err
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

// resolveSkillPath returns the real path of a file under a skill folder,
// refusing symlinks that point outside it. The lexical check alone is not
// enough: a skill directory (often a cloned repo) can contain a symlink to
// a host secret, and os.ReadFile follows it.
func resolveSkillPath(dir, rel string) (string, error) {
	rootAbs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("skill folder %s: %v", dir, err)
	}
	fpath := rel
	if !filepath.IsAbs(fpath) {
		fpath = filepath.Join(rootAbs, fpath)
	}
	fpath = filepath.Clean(fpath)
	if fpath != rootAbs && !strings.HasPrefix(fpath, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the skill folder")
	}
	real, err := filepath.EvalSymlinks(fpath)
	if err != nil {
		return "", err
	}
	if real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the skill folder via a symlink")
	}
	return real, nil
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

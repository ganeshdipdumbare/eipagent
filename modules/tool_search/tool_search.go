package tool_search

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Loads tools on demand. When a step would offer more than threshold
// tools, the model sees only the always-visible ones plus a search_tools
// tool; each search reveals the matching tools for the rest of the run.
// Useful once MCP servers bring dozens of tools.
//
// Search matches the query's words (three letters or more) against the
// start of words in tool names and descriptions; name matches count more.
// A hidden tool still runs if the model calls it by its exact name.
//
// Config:
//
//	threshold:   hide tools only when there are more than this many (default 15)
//	always:      tools that stay visible, globs like "github__*" (default none)
//	max_results: tools a search reveals at most (default 5)
//
// Example:
//
//	kiln.WithModuleConfig("tool_search", map[string]any{
//		"threshold": 10,
//		"always":    []any{"read_file", "bash"},
//	})
func Apply(p *sdk.Module) error {
	threshold := p.ConfigInt("threshold", 15)
	always := p.ConfigStrings("always")
	maxResults := p.ConfigInt("max_results", 5)

	var mu sync.Mutex
	loaded := map[string]map[string]bool{} // run ID -> revealed tools
	all := map[string][]sdk.Tool{}         // run ID -> the full tool list

	isAlways := func(name string) bool {
		if name == "search_tools" {
			return true
		}
		for _, g := range always {
			if ok, _ := path.Match(g, name); ok {
				return true
			}
		}
		return false
	}
	words := func(s string) []string {
		return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		})
	}

	p.BeforeStep(func(c context.Context, sc *sdk.StepContext) error {
		tools := sc.Request.Tools
		if len(tools) <= threshold {
			return nil
		}
		mu.Lock()
		all[sc.Run.ID] = tools
		seen := loaded[sc.Run.ID]
		mu.Unlock()
		var visible []sdk.Tool
		for _, t := range tools {
			if isAlways(t.Name) || seen[t.Name] {
				visible = append(visible, t)
			}
		}
		sc.Request.Tools = visible
		return nil
	})

	p.AddTool(sdk.Tool{
		Name: "search_tools",
		Description: "Find tools by what they do. More tools exist than are shown; search with a few words " +
			"(e.g. \"create github issue\") and the matching tools become available on your next step.",
		Schema: sdk.Object(map[string]any{"query": sdk.Str("what you want to do")}, "query"),
		Meta:   map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			return "All tools are already available.", nil
		},
	})
	// search_tools needs the run's tool list, which only hooks can see.
	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		if req.Call.Name != "search_tools" || req.Run == nil {
			return next(c, req)
		}
		mu.Lock()
		tools := all[req.Run.ID]
		mu.Unlock()
		if len(tools) == 0 {
			return next(c, req)
		}
		var q []string
		for _, w := range words(sdk.ArgString(req.Call.Args, "query")) {
			if len(w) >= 3 {
				q = append(q, w)
			}
		}
		hasPrefix := func(ws []string, w string) bool {
			for _, x := range ws {
				if strings.HasPrefix(x, w) {
					return true
				}
			}
			return false
		}
		type hit struct {
			t     sdk.Tool
			score int
		}
		var hits []hit
		for _, t := range tools {
			if isAlways(t.Name) {
				continue
			}
			name, desc := words(t.Name), words(t.Description)
			score := 0
			for _, w := range q {
				if hasPrefix(name, w) {
					score += 3
				}
				if hasPrefix(desc, w) {
					score++
				}
			}
			if score > 0 {
				hits = append(hits, hit{t, score})
			}
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
		if len(hits) > maxResults {
			hits = hits[:maxResults]
		}
		if len(hits) == 0 {
			return &sdk.ToolResult{Content: "No tools match. Try other words."}, nil
		}
		var b strings.Builder
		b.WriteString("These tools are now available:\n")
		mu.Lock()
		if loaded[req.Run.ID] == nil {
			loaded[req.Run.ID] = map[string]bool{}
		}
		for _, h := range hits {
			loaded[req.Run.ID][h.t.Name] = true
			fmt.Fprintf(&b, "- %s: %s\n", h.t.Name, h.t.Description)
		}
		mu.Unlock()
		return &sdk.ToolResult{Content: strings.TrimRight(b.String(), "\n")}, nil
	})

	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		delete(loaded, st.ID)
		delete(all, st.ID)
		mu.Unlock()
		return nil
	})
	return nil
}

// Module is the compiled form of "tool_search". modules/std registers it.
var Module = engine.Module{Name: "tool_search", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

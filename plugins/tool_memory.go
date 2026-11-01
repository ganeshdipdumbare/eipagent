//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"eipagent/sdk"
)

// Long-term memory tools over the "memory" service. Notes are kept per
// user (namespace ["memories", user]). Config: namespace.
var Requires = []string{"memory"}

func Apply(p *sdk.Plugin) error {
	store := p.Service("memory").(*sdk.Store)
	base := p.ConfigString("namespace", "memories")
	ns := func(c context.Context) []string {
		u := sdk.UserFrom(c)
		if u == "" {
			u = "default"
		}
		return []string{base, u}
	}
	p.AddTool(sdk.Tool{
		Name:        "remember",
		Description: "Store a note in long-term memory",
		Schema:      sdk.Object(map[string]any{"text": sdk.Str("the note")}, "text"),
		Run: func(c context.Context, a map[string]any) (string, error) {
			text, _ := a["text"].(string)
			if text == "" {
				return "", fmt.Errorf("text is required")
			}
			k := fmt.Sprintf("%d", time.Now().UnixNano())
			return "Saved.", store.Put(c, ns(c), k, map[string]any{"text": text})
		},
	})
	p.AddTool(sdk.Tool{
		Name:        "recall",
		Description: "Search long-term memory; empty query lists recent notes",
		Schema:      sdk.Object(map[string]any{"query": sdk.Str("words to search for")}),
		Meta:        map[string]any{"readonly": true},
		Run: func(c context.Context, a map[string]any) (string, error) {
			q, _ := a["query"].(string)
			items, err := store.Search(c, ns(c), q, 10)
			if err != nil {
				return "", err
			}
			if len(items) == 0 {
				return "Nothing saved yet.", nil
			}
			var notes []string
			for _, it := range items {
				t, _ := it.Value["text"].(string)
				notes = append(notes, t)
			}
			return strings.Join(notes, "; "), nil
		},
	})
	return nil
}

package eipagent

import (
	"fmt"
	"strings"
)

// Change is one file write made during a run (for review UIs).
type Change struct {
	Path, Before, After string
}

func changesOf(values map[string]any) []Change {
	var raw []map[string]any
	switch v := values["fs.changes"].(type) {
	case []map[string]any:
		raw = v
	case []any: // after a JSON round trip
		for _, x := range v {
			if m, ok := x.(map[string]any); ok {
				raw = append(raw, m)
			}
		}
	}
	var out []Change
	for _, m := range raw {
		c := Change{}
		c.Path, _ = m["path"].(string)
		c.Before, _ = m["before"].(string)
		c.After, _ = m["after"].(string)
		out = append(out, c)
	}
	return out
}

// Diff renders the change as a unified-style line diff.
func (c Change) Diff() string {
	a, b := splitLines(c.Before), splitLines(c.After)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", c.Path, c.Path)
	if len(a)*len(b) > 4_000_000 {
		fmt.Fprintf(&sb, "@@ file too large to diff: %d -> %d lines @@\n", len(a), len(b))
		return sb.String()
	}
	// LCS table, then walk it.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			sb.WriteString(" " + a[i] + "\n")
			i++
			j++
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			sb.WriteString("-" + a[i] + "\n")
			i++
		default:
			sb.WriteString("+" + b[j] + "\n")
			j++
		}
	}
	return sb.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

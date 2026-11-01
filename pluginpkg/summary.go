package pluginpkg

import (
	"fmt"
	"strings"
)

// Summary says in one line what a plugin's code does, so nobody has to
// classify a plugin first:
//
//	weather: adds 1 tool (weather) · wraps tool calls · requires ?secrets · permissions net
func Summary(name string, fa *Facts) string {
	if fa == nil {
		return name + ": (source didn't parse)"
	}
	var parts []string
	if n := len(fa.Tools); n > 0 {
		parts = append(parts, fmt.Sprintf("adds %d tool%s (%s)", n, plural(n), strings.Join(fa.Tools, ", ")))
	}
	if fa.RuntimeTools {
		parts = append(parts, "adds tools named at runtime")
	}
	hooks := map[string]string{
		"WrapModel": "wraps model calls", "WrapTool": "wraps tool calls", "BeforeStep": "rewrites each step",
		"AfterModel": "checks model replies", "OnRunStart": "runs at run start", "OnRunEnd": "runs at run end", "OnEvent": "watches events",
	}
	for _, h := range []string{"WrapModel", "WrapTool", "BeforeStep", "AfterModel", "OnRunStart", "OnRunEnd", "OnEvent"} {
		if contains(fa.Hooks, h) {
			parts = append(parts, hooks[h])
		}
	}
	if len(fa.Provides) > 0 {
		parts = append(parts, "provides "+strings.Join(canonical(fa.Provides), ", "))
	}
	if fa.Background {
		parts = append(parts, "runs background work")
	}
	var uses []string
	for _, s := range canonical(fa.Services) {
		if !contains(canonical(fa.Provides), s) {
			uses = append(uses, s)
		}
	}
	if len(uses) > 0 {
		parts = append(parts, "uses "+strings.Join(uses, ", "))
	}
	if len(parts) == 0 {
		parts = append(parts, "registers nothing found by static analysis")
	}
	if len(fa.Inject) > 0 {
		parts = append(parts, "requires "+strings.Join(canonical(fa.Inject), ", "))
	}
	if len(fa.Permissions) > 0 {
		parts = append(parts, "permissions "+strings.Join(fa.Permissions, ", "))
	} else {
		parts = append(parts, "no permissions")
	}
	return name + ": " + strings.Join(parts, " · ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

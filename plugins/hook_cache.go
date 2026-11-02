//go:build ignore

package plugin

import (
	"context"
	"fmt"

	"eipagent/sdk"
)

// Prompt caching: reuses the unchanged prefix of each request (tools,
// system prompt, earlier messages) so long agent runs cost less and
// answer faster. It sets ModelRequest.Extra["eip.cache"]; model plugins
// map it:
//
//	anthropic: cache_control breakpoints on the system prompt and on the
//	           conversation so far
//	openai:    prompt_cache_key = the thread (caching itself is automatic)
//	gemini:    nothing to do (implicit caching)
//
// Config: ttl (5m | 1h, default 5m).
func Apply(p *sdk.Plugin) error {
	ttl := p.ConfigString("ttl", "5m")
	if ttl != "5m" && ttl != "1h" {
		return fmt.Errorf("hook_cache: ttl must be 5m or 1h, got %q", ttl)
	}
	p.BeforeStep(func(c context.Context, sc *sdk.StepContext) error {
		if sc.Request.Extra == nil {
			sc.Request.Extra = map[string]any{}
		}
		sc.Request.Extra["eip.cache"] = map[string]any{"ttl": ttl, "key": sc.Request.Thread}
		return nil
	})
	return nil
}

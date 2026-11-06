package hook_cache

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"fmt"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Prompt caching: reuses the unchanged prefix of each request (tools,
// system prompt, earlier messages) so long agent runs cost less and
// answer faster. It sets ModelRequest.Extra["kiln.cache"]; model modules
// map it:
//
//	anthropic: cache_control breakpoints on the system prompt and on the
//	           conversation so far
//	openai:    prompt_cache_key = the thread (caching itself is automatic)
//	gemini:    nothing to do (implicit caching)
//
// Config: ttl (5m | 1h, default 5m).
//
// Example:
//
//	hook_cache.New(map[string]any{"ttl": "1h"})
func Apply(p *sdk.Scope) error {
	ttl := p.ConfigString("ttl", "5m")
	if ttl != "5m" && ttl != "1h" {
		return fmt.Errorf("hook_cache: ttl must be 5m or 1h, got %q", ttl)
	}
	p.BeforeStep(func(c context.Context, sc *sdk.StepContext) error {
		if sc.Request.Extra == nil {
			sc.Request.Extra = map[string]any{}
		}
		sc.Request.Extra["kiln.cache"] = map[string]any{"ttl": ttl, "key": sc.Request.Thread}
		return nil
	})
	return nil
}

// Module is the compiled form of "hook_cache". modules/std registers it.
var Module = engine.Module{Name: "hook_cache", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

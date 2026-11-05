package hook_fallback

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"errors"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Falls back to other models when a model call fails, or when the model
// refuses (finish reason "refusal": a safety stop or content filter).
//
// Config: models (["anthropic:claude-...", "openai:gpt-..."]), tried in
// order after the requested model; on_refusal (true): also fall back on a
// refusal. Default priority 20 (outside hook_retry).
//
// Example:
//
//	kiln.WithModuleConfig("hook_fallback", map[string]any{
//		"models":     []any{"openai:gpt-4.1", "gemini:gemini-2.5-pro"},
//		"on_refusal": true,
//	})
func Apply(p *sdk.Module) error {
	if p.Priority() == 0 {
		p.SetPriority(20)
	}
	models := p.ConfigStrings("models")
	onRefusal := p.ConfigBool("on_refusal", true)
	refused := func(r *sdk.ModelResponse) bool { return onRefusal && r != nil && r.FinishReason == "refusal" }
	p.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		resp, err := next(c, req)
		if errors.Is(err, context.Canceled) || (err == nil && !refused(resp)) {
			return resp, err
		}
		first, firstResp := err, resp
		for _, m := range models {
			if m == req.Model {
				continue
			}
			if pc, ok := p.Service("policy").(*sdk.PolicyCheck); ok && pc.Model != nil {
				if perr := pc.Model(c, m); perr != nil {
					p.Logf("%s", perr.Error())
					continue
				}
			}
			if err != nil {
				p.Logf("model %s failed (%v); falling back to %s", req.Model, err, m)
			} else {
				p.Logf("model %s refused; falling back to %s", req.Model, m)
			}
			alt := *req
			alt.Model = m
			if resp, err = next(c, &alt); err == nil && !refused(resp) {
				return resp, nil
			}
		}
		return firstResp, first
	})
	return nil
}

// Module is the compiled form of "hook_fallback". modules/std registers it.
var Module = engine.Module{Name: "hook_fallback", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

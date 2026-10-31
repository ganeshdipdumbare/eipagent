//go:build ignore

package plugin

import (
	"context"
	"errors"

	"eipagent/sdk"
)

// Falls back to other models when a model call fails.
//
// Config: models (["anthropic:claude-...", "openai:gpt-..."]), tried in
// order after the requested model. Default priority 20 (outside mw_retry).
func Apply(ctx *sdk.Context) error {
	if ctx.Priority() == 0 {
		ctx.SetPriority(20)
	}
	models := ctx.ConfigStrings("models")
	ctx.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		resp, err := next(c, req)
		if err == nil || errors.Is(err, context.Canceled) {
			return resp, err
		}
		first := err
		for _, m := range models {
			if m == req.Model {
				continue
			}
			ctx.Logf("model %s failed (%v); falling back to %s", req.Model, err, m)
			alt := *req
			alt.Model = m
			if resp, err = next(c, &alt); err == nil {
				return resp, nil
			}
		}
		return nil, first
	})
	return nil
}

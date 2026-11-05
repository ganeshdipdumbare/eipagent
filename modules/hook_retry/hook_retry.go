package hook_retry

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Retries transient model failures (HTTP 429/5xx, network errors) with
// exponential backoff, and optionally failing tools.
//
// Config: max_retries (3), initial_delay_ms (500), max_delay_ms (8000),
// backoff (2.0), jitter (true), tool_retries (0), retry_tools ([] = all).
// Default priority 10: inside hook_fallback, outside everything else.
//
// Example:
//
//	kiln.WithModuleConfig("hook_retry", map[string]any{"max_retries": 5, "initial_delay_ms": 1000, "tool_retries": 1})
func Apply(p *sdk.Module) error {
	if p.Priority() == 0 {
		p.SetPriority(10)
	}
	max := p.ConfigInt("max_retries", 3)
	initial := time.Duration(p.ConfigInt("initial_delay_ms", 500)) * time.Millisecond
	maxDelay := time.Duration(p.ConfigInt("max_delay_ms", 8000)) * time.Millisecond
	factor := p.ConfigFloat("backoff", 2)
	jitter := p.ConfigBool("jitter", true)
	toolRetries := p.ConfigInt("tool_retries", 0)
	only := p.ConfigStrings("retry_tools")

	delay := func(attempt int) time.Duration {
		d := float64(initial)
		for i := 0; i < attempt; i++ {
			d *= factor
		}
		if time.Duration(d) > maxDelay {
			d = float64(maxDelay)
		}
		if jitter {
			d = d * (0.5 + rand.Float64()/2)
		}
		return time.Duration(d)
	}
	sleep := func(c context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-c.Done():
			return c.Err()
		case <-t.C:
			return nil
		}
	}
	transient := func(err error) bool {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		var pe *sdk.ProviderError
		if errors.As(err, &pe) {
			return pe.Retryable
		}
		return true // network errors
	}

	p.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		for attempt := 0; ; attempt++ {
			resp, err := next(c, req)
			if err == nil || attempt >= max || !transient(err) {
				return resp, err
			}
			p.Logf("model %s failed (%v); retry %d/%d", req.Model, err, attempt+1, max)
			if serr := sleep(c, delay(attempt)); serr != nil {
				return nil, err
			}
		}
	})
	if toolRetries > 0 {
		p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
			match := len(only) == 0
			for _, n := range only {
				if n == req.Call.Name {
					match = true
				}
			}
			for attempt := 0; ; attempt++ {
				r, err := next(c, req)
				if err == nil || !match || attempt >= toolRetries {
					return r, err
				}
				var fatal *sdk.FatalError
				if errors.As(err, &fatal) {
					return r, err
				}
				if serr := sleep(c, delay(attempt)); serr != nil {
					return r, err
				}
			}
		})
	}
	return nil
}

// Module is the compiled form of "hook_retry". modules/std registers it.
var Module = engine.Module{Name: "hook_retry", Requires: nil, Provides: nil, Permissions: nil, Apply: Apply}

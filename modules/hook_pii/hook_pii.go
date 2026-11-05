package hook_pii

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Detects PII and redacts, masks, hashes or blocks it.
//
// Config:
//
//	types:      [email, credit_card, ip, url, phone] (default email, credit_card, ip)
//	strategy:   redact (default) | mask | hash | block
//	hash_key:   secret://NAME keying the hash strategy (HMAC-SHA256); without
//	            it a random per-process key is used (tokens differ per run)
//	input:      apply to messages sent to the model (default true)
//	output:     apply to model replies (default false)
//	tool_results: apply to tool results (default true)
//	system:     also apply to the system prompt (default false: instructions
//	            often name a support address on purpose)
//	(input covers user, assistant and tool messages in the history,
//	including the string arguments of earlier tool calls)
//	patterns:   {name: regex} extra detectors
//
// Example:
//
//	kiln.WithModuleConfig("hook_pii", map[string]any{
//		"types":    []any{"email", "credit_card", "phone"},
//		"strategy": "mask",
//		"patterns": map[string]any{"order_id": `ORD-\d{6}`},
//	})
var Requires = []string{"?secrets"} // only for hash_key

func Apply(p *sdk.Module) error {
	if p.Priority() == 0 {
		p.SetPriority(30)
	}
	types := p.ConfigStrings("types")
	if len(types) == 0 {
		types = []string{"email", "credit_card", "ip"}
	}
	strategy := p.ConfigString("strategy", "redact")
	// hash: keyed HMAC, so tokens can't be reversed by hashing candidate
	// emails or IPs. With hash_key (a secret://), tokens are stable across
	// restarts and replicas; without it a random per-process key is used.
	var hashKey []byte
	if strategy == "hash" {
		if ref := p.ConfigString("hash_key", ""); ref != "" {
			k, err := p.SecretValue(ref)
			if err != nil {
				return fmt.Errorf("hook_pii: hash_key: %v", err)
			}
			hashKey = []byte(k)
		} else {
			hashKey = make([]byte, 32)
			if _, err := rand.Read(hashKey); err != nil {
				return err
			}
		}
	}
	onIn := p.ConfigBool("input", true)
	onOut := p.ConfigBool("output", false)
	onTools := p.ConfigBool("tool_results", true)
	onSystem := p.ConfigBool("system", false)

	builtin := map[string]string{
		"email":       `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
		"credit_card": `\b(?:\d[ -]?){12,18}\d\b`,
		"ip":          `\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`,
		"url":         `https?://[^\s"'<>]+`,
		"phone":       `\+?\d[\d\s().-]{8,}\d`,
	}
	type det struct {
		name string
		re   *regexp.Regexp
	}
	var dets []det
	for _, t := range types {
		if pat, ok := builtin[t]; ok {
			dets = append(dets, det{t, regexp.MustCompile(pat)})
		}
	}
	for name, pat := range p.ConfigMap("patterns") {
		s := ""
		if x, ok := pat.(string); ok {
			s = x
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return fmt.Errorf("hook_pii pattern %s: %w", name, err)
		}
		dets = append(dets, det{name, re})
	}

	luhn := func(s string) bool {
		var digits []int
		for _, r := range s {
			if r >= '0' && r <= '9' {
				digits = append(digits, int(r-'0'))
			}
		}
		if len(digits) < 13 || len(digits) > 19 {
			return false
		}
		sum := 0
		for i := len(digits) - 1; i >= 0; i-- {
			d := digits[i]
			if (len(digits)-1-i)%2 == 1 {
				d *= 2
				if d > 9 {
					d -= 9
				}
			}
			sum += d
		}
		return sum%10 == 0
	}

	// scrub returns the cleaned text and the PII types found.
	scrub := func(text string) (string, []string) {
		var found []string
		for _, d := range dets {
			name := d.name
			text = d.re.ReplaceAllStringFunc(text, func(m string) string {
				if name == "credit_card" && !luhn(m) {
					return m
				}
				found = append(found, name)
				switch strategy {
				case "mask":
					if len(m) <= 4 {
						return strings.Repeat("*", len(m))
					}
					return strings.Repeat("*", len(m)-4) + m[len(m)-4:]
				case "hash":
					mac := hmac.New(sha256.New, hashKey)
					mac.Write([]byte(m))
					return "<" + name + ":" + hex.EncodeToString(mac.Sum(nil))[:16] + ">"
				default:
					return "[REDACTED_" + strings.ToUpper(name) + "]"
				}
			})
		}
		return text, found
	}

	p.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		if onIn {
			msgs := make([]sdk.Message, len(req.Messages))
			for i, m := range req.Messages {
				if m.Role == sdk.RoleUser || m.Role == sdk.RoleAssistant || (m.Role == sdk.RoleTool && onTools) || (m.Role == sdk.RoleSystem && onSystem) {
					clean, found := scrub(m.Content)
					if len(found) > 0 && strategy == "block" {
						return nil, fmt.Errorf("blocked: message contains %s", strings.Join(found, ", "))
					}
					m.Content = clean
					if m.Reasoning != "" {
						clean, found = scrub(m.Reasoning)
						if len(found) > 0 && strategy == "block" {
							return nil, fmt.Errorf("blocked: reasoning contains %s", strings.Join(found, ", "))
						}
						m.Reasoning = clean
					}
				}
				if len(m.ToolCalls) > 0 {
					// Earlier tool calls in the history carry arguments too.
					calls := make([]sdk.ToolCall, len(m.ToolCalls))
					for j, tc := range m.ToolCalls {
						if tc.Args != nil {
							clean, found := scrubAny(tc.Args, scrub)
							if len(found) > 0 && strategy == "block" {
								return nil, fmt.Errorf("blocked: tool call arguments contain %s", strings.Join(found, ", "))
							}
							if args, ok := clean.(map[string]any); ok {
								tc.Args = args
							}
						}
						calls[j] = tc
					}
					m.ToolCalls = calls
				}
				msgs[i] = m
			}
			r2 := *req
			r2.Messages = msgs
			if onSystem {
				clean, found := scrub(r2.System)
				if len(found) > 0 && strategy == "block" {
					return nil, fmt.Errorf("blocked: system prompt contains %s", strings.Join(found, ", "))
				}
				r2.System = clean
			}
			req = &r2
		}
		resp, err := next(c, req)
		if err == nil && onOut {
			clean, found := scrub(resp.Message.Content)
			if len(found) > 0 && strategy == "block" {
				return nil, fmt.Errorf("blocked: reply contains %s", strings.Join(found, ", "))
			}
			resp.Message.Content = clean
		}
		return resp, err
	})
	if onTools {
		p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
			r, err := next(c, req)
			if err != nil {
				clean, found := scrub(err.Error())
				if len(found) > 0 && strategy == "block" {
					return &sdk.ToolResult{Content: "blocked: tool error contained " + strings.Join(found, ", "), IsError: true}, nil
				}
				err = fmt.Errorf("%s", clean)
			}
			if r != nil && r.Interrupt == nil {
				clean, found := scrub(r.Content)
				if len(found) > 0 && strategy == "block" {
					return &sdk.ToolResult{Content: "blocked: tool result contained " + strings.Join(found, ", "), IsError: true}, nil
				}
				r.Content = clean
			}
			return r, err
		})
	}
	return nil
}

// scrubAny walks maps, slices and strings, returning a cleaned copy and
// the PII types found.
func scrubAny(v any, scrub func(string) (string, []string)) (any, []string) {
	switch x := v.(type) {
	case string:
		clean, found := scrub(x)
		return clean, found
	case map[string]any:
		out := make(map[string]any, len(x))
		var found []string
		for k, e := range x {
			clean, f := scrubAny(e, scrub)
			out[k] = clean
			found = append(found, f...)
		}
		return out, found
	case []any:
		out := make([]any, len(x))
		var found []string
		for i, e := range x {
			clean, f := scrubAny(e, scrub)
			out[i] = clean
			found = append(found, f...)
		}
		return out, found
	}
	return v, nil
}

// Module is the compiled form of "hook_pii". modules/std registers it.
var Module = engine.Module{Name: "hook_pii", Requires: Requires, Provides: nil, Permissions: nil, Apply: Apply}

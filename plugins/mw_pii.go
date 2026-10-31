//go:build ignore

package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"eipagent/sdk"
)

// Detects PII and redacts, masks, hashes or blocks it.
//
// Config:
//
//	types:      [email, credit_card, ip, url, phone] (default email, credit_card, ip)
//	strategy:   redact (default) | mask | hash | block
//	input:      apply to messages sent to the model (default true)
//	output:     apply to model replies (default false)
//	tool_results: apply to tool results (default true)
//	patterns:   {name: regex} extra detectors
func Apply(ctx *sdk.Context) error {
	if ctx.Priority() == 0 {
		ctx.SetPriority(30)
	}
	types := ctx.ConfigStrings("types")
	if len(types) == 0 {
		types = []string{"email", "credit_card", "ip"}
	}
	strategy := ctx.ConfigString("strategy", "redact")
	onIn := ctx.ConfigBool("input", true)
	onOut := ctx.ConfigBool("output", false)
	onTools := ctx.ConfigBool("tool_results", true)

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
		if p, ok := builtin[t]; ok {
			dets = append(dets, det{t, regexp.MustCompile(p)})
		}
	}
	for name, p := range ctx.ConfigMap("patterns") {
		s, _ := p.(string)
		re, err := regexp.Compile(s)
		if err != nil {
			return fmt.Errorf("mw_pii pattern %s: %w", name, err)
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
					h := sha256.Sum256([]byte(m))
					return "<" + name + ":" + hex.EncodeToString(h[:])[:10] + ">"
				default:
					return "[REDACTED_" + strings.ToUpper(name) + "]"
				}
			})
		}
		return text, found
	}

	ctx.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		if onIn {
			msgs := make([]sdk.Message, len(req.Messages))
			for i, m := range req.Messages {
				if m.Role == sdk.RoleUser || (m.Role == sdk.RoleTool && onTools) {
					clean, found := scrub(m.Content)
					if len(found) > 0 && strategy == "block" {
						return nil, fmt.Errorf("blocked: message contains %s", strings.Join(found, ", "))
					}
					m.Content = clean
				}
				msgs[i] = m
			}
			r2 := *req
			r2.Messages = msgs
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
		ctx.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
			r, err := next(c, req)
			if err == nil && r != nil && r.Interrupt == nil {
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

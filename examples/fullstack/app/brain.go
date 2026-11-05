package app

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// DemoModel is a deterministic stand-in for an LLM, provided as
// "model/demo" (model string "demo:support"). It makes the same tool calls a
// real model would for the demo's flows, so the app and the website replay
// work offline. Swap in a real model with -model ollama:qwen3.5:4b.
func DemoModel() kiln.Module {
	return kiln.Module{Name: "llm_demo", Provides: []string{"model/*"}, Apply: func(p *sdk.Module) error {
		return p.Provide("model/demo", &sdk.Provider{
			Name:         "demo",
			Capabilities: map[string]bool{"tools": true, "streaming": true},
			Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
				msg := decide(req.Messages, req.Tools)
				if req.Stream != nil && msg.Content != "" {
					for _, w := range strings.SplitAfter(msg.Content, " ") {
						select {
						case <-c.Done():
							return nil, c.Err()
						case <-time.After(18 * time.Millisecond):
						}
						req.Stream(sdk.Chunk{Type: "text", Text: w})
					}
				}
				in := 0
				for _, m := range req.Messages {
					in += len(m.Content)/4 + 8
				}
				out := len(msg.Content)/4 + 12*len(msg.ToolCalls)
				finish := "stop"
				if len(msg.ToolCalls) > 0 {
					finish = "tool_calls"
				}
				return &sdk.ModelResponse{Message: msg, FinishReason: finish,
					Usage: sdk.Usage{InputTokens: in, OutputTokens: out, TotalTokens: in + out}}, nil
			},
		})
	}}
}

var orderRe = regexp.MustCompile(`#?\b(1\d{3})\b`)

func intentOf(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "receipt") || strings.Contains(t, "email me"):
		return "receipt"
	case strings.Contains(t, "refund") || strings.Contains(t, "broken") || strings.Contains(t, "never arrived") || strings.Contains(t, "damaged"):
		return "refund"
	case strings.Contains(t, "policy") || strings.Contains(t, "how long") || strings.Contains(t, "return"):
		return "help"
	case orderRe.MatchString(t) || strings.Contains(t, "where") || strings.Contains(t, "track") || strings.Contains(t, "status"):
		return "status"
	}
	return "chat"
}

func call(name string, args map[string]any) sdk.Message {
	return sdk.Message{Role: sdk.RoleAssistant, ToolCalls: []sdk.ToolCall{{Name: name, Args: args}}}
}

func say(format string, a ...any) sdk.Message {
	return sdk.Message{Role: sdk.RoleAssistant, Content: fmt.Sprintf(format, a...)}
}

// demoTools are the app's own tools; any other tool came from a module
// added at runtime (the Modules tab).
var demoTools = map[string]bool{"lookup_order": true, "search_help_center": true, "issue_refund": true, "send_email": true}

var cityRe = regexp.MustCompile(`\b(?:to|in|for)\s+([A-Z][a-zA-Z]+(?:\s[A-Z][a-zA-Z]+)?)`)

// moduleTool finds a tool from a runtime module that fits the message,
// the way a real model would pick a newly available tool: the message
// names it ("shipping estimate" or "shipping_estimate"), or, when it
// isn't about an order, uses a word from the tool's name or description
// ("How long does shipping to Paris take?").
func moduleTool(text string, tools []sdk.Tool) *sdk.Tool {
	t := strings.ToLower(text)
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(t, func(r rune) bool { return !(r >= 'a' && r <= 'z') }) {
		words[w] = true
	}
	var loose *sdk.Tool
	for i := range tools {
		n := tools[i].Name
		if demoTools[n] {
			continue
		}
		if strings.Contains(t, n) || strings.Contains(t, strings.ReplaceAll(n, "_", " ")) {
			return &tools[i]
		}
		for _, w := range strings.FieldsFunc(strings.ToLower(n+" "+tools[i].Description), func(r rune) bool { return !(r >= 'a' && r <= 'z') }) {
			if len(w) >= 6 && words[w] && loose == nil {
				loose = &tools[i]
			}
		}
	}
	if loose != nil && !orderRe.MatchString(t) {
		return loose
	}
	return nil
}

// argsFor fills a module tool's arguments from the message: a city for
// city-like fields, the order number for order fields, else the text.
func argsFor(tool sdk.Tool, text, orderID string) map[string]any {
	args := map[string]any{}
	props, _ := tool.Schema["properties"].(map[string]any)
	for k := range props {
		switch lk := strings.ToLower(k); {
		case strings.Contains(lk, "city") || strings.Contains(lk, "destination") || strings.Contains(lk, "place"):
			city := "Berlin"
			if m := cityRe.FindStringSubmatch(text); m != nil {
				city = m[1]
			}
			args[k] = city
		case strings.Contains(lk, "order"):
			args[k] = orderID
		default:
			args[k] = text
		}
	}
	return args
}

func decide(msgs []sdk.Message, tools []sdk.Tool) sdk.Message {
	ui := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == sdk.RoleUser {
			ui = i
			break
		}
	}
	if ui < 0 {
		return say("Hi! How can I help?")
	}
	user := msgs[ui].Content
	orderID := ""
	for i := ui; i >= 0 && orderID == ""; i-- { // this turn first, then earlier turns
		if msgs[i].Role == sdk.RoleUser {
			if m := orderRe.FindStringSubmatch(msgs[i].Content); m != nil {
				orderID = m[1]
			}
		}
	}
	results := map[string]sdk.Message{}
	for _, m := range msgs[ui+1:] {
		if m.Role == sdk.RoleTool {
			results[m.ToolName] = m
		}
	}
	if t := moduleTool(user, tools); t != nil {
		if r, ok := results[t.Name]; ok {
			if r.IsError {
				return say("The %s module couldn't answer: %s", t.Name, r.Content)
			}
			return say("%s", r.Content)
		}
		return call(t.Name, argsFor(*t, user, orderID))
	}
	var order Order
	if r, ok := results["lookup_order"]; ok && !r.IsError {
		json.Unmarshal([]byte(r.Content), &order)
	}

	switch intentOf(user) {
	case "help":
		r, ok := results["search_help_center"]
		if !ok {
			return call("search_help_center", map[string]any{"query": user})
		}
		return say("Here's what our help center says. %s Is there an order I can check for you?", strings.SplitN(r.Content, ": ", 2)[len(strings.SplitN(r.Content, ": ", 2))-1])

	case "status", "refund", "receipt":
		if orderID == "" {
			return say("I can help with that. What's your order number? It starts with 1 and has four digits, like 1042.")
		}
		r, looked := results["lookup_order"]
		if !looked {
			return call("lookup_order", map[string]any{"order_id": orderID})
		}
		if r.IsError {
			return say("I couldn't find order %s. Could you double-check the number?", orderID)
		}
	default:
		return say("Hi, I'm the Acme support assistant. I can track orders, explain our policies, issue refunds and email receipts. What can I do for you?")
	}

	switch intentOf(user) {
	case "status":
		return say("Order #%s (%s) is %s: %s. The total was $%.2f.", order.ID, order.Item, order.Status, order.Detail, order.Total)

	case "refund":
		r, done := results["issue_refund"]
		if !done {
			if order.Refunded >= order.Total-0.001 {
				return say("Order #%s was already refunded in full (reference %s).", order.ID, order.RefundRef)
			}
			reason := "customer request"
			t := strings.ToLower(user)
			switch {
			case strings.Contains(t, "broken") || strings.Contains(t, "damaged") || strings.Contains(t, "cracked"):
				reason = "item arrived damaged"
			case strings.Contains(t, "never arrived") || strings.Contains(t, "lost"):
				reason = "parcel lost in transit"
			}
			return call("issue_refund", map[string]any{"order_id": order.ID, "amount": order.Total - order.Refunded, "reason": reason})
		}
		if r.IsError {
			why := strings.TrimPrefix(r.Content, "rejected by reviewer: ")
			return say("I wasn't able to refund order #%s: a supervisor declined it (%s). I've added a note to your ticket and a teammate will follow up by email.", order.ID, why)
		}
		return say("Done. %s You'll see it on your statement within 5 to 7 business days. Sorry about the trouble with the %s.", r.Content, strings.ToLower(order.Item))

	case "receipt":
		r, sent := results["send_email"]
		if !sent {
			return call("send_email", map[string]any{
				"to":      order.Email,
				"subject": fmt.Sprintf("Your receipt for order #%s", order.ID),
				"body":    fmt.Sprintf("Hi %s,\n\nThanks for your order. %s, $%.2f, paid with the card ending %s.\n\nAcme Support", strings.Fields(order.Customer)[0], order.Item, order.Total, order.Card),
			})
		}
		if r.IsError {
			return say("I didn't send the email: %s", strings.TrimPrefix(r.Content, "rejected by reviewer: "))
		}
		return say("I've emailed the receipt for order #%s to %s.", order.ID, order.Email)
	}
	return say("How else can I help?")
}

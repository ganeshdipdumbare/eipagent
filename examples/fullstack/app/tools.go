package app

import (
	"context"
	"fmt"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Tools turns the shop's operations into typed tools. The argument
// schemas the model sees are derived from the input structs.
func Tools(shop *Shop) []kiln.ToolDef {
	type OrderIn struct {
		OrderID string `json:"order_id" desc:"order number, e.g. 1077"`
	}
	lookup := kiln.ReadOnly(kiln.Tool("lookup_order", "Look up an order: item, total, shipping status, customer",
		func(ctx context.Context, in OrderIn) (Order, error) {
			o, ok := shop.Order(in.OrderID)
			if !ok {
				return Order{}, fmt.Errorf("no order %s", in.OrderID)
			}
			return o, nil
		}))

	type SearchIn struct {
		Query string `json:"query" desc:"what the customer is asking about"`
	}
	search := kiln.ReadOnly(kiln.Tool("search_help_center", "Search help-center articles (returns, shipping, damage)",
		func(ctx context.Context, in SearchIn) (string, error) {
			a, ok := Search(in.Query)
			if !ok {
				return "No article found.", nil
			}
			return a.Title + ": " + a.Body, nil
		}))

	type RefundIn struct {
		OrderID string  `json:"order_id" desc:"order number"`
		Amount  float64 `json:"amount" desc:"amount in USD"`
		Reason  string  `json:"reason" desc:"short reason for the refund"`
	}
	refund := kiln.WithMeta(kiln.Tool("issue_refund", "Refund money to the customer's card",
		func(ctx context.Context, in RefundIn) (string, error) {
			o, err := shop.Refund(in.OrderID, in.Amount)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Refunded $%.2f to the card ending %s for order %s (%s). Reference %s.",
				in.Amount, o.Card, o.ID, o.Item, o.RefundRef), nil
		}), "risk", "high")

	return []kiln.ToolDef{lookup, search, refund}
}

type EmailIn struct {
	To      string `json:"to" desc:"customer email address"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Mailer is a compiled module that owns the send_email tool. Delivery
// goes through a mail relay whose token it reads from the "secrets"
// service when it sends: only this module is granted MAIL_RELAY_TOKEN,
// every read lands in the audit log as secret.access, and the value is
// redacted from logs, traces and audit records.
func Mailer(shop *Shop) kiln.Module {
	return kiln.Module{Name: "mailer", Requires: []string{"secrets"}, Secrets: []string{"MAIL_RELAY_TOKEN"}, Apply: func(p *sdk.Module) error {
		p.AddTool(kiln.Tool("send_email", "Email the customer",
			func(c context.Context, in EmailIn) (string, error) {
				if _, err := p.Secret("secret://MAIL_RELAY_TOKEN"); err != nil {
					return "", fmt.Errorf("mail relay: %w", err)
				}
				shop.Send(Email{To: in.To, Subject: in.Subject, Body: in.Body})
				return "Email sent to " + in.To + ".", nil
			}))
		return nil
	}}
}

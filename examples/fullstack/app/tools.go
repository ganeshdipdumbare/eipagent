package app

import (
	"context"
	"fmt"

	"eipagent"
)

// Tools turns the shop's operations into typed tools. The argument
// schemas the model sees are derived from the input structs.
func Tools(shop *Shop) []eipagent.ToolDef {
	type OrderIn struct {
		OrderID string `json:"order_id" desc:"order number, e.g. 1077"`
	}
	lookup := eipagent.ReadOnly(eipagent.Tool("lookup_order", "Look up an order: item, total, shipping status, customer",
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
	search := eipagent.ReadOnly(eipagent.Tool("search_help_center", "Search help-center articles (returns, shipping, damage)",
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
	refund := eipagent.WithMeta(eipagent.Tool("issue_refund", "Refund money to the customer's card",
		func(ctx context.Context, in RefundIn) (string, error) {
			o, err := shop.Refund(in.OrderID, in.Amount)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Refunded $%.2f to the card ending %s for order %s (%s). Reference %s.",
				in.Amount, o.Card, o.ID, o.Item, o.RefundRef), nil
		}), "risk", "high")

	type EmailIn struct {
		To      string `json:"to" desc:"customer email address"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	email := eipagent.Tool("send_email", "Email the customer",
		func(ctx context.Context, in EmailIn) (string, error) {
			shop.Send(Email{To: in.To, Subject: in.Subject, Body: in.Body})
			return "Email sent to " + in.To + ".", nil
		})

	return []eipagent.ToolDef{lookup, search, refund, email}
}

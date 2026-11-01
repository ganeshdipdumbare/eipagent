// Human in the loop with a durable thread: the run pauses before a risky
// tool, the decision can come later (even in another process sharing the
// checkpointer), and the run resumes.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ganeshdipdumbare/eipagent"
)

func main() {
	ctx := context.Background()
	refund := eipagent.Tool("refund", "Refund an order", func(ctx context.Context, in struct {
		Order  string  `json:"order"`
		Amount float64 `json:"amount"`
	}) (string, error) {
		return fmt.Sprintf("refunded %.2f on %s", in.Amount, in.Order), nil
	})
	a, err := eipagent.New(
		eipagent.WithModel("mock:demo"),
		eipagent.WithPlugin("model_mock", map[string]any{"script": []any{
			map[string]any{"tool": "refund", "args": map[string]any{"order": "A-17", "amount": 40}},
			map[string]any{"text": "The refund is done."},
		}}),
		eipagent.WithTools(refund),
		eipagent.WithPlugin("hook_approval", map[string]any{"interrupt_on": map[string]any{
			"refund": map[string]any{"allowed": []any{"approve", "reject"}, "description": "Approve this refund?"},
		}}),
		eipagent.WithPlugin("threads_file", map[string]any{"dir": ".eipagent/threads"}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
	th := a.Thread("ticket-981")
	res, err := th.Generate(ctx, "Customer wants a refund for A-17")
	if err != nil {
		log.Fatal(err)
	}
	for _, in := range res.Interrupts {
		fmt.Printf("waiting for approval: %s %v\n", in.Message, in.ToolCall.Args)
		res, err = th.Resume(ctx, eipagent.Approve(in.ID))
		if err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println(res.Text)
}

//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"sync"

	"eipagent/sdk"
)

// Stops the model from repeating itself. When it calls the same tool
// with the same arguments more than max_repeats times in a run, the call
// is not run and the model is told to try something else; past
// stop_after repeats the run ends with an error.
//
// Config: max_repeats (default 3), stop_after (default 6; 0 = never stop).
//
// Example:
//
//	eipagent.WithPlugin("hook_loop_detect", map[string]any{"max_repeats": 2, "stop_after": 4})
func Apply(p *sdk.Plugin) error {
	maxRepeats := p.ConfigInt("max_repeats", 3)
	stopAfter := p.ConfigInt("stop_after", 6)

	var mu sync.Mutex
	counts := map[string]map[string]int{} // run ID -> call -> times seen

	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		if req.Run == nil || req.Decision != nil { // a resumed, already-decided call isn't a repeat
			return next(c, req)
		}
		key := req.Call.Name + " " + sdk.ToJSON(req.Call.Args)
		mu.Lock()
		if counts[req.Run.ID] == nil {
			counts[req.Run.ID] = map[string]int{}
		}
		counts[req.Run.ID][key]++
		n := counts[req.Run.ID][key]
		mu.Unlock()
		if stopAfter > 0 && n > stopAfter {
			return nil, sdk.Fatal(fmt.Errorf("hook_loop_detect: %s was called with the same arguments %d times; stopping the run", req.Call.Name, n))
		}
		if n > maxRepeats {
			p.Logf("%s repeated %d times", req.Call.Name, n)
			return &sdk.ToolResult{IsError: true, Content: fmt.Sprintf(
				"Not run: you already called %s with exactly these arguments %d times. The result will not change. Use what you have, try different arguments or another tool, or answer.",
				req.Call.Name, n-1)}, nil
		}
		return next(c, req)
	})

	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		delete(counts, st.ID)
		mu.Unlock()
		return nil
	})
	return nil
}

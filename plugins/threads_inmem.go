//go:build ignore

package plugin

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"eipagent/sdk"
)

// In-memory thread storage, provided as "threads". State is
// deep-copied (via JSON) so callers can't mutate stored history.
//
// Example:
//
//	eipagent.WithPlugins("threads_inmem") // the default; lost on exit
func Apply(p *sdk.Plugin) error {
	var mu sync.Mutex
	data := map[string][]byte{}
	vers := map[string]int64{}
	return p.Provide("threads", &sdk.Checkpointer{
		Load: func(c context.Context, id string) (*sdk.ThreadState, error) {
			mu.Lock()
			defer mu.Unlock()
			raw, ok := data[id]
			if !ok {
				return nil, nil
			}
			var st sdk.ThreadState
			if err := json.Unmarshal(raw, &st); err != nil {
				return nil, err
			}
			return &st, nil
		},
		Save: func(c context.Context, st *sdk.ThreadState, expected int64) error {
			mu.Lock()
			defer mu.Unlock()
			if vers[st.ID] != expected {
				return sdk.ErrVersionConflict
			}
			st.Version = expected + 1
			raw, err := json.Marshal(st)
			if err != nil {
				return err
			}
			data[st.ID], vers[st.ID] = raw, st.Version
			return nil
		},
		Delete: func(c context.Context, id string) error {
			mu.Lock()
			defer mu.Unlock()
			delete(data, id)
			delete(vers, id)
			return nil
		},
		List: func(c context.Context, prefix string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			var out []string
			for id := range data {
				if strings.HasPrefix(id, prefix) {
					out = append(out, id)
				}
			}
			sort.Strings(out)
			return out, nil
		},
	})
}

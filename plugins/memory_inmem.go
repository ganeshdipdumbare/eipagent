//go:build ignore

package plugin

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"eipagent/sdk"
)

// In-memory long-term memory, provided as "memory".
func Apply(p *sdk.Plugin) error {
	var mu sync.Mutex
	items := map[string]sdk.StoreItem{}
	key := func(ns []string, k string) string { return strings.Join(ns, "\x00") + "\x01" + k }
	under := func(ns, prefix []string) bool {
		if len(ns) < len(prefix) {
			return false
		}
		for i := range prefix {
			if ns[i] != prefix[i] {
				return false
			}
		}
		return true
	}
	return p.Provide("memory", &sdk.Store{
		Put: func(c context.Context, ns []string, k string, v map[string]any) error {
			mu.Lock()
			defer mu.Unlock()
			items[key(ns, k)] = sdk.StoreItem{Namespace: append([]string{}, ns...), Key: k, Value: v, Updated: time.Now().UnixMilli()}
			return nil
		},
		Get: func(c context.Context, ns []string, k string) (*sdk.StoreItem, error) {
			mu.Lock()
			defer mu.Unlock()
			it, ok := items[key(ns, k)]
			if !ok {
				return nil, nil
			}
			return &it, nil
		},
		Search: func(c context.Context, ns []string, q string, limit int) ([]sdk.StoreItem, error) {
			mu.Lock()
			defer mu.Unlock()
			q = strings.ToLower(q)
			var out []sdk.StoreItem
			for _, it := range items {
				if !under(it.Namespace, ns) {
					continue
				}
				if q != "" && !strings.Contains(strings.ToLower(sdk.ToJSON(it.Value)), q) && !strings.Contains(strings.ToLower(it.Key), q) {
					continue
				}
				out = append(out, it)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
			if limit > 0 && len(out) > limit {
				out = out[:limit]
			}
			return out, nil
		},
		Delete: func(c context.Context, ns []string, k string) error {
			mu.Lock()
			defer mu.Unlock()
			delete(items, key(ns, k))
			return nil
		},
	})
}

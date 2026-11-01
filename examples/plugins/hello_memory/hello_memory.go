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

// hello_memory is long-term memory, provided as "memory". This one
// keeps items in memory; replace the map with your database.
func Apply(p *sdk.Plugin) error {
	var mu sync.Mutex
	items := map[string]sdk.StoreItem{}
	key := func(ns []string, k string) string { return strings.Join(ns, "/") + "#" + k }
	return p.Provide("memory", &sdk.Store{
		Put: func(c context.Context, ns []string, k string, val map[string]any) error {
			mu.Lock()
			defer mu.Unlock()
			items[key(ns, k)] = sdk.StoreItem{Namespace: ns, Key: k, Value: val, Updated: time.Now().Unix()}
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
		Search: func(c context.Context, ns []string, query string, limit int) ([]sdk.StoreItem, error) {
			mu.Lock()
			defer mu.Unlock()
			prefix := strings.Join(ns, "/") + "#"
			var out []sdk.StoreItem
			for k, it := range items {
				if strings.HasPrefix(k, prefix) && strings.Contains(strings.ToLower(sdk.ToJSON(it.Value)), strings.ToLower(query)) {
					out = append(out, it)
				}
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

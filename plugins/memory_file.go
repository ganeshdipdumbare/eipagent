//go:build ignore

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"eipagent/sdk"
)

// Long-term store kept in one JSON file, provided as "memory". Good for
// single-node and development use; use memory_sql for shared storage.
//
// Config: path (default ".eipagent/store.json").
//
// Example:
//
//	eipagent.WithPlugin("memory_file", map[string]any{"path": ".eipagent/memory.json"})
//	eipagent.WithPlugins("tool_memory") // lets the model remember and recall
var Permissions = []string{"fs:read", "fs:write"}

func Apply(p *sdk.Plugin) error {
	path := p.ConfigString("path", ".eipagent/store.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var mu sync.Mutex
	key := func(ns []string, k string) string { return strings.Join(ns, "\x1f") + "\x1e" + k }
	read := func() (map[string]sdk.StoreItem, error) {
		items := map[string]sdk.StoreItem{}
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return items, nil
		}
		if err != nil {
			return nil, err
		}
		return items, json.Unmarshal(raw, &items)
	}
	write := func(items map[string]sdk.StoreItem) error {
		raw, err := json.Marshal(items)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
			return err
		}
		return os.Rename(path+".tmp", path)
	}
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
			items, err := read()
			if err != nil {
				return err
			}
			items[key(ns, k)] = sdk.StoreItem{Namespace: ns, Key: k, Value: v, Updated: time.Now().UnixMilli()}
			return write(items)
		},
		Get: func(c context.Context, ns []string, k string) (*sdk.StoreItem, error) {
			mu.Lock()
			defer mu.Unlock()
			items, err := read()
			if err != nil {
				return nil, err
			}
			it, ok := items[key(ns, k)]
			if !ok {
				return nil, nil
			}
			return &it, nil
		},
		Search: func(c context.Context, ns []string, q string, limit int) ([]sdk.StoreItem, error) {
			mu.Lock()
			defer mu.Unlock()
			items, err := read()
			if err != nil {
				return nil, err
			}
			q = strings.ToLower(q)
			var out []sdk.StoreItem
			for _, it := range items {
				if !under(it.Namespace, ns) {
					continue
				}
				if q != "" && !strings.Contains(strings.ToLower(it.Key+" "+sdk.ToJSON(it.Value)), q) {
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
			items, err := read()
			if err != nil {
				return err
			}
			delete(items, key(ns, k))
			return write(items)
		},
	})
}

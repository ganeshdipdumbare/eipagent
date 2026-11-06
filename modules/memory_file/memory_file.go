package memory_file

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Long-term store kept in one JSON file, provided as "memory". Good for
// single-node and development use; use memory_sql for shared storage.
//
// Config: path (default ".kiln/store.json"). The file's directory is
// granted through fs:read and fs:write and must already exist.
//
// Example:
//
//	memory_file.New(map[string]any{"path": ".kiln/memory.json"})
//	kiln.Config{Modules: []kiln.Module{tool_memory.New()}} // lets the model remember and recall
var Permissions = []string{"fs:read", "fs:write"}

func Apply(p *sdk.Scope) error {
	path := p.ConfigString("path", ".kiln/store.json")
	d, err := p.Dir(filepath.Dir(path), true)
	if err != nil {
		return err
	}
	p.OnClose(func() { d.Close() })
	name := filepath.Base(path)
	var mu sync.Mutex
	key := func(ns []string, k string) string { return strings.Join(ns, "\x1f") + "\x1e" + k }
	read := func() (map[string]sdk.StoreItem, error) {
		items := map[string]sdk.StoreItem{}
		raw, err := d.ReadFile(name)
		if errors.Is(err, fs.ErrNotExist) {
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
		if err := d.WriteFile(name+".tmp", raw, 0o600); err != nil {
			return err
		}
		return d.Rename(name+".tmp", name)
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
			items[key(ns, k)] = sdk.StoreItem{Namespace: ns, Key: k, Value: sdk.RedactMap(v), Updated: time.Now().UnixMilli()}
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

// Module is the compiled form of "memory_file". modules/std registers it.
var Module = engine.Module{Name: "memory_file", Requires: nil, Provides: []string{"memory"}, Permissions: Permissions, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

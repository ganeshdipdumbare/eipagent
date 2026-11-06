package threads_file

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"sort"
	"strings"

	"github.com/ganeshdipdumbare/kiln/internal/filelock"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Thread storage keeping one JSON file per thread, provided as
// "threads". Safe across processes on one machine (PID lock files).
//
// Config: dir (default ".kiln/threads"). The directory is granted
// through fs:read and fs:write and must already exist.
//
// Example:
//
//	threads_file.New(map[string]any{"dir": ".kiln/threads"})
//	// Shortcut with memory_file too: kiln.Persistence("file", ".kiln")
var Permissions = []string{"fs:read", "fs:write"}

func Apply(p *sdk.Scope) error {
	dir := p.ConfigString("dir", ".kiln/threads")
	d, err := p.Dir(dir, true)
	if err != nil {
		return err
	}
	p.OnClose(func() { d.Close() })
	path := func(id string) string { return url.PathEscape(id) + ".json" }
	load := func(id string) (*sdk.ThreadState, error) {
		raw, err := d.ReadFile(path(id))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var st sdk.ThreadState
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, err
		}
		return &st, nil
	}
	return p.Provide("threads", &sdk.Checkpointer{
		Load: func(c context.Context, id string) (*sdk.ThreadState, error) { return load(id) },
		Save: func(c context.Context, st *sdk.ThreadState, expected int64) error {
			unlock, err := filelock.Lock(c, d, path(st.ID)+".lock")
			if err != nil {
				return err
			}
			defer unlock()
			cur, err := load(st.ID)
			if err != nil {
				return err
			}
			var have int64
			if cur != nil {
				have = cur.Version
			}
			if have != expected {
				return sdk.ErrVersionConflict
			}
			st.Version = expected + 1
			out := sdk.RedactThreadState(st)
			raw, err := json.Marshal(out)
			if err != nil {
				return err
			}
			tmp := path(st.ID) + ".tmp"
			if err := d.WriteFile(tmp, raw, 0o600); err != nil {
				return err
			}
			return d.Rename(tmp, path(st.ID))
		},
		Delete: func(c context.Context, id string) error {
			err := d.Remove(path(id))
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		},
		List: func(c context.Context, prefix string) ([]string, error) {
			ents, err := d.ReadDir(".")
			if err != nil {
				return nil, err
			}
			var out []string
			for _, e := range ents {
				n := e.Name()
				if !strings.HasSuffix(n, ".json") {
					continue
				}
				id, err := url.PathUnescape(strings.TrimSuffix(n, ".json"))
				if err == nil && strings.HasPrefix(id, prefix) {
					out = append(out, id)
				}
			}
			sort.Strings(out)
			return out, nil
		},
	})
}

// Module is the compiled form of "threads_file". modules/std registers it.
var Module = engine.Module{Name: "threads_file", Requires: nil, Provides: []string{"threads"}, Permissions: Permissions, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

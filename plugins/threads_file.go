//go:build ignore

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"eipagent/sdk"
)

// Thread storage keeping one JSON file per thread, provided as
// "threads". Safe across processes on one machine (lock files).
//
// Config: dir (default ".eipagent/threads").
var Permissions = []string{"fs:read", "fs:write"}

func Apply(p *sdk.Plugin) error {
	dir := p.ConfigString("dir", ".eipagent/threads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := func(id string) string { return filepath.Join(dir, url.PathEscape(id)+".json") }
	lock := func(c context.Context, id string) (func(), error) {
		lp := path(id) + ".lock"
		for i := 0; ; i++ {
			f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err == nil {
				f.Close()
				return func() { os.Remove(lp) }, nil
			}
			if st, serr := os.Stat(lp); serr == nil && time.Since(st.ModTime()) > 30*time.Second {
				os.Remove(lp) // stale lock from a crashed process
				continue
			}
			if i > 200 {
				return nil, fmt.Errorf("threads_file: thread %s is locked", id)
			}
			select {
			case <-c.Done():
				return nil, c.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	load := func(id string) (*sdk.ThreadState, error) {
		raw, err := os.ReadFile(path(id))
		if errors.Is(err, os.ErrNotExist) {
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
			unlock, err := lock(c, st.ID)
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
			raw, err := json.Marshal(st)
			if err != nil {
				return err
			}
			tmp := path(st.ID) + ".tmp"
			if err := os.WriteFile(tmp, raw, 0o600); err != nil {
				return err
			}
			return os.Rename(tmp, path(st.ID))
		},
		Delete: func(c context.Context, id string) error {
			err := os.Remove(path(id))
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		},
		List: func(c context.Context, prefix string) ([]string, error) {
			ents, err := os.ReadDir(dir)
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

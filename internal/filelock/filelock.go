// Package filelock provides PID-based exclusive lock files for sdk.Dir.
package filelock

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Lock creates name exclusively under d and writes this process's PID.
// It only removes an existing lock when the recorded PID is gone (or the
// file has no usable PID and is older than 30s). Call the returned unlock
// to release.
func Lock(ctx context.Context, d *sdk.Dir, name string) (func(), error) {
	for i := 0; ; i++ {
		if err := d.CreateNew(name, 0o600); err == nil {
			_ = d.WriteFile(name, []byte(strconv.Itoa(os.Getpid())), 0o600)
			return func() { _ = d.Remove(name) }, nil
		}
		if staleLock(d, name) {
			_ = d.Remove(name)
			continue
		}
		if i > 200 {
			return nil, fmt.Errorf("filelock: %s is locked", name)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func staleLock(d *sdk.Dir, name string) bool {
	raw, err := d.ReadFile(name)
	if err == nil {
		pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if perr == nil && pid > 0 {
			return !pidAlive(pid)
		}
	}
	st, serr := d.Stat(name)
	return serr == nil && time.Since(st.ModTime()) > 30*time.Second
}

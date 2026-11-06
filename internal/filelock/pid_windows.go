//go:build windows

package filelock

import "time"

// On Windows we cannot probe another process cheaply; treat locks with a
// valid PID as alive until the 30s mtime fallback in staleLock fires.
func pidAlive(pid int) bool {
	_ = time.Now
	return pid > 0
}

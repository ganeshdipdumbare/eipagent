package engine

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// protectedWriteDirs are never writable by plugins, whatever their
// permissions: procfs/sysfs/devfs expose the host process and devices,
// not files a plugin should change.
var protectedWriteDirs = []string{"/proc", "/sys", "/dev"}

// protectedReadDirs are never readable by interpreted plugins: procfs and
// sysfs expose the host process and kernel state, and /dev exposes devices.
var protectedReadDirs = []string{"/proc", "/sys", "/dev"}

type deniedFS struct{ err error }

func (d deniedFS) Open(string) (fs.File, error) { return nil, d.err }

type guardedRootFS struct{ root string }

func (g guardedRootFS) Open(name string) (fs.File, error) {
	target, err := readTarget(filepath.Join(g.root, filepath.FromSlash(name)))
	if err != nil {
		return nil, err
	}
	return os.Open(target)
}

// readTarget refuses reads under /proc, /sys and /dev, following symlinks
// so /proc/self/root/... cannot be used as a side door.
func readTarget(p string) (string, error) {
	target := resolveWritePath(p)
	for _, root := range protectedReadDirs {
		if target == root || strings.HasPrefix(target, root+string(filepath.Separator)) {
			return "", fmt.Errorf("os: refusing to read %s: protected system path", p)
		}
	}
	return target, nil
}

func guardedReadFile(name string) ([]byte, error) {
	target, err := readTarget(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(target)
}

func guardedOpen(name string) (*os.File, error) {
	target, err := readTarget(name)
	if err != nil {
		return nil, err
	}
	return os.Open(target)
}

func guardedReadDir(name string) ([]os.DirEntry, error) {
	target, err := readTarget(name)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(target)
}

func guardedStat(name string) (os.FileInfo, error) {
	target, err := readTarget(name)
	if err != nil {
		return nil, err
	}
	return os.Stat(target)
}

func guardedLstat(name string) (os.FileInfo, error) {
	target, err := readTarget(name)
	if err != nil {
		return nil, err
	}
	return os.Lstat(target)
}

func guardedReadlink(name string) (string, error) {
	target, err := readTarget(name)
	if err != nil {
		return "", err
	}
	return os.Readlink(target)
}

func guardedDirFS(dir string) fs.FS {
	if _, err := readTarget(dir); err != nil {
		return deniedFS{err}
	}
	return guardedRootFS{root: dir}
}

// resolveWritePath follows symlinks component by component, including a
// link whose target does not exist, so a dangling link can't smuggle a
// write into a protected path.
func resolveWritePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return resolveParts(filepath.Clean(abs), 0)
}

func resolveParts(abs string, depth int) string {
	if depth > 64 {
		return filepath.Clean(abs)
	}
	vol := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, vol)
	parts := strings.Split(rest, string(filepath.Separator))
	resolved := vol
	if resolved == "" {
		resolved = string(filepath.Separator)
	}
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		candidate := filepath.Join(resolved, part)
		fi, err := os.Lstat(candidate)
		if err != nil {
			// Nothing below this point exists: keep the rest as written.
			out := candidate
			for _, r := range parts[i+1:] {
				if r != "" && r != "." {
					out = filepath.Join(out, r)
				}
			}
			return filepath.Clean(out)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(candidate)
			if err != nil {
				return filepath.Clean(candidate)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(candidate), target)
			}
			next := []string{target}
			next = append(next, parts[i+1:]...)
			return resolveParts(filepath.Join(next...), depth+1)
		}
		resolved = candidate
	}
	return filepath.Clean(resolved)
}

// underOrAncestor reports whether target is root, under root, or an
// ancestor of root. Ancestors matter for RemoveAll and Rename: deleting
// or moving a directory that holds a protected file must be refused too.
func underOrAncestor(target, root string) bool {
	return target == root ||
		strings.HasPrefix(target, root+string(filepath.Separator)) ||
		strings.HasPrefix(root, target+string(filepath.Separator))
}

// writeTarget refuses paths under /proc, /sys, /dev and the paths
// holding the agent's own configuration and plugin store. It also
// refuses ancestors of those paths, so a recursive delete or rename
// can't remove or move them. It returns the resolved path so callers
// can open that instead of the original (narrowing the race between the
// check and the operation).
func writeTarget(p string, deny []string, protectAncestors bool) (string, error) {
	target := resolveWritePath(p)
	match := func(root string) bool {
		if target == root || strings.HasPrefix(target, root+string(filepath.Separator)) {
			return true
		}
		if !protectAncestors {
			return false
		}
		// The filesystem root is an ancestor of every protect path. The
		// prefix check misses it because target+sep is "//".
		return target == string(filepath.Separator) || strings.HasPrefix(root, target+string(filepath.Separator))
	}
	for _, root := range protectedWriteDirs {
		if match(root) {
			return "", fmt.Errorf("os: refusing to write %s: protected system path", p)
		}
	}
	for _, d := range deny {
		if d == "" {
			continue
		}
		if match(resolveWritePath(d)) {
			return "", fmt.Errorf("os: refusing to write %s: protected agent path", p)
		}
	}
	return target, nil
}

// checkWritePath is for writes that can only touch the target itself.
func checkWritePath(p string, deny []string) error {
	_, err := writeTarget(p, deny, false)
	return err
}

// checkWriteTree is for recursive operations (delete, rename, copy): an
// ancestor of a protected path must be refused too.
func checkWriteTree(p string, deny []string) error {
	_, err := writeTarget(p, deny, true)
	return err
}

// The wrappers below are os symbols for plugins holding fs:write. The
// write-only variants are for plugins without fs:read: they never hand
// the plugin a readable handle.

func guardedCreate(name string, deny []string, readable bool) (*os.File, error) {
	target, err := writeTarget(name, deny, false)
	if err != nil {
		return nil, err
	}
	if readable {
		return os.Create(target)
	}
	return os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
}

func guardedCreateTemp(dir, pattern string, deny []string, readable bool) (*os.File, error) {
	d := dir
	if d == "" {
		d = os.TempDir()
	}
	target, err := writeTarget(d, deny, false)
	if err != nil {
		return nil, err
	}
	if readable {
		return os.CreateTemp(target, pattern)
	}
	f, err := os.CreateTemp(target, pattern)
	if err != nil {
		return nil, err
	}
	name := f.Name()
	f.Close()
	wf, err := os.OpenFile(name, os.O_WRONLY, 0o600)
	if err != nil {
		os.Remove(name)
		return nil, err
	}
	return wf, nil
}

func guardedWriteFile(name string, data []byte, perm os.FileMode, deny []string) error {
	target, err := writeTarget(name, deny, false)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, perm)
}

func guardedMkdir(name string, perm os.FileMode, deny []string) error {
	target, err := writeTarget(name, deny, false)
	if err != nil {
		return err
	}
	return os.Mkdir(target, perm)
}

func guardedMkdirAll(name string, perm os.FileMode, deny []string) error {
	target, err := writeTarget(name, deny, false)
	if err != nil {
		return err
	}
	return os.MkdirAll(target, perm)
}

func guardedMkdirTemp(dir, pattern string, deny []string) (string, error) {
	d := dir
	if d == "" {
		d = os.TempDir()
	}
	target, err := writeTarget(d, deny, false)
	if err != nil {
		return "", err
	}
	return os.MkdirTemp(target, pattern)
}

func guardedRemove(name string, deny []string) error {
	target, err := writeTarget(name, deny, true)
	if err != nil {
		return err
	}
	return os.Remove(target)
}

func guardedRemoveAll(name string, deny []string) error {
	target, err := writeTarget(name, deny, true)
	if err != nil {
		return err
	}
	return os.RemoveAll(target)
}

func guardedRename(oldname, newname string, deny []string) error {
	oldTarget, err := writeTarget(oldname, deny, true)
	if err != nil {
		return err
	}
	newTarget, err := writeTarget(newname, deny, true)
	if err != nil {
		return err
	}
	return os.Rename(oldTarget, newTarget)
}

func guardedOpenFile(name string, flag int, perm os.FileMode, deny []string, readable bool) (*os.File, error) {
	target, err := writeTarget(name, deny, false)
	if err != nil {
		return nil, err
	}
	if !readable && flag&(os.O_WRONLY|os.O_RDWR) != os.O_WRONLY {
		return nil, fmt.Errorf("open %s: reading files needs the fs:read permission (use os.O_WRONLY)", name)
	}
	if readable && flag&os.O_WRONLY == 0 {
		if _, err := readTarget(name); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(target, flag, perm)
}

func guardedChmod(name string, mode os.FileMode, deny []string) error {
	target, err := writeTarget(name, deny, false)
	if err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

func guardedChtimes(name string, atime, mtime time.Time, deny []string) error {
	target, err := writeTarget(name, deny, false)
	if err != nil {
		return err
	}
	return os.Chtimes(target, atime, mtime)
}

func guardedTruncate(name string, size int64, deny []string) error {
	target, err := writeTarget(name, deny, false)
	if err != nil {
		return err
	}
	return os.Truncate(target, size)
}

func guardedIoutilWriteFile(name string, data []byte, perm os.FileMode, deny []string) error {
	return guardedWriteFile(name, data, perm, deny)
}

func guardedIoutilTempFile(dir, pattern string, deny []string, readable bool) (*os.File, error) {
	return guardedCreateTemp(dir, pattern, deny, readable)
}

func guardedIoutilTempDir(dir, pattern string, deny []string) (string, error) {
	return guardedMkdirTemp(dir, pattern, deny)
}

// envView is the environment an env-only plugin sees: secret-looking
// names are hidden even if the interpreter's Env were not filtered.
func envViewEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !SecretLike(name) {
			out = append(out, kv)
		}
	}
	return out
}

func envViewGetenv(name string) string {
	if SecretLike(name) {
		return ""
	}
	return os.Getenv(name)
}

func envViewLookupEnv(name string) (string, bool) {
	if SecretLike(name) {
		return "", false
	}
	return os.LookupEnv(name)
}

func envViewExpandEnv(s string) string {
	return os.Expand(s, func(name string) string {
		if SecretLike(name) {
			return ""
		}
		return os.Getenv(name)
	})
}

// guardOSSymbols replaces the os mutators with deny-checked wrappers and
// hides secret-looking environment names from env-only plugins.
func guardOSSymbols(filtered map[string]reflect.Value, has map[string]bool, deny []string) {
	if has[PermEnv] && !has[PermEnvSecrets] {
		setFiltered(filtered, "Getenv", envViewGetenv)
		setFiltered(filtered, "LookupEnv", envViewLookupEnv)
		setFiltered(filtered, "Environ", envViewEnviron)
		setFiltered(filtered, "ExpandEnv", envViewExpandEnv)
	}
	if has[PermFSRead] {
		setFiltered(filtered, "Open", guardedOpen)
		setFiltered(filtered, "ReadFile", guardedReadFile)
		setFiltered(filtered, "ReadDir", guardedReadDir)
		setFiltered(filtered, "Stat", guardedStat)
		setFiltered(filtered, "Lstat", guardedLstat)
		setFiltered(filtered, "Readlink", guardedReadlink)
		setFiltered(filtered, "DirFS", guardedDirFS)
	}
	if !has[PermFSWrite] {
		return
	}
	readable := has[PermFSRead]
	setFiltered(filtered, "Create", func(name string) (*os.File, error) {
		return guardedCreate(name, deny, readable)
	})
	setFiltered(filtered, "CreateTemp", func(dir, pattern string) (*os.File, error) {
		return guardedCreateTemp(dir, pattern, deny, readable)
	})
	setFiltered(filtered, "WriteFile", func(name string, data []byte, perm os.FileMode) error {
		return guardedWriteFile(name, data, perm, deny)
	})
	setFiltered(filtered, "Mkdir", func(name string, perm os.FileMode) error {
		return guardedMkdir(name, perm, deny)
	})
	setFiltered(filtered, "MkdirAll", func(name string, perm os.FileMode) error {
		return guardedMkdirAll(name, perm, deny)
	})
	setFiltered(filtered, "MkdirTemp", func(dir, pattern string) (string, error) {
		return guardedMkdirTemp(dir, pattern, deny)
	})
	setFiltered(filtered, "Remove", func(name string) error { return guardedRemove(name, deny) })
	setFiltered(filtered, "RemoveAll", func(name string) error { return guardedRemoveAll(name, deny) })
	setFiltered(filtered, "Rename", func(oldname, newname string) error {
		return guardedRename(oldname, newname, deny)
	})
	setFiltered(filtered, "OpenFile", func(name string, flag int, perm os.FileMode) (*os.File, error) {
		return guardedOpenFile(name, flag, perm, deny, readable)
	})
	setFiltered(filtered, "Chmod", func(name string, mode os.FileMode) error {
		return guardedChmod(name, mode, deny)
	})
	setFiltered(filtered, "Chtimes", func(name string, atime, mtime time.Time) error {
		return guardedChtimes(name, atime, mtime, deny)
	})
	setFiltered(filtered, "Truncate", func(name string, size int64) error {
		return guardedTruncate(name, size, deny)
	})
}

func guardIoutilSymbols(out map[string]map[string]reflect.Value, has map[string]bool, deny []string) {
	syms, ok := out["io/ioutil/ioutil"]
	if ok && has[PermFSRead] {
		cp := make(map[string]reflect.Value, len(syms))
		for k, v := range syms {
			cp[k] = v
		}
		if _, ok := cp["ReadFile"]; ok {
			cp["ReadFile"] = reflect.ValueOf(guardedReadFile)
		}
		if _, ok := cp["ReadDir"]; ok {
			cp["ReadDir"] = reflect.ValueOf(func(name string) ([]os.FileInfo, error) {
				ents, err := guardedReadDir(name)
				if err != nil {
					return nil, err
				}
				var out []os.FileInfo
				for _, e := range ents {
					info, err := e.Info()
					if err != nil {
						return nil, err
					}
					out = append(out, info)
				}
				return out, nil
			})
		}
		out["io/ioutil/ioutil"] = cp
		syms = cp
	}
	if !has[PermFSWrite] {
		return
	}
	if !ok {
		return
	}
	cp := make(map[string]reflect.Value, len(syms))
	for k, v := range syms {
		cp[k] = v
	}
	readable := has[PermFSRead]
	if _, ok := cp["WriteFile"]; ok {
		cp["WriteFile"] = reflect.ValueOf(func(name string, data []byte, perm os.FileMode) error {
			return guardedIoutilWriteFile(name, data, perm, deny)
		})
	}
	if _, ok := cp["TempFile"]; ok {
		cp["TempFile"] = reflect.ValueOf(func(dir, pattern string) (*os.File, error) {
			return guardedIoutilTempFile(dir, pattern, deny, readable)
		})
	}
	if _, ok := cp["TempDir"]; ok {
		cp["TempDir"] = reflect.ValueOf(func(dir, pattern string) (string, error) {
			return guardedIoutilTempDir(dir, pattern, deny)
		})
	}
	out["io/ioutil/ioutil"] = cp
}

func setFiltered(m map[string]reflect.Value, name string, fn any) {
	if _, ok := m[name]; ok {
		m[name] = reflect.ValueOf(fn)
	}
}

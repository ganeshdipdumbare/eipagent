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

// resolveWritePath follows symlinks as far as the path exists, so a
// check can't be fooled by a link to a protected file or directory.
func resolveWritePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	abs = filepath.Clean(abs)
	existing, rest := abs, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		real = existing
	}
	return filepath.Join(real, rest)
}

// checkWritePath refuses paths under /proc, /sys, /dev and the paths
// holding the agent's own configuration and plugin store.
func checkWritePath(p string, deny []string) error {
	target := resolveWritePath(p)
	for _, root := range protectedWriteDirs {
		if target == root || strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("os: refusing to write %s: protected system path", p)
		}
	}
	for _, d := range deny {
		if d == "" {
			continue
		}
		root := resolveWritePath(d)
		if target == root || strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("os: refusing to write %s: protected agent path", p)
		}
	}
	return nil
}

// The wrappers below are os symbols for plugins holding fs:write. The
// write-only variants are for plugins without fs:read: they never hand
// the plugin a readable handle.

func guardedCreate(name string, deny []string, readable bool) (*os.File, error) {
	if err := checkWritePath(name, deny); err != nil {
		return nil, err
	}
	if readable {
		return os.Create(name)
	}
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
}

func guardedCreateTemp(dir, pattern string, deny []string, readable bool) (*os.File, error) {
	d := dir
	if d == "" {
		d = os.TempDir()
	}
	if err := checkWritePath(d, deny); err != nil {
		return nil, err
	}
	if readable {
		return os.CreateTemp(dir, pattern)
	}
	f, err := os.CreateTemp(dir, pattern)
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
	if err := checkWritePath(name, deny); err != nil {
		return err
	}
	return os.WriteFile(name, data, perm)
}

func guardedMkdir(name string, perm os.FileMode, deny []string) error {
	if err := checkWritePath(name, deny); err != nil {
		return err
	}
	return os.Mkdir(name, perm)
}

func guardedMkdirAll(name string, perm os.FileMode, deny []string) error {
	if err := checkWritePath(name, deny); err != nil {
		return err
	}
	return os.MkdirAll(name, perm)
}

func guardedMkdirTemp(dir, pattern string, deny []string) (string, error) {
	d := dir
	if d == "" {
		d = os.TempDir()
	}
	if err := checkWritePath(d, deny); err != nil {
		return "", err
	}
	return os.MkdirTemp(dir, pattern)
}

func guardedRemove(name string, deny []string) error {
	if err := checkWritePath(name, deny); err != nil {
		return err
	}
	return os.Remove(name)
}

func guardedRemoveAll(name string, deny []string) error {
	if err := checkWritePath(name, deny); err != nil {
		return err
	}
	return os.RemoveAll(name)
}

func guardedRename(oldname, newname string, deny []string) error {
	if err := checkWritePath(oldname, deny); err != nil {
		return err
	}
	if err := checkWritePath(newname, deny); err != nil {
		return err
	}
	return os.Rename(oldname, newname)
}

func guardedOpenFile(name string, flag int, perm os.FileMode, deny []string, readable bool) (*os.File, error) {
	if err := checkWritePath(name, deny); err != nil {
		return nil, err
	}
	if !readable && flag&(os.O_WRONLY|os.O_RDWR) != os.O_WRONLY {
		return nil, fmt.Errorf("open %s: reading files needs the fs:read permission (use os.O_WRONLY)", name)
	}
	return os.OpenFile(name, flag, perm)
}

func guardedChmod(name string, mode os.FileMode, deny []string) error {
	if err := checkWritePath(name, deny); err != nil {
		return err
	}
	return os.Chmod(name, mode)
}

func guardedChtimes(name string, atime, mtime time.Time, deny []string) error {
	if err := checkWritePath(name, deny); err != nil {
		return err
	}
	return os.Chtimes(name, atime, mtime)
}

func guardedTruncate(name string, size int64, deny []string) error {
	if err := checkWritePath(name, deny); err != nil {
		return err
	}
	return os.Truncate(name, size)
}

func guardedCopyFS(dir string, fsys fs.FS, deny []string) error {
	if err := checkWritePath(dir, deny); err != nil {
		return err
	}
	return os.CopyFS(dir, fsys)
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
	setFiltered(filtered, "CopyFS", func(dir string, fsys fs.FS) error {
		return guardedCopyFS(dir, fsys, deny)
	})
}

func guardIoutilSymbols(out map[string]map[string]reflect.Value, has map[string]bool, deny []string) {
	if !has[PermFSWrite] {
		return
	}
	syms, ok := out["io/ioutil/ioutil"]
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

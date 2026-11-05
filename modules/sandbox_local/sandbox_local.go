package sandbox_local

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Host "sandbox": commands run as host processes inside a workdir, and
// file access is confined to that workdir. Provided as "sandbox/local".
//
// A local sandbox is NOT an isolation boundary (Isolation "none"): its
// shell runs /bin/sh as the agent's own user, which can reach any host
// file by absolute path. Use it for development, or when the whole agent
// already runs inside a sandbox.
//
// Config: root (default ".kiln/sandboxes"), workdir (use one fixed
// directory instead of one per sandbox: every thread then shares it and
// its files outlive sandbox_destroy, so use it only for a single user's
// own workspace), env_allow (["PATH","HOME","LANG","TMPDIR"]): host
// variables passed to commands. Credential-looking names are refused: a
// secret value must be passed by a secret:// reference, never inherited
// from the host environment. The env_secrets option was removed; setting
// it to true is an error.
//
// Example:
//
//	kiln.WithModuleConfig("sandbox_local", map[string]any{"workdir": "."}) // no isolation: this project folder
//	kiln.WithModules("workspace_local", "tool_shell", "tool_fs")
var Permissions = []string{"exec", "fs:read", "fs:write", "env"}

func Apply(p *sdk.Module) error {
	if p.ConfigBool("env_secrets", false) {
		return errors.New("sandbox_local: env_secrets was removed; secret values must be passed by secret:// references, not inherited from the host environment")
	}
	root := p.ConfigString("root", ".kiln/sandboxes")
	fixed := p.ConfigString("workdir", "")
	if fixed != "" {
		p.Logf("sandbox_local: every sandbox shares %s (fixed workdir); threads can see each other's files", fixed)
	}
	allow := p.ConfigStrings("env_allow")
	if len(allow) == 0 {
		allow = []string{"PATH", "HOME", "LANG", "TMPDIR"}
	}
	// The shell is the only program this sandbox may start.
	shell, err := p.Exec("/bin/sh")
	if err != nil {
		return err
	}
	baseEnv := map[string]string{}
	for _, k := range allow {
		if sdk.SecretLike(k) {
			return fmt.Errorf("sandbox_local: env_allow %s looks like a credential; secret values must be passed by secret:// references, not inherited from the host environment", k)
		}
		v, err := p.Env(k)
		if err != nil {
			return err
		}
		if v != "" {
			baseEnv[k] = v
		}
	}
	return p.Provide("sandbox/local", &sdk.SandboxProvider{
		Name:      "local",
		Isolation: "none",
		Create: func(c context.Context, spec sdk.SandboxSpec) (*sdk.Sandbox, error) {
			if spec.Network.Mode == "none" || spec.Network.Mode == "allowlist" {
				p.Logf("warning: sandbox/local cannot restrict network (requested %q)", spec.Network.Mode)
			}
			dir := fixed
			id := "local"
			if dir == "" {
				b := make([]byte, 6)
				rand.Read(b)
				id = "local-" + hex.EncodeToString(b)
				dir = filepath.Join(root, id)
			}
			if err := ensureDir(p, dir, 0o700); err != nil {
				return nil, err
			}
			d, err := p.Dir(dir, true)
			if err != nil {
				return nil, err
			}
			return box(id, d, p, shell, baseEnv), nil
		},
		Attach: func(c context.Context, id string) (*sdk.Sandbox, error) {
			dir := fixed
			if id != "local" {
				// ids come back from saved threads: only names this module
				// makes ("local-<hex>"), never "..", separators or absolute paths.
				if !validBoxID(id) {
					return nil, fmt.Errorf("sandbox %s: invalid id", id)
				}
				dir = filepath.Join(root, id)
			}
			// An empty dir would open the process's working directory.
			if dir == "" {
				return nil, fmt.Errorf("sandbox %s not found", id)
			}
			d, err := p.Dir(dir, true)
			if err != nil {
				return nil, fmt.Errorf("sandbox %s not found", id)
			}
			return box(id, d, p, shell, baseEnv), nil
		},
	})
}

func validBoxID(id string) bool {
	if !strings.HasPrefix(id, "local-") || len(id) > 80 {
		return false
	}
	for _, r := range id[len("local-"):] {
		if !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9') {
			return false
		}
	}
	return len(id) > len("local-")
}

// ensureDir creates dir and any missing parents with perm, like os.MkdirAll.
// The broker only opens existing directories, so each missing level is
// created inside its existing parent.
func ensureDir(p *sdk.Module, dir string, perm fs.FileMode) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	d, err := p.Dir(abs, true)
	if err == nil {
		return d.Close()
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return err
	}
	if err := ensureDir(p, parent, perm); err != nil {
		return err
	}
	pd, err := p.Dir(parent, true)
	if err != nil {
		return err
	}
	defer pd.Close()
	return pd.MkdirAll(filepath.Base(abs), perm)
}

// box runs sandbox operations on d, the sandbox's directory from p.Dir:
// every file operation goes through d, which refuses paths and symlinks
// that leave the directory. Host paths (Upload, Download) go through p.Dir
// on their own parent directory.
func box(id string, d *sdk.Dir, p *sdk.Module, shell *sdk.Exec, baseEnv map[string]string) *sdk.Sandbox {
	abs := d.Name()
	// rel turns a path (absolute, or relative to the root) into a name
	// under the root. Symlinks that leave the root are refused by d.
	rel := func(fpath string) (string, error) {
		if !filepath.IsAbs(fpath) {
			fpath = filepath.Join(abs, fpath)
		}
		fpath = filepath.Clean(fpath)
		if fpath != abs && !strings.HasPrefix(fpath, abs+string(filepath.Separator)) {
			return "", fmt.Errorf("path %s is outside the sandbox", fpath)
		}
		return filepath.Rel(abs, fpath)
	}
	// command builds the shell invocation and its options for one request.
	command := func(req sdk.ExecRequest) (string, []string, sdk.ProcOptions, error) {
		o := sdk.ProcOptions{Dir: abs, Env: map[string]string{}}
		if req.Dir != "" {
			r, err := rel(req.Dir)
			if err != nil {
				return "", nil, o, err
			}
			st, err := d.Stat(r)
			if err != nil || !st.IsDir() {
				return "", nil, o, fmt.Errorf("sandbox_local: %s is not a directory in the sandbox", req.Dir)
			}
			o.Dir = filepath.Join(abs, r)
		}
		for k, v := range baseEnv {
			o.Env[k] = v
		}
		for k, v := range req.Env {
			o.Env[k] = v
		}
		switch {
		case req.Shell != "":
			return "/bin/sh", []string{"-c", req.Shell}, o, nil
		case req.Cmd != "":
			// sh execs the program with the arguments exactly as given.
			return "/bin/sh", append([]string{"-c", `exec "$0" "$@"`, req.Cmd}, req.Args...), o, nil
		}
		return "", nil, o, errors.New("sandbox_local: nothing to run (set Shell or Cmd)")
	}
	return &sdk.Sandbox{
		ID: id, Provider: "local", Isolation: "none",
		Exec: func(c context.Context, req sdk.ExecRequest) (*sdk.ExecResult, error) {
			if req.TimeoutSeconds > 0 {
				var cancel context.CancelFunc
				c, cancel = context.WithTimeout(c, time.Duration(req.TimeoutSeconds)*time.Second)
				defer cancel()
			}
			prog, args, o, err := command(req)
			if err != nil {
				return nil, err
			}
			o.Stdin = req.Stdin
			t := time.Now()
			r, err := shell.Run(c, prog, args, o)
			res := &sdk.ExecResult{Stdout: string(r.Stdout), Stderr: string(r.Stderr), ExitCode: r.ExitCode, DurationMS: time.Since(t).Milliseconds()}
			if err == nil && r.ExitCode != 0 && c.Err() != nil {
				res.Stderr += "\n(timed out)"
			}
			return res, err
		},
		Start: func(c context.Context, req sdk.ExecRequest) (*sdk.Process, error) {
			prog, args, o, err := command(req)
			if err != nil {
				return nil, err
			}
			proc, err := shell.Start(c, prog, args, o)
			if err != nil {
				return nil, err
			}
			return &sdk.Process{Stdin: proc.Stdin, Stdout: proc.Stdout, Stderr: proc.Stderr,
				Wait: func() (int, error) {
					err := proc.Wait()
					if code, ok := sdk.ExitStatus(err); ok {
						return code, nil
					}
					return 0, err
				},
				Kill: proc.Kill}, nil
		},
		ReadFile: func(c context.Context, fpath string) ([]byte, error) {
			r, err := rel(fpath)
			if err != nil {
				return nil, err
			}
			return d.ReadFile(r)
		},
		WriteFile: func(c context.Context, fpath string, data []byte) error {
			r, err := rel(fpath)
			if err != nil {
				return err
			}
			if err := d.MkdirAll(filepath.Dir(r), 0o755); err != nil {
				return err
			}
			return d.WriteFile(r, data, 0o644)
		},
		ListDir: func(c context.Context, fpath string) ([]sdk.FileInfo, error) {
			r, err := rel(fpath)
			if err != nil {
				return nil, err
			}
			ents, err := d.ReadDir(r)
			if err != nil {
				return nil, err
			}
			var out []sdk.FileInfo
			for _, e := range ents {
				fi := sdk.FileInfo{Name: e.Name(), IsDir: e.IsDir()}
				if info, err := e.Info(); err == nil {
					fi.Size = info.Size()
				}
				out = append(out, fi)
			}
			return out, nil
		},
		Remove: func(c context.Context, fpath string) error {
			r, err := rel(fpath)
			if err != nil {
				return err
			}
			if r == "." {
				return errors.New("refusing to remove the sandbox root")
			}
			if id == "local" {
				// The fixed workdir is shared: never remove the root through
				// a path that resolves back to it.
				if same, err := d.SameFile(".", r); err == nil && same {
					return errors.New("refusing to remove the shared sandbox root")
				}
				if fi, err := d.Lstat(r); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
					return errors.New("refusing to remove a symlink in the shared workdir")
				}
			}
			return removeTree(d, r)
		},
		Upload: func(c context.Context, hostPath, boxPath string) error {
			r, err := rel(boxPath)
			if err != nil {
				return err
			}
			// Host symlinks are followed, as os.ReadFile did; the resolved
			// file is then read through a broker directory of its own.
			resolved, err := filepath.EvalSymlinks(hostPath)
			if err != nil {
				return err
			}
			hd, err := p.Dir(filepath.Dir(resolved), false)
			if err != nil {
				return err
			}
			defer hd.Close()
			name := filepath.Base(resolved)
			st, err := hd.Stat(name)
			if err != nil {
				return err
			}
			data, err := hd.ReadFile(name)
			if err != nil {
				return err
			}
			if err := d.MkdirAll(filepath.Dir(r), 0o755); err != nil {
				return err
			}
			return d.WriteFile(r, data, st.Mode().Perm())
		},
		Download: func(c context.Context, boxPath, hostPath string) error {
			r, err := rel(boxPath)
			if err != nil {
				return err
			}
			in, err := d.Open(r)
			if err != nil {
				return err
			}
			defer in.Close()
			st, err := in.Stat()
			if err != nil {
				return err
			}
			dst := hostPath
			if resolved, err := filepath.EvalSymlinks(hostPath); err == nil {
				dst = resolved // write through a host symlink, as os.OpenFile did
			}
			_ = ensureDir(p, filepath.Dir(dst), 0o755) // a failure surfaces at open below
			hd, err := p.Dir(filepath.Dir(dst), true)
			if err != nil {
				return err
			}
			defer hd.Close()
			name := filepath.Base(dst)
			// Truncate, then stream: the broker has no create-and-write
			// handle, so WriteFile empties the file and AppendFile fills it.
			if err := hd.WriteFile(name, nil, st.Mode().Perm()); err != nil {
				return err
			}
			out, err := hd.AppendFile(name, st.Mode().Perm())
			if err != nil {
				return err
			}
			_, err = io.Copy(out, in)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			return err
		},
		Close: func(c context.Context) error {
			if id == "local" {
				return d.Close() // fixed workdir: keep the user's files
			}
			defer d.Close()
			ents, err := d.ReadDir(".")
			if err != nil {
				return err
			}
			for _, e := range ents {
				if err := removeTree(d, e.Name()); err != nil {
					return err
				}
			}
			// The root itself is removed through its parent.
			parent, err := p.Dir(filepath.Dir(abs), true)
			if err != nil {
				return err
			}
			defer parent.Close()
			return parent.Remove(filepath.Base(abs))
		},
	}
}

// removeTree deletes name under d, recursively. A symlink is removed as a
// link and never followed.
func removeTree(d *sdk.Dir, name string) error {
	err := d.Remove(name)
	// fs.ErrExist matches ENOTEMPTY (a non-empty directory) on Unix.
	if err == nil || !errors.Is(err, fs.ErrExist) {
		return err
	}
	ents, err := d.ReadDir(name)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := removeTree(d, filepath.Join(name, e.Name())); err != nil {
			return err
		}
	}
	return d.Remove(name)
}

// Module is the compiled form of "sandbox_local". modules/std registers it.
var Module = engine.Module{Name: "sandbox_local", Requires: nil, Provides: []string{"sandbox/local"}, Permissions: Permissions, Apply: Apply}

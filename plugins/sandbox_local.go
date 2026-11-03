//go:build ignore

package plugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"eipagent/sdk"
)

// Host "sandbox": commands run as host processes inside a workdir, file
// access is confined to that workdir, and only allowlisted env vars are
// passed. NO isolation (Isolation "none"): for development, or when the
// whole agent already runs inside a sandbox. Provided as "sandbox/local".
//
// Config: root (default ".eipagent/sandboxes"), workdir (use one fixed
// directory instead of one per sandbox: every thread then shares it and
// its files outlive sandbox_destroy, so use it only for a single user's
// own workspace), env_allow (["PATH","HOME","LANG"]; secret-looking
// names are never passed without the env:secrets permission).
//
// Example:
//
//	eipagent.WithPlugin("sandbox_local", map[string]any{"workdir": "."}) // no isolation: this project folder
//	eipagent.WithPlugins("workspace_local", "tool_shell", "tool_fs")
var Permissions = []string{"exec", "fs:read", "fs:write", "env"}

func Apply(p *sdk.Plugin) error {
	root := p.ConfigString("root", ".eipagent/sandboxes")
	fixed := p.ConfigString("workdir", "")
	if fixed != "" {
		p.Logf("sandbox_local: every sandbox shares %s (fixed workdir); threads can see each other's files", fixed)
	}
	allow := p.ConfigStrings("env_allow")
	if len(allow) == 0 {
		allow = []string{"PATH", "HOME", "LANG", "TMPDIR"}
	}
	baseEnv := []string{}
	for _, k := range allow {
		if v, ok := os.LookupEnv(k); ok {
			baseEnv = append(baseEnv, k+"="+v)
		}
	}
	open := func(id, dir string) *sdk.Sandbox { return box(id, dir, baseEnv) }
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
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			return open(id, dir), nil
		},
		Attach: func(c context.Context, id string) (*sdk.Sandbox, error) {
			dir := fixed
			if id != "local" {
				// ids come back from saved threads: only names this plugin
				// makes ("local-<hex>"), never "..", separators or absolute paths.
				if !validBoxID(id) {
					return nil, fmt.Errorf("sandbox %s: invalid id", id)
				}
				dir = filepath.Join(root, id)
			}
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return nil, fmt.Errorf("sandbox %s not found", id)
			}
			return open(id, dir), nil
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

func box(id, dir string, baseEnv []string) *sdk.Sandbox {
	abs, _ := filepath.Abs(dir)
	resolve := func(fpath string) (string, error) {
		if !filepath.IsAbs(fpath) {
			fpath = filepath.Join(abs, fpath)
		}
		fpath = filepath.Clean(fpath)
		if fpath != abs && !strings.HasPrefix(fpath, abs+string(filepath.Separator)) {
			return "", fmt.Errorf("path %s is outside the sandbox", fpath)
		}
		// Refuse symlinks that point outside. A path that doesn't exist yet
		// (a file about to be written) is checked through its deepest
		// existing parent, so a planted symlinked directory can't be used
		// to create files outside the sandbox.
		ra, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", err
		}
		existing, rest := fpath, ""
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
			return "", fmt.Errorf("path %s: %v", fpath, err)
		}
		real = filepath.Join(real, rest)
		if real != ra && !strings.HasPrefix(real, ra+string(filepath.Separator)) {
			return "", fmt.Errorf("path %s escapes the sandbox via a symlink", fpath)
		}
		return fpath, nil
	}
	command := func(c context.Context, req sdk.ExecRequest) (*exec.Cmd, error) {
		var cmd *exec.Cmd
		if req.Shell != "" {
			cmd = exec.CommandContext(c, "/bin/sh", "-c", req.Shell)
		} else {
			cmd = exec.CommandContext(c, req.Cmd, req.Args...)
		}
		cmd.Dir = abs
		if req.Dir != "" {
			d, err := resolve(req.Dir)
			if err != nil {
				return nil, err
			}
			cmd.Dir = d
		}
		cmd.Env = append([]string{}, baseEnv...)
		for k, v := range req.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		return cmd, nil
	}
	return &sdk.Sandbox{
		ID: id, Provider: "local", Isolation: "none",
		Exec: func(c context.Context, req sdk.ExecRequest) (*sdk.ExecResult, error) {
			if req.TimeoutSeconds > 0 {
				var cancel context.CancelFunc
				c, cancel = context.WithTimeout(c, time.Duration(req.TimeoutSeconds)*time.Second)
				defer cancel()
			}
			cmd, err := command(c, req)
			if err != nil {
				return nil, err
			}
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			if req.Stdin != nil {
				cmd.Stdin = bytes.NewReader(req.Stdin)
			}
			t := time.Now()
			err = cmd.Run()
			res := &sdk.ExecResult{Stdout: out.String(), Stderr: errb.String(), DurationMS: time.Since(t).Milliseconds()}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				res.ExitCode = ee.ExitCode()
				if c.Err() != nil {
					res.Stderr += "\n(timed out)"
				}
				return res, nil
			}
			return res, err
		},
		Start: func(c context.Context, req sdk.ExecRequest) (*sdk.Process, error) {
			cmd, err := command(c, req)
			if err != nil {
				return nil, err
			}
			in, err := cmd.StdinPipe()
			if err != nil {
				return nil, err
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				return nil, err
			}
			errp, err := cmd.StderrPipe()
			if err != nil {
				return nil, err
			}
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return &sdk.Process{Stdin: in, Stdout: out, Stderr: errp,
				Wait: func() (int, error) {
					err := cmd.Wait()
					var ee *exec.ExitError
					if errors.As(err, &ee) {
						return ee.ExitCode(), nil
					}
					return 0, err
				},
				Kill: func() error { return cmd.Process.Kill() }}, nil
		},
		ReadFile: func(c context.Context, fpath string) ([]byte, error) {
			r, err := resolve(fpath)
			if err != nil {
				return nil, err
			}
			return os.ReadFile(r)
		},
		WriteFile: func(c context.Context, fpath string, data []byte) error {
			r, err := resolve(fpath)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(r), 0o755); err != nil {
				return err
			}
			return os.WriteFile(r, data, 0o644)
		},
		ListDir: func(c context.Context, fpath string) ([]sdk.FileInfo, error) {
			r, err := resolve(fpath)
			if err != nil {
				return nil, err
			}
			ents, err := os.ReadDir(r)
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
			r, err := resolve(fpath)
			if err != nil {
				return err
			}
			if r == abs {
				return errors.New("refusing to remove the sandbox root")
			}
			return os.RemoveAll(r)
		},
		Upload: func(c context.Context, hostPath, boxPath string) error {
			r, err := resolve(boxPath)
			if err != nil {
				return err
			}
			return copyFile(hostPath, r)
		},
		Download: func(c context.Context, boxPath, hostPath string) error {
			r, err := resolve(boxPath)
			if err != nil {
				return err
			}
			return copyFile(r, hostPath)
		},
		Close: func(c context.Context) error {
			if id == "local" {
				return nil // fixed workdir: keep the user's files
			}
			return os.RemoveAll(abs)
		},
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

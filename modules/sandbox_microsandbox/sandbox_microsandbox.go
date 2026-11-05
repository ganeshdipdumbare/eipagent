package sandbox_microsandbox

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// childVars splits the configured child variables: a secret:// reference
// is resolved through the secrets service and goes to Secrets; any other
// value goes to Env. A credential-looking name must use a reference.
func childVars(p *sdk.Module, raw map[string]any) (env, secrets map[string]string, err error) {
	env, secrets = map[string]string{}, map[string]string{}
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, nil, fmt.Errorf("sandbox_microsandbox: env %s must be a string", k)
		}
		switch {
		case strings.HasPrefix(s, "secret://"):
			val, err := p.SecretValue(s)
			if err != nil {
				return nil, nil, fmt.Errorf("sandbox_microsandbox: env %s: %w", k, err)
			}
			secrets[k] = val
		case sdk.SecretLike(k):
			return nil, nil, fmt.Errorf("sandbox_microsandbox: env %s looks like a credential; set it to a secret:// reference", k)
		default:
			env[k] = s
		}
	}
	return env, secrets, nil
}

// MicroVM sandboxes through the microsandbox CLI (msb), provided as
// "sandbox/microsandbox". Each sandbox is its own lightweight VM with its
// own kernel, so it is a stronger boundary than a container. Sandboxes
// have no network by default; allowlist mode turns hosts into msb
// network rules.
//
// Config: binary ("msb"), image ("alpine"), name ("microsandbox"),
// extra_args ([]), env ({}): variables for the msb child. A value written
// as secret://NAME is resolved through the secrets service and passed as a
// secret; any other value is passed as is. A credential-looking name
// (MSB_API_KEY ...) must use a secret:// reference. The child gets only
// the listed variables plus a few system ones, not the host environment.
//
// To run on microsandbox cloud (private beta), set env to
// {"MSB_BACKEND": "cloud", "MSB_API_KEY": "secret://MSB_API_KEY"} (or
// "MSB_PROFILE" for a cloud profile). The same provider and msb CLI then
// create cloud sandboxes.
//
// Example:
//
//	kiln.WithModuleConfig("sandbox_microsandbox", map[string]any{"image": "python"})
//	kiln.WithModuleConfig("workspace_sandbox", map[string]any{"provider": "microsandbox"})
//	kiln.WithModules("tool_shell", "tool_fs")
var Permissions = []string{"exec"}

// Requires: the secrets service is optional, needed only for secret:// refs.
var Requires = []string{"?secrets"}

func Apply(p *sdk.Module) error {
	bin := p.ConfigString("binary", "msb")
	image := p.ConfigString("image", "alpine")
	name := p.ConfigString("name", "microsandbox")
	extra := p.ConfigStrings("extra_args")
	// Only the msb binary may run.
	x, err := p.Exec(bin)
	if err != nil {
		return err
	}
	env, secrets, err := childVars(p, p.ConfigMap("env"))
	if err != nil {
		return err
	}

	run := func(c context.Context, stdin []byte, args ...string) (string, string, int, error) {
		res, err := x.Run(c, bin, args, sdk.ProcOptions{Stdin: stdin, Env: env, Secrets: secrets})
		return string(res.Stdout), string(res.Stderr), res.ExitCode, err
	}
	start := func(c context.Context, args ...string) (*sdk.Process, error) {
		proc, err := x.Start(c, bin, args, sdk.ProcOptions{Env: env, Secrets: secrets})
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
	}
	return p.Provide("sandbox/"+name, &sdk.SandboxProvider{
		Name:      name,
		Isolation: "microvm",
		Create: func(c context.Context, spec sdk.SandboxSpec) (*sdk.Sandbox, error) {
			img := spec.Image
			if img == "" {
				img = image
			}
			wd := spec.Workdir
			if wd == "" {
				wd = "/workspace"
			}
			b := make([]byte, 6)
			rand.Read(b)
			id := "kiln-" + hex.EncodeToString(b)
			args := []string{"create", "-q", "-n", id, "--mkdir", wd, "-w", wd, "--label", "kiln=1"}
			switch spec.Network.Mode {
			case "", "none":
				args = append(args, "--no-net")
			case "allowlist":
				args = append(args, "--no-net")
				for _, h := range spec.Network.Allow {
					args = append(args, "--net-rule", "allow@"+h)
				}
			case "full":
			default:
				return nil, fmt.Errorf("sandbox_microsandbox: network mode %q is not supported (none, allowlist or full)", spec.Network.Mode)
			}
			if spec.CPUs > 0 {
				args = append(args, fmt.Sprintf("--cpus=%d", int(spec.CPUs+0.5)))
			}
			if spec.MemoryMB > 0 {
				args = append(args, fmt.Sprintf("--memory=%dM", spec.MemoryMB))
			}
			for _, m := range spec.Mounts {
				v := m.Host + ":" + m.Box
				if m.ReadOnly {
					v += ":ro"
				}
				args = append(args, "-v", v)
			}
			for k, v := range spec.Env {
				args = append(args, "-e", k+"="+v)
			}
			for k, v := range spec.Labels {
				args = append(args, "--label", k+"="+v)
			}
			if spec.TTLSeconds > 0 {
				args = append(args, fmt.Sprintf("--max-duration=%ds", spec.TTLSeconds))
			}
			args = append(append(args, extra...), "--", img)
			if _, errs, code, err := run(c, nil, args...); err != nil || code != 0 {
				return nil, fmt.Errorf("sandbox_microsandbox: %v %s", err, strings.TrimSpace(errs))
			}
			return vm(id, name, wd, run, start), nil
		},
		Attach: func(c context.Context, id string) (*sdk.Sandbox, error) {
			if !validID(id) { // ids come back from saved threads; never let one become a flag
				return nil, fmt.Errorf("sandbox_microsandbox: invalid sandbox name %q", id)
			}
			if _, _, code, err := run(c, nil, "inspect", "--", id); err != nil || code != 0 {
				return nil, fmt.Errorf("sandbox %s not found", id)
			}
			out, _, code, err := run(c, nil, "exec", "-q", "--no-stdin", id, "--", "pwd")
			if err != nil || code != 0 || strings.TrimSpace(out) == "" {
				return nil, fmt.Errorf("sandbox %s: cannot determine the workdir", id)
			}
			return vm(id, name, strings.TrimSpace(out), run, start), nil
		},
	})
}

// validID accepts microsandbox sandbox names, never flags.
func validID(id string) bool {
	if id == "" || len(id) > 128 || id[0] == '-' {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

type runner func(c context.Context, stdin []byte, args ...string) (string, string, int, error)

type starter func(c context.Context, args ...string) (*sdk.Process, error)

func vm(id, provider, wd string, run runner, start starter) *sdk.Sandbox {
	abs := func(p string) string {
		if strings.HasPrefix(p, "/") {
			return p
		}
		return wd + "/" + p // relative to the workdir, like Exec
	}
	execArgs := func(req sdk.ExecRequest, stream bool) []string {
		args := []string{"exec", "-q"}
		switch {
		case stream:
			args = append(args, "--stream")
		case req.Stdin == nil:
			args = append(args, "--no-stdin")
		default:
			args = append(args, "--no-tty")
		}
		if req.Dir != "" {
			args = append(args, "-w", req.Dir)
		}
		for k, v := range req.Env {
			args = append(args, "-e", k+"="+v)
		}
		args = append(args, id, "--")
		if req.Shell != "" {
			return append(args, "sh", "-c", req.Shell)
		}
		return append(append(args, req.Cmd), req.Args...)
	}
	sh := func(c context.Context, stdin []byte, script string, params ...string) (string, error) {
		mode := "--no-stdin"
		if stdin != nil {
			mode = "--no-tty"
		}
		args := append([]string{"exec", "-q", mode, id, "--", "sh", "-c", script, "_"}, params...)
		out, errs, code, err := run(c, stdin, args...)
		if err != nil {
			return "", err
		}
		if code != 0 {
			return "", errors.New(strings.TrimSpace(errs))
		}
		return out, nil
	}
	return &sdk.Sandbox{
		ID: id, Provider: provider, Isolation: "microvm",
		Exec: func(c context.Context, req sdk.ExecRequest) (*sdk.ExecResult, error) {
			if req.TimeoutSeconds > 0 {
				var cancel context.CancelFunc
				c, cancel = context.WithTimeout(c, time.Duration(req.TimeoutSeconds)*time.Second)
				defer cancel()
			}
			t := time.Now()
			out, errs, code, err := run(c, req.Stdin, execArgs(req, false)...)
			if err != nil && c.Err() != nil {
				return &sdk.ExecResult{Stdout: out, Stderr: errs + "\n(timed out)", ExitCode: 124}, nil
			}
			return &sdk.ExecResult{Stdout: out, Stderr: errs, ExitCode: code, DurationMS: time.Since(t).Milliseconds()}, err
		},
		Start: func(c context.Context, req sdk.ExecRequest) (*sdk.Process, error) {
			return start(c, execArgs(req, true)...)
		},
		ReadFile: func(c context.Context, fpath string) ([]byte, error) {
			out, err := sh(c, nil, `cat -- "$1"`, abs(fpath))
			return []byte(out), err
		},
		WriteFile: func(c context.Context, fpath string, data []byte) error {
			_, err := sh(c, data, `mkdir -p "$(dirname -- "$1")" && cat > "$1"`, abs(fpath))
			return err
		},
		ListDir: func(c context.Context, fpath string) ([]sdk.FileInfo, error) {
			out, err := sh(c, nil, `ls -1Ap -- "$1"`, abs(fpath))
			if err != nil {
				return nil, err
			}
			var fis []sdk.FileInfo
			for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
				if l == "" {
					continue
				}
				fis = append(fis, sdk.FileInfo{Name: strings.TrimSuffix(l, "/"), IsDir: strings.HasSuffix(l, "/")})
			}
			return fis, nil
		},
		Remove: func(c context.Context, fpath string) error {
			_, err := sh(c, nil, `rm -rf -- "$1"`, abs(fpath))
			return err
		},
		Upload: func(c context.Context, hostPath, boxPath string) error {
			boxPath = abs(boxPath)
			if _, err := sh(c, nil, `mkdir -p "$(dirname -- "$1")"`, boxPath); err != nil {
				return err
			}
			_, errs, code, err := run(c, nil, "copy", "-q", hostPath, id+":"+boxPath)
			if err == nil && code != 0 {
				err = errors.New(strings.TrimSpace(errs))
			}
			return err
		},
		Download: func(c context.Context, boxPath, hostPath string) error {
			_, errs, code, err := run(c, nil, "copy", "-q", id+":"+abs(boxPath), hostPath)
			if err == nil && code != 0 {
				err = errors.New(strings.TrimSpace(errs))
			}
			return err
		},
		Close: func(c context.Context) error {
			cc, cancel := context.WithTimeout(context.Background(), 30*time.Second) // a hung runtime must not block shutdown
			defer cancel()
			_, _, _, err := run(cc, nil, "rm", "-f", "-q", id)
			return err
		},
	}
}

// Module is the compiled form of "sandbox_microsandbox". modules/std registers it.
var Module = engine.Module{Name: "sandbox_microsandbox", Requires: Requires, Provides: []string{"sandbox/*"}, Permissions: Permissions, Apply: Apply}

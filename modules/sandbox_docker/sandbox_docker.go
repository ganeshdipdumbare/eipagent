package sandbox_docker

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Container sandboxes through the docker (or podman) CLI, provided as
// "sandbox/docker". Containers run with no network by default, a
// read-only root filesystem off, dropped capabilities and no new
// privileges.
//
// Config: binary ("docker"), image ("alpine:3"), name ("docker"),
// pids_limit (256), extra_args ([]).
//
// Example:
//
//	kiln.WithModuleConfig("sandbox_docker", map[string]any{"image": "python:3.12-slim", "pids_limit": 128})
//	kiln.WithModules("workspace_sandbox", "tool_shell", "tool_fs")
//	// Shortcut: kiln.WithSandbox("docker")
var Permissions = []string{"exec"}

func Apply(p *sdk.Module) error {
	bin := p.ConfigString("binary", "docker")
	image := p.ConfigString("image", "alpine:3")
	name := p.ConfigString("name", "docker")
	pids := p.ConfigInt("pids_limit", 256)
	extra := p.ConfigStrings("extra_args")
	// Only the configured CLI may run: the binary itself, nothing else.
	x, err := p.Exec(bin)
	if err != nil {
		return err
	}

	run := func(c context.Context, stdin []byte, args ...string) (string, string, int, error) {
		res, err := x.Run(c, bin, args, sdk.ProcOptions{Stdin: stdin})
		return string(res.Stdout), string(res.Stderr), res.ExitCode, err
	}
	start := func(c context.Context, args ...string) (*sdk.Process, error) {
		proc, err := x.Start(c, bin, args, sdk.ProcOptions{})
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
		Isolation: "container",
		Create: func(c context.Context, spec sdk.SandboxSpec) (*sdk.Sandbox, error) {
			img := spec.Image
			if img == "" {
				img = image
			}
			args := []string{"run", "-d", "--init", "--cap-drop=ALL", "--security-opt", "no-new-privileges", fmt.Sprintf("--pids-limit=%d", pids), "--label", "kiln=1"}
			switch spec.Network.Mode {
			case "", "none":
				args = append(args, "--network", "none")
			case "full":
			default:
				return nil, fmt.Errorf("sandbox_docker: network mode %q is not supported (use none or full, or a provider with egress filtering)", spec.Network.Mode)
			}
			if spec.CPUs > 0 {
				args = append(args, fmt.Sprintf("--cpus=%g", spec.CPUs))
			}
			if spec.MemoryMB > 0 {
				args = append(args, fmt.Sprintf("--memory=%dm", spec.MemoryMB))
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
			wd := spec.Workdir
			if wd == "" {
				wd = "/workspace"
			}
			args = append(args, "-w", wd)
			args = append(args, extra...)
			args = append(args, img, "sh", "-c", `mkdir -p -- "$1" && exec sleep infinity`, "_", wd)
			out, errs, code, err := run(c, nil, args...)
			if err != nil || code != 0 {
				return nil, fmt.Errorf("sandbox_docker: %v %s", err, strings.TrimSpace(errs))
			}
			id := strings.TrimSpace(out)
			if i := strings.LastIndex(id, "\n"); i >= 0 { // warnings may precede the id
				id = strings.TrimSpace(id[i+1:])
			}
			if len(id) > 12 {
				id = id[:12]
			}
			if !validID(id) {
				return nil, fmt.Errorf("sandbox_docker: unexpected container id %q", id)
			}
			return container(id, name, wd, run, start), nil
		},
		Attach: func(c context.Context, id string) (*sdk.Sandbox, error) {
			if !validID(id) { // ids come back from saved threads; never let one become a flag
				return nil, fmt.Errorf("sandbox_docker: invalid container id %q", id)
			}
			out, _, code, err := run(c, nil, "inspect", "-f", "{{.Config.WorkingDir}}", "--", id)
			if err != nil || code != 0 {
				return nil, fmt.Errorf("sandbox %s not found", id)
			}
			return container(id, name, strings.TrimSpace(out), run, start), nil
		},
	})
}

// validID accepts docker/podman container ids and names, never flags.
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

func container(id, provider, wd string, run runner, start starter) *sdk.Sandbox {
	execArgs := func(req sdk.ExecRequest) []string {
		args := []string{"exec", "-i"}
		if req.Dir != "" {
			args = append(args, "-w", req.Dir)
		}
		for k, v := range req.Env {
			args = append(args, "-e", k+"="+v)
		}
		args = append(args, id)
		if req.Shell != "" {
			return append(args, "sh", "-c", req.Shell)
		}
		return append(append(args, req.Cmd), req.Args...)
	}
	sh := func(c context.Context, stdin []byte, script string, params ...string) (string, error) {
		args := append([]string{"exec", "-i", id, "sh", "-c", script, "_"}, params...)
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
		ID: id, Provider: provider, Isolation: "container",
		Exec: func(c context.Context, req sdk.ExecRequest) (*sdk.ExecResult, error) {
			if req.TimeoutSeconds > 0 {
				var cancel context.CancelFunc
				c, cancel = context.WithTimeout(c, time.Duration(req.TimeoutSeconds)*time.Second)
				defer cancel()
			}
			t := time.Now()
			out, errs, code, err := run(c, req.Stdin, execArgs(req)...)
			if err != nil && c.Err() != nil {
				return &sdk.ExecResult{Stdout: out, Stderr: errs + "\n(timed out)", ExitCode: 124}, nil
			}
			return &sdk.ExecResult{Stdout: out, Stderr: errs, ExitCode: code, DurationMS: time.Since(t).Milliseconds()}, err
		},
		Start: func(c context.Context, req sdk.ExecRequest) (*sdk.Process, error) {
			return start(c, execArgs(req)...)
		},
		ReadFile: func(c context.Context, fpath string) ([]byte, error) {
			out, err := sh(c, nil, `cat -- "$1"`, fpath)
			return []byte(out), err
		},
		WriteFile: func(c context.Context, fpath string, data []byte) error {
			_, err := sh(c, data, `mkdir -p "$(dirname -- "$1")" && cat > "$1"`, fpath)
			return err
		},
		ListDir: func(c context.Context, fpath string) ([]sdk.FileInfo, error) {
			out, err := sh(c, nil, `ls -1Ap -- "$1"`, fpath)
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
			_, err := sh(c, nil, `rm -rf -- "$1"`, fpath)
			return err
		},
		Upload: func(c context.Context, hostPath, boxPath string) error {
			if !strings.HasPrefix(boxPath, "/") {
				boxPath = wd + "/" + boxPath // relative to the workdir, like Exec
			}
			if _, err := sh(c, nil, `mkdir -p "$(dirname -- "$1")"`, boxPath); err != nil {
				return err
			}
			_, errs, code, err := run(c, nil, "cp", hostPath, id+":"+boxPath)
			if err == nil && code != 0 {
				err = errors.New(errs)
			}
			return err
		},
		Download: func(c context.Context, boxPath, hostPath string) error {
			if !strings.HasPrefix(boxPath, "/") {
				boxPath = wd + "/" + boxPath
			}
			_, errs, code, err := run(c, nil, "cp", id+":"+boxPath, hostPath)
			if err == nil && code != 0 {
				err = errors.New(errs)
			}
			return err
		},
		Close: func(c context.Context) error {
			cc, cancel := context.WithTimeout(context.Background(), 30*time.Second) // a hung daemon must not block shutdown
			defer cancel()
			_, _, _, err := run(cc, nil, "rm", "-f", "--", id)
			return err
		},
	}
}

// Module is the compiled form of "sandbox_docker". modules/std registers it.
var Module = engine.Module{Name: "sandbox_docker", Requires: nil, Provides: []string{"sandbox/*"}, Permissions: Permissions, Apply: Apply}

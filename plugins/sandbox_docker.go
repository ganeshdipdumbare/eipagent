//go:build ignore

package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"eipagent/sdk"
)

// Container sandboxes through the docker (or podman) CLI, provided as
// "sandbox/docker". Containers run with no network by default, a
// read-only root filesystem off, dropped capabilities and no new
// privileges.
//
// Config: binary ("docker"), image ("alpine:3"), name ("docker"),
// pids_limit (256), extra_args ([]).
var Permissions = []string{"exec"}

func Apply(ctx *sdk.Context) error {
	bin := ctx.ConfigString("binary", "docker")
	image := ctx.ConfigString("image", "alpine:3")
	name := ctx.ConfigString("name", "docker")
	pids := ctx.ConfigInt("pids_limit", 256)
	extra := ctx.ConfigStrings("extra_args")

	run := func(c context.Context, stdin []byte, args ...string) (string, string, int, error) {
		cmd := exec.CommandContext(c, bin, args...)
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.String(), errb.String(), ee.ExitCode(), nil
		}
		return out.String(), errb.String(), 0, err
	}
	start := func(c context.Context, args ...string) (*sdk.Process, error) {
		cmd := exec.CommandContext(c, bin, args...)
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		errp, _ := cmd.StderrPipe()
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
	}
	return ctx.Provide("sandbox/"+name, &sdk.SandboxProvider{
		Name:      name,
		Isolation: "container",
		Create: func(c context.Context, spec sdk.SandboxSpec) (*sdk.Sandbox, error) {
			img := spec.Image
			if img == "" {
				img = image
			}
			args := []string{"run", "-d", "--init", "--cap-drop=ALL", "--security-opt", "no-new-privileges", fmt.Sprintf("--pids-limit=%d", pids), "--label", "eipagent=1"}
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
			args = append(args, img, "sh", "-c", "mkdir -p "+wd+" && exec sleep infinity")
			out, errs, code, err := run(c, nil, args...)
			if err != nil || code != 0 {
				return nil, fmt.Errorf("sandbox_docker: %v %s", err, strings.TrimSpace(errs))
			}
			return container(strings.TrimSpace(out)[:12], name, wd, run, start), nil
		},
		Attach: func(c context.Context, id string) (*sdk.Sandbox, error) {
			out, _, code, err := run(c, nil, "inspect", "-f", "{{.Config.WorkingDir}}", id)
			if err != nil || code != 0 {
				return nil, fmt.Errorf("sandbox %s not found", id)
			}
			return container(id, name, strings.TrimSpace(out), run, start), nil
		},
	})
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
		ReadFile: func(c context.Context, p string) ([]byte, error) {
			out, err := sh(c, nil, `cat -- "$1"`, p)
			return []byte(out), err
		},
		WriteFile: func(c context.Context, p string, data []byte) error {
			_, err := sh(c, data, `mkdir -p "$(dirname -- "$1")" && cat > "$1"`, p)
			return err
		},
		ListDir: func(c context.Context, p string) ([]sdk.FileInfo, error) {
			out, err := sh(c, nil, `ls -1Ap -- "$1"`, p)
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
		Remove: func(c context.Context, p string) error {
			_, err := sh(c, nil, `rm -rf -- "$1"`, p)
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
			_, _, _, err := run(context.Background(), nil, "rm", "-f", id)
			return err
		},
	}
}

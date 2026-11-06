package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sixt/tensorlake-go"

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// tensorlake is a sandbox provider for Tensorlake cloud sandboxes. The
// host process stays local (or on your server); the agent, modules and tools
// run in the cloud sandbox.
//
// It reads TENSORLAKE_API_KEY from the host environment and never sends it
// into the sandbox. Host files (the sandbox binary, uploads) go through
// p.Dir, and variables through p.Env, so the module imports no os package.
func tensorlakeProvider() engine.Module {
	return engine.Module{
		Name:        "sandbox_tensorlake",
		Provides:    []string{"sandbox/tensorlake"},
		Permissions: []string{"env", "fs:read", "fs:write"},
		Apply: func(p *sdk.Scope) error {
			opts := []tensorlake.Option{}
			if key := p.ConfigString("api_key", ""); key != "" {
				opts = append(opts, tensorlake.WithAPIKey(key))
			}
			if base := p.ConfigString("sandbox_api", ""); base != "" {
				opts = append(opts, tensorlake.WithSandboxAPIBaseURL(base))
			}
			if proxy := p.ConfigString("sandbox_proxy", ""); proxy != "" {
				opts = append(opts, tensorlake.WithSandboxProxyBaseURL(proxy))
			}
			client := tensorlake.NewClient(opts...)
			image := p.ConfigString("image", envOr(p, "TL_IMAGE", "tensorlake/ubuntu-minimal"))
			timeout := int64(p.ConfigInt("timeout_secs", envInt(p, "TL_TIMEOUT_SECS", 900)))
			workdir := p.ConfigString("workdir", envOr(p, "TL_WORKDIR", "/home/tl-user"))
			return p.Provide("sandbox/tensorlake", &sdk.SandboxProvider{
				Name:      "tensorlake",
				Isolation: "remote",
				Create: func(c context.Context, spec sdk.SandboxSpec) (*sdk.Sandbox, error) {
					if len(spec.Mounts) > 0 {
						return nil, errors.New("tensorlake: host mounts are not supported")
					}
					img := image
					if spec.Image != "" {
						img = spec.Image
					}
					cpus, mem := float64(1), int64(1024)
					if spec.CPUs > 0 {
						cpus = spec.CPUs
					}
					if spec.MemoryMB > 0 {
						mem = int64(spec.MemoryMB)
					}
					t := timeout
					if spec.TTLSeconds > 0 {
						t = int64(spec.TTLSeconds)
					}
					req := &tensorlake.CreateSandboxRequest{
						Image:       img,
						TimeoutSecs: &t,
						Resources:   &tensorlake.SandboxResourceOverrides{CPUs: cpus, MemoryMB: mem},
						Network:     tensorlakeNetwork(spec.Network),
					}
					resp, err := client.CreateSandbox(c, req)
					if err != nil {
						return nil, fmt.Errorf("tensorlake: %w", err)
					}
					if err := waitSandbox(c, client, resp.SandboxId); err != nil {
						return nil, err
					}
					p.Logf("created Tensorlake sandbox %s (image %s)", resp.SandboxId, img)
					return wrapTensorlake(p, client, resp.SandboxId, workdir), nil
				},
				Attach: func(c context.Context, id string) (*sdk.Sandbox, error) {
					info, err := client.GetSandbox(c, id)
					if err != nil {
						return nil, fmt.Errorf("tensorlake: attach %s: %w", id, err)
					}
					if info.Status == tensorlake.SandboxStatusSuspended {
						if err := client.ResumeSandbox(c, id); err != nil {
							return nil, fmt.Errorf("tensorlake: resume %s: %w", id, err)
						}
					}
					if err := waitSandbox(c, client, id); err != nil {
						return nil, err
					}
					p.Logf("attached Tensorlake sandbox %s", id)
					return wrapTensorlake(p, client, id, workdir), nil
				},
			})
		},
	}
}

func tensorlakeNetwork(n sdk.NetworkPolicy) *tensorlake.SandboxNetworkAccessControl {
	switch n.Mode {
	case "", "none":
		return &tensorlake.SandboxNetworkAccessControl{AllowInternetAccess: false}
	case "allowlist":
		return &tensorlake.SandboxNetworkAccessControl{AllowInternetAccess: true, AllowOut: append([]string(nil), n.Allow...)}
	case "full":
		return &tensorlake.SandboxNetworkAccessControl{AllowInternetAccess: true}
	default:
		return &tensorlake.SandboxNetworkAccessControl{AllowInternetAccess: false}
	}
}

func waitSandbox(ctx context.Context, client *tensorlake.Client, id string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		info, err := client.GetSandbox(ctx, id)
		if err != nil {
			return fmt.Errorf("tensorlake: get %s: %w", id, err)
		}
		switch info.Status {
		case tensorlake.SandboxStatusRunning:
			return nil
		case tensorlake.SandboxStatusTerminated:
			return fmt.Errorf("tensorlake: sandbox %s terminated while starting (%s)", id, info.Outcome)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("tensorlake: sandbox %s did not become running", id)
}

func wrapTensorlake(p *sdk.Scope, client *tensorlake.Client, id, wd string) *sdk.Sandbox {
	abs := func(p string) string {
		if strings.HasPrefix(p, "/") {
			return p
		}
		return path.Join(wd, p)
	}
	exec := func(c context.Context, req sdk.ExecRequest) (*sdk.ExecResult, error) {
		cmd, args := req.Cmd, req.Args
		if req.Shell != "" {
			cmd, args = "/bin/bash", []string{"-c", req.Shell}
		}
		dir := wd
		if req.Dir != "" {
			dir = abs(req.Dir)
		}
		startReq := &tensorlake.StartProcessRequest{
			Command: cmd, Args: args, WorkingDir: dir, Env: req.Env,
			StdinMode: tensorlake.StdinModeClosed, StdoutMode: tensorlake.OutputModeCapture, StderrMode: tensorlake.OutputModeCapture,
		}
		if len(req.Stdin) > 0 {
			startReq.StdinMode = tensorlake.StdinModePipe
		}
		proc, err := client.StartProcess(c, id, startReq)
		if err != nil {
			return nil, fmt.Errorf("tensorlake: exec: %w", err)
		}
		if len(req.Stdin) > 0 {
			if err := client.WriteProcessStdin(c, id, proc.PID, bytes.NewReader(req.Stdin)); err != nil {
				return nil, fmt.Errorf("tensorlake: stdin: %w", err)
			}
			_ = client.CloseProcessStdin(c, id, proc.PID)
		}
		t := time.Now()
		code, err := waitProcess(c, client, id, proc.PID, req.TimeoutSeconds)
		if err != nil {
			return nil, err
		}
		stdout, _ := client.GetProcessStdout(c, id, proc.PID)
		stderr, _ := client.GetProcessStderr(c, id, proc.PID)
		return &sdk.ExecResult{
			Stdout: lines(stdout), Stderr: lines(stderr), ExitCode: code,
			DurationMS: time.Since(t).Milliseconds(),
		}, nil
	}
	start := func(c context.Context, req sdk.ExecRequest) (*sdk.Process, error) {
		cmd, args := req.Cmd, req.Args
		if req.Shell != "" {
			cmd, args = "/bin/bash", []string{"-c", req.Shell}
		} else if strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
			cmd = abs(cmd)
		}
		dir := wd
		if req.Dir != "" {
			dir = abs(req.Dir)
		}
		if strings.HasPrefix(cmd, "/") || strings.HasPrefix(cmd, "./") {
			_, _ = exec(c, sdk.ExecRequest{Shell: "chmod +x -- " + shellQuote(cmd)})
		}
		proc, err := client.StartProcess(c, id, &tensorlake.StartProcessRequest{
			Command: cmd, Args: args, WorkingDir: dir, Env: req.Env,
			StdinMode: tensorlake.StdinModePipe, StdoutMode: tensorlake.OutputModeCapture, StderrMode: tensorlake.OutputModeCapture,
		})
		if err != nil {
			return nil, fmt.Errorf("tensorlake: start: %w", err)
		}
		stdoutR, stdoutW := io.Pipe()
		stderrR, stderrW := io.Pipe()
		var closeOnce sync.Once
		closeAll := func() {
			closeOnce.Do(func() {
				stdoutW.Close()
				stderrW.Close()
			})
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for evt, err := range client.FollowProcessStdout(c, id, proc.PID) {
				if err != nil {
					stdoutW.CloseWithError(err)
					return
				}
				if _, err := io.WriteString(stdoutW, evt.Line+"\n"); err != nil {
					return
				}
			}
			_ = stdoutW.Close()
		}()
		go func() {
			defer wg.Done()
			for evt, err := range client.FollowProcessStderr(c, id, proc.PID) {
				if err != nil {
					stderrW.CloseWithError(err)
					return
				}
				if _, err := io.WriteString(stderrW, evt.Line+"\n"); err != nil {
					return
				}
			}
			_ = stderrW.Close()
		}()
		return &sdk.Process{
			Stdin:  &tlStdin{client: client, id: id, pid: proc.PID},
			Stdout: stdoutR, Stderr: stderrR,
			Wait: func() (int, error) {
				code, err := waitProcess(context.Background(), client, id, proc.PID, 0)
				done := make(chan struct{})
				go func() { wg.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
				closeAll()
				return code, err
			},
			Kill: func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				err := client.KillProcess(ctx, id, proc.PID)
				closeAll()
				return err
			},
		}, nil
	}
	return &sdk.Sandbox{
		ID: id, Provider: "tensorlake", Isolation: "remote",
		Exec: exec, Start: start,
		ReadFile: func(c context.Context, p string) ([]byte, error) {
			b, err := client.ReadSandboxFile(c, id, abs(p))
			if err != nil {
				return nil, fmt.Errorf("tensorlake: %w", err)
			}
			return b, nil
		},
		WriteFile: func(c context.Context, p string, data []byte) error {
			if err := mkdirp(c, exec, abs(p)); err != nil {
				return err
			}
			if err := client.WriteSandboxFile(c, id, abs(p), bytes.NewReader(data)); err != nil {
				return fmt.Errorf("tensorlake: %w", err)
			}
			return nil
		},
		ListDir: func(c context.Context, p string) ([]sdk.FileInfo, error) {
			res, err := client.ListSandboxDirectory(c, id, abs(p))
			if err != nil {
				return nil, fmt.Errorf("tensorlake: %w", err)
			}
			var out []sdk.FileInfo
			for _, e := range res.Entries {
				fi := sdk.FileInfo{Name: e.Name, IsDir: e.IsDir}
				if e.Size != nil {
					fi.Size = *e.Size
				}
				out = append(out, fi)
			}
			return out, nil
		},
		Remove: func(c context.Context, p string) error {
			res, err := exec(c, sdk.ExecRequest{Shell: "rm -rf -- " + shellQuote(abs(p))})
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return errors.New(strings.TrimSpace(res.Stderr))
			}
			return nil
		},
		Upload: func(c context.Context, hostPath, boxPath string) error {
			hd, err := p.Dir(filepath.Dir(hostPath), false)
			if err != nil {
				return err
			}
			defer hd.Close()
			f, err := hd.Open(filepath.Base(hostPath))
			if err != nil {
				return err
			}
			defer f.Close()
			if err := mkdirp(c, exec, abs(boxPath)); err != nil {
				return err
			}
			if err := client.WriteSandboxFile(c, id, abs(boxPath), f); err != nil {
				return fmt.Errorf("tensorlake: %w", err)
			}
			return nil
		},
		Download: func(c context.Context, boxPath, hostPath string) error {
			b, err := client.ReadSandboxFile(c, id, abs(boxPath))
			if err != nil {
				return fmt.Errorf("tensorlake: %w", err)
			}
			hd, err := p.Dir(filepath.Dir(hostPath), true)
			if err != nil {
				return err
			}
			defer hd.Close()
			return hd.WriteFile(filepath.Base(hostPath), b, 0o644)
		},
		Close: func(c context.Context) error {
			cc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := client.DeleteSandbox(cc, id); err != nil && !strings.Contains(strings.ToLower(err.Error()), "not found") {
				return err
			}
			return nil
		},
	}
}

type tlStdin struct {
	client *tensorlake.Client
	id     string
	pid    int32
	once   sync.Once
}

func (s *tlStdin) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.client.WriteProcessStdin(ctx, s.id, s.pid, bytes.NewReader(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *tlStdin) Close() error {
	var err error
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = s.client.CloseProcessStdin(ctx, s.id, s.pid)
	})
	return err
}

func waitProcess(ctx context.Context, client *tensorlake.Client, id string, pid int32, timeoutSecs int) (int, error) {
	deadline := time.Now().Add(10 * time.Minute)
	if timeoutSecs > 0 {
		deadline = time.Now().Add(time.Duration(timeoutSecs) * time.Second)
	}
	for time.Now().Before(deadline) {
		info, err := client.GetProcess(ctx, id, pid)
		if err != nil {
			return 0, fmt.Errorf("tensorlake: process %d: %w", pid, err)
		}
		switch info.Status {
		case tensorlake.ProcessStatusExited, tensorlake.ProcessStatusSignaled:
			if info.ExitCode != nil {
				return int(*info.ExitCode), nil
			}
			return 0, nil
		}
		select {
		case <-ctx.Done():
			return 124, nil
		case <-time.After(200 * time.Millisecond):
		}
	}
	_ = client.KillProcess(context.Background(), id, pid)
	return 124, nil
}

func mkdirp(c context.Context, exec func(context.Context, sdk.ExecRequest) (*sdk.ExecResult, error), fpath string) error {
	res, err := exec(c, sdk.ExecRequest{Shell: "mkdir -p -- " + shellQuote(path.Dir(fpath))})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return errors.New(strings.TrimSpace(res.Stderr))
	}
	return nil
}

func lines(r *tensorlake.ProcessOutputResponse) string {
	if r == nil || len(r.Lines) == 0 {
		return ""
	}
	return strings.Join(r.Lines, "\n") + "\n"
}

// envOr and envInt read a host variable through the module's "env" grant.
func envOr(p *sdk.Scope, key, def string) string {
	if v, err := p.Env(key); err == nil && v != "" {
		return v
	}
	return def
}

func envInt(p *sdk.Scope, key string, def int) int {
	if v, err := p.Env(key); err == nil && v != "" {
		fmt.Sscanf(v, "%d", &def)
	}
	return def
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

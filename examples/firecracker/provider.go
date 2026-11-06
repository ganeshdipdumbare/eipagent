package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// firecracker is a sandbox provider for a self-hosted Firecracker microVM.
// It creates a tap device, boots Firecracker, and talks to the guest over
// SSH, so it behaves like a small hosted sandbox: create, connect, run, and
// destroy.
//
// It needs Linux with /dev/kvm and passwordless sudo (for the tap device and
// the Firecracker process). examples/firecracker/setup.sh downloads a kernel,
// rootfs, and Firecracker binary.
type fcConfig struct {
	bin        string
	kernel     string
	rootfs     string
	sshKey     string
	workdir    string
	bootArgs   string
	cpus       int
	memMB      int
	tapPrefix  string
	sshTimeout time.Duration
	remoteHost string
	remoteConf string
}

func firecracker() kiln.Module {
	return kiln.Module{
		Name:     "sandbox_firecracker",
		Provides: []string{"sandbox/firecracker"},
		Apply: func(p *sdk.Scope) error {
			cfg := fcConfig{
				bin:        envOr("FC_BIN", "/usr/local/bin/firecracker"),
				kernel:     envOr("FC_KERNEL", ".kiln/firecracker/vmlinux"),
				rootfs:     envOr("FC_ROOTFS", ".kiln/firecracker/rootfs.ext4"),
				sshKey:     envOr("FC_SSH_KEY", ".kiln/firecracker/id_ed25519"),
				workdir:    envOr("FC_WORKDIR", defaultWorkdir(os.Getenv("FC_HOST"))),
				bootArgs:   envOr("FC_BOOT_ARGS", defaultBootArgs()),
				cpus:       envInt("FC_CPUS", 1),
				memMB:      envInt("FC_MEM_MB", 512),
				tapPrefix:  envOr("FC_TAP_PREFIX", "fct"),
				sshTimeout: 90 * time.Second,
				remoteHost: os.Getenv("FC_HOST"),
				remoteConf: envOr("FC_SSH_CONFIG", defaultSSHConfig()),
			}
			cfg = cfg.absolutize()
			if err := cfg.check(); err != nil {
				return err
			}
			return p.Provide("sandbox/firecracker", &sdk.SandboxProvider{
				Name:      "firecracker",
				Isolation: "microvm",
				Create: func(c context.Context, spec sdk.SandboxSpec) (*sdk.Sandbox, error) {
					return createVM(c, p, cfg, spec)
				},
				Attach: func(c context.Context, id string) (*sdk.Sandbox, error) {
					return attachVM(c, p, cfg, id)
				},
			})
		},
	}
}

func (c fcConfig) absolutize() fcConfig {
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	c.bin = abs(c.bin)
	c.kernel = abs(c.kernel)
	c.rootfs = abs(c.rootfs)
	c.sshKey = abs(c.sshKey)
	c.workdir = abs(c.workdir)
	c.remoteConf = abs(c.remoteConf)
	return c
}

func defaultWorkdir(remoteHost string) string {
	if remoteHost != "" {
		return "/tmp/kiln-firecracker"
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "kiln-firecracker")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "kiln-firecracker")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("kiln-firecracker-%d", os.Getuid()))
}

func defaultSSHConfig() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".lima", "firecracker", "ssh.config")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func (c fcConfig) check() error {
	if c.remoteHost != "" {
		if _, err := os.Stat(c.sshKey); err != nil {
			return fmt.Errorf("firecracker: ssh key %s: %w (run examples/firecracker/setup.sh)", c.sshKey, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, args := range [][]string{
			{"test", "-x", c.bin},
			{"test", "-r", c.kernel},
			{"test", "-r", c.rootfs},
			{"test", "-e", "/dev/kvm"},
		} {
			if err := c.runRemote(ctx, args...); err != nil {
				return fmt.Errorf("firecracker: remote %s: %w", c.remoteHost, err)
			}
		}
		return nil
	}
	for name, path := range map[string]string{c.bin: "firecracker", c.kernel: "kernel", c.rootfs: "rootfs", c.sshKey: "ssh key"} {
		if _, err := os.Stat(name); err != nil {
			return fmt.Errorf("firecracker: %s %s: %w (run examples/firecracker/setup.sh)", path, name, err)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("firecracker: /dev/kvm: %w (need Linux with KVM; on macOS use Lima with nestedVirtualization)", err)
	}
	return nil
}

func createVM(ctx context.Context, p *sdk.Scope, cfg fcConfig, spec sdk.SandboxSpec) (*sdk.Sandbox, error) {
	if len(spec.Mounts) > 0 {
		return nil, errors.New("firecracker: host mounts are not supported")
	}
	if len(spec.Env) > 0 {
		return nil, errors.New("firecracker: sandbox env is not supported; put config in the rootfs or upload files")
	}
	if spec.Network.Mode != "" && spec.Network.Mode != "none" {
		return nil, fmt.Errorf("firecracker: network mode %q is not implemented (only none; the TAP is host-local)", spec.Network.Mode)
	}
	if cfg.remoteHost == "" {
		if err := os.MkdirAll(cfg.workdir, 0o700); err != nil {
			return nil, err
		}
	} else if err := cfg.runRemote(ctx, "mkdir", "-p", cfg.workdir); err != nil {
		return nil, err
	}
	id := "fc-" + randomHex(6)
	idx := randomIndex()
	tap := fmt.Sprintf("%s%d", cfg.tapPrefix, idx)
	for i := 0; i < 20; i++ {
		if err := cfg.runPriv(ctx, "ip", "link", "show", tap); err != nil {
			break
		}
		idx = randomIndex()
		tap = fmt.Sprintf("%s%d", cfg.tapPrefix, idx)
	}
	dir := filepath.Join(cfg.workdir, id)
	if cfg.remoteHost == "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	} else if err := cfg.runRemote(ctx, "mkdir", "-p", dir); err != nil {
		return nil, err
	}
	hostIP := fmt.Sprintf("172.16.%d.1", idx)
	guestIP := fmt.Sprintf("172.16.%d.2", idx)
	mac := fmt.Sprintf("06:00:AC:10:%02x:02", idx)
	m := &meta{ID: id, Dir: dir, Tap: tap, HostIP: hostIP, GuestIP: guestIP, MAC: mac, PidFile: filepath.Join(dir, "firecracker.pid"), Socket: filepath.Join(dir, "firecracker.sock"), Network: spec.Network.Mode}
	ok := false
	defer func() {
		if !ok {
			stop(m, cfg)
		}
	}()

	if err := cfg.runPriv(ctx, "ip", "tuntap", "add", "dev", tap, "mode", "tap"); err != nil {
		return nil, fmt.Errorf("firecracker: create tap %s: %w", tap, err)
	}
	if err := cfg.runPriv(ctx, "ip", "addr", "add", hostIP+"/30", "dev", tap); err != nil {
		return nil, fmt.Errorf("firecracker: address tap %s: %w", tap, err)
	}
	if err := cfg.runPriv(ctx, "ip", "link", "set", "dev", tap, "up"); err != nil {
		return nil, fmt.Errorf("firecracker: bring up tap %s: %w", tap, err)
	}
	if spec.Network.Mode == "" || spec.Network.Mode == "none" {
		// The TAP is needed for SSH/control, but the guest must not reach
		// host services. Allow only replies to host-initiated connections
		// and refuse to continue if the firewall cannot be installed.
		if err := cfg.runPriv(ctx, "iptables", "-w", "5", "-I", "INPUT", "1", "-i", tap, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
			return nil, fmt.Errorf("firecracker: network none needs iptables: %w", err)
		}
		if err := cfg.runPriv(ctx, "iptables", "-w", "5", "-I", "INPUT", "2", "-i", tap, "-j", "DROP"); err != nil {
			return nil, fmt.Errorf("firecracker: network none needs iptables: %w", err)
		}
		if err := cfg.runPriv(ctx, "iptables", "-w", "5", "-I", "FORWARD", "1", "-i", tap, "-j", "DROP"); err != nil {
			return nil, fmt.Errorf("firecracker: network none needs iptables: %w", err)
		}
		if err := cfg.runPriv(ctx, "iptables", "-w", "5", "-I", "FORWARD", "2", "-o", tap, "-j", "DROP"); err != nil {
			return nil, fmt.Errorf("firecracker: network none needs iptables: %w", err)
		}
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if cfg.remoteHost == "" {
		if out, err := exec.CommandContext(ctx, "cp", "--reflink=auto", cfg.rootfs, rootfs).CombinedOutput(); err != nil {
			if out2, err2 := exec.CommandContext(ctx, "cp", cfg.rootfs, rootfs).CombinedOutput(); err2 != nil {
				return nil, fmt.Errorf("firecracker: copy rootfs: %v %s / %v %s", err, out, err2, out2)
			}
		}
	} else {
		if err := cfg.runRemote(ctx, "cp", "--reflink=auto", cfg.rootfs, rootfs); err != nil {
			if err := cfg.runRemote(ctx, "cp", cfg.rootfs, rootfs); err != nil {
				return nil, fmt.Errorf("firecracker: remote copy rootfs: %w", err)
			}
		}
	}
	cpus, mem := cfg.cpus, cfg.memMB
	if spec.CPUs > 0 {
		cpus = int(spec.CPUs + 0.5)
	}
	if spec.MemoryMB > 0 {
		mem = spec.MemoryMB
	}
	if cfg.remoteHost != "" {
		if err := writeVMConfig(ctx, cfg, m, cpus, mem); err != nil {
			return nil, err
		}
		if err := startFirecrackerRemote(ctx, cfg, m); err != nil {
			return nil, err
		}
	} else {
		if err := startFirecracker(cfg, m); err != nil {
			return nil, err
		}
		if err := configureFirecracker(ctx, cfg, m, cpus, mem); err != nil {
			return nil, err
		}
	}
	if err := waitSSH(ctx, cfg, guestIP, cfg.sshTimeout); err != nil {
		return nil, err
	}
	m.VCPUs, m.MemoryMB = cpus, mem
	if err := writeMeta(ctx, cfg, m); err != nil {
		return nil, err
	}
	box := wrapVM(cfg, m)
	if spec.TTLSeconds > 0 {
		time.AfterFunc(time.Duration(spec.TTLSeconds)*time.Second, func() {
			box.Close(context.Background())
		})
	}
	ok = true
	p.Logf("created firecracker microVM %s at %s", id, guestIP)
	return box, nil
}

type meta struct {
	ID       string `json:"id"`
	Dir      string `json:"dir"`
	Tap      string `json:"tap"`
	HostIP   string `json:"host_ip"`
	GuestIP  string `json:"guest_ip"`
	MAC      string `json:"mac"`
	PidFile  string `json:"pid_file"`
	Socket   string `json:"socket"`
	VCPUs    int    `json:"vcpus"`
	MemoryMB int    `json:"memory_mb"`
	Network  string `json:"network"`
}

func writeMeta(ctx context.Context, cfg fcConfig, m *meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if cfg.remoteHost != "" {
		return cfg.remoteWrite(ctx, filepath.Join(m.Dir, "meta.json"), b)
	}
	return os.WriteFile(filepath.Join(m.Dir, "meta.json"), b, 0o644)
}

func attachVM(ctx context.Context, p *sdk.Scope, cfg fcConfig, id string) (*sdk.Sandbox, error) {
	path := filepath.Join(cfg.workdir, id, "meta.json")
	var b []byte
	var err error
	if cfg.remoteHost != "" {
		b, err = cfg.remoteRead(ctx, path)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("firecracker: attach %s: %w", id, err)
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	wantDir := filepath.Join(cfg.workdir, id)
	if m.ID != id || filepath.Clean(m.Dir) != filepath.Clean(wantDir) ||
		!strings.HasPrefix(filepath.Clean(m.PidFile), filepath.Clean(wantDir)+string(filepath.Separator)) ||
		!strings.HasPrefix(filepath.Clean(m.Socket), filepath.Clean(wantDir)+string(filepath.Separator)) ||
		!strings.HasPrefix(m.Tap, cfg.tapPrefix) {
		return nil, fmt.Errorf("firecracker: attach %s: meta does not belong to this sandbox", id)
	}
	if err := waitSSH(ctx, cfg, m.GuestIP, 10*time.Second); err != nil {
		return nil, fmt.Errorf("firecracker: attach %s: %w", id, err)
	}
	p.Logf("attached firecracker microVM %s at %s", id, m.GuestIP)
	return wrapVM(cfg, &m), nil
}

func startFirecracker(cfg fcConfig, m *meta) error {
	logf, err := os.Create(filepath.Join(m.Dir, "firecracker.log"))
	if err != nil {
		return err
	}
	script := fmt.Sprintf("echo $$ > %s && exec %s --api-sock %s",
		shellQuote(m.PidFile), shellQuote(cfg.bin), shellQuote(m.Socket))
	args := append(priv(), "sh", "-c", script)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = m.Dir
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("firecracker: start: %w", err)
	}
	_ = logf.Close()
	go func() { _ = cmd.Wait() }()
	// The socket is created by root; every API call and the SSH client run
	// as the caller.
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(m.Socket); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(m.Socket); err != nil {
		return fmt.Errorf("firecracker: API socket did not appear: %w", err)
	}
	_ = cfg.runPriv(context.Background(), "chown", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), m.Socket)
	_ = cfg.runPriv(context.Background(), "chmod", "600", m.Socket)
	return nil
}

func configureFirecracker(ctx context.Context, cfg fcConfig, m *meta, cpus, mem int) error {
	requests := []struct {
		path string
		body any
	}{
		{"/boot-source", map[string]any{"kernel_image_path": cfg.kernel, "boot_args": cfg.bootArgs}},
		{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": filepath.Join(m.Dir, "rootfs.ext4"), "is_root_device": true, "is_read_only": false}},
		{"/machine-config", map[string]any{"vcpu_count": cpus, "mem_size_mib": mem}},
		{"/network-interfaces/net1", map[string]any{"iface_id": "net1", "guest_mac": m.MAC, "host_dev_name": m.Tap}},
		{"/actions", map[string]any{"action_type": "InstanceStart"}},
	}
	for _, r := range requests {
		if err := fcAPI(ctx, m.Socket, r.path, r.body); err != nil {
			return err
		}
	}
	return nil
}

func fcAPI(ctx context.Context, sock, endpoint string, body any) error {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker: PUT %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("firecracker: PUT %s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func waitSSH(ctx context.Context, cfg fcConfig, ip string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := sshRun(c, cfg, ip, "true", nil).Run()
		cancel()
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("firecracker: guest SSH at %s not ready: %w", ip, last)
}

func writeVMConfig(ctx context.Context, cfg fcConfig, m *meta, cpus, mem int) error {
	vm := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": cfg.kernel,
			"boot_args":         cfg.bootArgs,
		},
		"drives": []any{map[string]any{
			"drive_id":       "rootfs",
			"path_on_host":   filepath.Join(m.Dir, "rootfs.ext4"),
			"is_root_device": true,
			"is_read_only":   false,
		}},
		"machine-config": map[string]any{
			"vcpu_count":   cpus,
			"mem_size_mib": mem,
		},
		"network-interfaces": []any{map[string]any{
			"iface_id":      "net1",
			"guest_mac":     m.MAC,
			"host_dev_name": m.Tap,
		}},
	}
	b, err := json.MarshalIndent(vm, "", "  ")
	if err != nil {
		return err
	}
	return cfg.remoteWrite(ctx, filepath.Join(m.Dir, "vm.json"), b)
}

func startFirecrackerRemote(ctx context.Context, cfg fcConfig, m *meta) error {
	script := fmt.Sprintf("echo $$ > %s && exec %s --config-file %s",
		shellQuote(m.PidFile), shellQuote(cfg.bin), shellQuote(filepath.Join(m.Dir, "vm.json")))
	cmd := fmt.Sprintf("sudo -n setsid sh -c %s </dev/null >>%s 2>&1 &",
		shellQuote(script), shellQuote(filepath.Join(m.Dir, "firecracker.log")))
	return cfg.runRemoteRaw(ctx, cmd)
}

func stop(m *meta, cfg fcConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var pidb []byte
	var err error
	if cfg.remoteHost != "" {
		pidb, err = cfg.remoteRead(ctx, m.PidFile)
	} else {
		pidb, err = os.ReadFile(m.PidFile)
	}
	if err == nil {
		pid := strings.TrimSpace(string(pidb))
		if pid != "" {
			_ = cfg.runPriv(ctx, "kill", "-TERM", pid)
			time.Sleep(300 * time.Millisecond)
			_ = cfg.runPriv(ctx, "kill", "-KILL", pid)
		}
	}
	_ = cfg.runPriv(ctx, "pkill", "-f", m.Socket)
	if m.Network == "" || m.Network == "none" {
		_ = cfg.runPriv(ctx, "iptables", "-w", "5", "-D", "INPUT", "-i", m.Tap, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
		_ = cfg.runPriv(ctx, "iptables", "-w", "5", "-D", "INPUT", "-i", m.Tap, "-j", "DROP")
		_ = cfg.runPriv(ctx, "iptables", "-w", "5", "-D", "FORWARD", "-i", m.Tap, "-j", "DROP")
		_ = cfg.runPriv(ctx, "iptables", "-w", "5", "-D", "FORWARD", "-o", m.Tap, "-j", "DROP")
	}
	_ = cfg.runPriv(ctx, "ip", "link", "del", m.Tap)
	if cfg.remoteHost != "" {
		_ = cfg.runPriv(ctx, "rm", "-rf", m.Dir)
	} else if err := os.RemoveAll(m.Dir); err != nil {
		_ = cfg.runPriv(ctx, "rm", "-rf", m.Dir)
	}
}

func wrapVM(cfg fcConfig, m *meta) *sdk.Sandbox {
	abs := func(p string) string {
		if strings.HasPrefix(p, "/") {
			return p
		}
		return path.Join("/root", p)
	}
	sh := func(c context.Context, line string, stdin []byte, timeout int) (string, string, int, error) {
		if timeout > 0 {
			var cancel context.CancelFunc
			c, cancel = context.WithTimeout(c, time.Duration(timeout)*time.Second)
			defer cancel()
		}
		cmd := sshRun(c, cfg, m.GuestIP, line, stdin)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.String(), errb.String(), ee.ExitCode(), nil
		}
		if err != nil && c.Err() != nil {
			return out.String(), errb.String() + "\n(timed out)", 124, nil
		}
		return out.String(), errb.String(), 0, err
	}
	runExec := func(c context.Context, req sdk.ExecRequest) (*sdk.ExecResult, error) {
		var line string
		if req.Shell != "" {
			line = req.Shell
		} else {
			line = shellLine(req.Cmd, req.Args)
		}
		if req.Dir != "" {
			line = "cd -- " + shellQuote(abs(req.Dir)) + " && " + line
		}
		for k, v := range req.Env {
			line = "export " + k + "=" + shellQuote(v) + "; " + line
		}
		t := time.Now()
		out, errs, code, err := sh(c, line, req.Stdin, req.TimeoutSeconds)
		if err != nil {
			return nil, err
		}
		return &sdk.ExecResult{Stdout: out, Stderr: errs, ExitCode: code, DurationMS: time.Since(t).Milliseconds()}, nil
	}
	start := func(c context.Context, req sdk.ExecRequest) (*sdk.Process, error) {
		line := req.Shell
		if line == "" {
			line = shellLine(abs(req.Cmd), req.Args)
		}
		if req.Dir != "" {
			line = "cd -- " + shellQuote(abs(req.Dir)) + " && " + line
		}
		for k, v := range req.Env {
			line = "export " + k + "=" + shellQuote(v) + "; " + line
		}
		if strings.HasPrefix(req.Cmd, "/") || strings.HasPrefix(req.Cmd, "./") {
			_, _, _, _ = sh(c, "chmod +x -- "+shellQuote(abs(req.Cmd)), nil, 5)
		}
		cmd := sshRun(c, cfg, m.GuestIP, line, nil)
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
	return &sdk.Sandbox{
		ID: m.ID, Provider: "firecracker", Isolation: "microvm",
		Exec: runExec, Start: start,
		ReadFile: func(c context.Context, fpath string) ([]byte, error) {
			out, errs, code, err := sh(c, "cat -- "+shellQuote(abs(fpath)), nil, 30)
			if err != nil {
				return nil, err
			}
			if code != 0 {
				return nil, errors.New(strings.TrimSpace(errs))
			}
			return []byte(out), nil
		},
		WriteFile: func(c context.Context, fpath string, data []byte) error {
			_, errs, code, err := sh(c, "mkdir -p -- \"$(dirname -- "+shellQuote(abs(fpath))+"))\" && cat > "+shellQuote(abs(fpath)), data, 60)
			if err != nil {
				return err
			}
			if code != 0 {
				return errors.New(strings.TrimSpace(errs))
			}
			return nil
		},
		ListDir: func(c context.Context, fpath string) ([]sdk.FileInfo, error) {
			out, errs, code, err := sh(c, "ls -1Ap -- "+shellQuote(abs(fpath)), nil, 30)
			if err != nil {
				return nil, err
			}
			if code != 0 {
				return nil, errors.New(strings.TrimSpace(errs))
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
			_, errs, code, err := sh(c, "rm -rf -- "+shellQuote(abs(fpath)), nil, 30)
			if err != nil {
				return err
			}
			if code != 0 {
				return errors.New(strings.TrimSpace(errs))
			}
			return nil
		},
		Upload: func(c context.Context, hostPath, boxPath string) error {
			f, err := os.Open(hostPath)
			if err != nil {
				return err
			}
			defer f.Close()
			data, err := io.ReadAll(f)
			if err != nil {
				return err
			}
			return func() error {
				_, errs, code, err := sh(c, "mkdir -p -- \"$(dirname -- "+shellQuote(abs(boxPath))+"))\" && cat > "+shellQuote(abs(boxPath)), data, 300)
				if err != nil {
					return err
				}
				if code != 0 {
					return errors.New(strings.TrimSpace(errs))
				}
				return nil
			}()
		},
		Download: func(c context.Context, boxPath, hostPath string) error {
			out, errs, code, err := sh(c, "cat -- "+shellQuote(abs(boxPath)), nil, 300)
			if err != nil {
				return err
			}
			if code != 0 {
				return errors.New(strings.TrimSpace(errs))
			}
			return os.WriteFile(hostPath, []byte(out), 0o644)
		},
		Close: func(c context.Context) error {
			stop(m, cfg)
			return nil
		},
	}
}

func sshRun(ctx context.Context, cfg fcConfig, ip, line string, stdin []byte) *exec.Cmd {
	known := os.Getenv("FC_KNOWN_HOSTS")
	if known == "" {
		known = filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")
	}
	strict := "accept-new"
	if os.Getenv("FC_INSECURE_SSH") != "" {
		strict, known = "no", "/dev/null"
	}
	args := []string{"-i", cfg.sshKey,
		"-o", "StrictHostKeyChecking=" + strict, "-o", "UserKnownHostsFile=" + known,
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR"}
	if cfg.remoteHost != "" {
		jump := "ssh"
		if cfg.remoteConf != "" {
			jump += " -F " + shellQuote(cfg.remoteConf)
		}
		jump += " -W %h:%p " + shellQuote(cfg.remoteHost)
		args = append(args, "-o", "ProxyCommand="+jump)
	}
	args = append(args, "-T", "root@"+ip, "--", "bash", "-c", shellQuote(line))
	cmd := exec.CommandContext(ctx, "ssh", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd
}

func priv() []string {
	if os.Geteuid() == 0 {
		return nil
	}
	return []string{"sudo", "-n"}
}

// runPriv runs a privileged command on this host, or on the remote
// Firecracker host when FC_HOST is set.
func (c fcConfig) runPriv(ctx context.Context, name string, args ...string) error {
	if c.remoteHost != "" {
		all := append([]string{"sudo", "-n", name}, args...)
		return c.runRemote(ctx, all...)
	}
	all := append(priv(), name)
	all = append(all, args...)
	out, err := exec.CommandContext(ctx, all[0], all[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(all, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c fcConfig) remoteSSHBase() []string {
	args := []string{}
	if c.remoteConf != "" {
		args = append(args, "-F", c.remoteConf)
	}
	known := os.Getenv("FC_KNOWN_HOSTS")
	if known == "" {
		known = filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")
	}
	strict := "accept-new"
	if os.Getenv("FC_INSECURE_SSH") != "" {
		strict, known = "no", "/dev/null"
	}
	return append(args, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking="+strict, "-o", "UserKnownHostsFile="+known,
		"-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", c.remoteHost)
}

func (c fcConfig) runRemote(ctx context.Context, args ...string) error {
	return c.runRemoteRaw(ctx, shellLine(args[0], args[1:]))
}

func (c fcConfig) runRemoteRaw(ctx context.Context, command string) error {
	sshArgs := append(c.remoteSSHBase(), command)
	out, err := exec.CommandContext(ctx, "ssh", sshArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh %s: %w: %s", c.remoteHost, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c fcConfig) remoteOutput(ctx context.Context, command string) ([]byte, error) {
	sshArgs := append(c.remoteSSHBase(), command)
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ssh %s: %w: %s", c.remoteHost, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func (c fcConfig) remoteWrite(ctx context.Context, path string, data []byte) error {
	sshArgs := append(c.remoteSSHBase(), "cat > "+shellQuote(path))
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh %s: write %s: %w: %s", c.remoteHost, path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c fcConfig) remoteRead(ctx context.Context, path string) ([]byte, error) {
	return c.remoteOutput(ctx, "cat -- "+shellQuote(path))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func defaultBootArgs() string {
	if runtime.GOARCH == "arm64" {
		return "keep_bootcon console=ttyS0 reboot=k panic=1"
	}
	return "console=ttyS0 reboot=k panic=1"
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func randomIndex() int {
	b := make([]byte, 1)
	rand.Read(b)
	return int(b[0]%200) + 1 // 172.16.1.0/30 ... 172.16.200.0/30
}

func shellLine(cmd string, args []string) string {
	parts := []string{shellQuote(cmd)}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

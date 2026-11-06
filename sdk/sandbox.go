package sdk

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ---- Sandboxes: provided as "sandbox/<name>" by provider modules ----

type SandboxSpec struct {
	Image      string
	Workdir    string
	Env        map[string]string
	Mounts     []Mount
	CPUs       float64
	MemoryMB   int
	Network    NetworkPolicy
	TTLSeconds int
	Labels     map[string]string
}

type Mount struct {
	Host, Box string
	ReadOnly  bool
}

type NetworkPolicy struct {
	Mode  string   // none (default) | allowlist | full
	Allow []string // hosts, for allowlist
}

type ExecRequest struct {
	Cmd            string   // program, or
	Shell          string   // a shell command line (run with sh -c)
	Args           []string // arguments for Cmd
	Stdin          []byte
	Env            map[string]string
	Dir            string
	TimeoutSeconds int
}

type ExecResult struct {
	Stdout, Stderr string
	ExitCode       int
	DurationMS     int64
}

// Process is a long-running command (e.g. `kiln serve --stdio` inside a box).
type Process struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
	Wait   func() (int, error)
	Kill   func() error
}

type FileInfo struct {
	Name  string
	Size  int64
	IsDir bool
}

// Sandbox is one execution world. Fields may be nil when unsupported.
type Sandbox struct {
	ID        string
	Provider  string
	Isolation string // none | container | microvm | remote
	Exec      func(ctx context.Context, req ExecRequest) (*ExecResult, error)
	Start     func(ctx context.Context, req ExecRequest) (*Process, error)
	ReadFile  func(ctx context.Context, path string) ([]byte, error)
	WriteFile func(ctx context.Context, path string, data []byte) error
	ListDir   func(ctx context.Context, path string) ([]FileInfo, error)
	Remove    func(ctx context.Context, path string) error
	Upload    func(ctx context.Context, hostPath, boxPath string) error
	Download  func(ctx context.Context, boxPath, hostPath string) error
	Expose    func(ctx context.Context, port int) (string, error)
	Close     func(ctx context.Context) error
}

type SandboxProvider struct {
	Name      string
	Isolation string
	Create    func(ctx context.Context, spec SandboxSpec) (*Sandbox, error)
	Attach    func(ctx context.Context, id string) (*Sandbox, error) // reconnect after restart
}

// ExecWorld is where shell and file tools act, provided as "exec":
// the host workspace (workspace_local) or a per-thread sandbox (workspace_sandbox).
type ExecWorld struct {
	Name string
	For  func(ctx context.Context) (*Sandbox, error)
}

// moduleSandboxes is what Service("sandbox/<name>") hands a module: the
// same provider, but its sandboxes have no Upload / Download and it
// refuses host mounts. Both move files between a host path and the box,
// so a module without fs permissions could use them to read or write
// anywhere on the host. Only the host application (Config.RunIn) moves files
// across that boundary; modules use ReadFile / WriteFile inside the box.
func moduleSandboxes(p *SandboxProvider, allowAttach bool) *SandboxProvider {
	strip := func(b *Sandbox, err error) (*Sandbox, error) {
		if b == nil {
			return nil, err
		}
		cp := *guardEnv(b)
		cp.Upload, cp.Download = nil, nil
		return &cp, err
	}
	cp := *p
	if p.Create != nil {
		cp.Create = func(ctx context.Context, spec SandboxSpec) (*Sandbox, error) {
			if len(spec.Mounts) > 0 {
				return nil, fmt.Errorf("sandbox mounts are set by the host application, not by modules")
			}
			if err := CheckSandboxSpec(spec); err != nil {
				return nil, err
			}
			return strip(p.Create(ctx, spec))
		}
	}
	if p.Attach != nil && allowAttach {
		cp.Attach = func(ctx context.Context, id string) (*Sandbox, error) { return strip(p.Attach(ctx, id)) }
	} else {
		cp.Attach = nil
	}
	return &cp
}

// guardEnv wraps Exec and Start so every command run in the box passes
// CheckSandboxEnv.
func guardEnv(b *Sandbox) *Sandbox {
	if b == nil {
		return nil
	}
	cp := *b
	if b.Exec != nil {
		cp.Exec = func(ctx context.Context, req ExecRequest) (*ExecResult, error) {
			if err := CheckSandboxEnv(req.Env); err != nil {
				return nil, err
			}
			return b.Exec(ctx, req)
		}
	}
	if b.Start != nil {
		cp.Start = func(ctx context.Context, req ExecRequest) (*Process, error) {
			if err := CheckSandboxEnv(req.Env); err != nil {
				return nil, err
			}
			return b.Start(ctx, req)
		}
	}
	return &cp
}

// CheckSandboxEnv refuses environment variables that would put a real
// credential into a sandbox: a value holding a resolved secret, or a
// credential-looking name whose value isn't a placeholder. Placeholders
// (see NewPlaceholder) are the only form a secret may take in a box.
func CheckSandboxEnv(env map[string]string) error {
	for k, v := range env {
		if ContainsSecret(v) {
			return fmt.Errorf("sandbox env %s holds a secret value; secrets stay outside the sandbox", k)
		}
		if SecretLike(k) && v != "" && len(FindPlaceholders(v)) == 0 {
			return fmt.Errorf("sandbox env %s looks like a credential; pass a secret:// placeholder, not a value", k)
		}
	}
	return nil
}

// CheckSandboxSpec applies CheckSandboxEnv, refuses an image name a
// runtime CLI would read as a flag, and refuses mounts that would hand the
// box the host itself: "/" or a container runtime socket (with it a box
// could start containers outside every rule here). Mount sources are
// checked after resolving symlinks, and a directory is refused when it
// directly holds a runtime socket (/var/run, /run ...).
func CheckSandboxSpec(spec SandboxSpec) error {
	if spec.Workdir != "" {
		if !filepath.IsAbs(spec.Workdir) || strings.HasPrefix(spec.Workdir, "-") {
			return fmt.Errorf("sandbox workdir %q must be an absolute path", spec.Workdir)
		}
		for _, r := range spec.Workdir {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("sandbox workdir %q contains a control character", spec.Workdir)
			}
		}
		for _, part := range strings.Split(filepath.ToSlash(spec.Workdir), "/") {
			if part == ".." {
				return fmt.Errorf("sandbox workdir %q must not contain ..", spec.Workdir)
			}
		}
	}
	if err := CheckSandboxEnv(spec.Env); err != nil {
		return err
	}
	if strings.HasPrefix(spec.Image, "-") || strings.ContainsAny(spec.Image, " \t\r\n") {
		return fmt.Errorf("sandbox image %q is not a valid image name", spec.Image)
	}
	for _, m := range spec.Mounts {
		if !filepath.IsAbs(m.Host) {
			return fmt.Errorf("sandbox mount %s must be an absolute host path", m.Host)
		}
		if m.Box == "" || !filepath.IsAbs(m.Box) || strings.ContainsAny(m.Box, ":,\t\r\n ") {
			return fmt.Errorf("sandbox mount destination %q must be an absolute path without :, , or whitespace", m.Box)
		}
		for _, part := range strings.Split(filepath.ToSlash(m.Box), "/") {
			if part == ".." {
				return fmt.Errorf("sandbox mount destination %q must not contain ..", m.Box)
			}
		}
		h := filepath.Clean(m.Host)
		real, err := filepath.EvalSymlinks(h)
		if err != nil {
			return fmt.Errorf("sandbox mount %s: %v", m.Host, err)
		}
		h = real
		if h == "/" || runtimeSocket(h) || runtimeMountPath(h) {
			return fmt.Errorf("sandbox mount %s is not allowed (host root or container runtime socket)", m.Host)
		}
		if entries, err := os.ReadDir(h); err == nil {
			for _, e := range entries {
				if runtimeSocket(filepath.Join(h, e.Name())) {
					return fmt.Errorf("sandbox mount %s is not allowed (it holds the container runtime socket %s)", m.Host, e.Name())
				}
			}
		}
	}
	return nil
}

// runtimeMountPath reports a path that could expose a container runtime
// socket directly or as an ancestor of its directory.
func runtimeMountPath(p string) bool {
	home := os.Getenv("HOME")
	dirs := []string{"/var/run", "/run", "/var/lib/docker", "/var/lib/containerd",
		"/var/lib/containers", filepath.Join(home, ".docker"), filepath.Join(home, ".podman")}
	for _, d := range dirs {
		if d == "" || d == "/" {
			continue
		}
		if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) || strings.HasPrefix(d, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// runtimeSocket reports whether path names a docker/podman/containerd
// socket, by name or (for a renamed link target) by being a socket file
// in one of their directories.
func runtimeSocket(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".sock") && (strings.Contains(base, "docker") || strings.Contains(base, "podman") || strings.Contains(base, "containerd")) {
		return true
	}
	// A mount of any Unix socket lets the box talk to a host daemon; a
	// renamed docker.sock in /tmp is still a docker socket.
	if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		return true
	}
	return false
}

package sdk

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// ---- Sandboxes: provided as "sandbox/<name>" by provider plugins ----

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

// Process is a long-running command (e.g. `eip serve --stdio` inside a box).
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

// pluginSandboxes is what Service("sandbox/<name>") hands a plugin: the
// same provider, but its sandboxes have no Upload / Download. Those copy
// between a host path and the box, so a plugin without fs permissions
// could use them to read or write anywhere on the host. Only the host
// application (WithRunIn) moves files across that boundary; plugins use
// ReadFile / WriteFile inside the box.
func pluginSandboxes(p *SandboxProvider) *SandboxProvider {
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
			if err := CheckSandboxSpec(spec); err != nil {
				return nil, err
			}
			return strip(p.Create(ctx, spec))
		}
	}
	if p.Attach != nil {
		cp.Attach = func(ctx context.Context, id string) (*Sandbox, error) { return strip(p.Attach(ctx, id)) }
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

// CheckSandboxSpec applies CheckSandboxEnv and refuses mounts that would
// hand the box the host itself: "/" or a container runtime socket (with
// it a box could start containers outside every rule here).
func CheckSandboxSpec(spec SandboxSpec) error {
	if err := CheckSandboxEnv(spec.Env); err != nil {
		return err
	}
	for _, m := range spec.Mounts {
		h := filepath.Clean(m.Host)
		base := filepath.Base(h)
		if h == "/" || (strings.HasSuffix(base, ".sock") && (strings.Contains(base, "docker") || strings.Contains(base, "podman") || strings.Contains(base, "containerd"))) {
			return fmt.Errorf("sandbox mount %s is not allowed (host root or container runtime socket)", m.Host)
		}
	}
	return nil
}

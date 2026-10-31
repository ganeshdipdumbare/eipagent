package sdk

import (
	"context"
	"io"
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
// the host workspace (exec_local) or a per-thread sandbox (exec_sandbox).
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
		cp := *b
		cp.Upload, cp.Download = nil, nil
		return &cp, err
	}
	cp := *p
	if p.Create != nil {
		cp.Create = func(ctx context.Context, spec SandboxSpec) (*Sandbox, error) { return strip(p.Create(ctx, spec)) }
	}
	if p.Attach != nil {
		cp.Attach = func(ctx context.Context, id string) (*Sandbox, error) { return strip(p.Attach(ctx, id)) }
	}
	return &cp
}

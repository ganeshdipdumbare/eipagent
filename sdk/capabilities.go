package sdk

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ---- Network ----

// HTTPOptions configures a module's HTTP client. AllowHosts is required:
// an empty list denies every request.
type HTTPOptions struct {
	// AllowHosts lists hosts a request may go to: "api.example.com" or a
	// glob such as "*.example.com". Matching is case-insensitive.
	AllowHosts []string
	// Timeout bounds one request. Zero means 60 seconds.
	Timeout time.Duration
	// AllowPrivate lets requests reach loopback, private, link-local and
	// unspecified addresses (a local model server, a test server). Cloud
	// metadata addresses are refused regardless.
	AllowPrivate bool
	// AllowInsecure lets plain http reach a non-loopback host. Credentials
	// over plain http are refused unless this is set.
	AllowInsecure bool
}

// HTTPClient is the only way a module talks to the network. It refuses
// hosts outside AllowHosts, private addresses (checked when connecting,
// so DNS rebinding can't bypass it), redirects, and plain http to remote
// hosts unless allowed.
type HTTPClient struct {
	client *http.Client
	opts   HTTPOptions
}

// HTTP returns an HTTP client limited to opts. It needs the "net"
// capability.
func (c *Scope) HTTP(opts HTTPOptions) (*HTTPClient, error) {
	if err := c.requireGrant(PermNet); err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("refusing to connect to non-IP address %q", host)
		}
		return checkAddress(ip, opts.AllowPrivate)
	}}
	tr := &http.Transport{
		Proxy:                 nil, // no proxy: it would route around the host checks
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: opts.Timeout,
	}
	return &HTTPClient{
		opts: opts,
		client: &http.Client{
			Transport: tr,
			Timeout:   opts.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("sdk: redirects are refused; request the final URL")
			},
		},
	}, nil
}

// egressBridge, when set, carries a box's HTTP requests to its host instead
// of dialing from inside the box (see ServeBox). It is process-wide because
// a box runs one agent.
var egressBridge atomic.Value // func(*http.Request) (*http.Response, error)

// SetEgressBridge routes every broker HTTP request through fn. Pass nil to
// dial directly again. Only a box sets this; the host never does.
func SetEgressBridge(fn func(*http.Request) (*http.Response, error)) {
	if fn == nil {
		egressBridge.Store((func(*http.Request) (*http.Response, error))(nil))
		return
	}
	egressBridge.Store(fn)
}

func bridgedHTTP() func(*http.Request) (*http.Response, error) {
	fn, _ := egressBridge.Load().(func(*http.Request) (*http.Response, error))
	return fn
}

// Do sends req if its scheme and host pass the module's HTTP options.
func (h *HTTPClient) Do(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("sdk: nil request")
	}
	host := strings.ToLower(req.URL.Hostname())
	if !hostAllowed(host, h.opts.AllowHosts) {
		return nil, fmt.Errorf("sdk: host %q is not in AllowHosts", host)
	}
	switch req.URL.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(host) && !h.opts.AllowInsecure {
			return nil, fmt.Errorf("sdk: plain http to %q is refused (use https or AllowInsecure)", host)
		}
	default:
		return nil, fmt.Errorf("sdk: scheme %q is not allowed", req.URL.Scheme)
	}
	if fn := bridgedHTTP(); fn != nil {
		return fn(req)
	}
	return h.client.Do(req)
}

// Listen opens a listener for the module's own server (a webhook). It
// needs the "net" capability and binds to loopback unless allowAny is
// true.
func (c *Scope) Listen(addr string, allowAny bool) (net.Listener, error) {
	if err := c.requireGrant(PermNet); err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if !allowAny && !isLoopbackHost(host) {
		return nil, fmt.Errorf("sdk: listening on %q needs allowAny", addr)
	}
	return net.Listen("tcp", addr)
}

// checkAddress refuses addresses an agent should not reach by default.
func checkAddress(ip net.IP, allowPrivate bool) error {
	if ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return errors.New("sdk: cloud metadata address refused")
	}
	if allowPrivate {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("sdk: connection to private address %s is refused (set AllowPrivate to allow)", ip)
	}
	for _, n := range reservedNets {
		if n.Contains(ip) {
			return fmt.Errorf("sdk: connection to reserved address %s (%s) is refused (set AllowPrivate to allow)", ip, n)
		}
	}
	return nil
}

// reservedNets are ranges that are never public internet destinations:
// "this network", carrier-grade NAT (which includes some cloud metadata
// services), documentation and benchmarking blocks, and class E.
var reservedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"} {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hostAllowed(host string, allow []string) bool {
	for _, pat := range allow {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if pat == "" {
			continue
		}
		if ok, _ := path.Match(pat, host); ok {
			return true
		}
	}
	return false
}

// ---- Directories ----

// Dir is a directory a module may read (and write, if granted). Paths are
// relative to the root; os.Root refuses ".." and symlinks that leave it.
type Dir struct {
	root  *os.Root
	name  string
	write bool
}

// Dir opens root for the module. It needs "fs:read", and "fs:write" when
// write is true. The root must exist and must not be the filesystem root.
func (c *Scope) Dir(root string, write bool) (*Dir, error) {
	if err := c.requireGrant(PermFSRead); err != nil {
		return nil, err
	}
	if write {
		if err := c.requireGrant(PermFSWrite); err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(abs) == abs {
		return nil, fmt.Errorf("sdk: refusing to expose the filesystem root %q", abs)
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &Dir{root: r, name: abs, write: write}, nil
}

// Name is the absolute path of the root.
func (d *Dir) Name() string { return d.name }

// Close releases the root handle.
func (d *Dir) Close() error { return d.root.Close() }

// ReadFile reads a file under the root.
func (d *Dir) ReadFile(name string) ([]byte, error) { return d.root.ReadFile(name) }

// ReadDir lists a directory under the root.
func (d *Dir) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(d.root.FS(), name) }

// Stat describes a path under the root.
func (d *Dir) Stat(name string) (fs.FileInfo, error) { return d.root.Stat(name) }

// Open opens a file for reading under the root.
func (d *Dir) Open(name string) (*os.File, error) { return d.root.Open(name) }

// WriteFile writes a file under the root. It needs "fs:write".
func (d *Dir) WriteFile(name string, data []byte, perm fs.FileMode) error {
	if !d.write {
		return errors.New("sdk: directory was opened read-only")
	}
	return d.root.WriteFile(name, data, perm)
}

// MkdirAll creates a directory under the root. It needs "fs:write".
func (d *Dir) MkdirAll(name string, perm fs.FileMode) error {
	if !d.write {
		return errors.New("sdk: directory was opened read-only")
	}
	return d.root.MkdirAll(name, perm)
}

// Remove deletes a file or empty directory under the root. It needs "fs:write".
func (d *Dir) Remove(name string) error {
	if !d.write {
		return errors.New("sdk: directory was opened read-only")
	}
	return d.root.Remove(name)
}

// Lstat describes a path under the root without following a final symlink.
func (d *Dir) Lstat(name string) (fs.FileInfo, error) { return d.root.Lstat(name) }

// SameFile reports whether a and b (paths under the root, following
// symlinks inside it) name the same file.
func (d *Dir) SameFile(a, b string) (bool, error) {
	fa, err := d.root.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := d.root.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// Rename moves a file under the root, replacing the target atomically. It
// needs "fs:write".
func (d *Dir) Rename(oldName, newName string) error {
	if !d.write {
		return errors.New("sdk: directory was opened read-only")
	}
	return d.root.Rename(oldName, newName)
}

// AppendFile is a file opened for appending by Dir.AppendFile.
type AppendFile interface {
	io.WriteCloser
	Stat() (fs.FileInfo, error)
	Sync() error
	Truncate(size int64) error
}

// AppendFile opens name for appending, creating it if missing. It needs
// "fs:write".
func (d *Dir) AppendFile(name string, perm fs.FileMode) (AppendFile, error) {
	if !d.write {
		return nil, errors.New("sdk: directory was opened read-only")
	}
	f, err := d.root.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, perm)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// CreateNew creates an empty file under the root and fails if it already
// exists (an exclusive lock file). It needs "fs:write".
func (d *Dir) CreateNew(name string, perm fs.FileMode) error {
	if !d.write {
		return errors.New("sdk: directory was opened read-only")
	}
	f, err := d.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	return f.Close()
}

// ---- Processes ----

// Exec runs child processes from a fixed list of program names. Nothing
// runs through a shell, the child environment is filtered, and a program
// outside the list is refused. It needs the "exec" capability.
type Exec struct {
	allowed map[string]bool
}

// ProcOptions adds to one run.
type ProcOptions struct {
	Dir   string            // working directory; "" = the process's
	Stdin []byte            // standard input
	Env   map[string]string // extra variables; secret-looking names are refused
	// Secrets are values the module resolved through the secrets service
	// (p.SecretValue), which is audited and grant-checked. They may have
	// secret-looking names. Raw environment variables never reach a child.
	Secrets map[string]string
}

// ProcResult is the outcome of one run.
type ProcResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Exec returns a runner that may start the named programs.
func (c *Scope) Exec(programs ...string) (*Exec, error) {
	if err := c.requireGrant(PermExec); err != nil {
		return nil, err
	}
	x := &Exec{allowed: map[string]bool{}}
	for _, p := range programs {
		if p == "" || strings.ContainsAny(p, " \t\n;|&$`<>()") {
			return nil, fmt.Errorf("sdk: invalid program name %q", p)
		}
		x.allowed[p] = true
	}
	return x, nil
}

// Run starts program with args. Program must be on the allow list.
func (x *Exec) Run(ctx context.Context, program string, args []string, opts ProcOptions) (ProcResult, error) {
	if !x.allowed[program] {
		return ProcResult{}, fmt.Errorf("sdk: program %q is not allowed", program)
	}
	bin, err := exec.LookPath(program)
	if err != nil {
		return ProcResult{}, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = opts.Dir
	cmd.Env = childEnv(opts)
	if opts.Stdin != nil {
		cmd.Stdin = bytes.NewReader(opts.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	res := ProcResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return res, nil // a non-zero exit is a result, not a failure of the broker
		}
		return res, err
	}
	return res, nil
}

// Proc is a running child started by Exec.Start. Stdin, Stdout and Stderr
// are the child's pipes. Read Stdout and Stderr until EOF before calling
// Wait: Wait closes the pipes.
type Proc struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
	Pid    int
	cmd    *exec.Cmd
}

// Start starts program with args and returns without waiting. Program must
// be on the allow list; the environment and the no-shell rule are the same
// as Run. Input goes through Proc.Stdin, so opts.Stdin must be nil.
func (x *Exec) Start(ctx context.Context, program string, args []string, opts ProcOptions) (*Proc, error) {
	if opts.Stdin != nil {
		return nil, errors.New("sdk: Start takes no ProcOptions.Stdin; write to Proc.Stdin")
	}
	if !x.allowed[program] {
		return nil, fmt.Errorf("sdk: program %q is not allowed", program)
	}
	bin, err := exec.LookPath(program)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = opts.Dir
	cmd.Env = childEnv(opts)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Proc{Stdin: stdin, Stdout: stdout, Stderr: stderr, Pid: cmd.Process.Pid, cmd: cmd}, nil
}

// Wait waits for the child to exit and releases its pipes. A non-zero exit
// is returned as an error; ExitStatus reads its code.
func (p *Proc) Wait() error { return p.cmd.Wait() }

// Kill stops the child.
func (p *Proc) Kill() error {
	if p.cmd.Process == nil {
		return errors.New("sdk: process not started")
	}
	return p.cmd.Process.Kill()
}

// ExitStatus reports the exit code carried by an error from Proc.Wait. ok is
// false when err is not an exit status (for example, a failed wait).
func ExitStatus(err error) (code int, ok bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// childEnv builds the environment for a child: a few system variables,
// the explicit extras, and the resolved secrets. Secret-looking names are
// refused in Env, so a child can only receive a secret through Secrets.
func childEnv(opts ProcOptions) []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TERM", "TMPDIR", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range opts.Env {
		if SecretLike(k) {
			continue // a caller must pass secrets through the secrets service
		}
		env = append(env, k+"="+v)
	}
	for k, v := range opts.Secrets {
		env = append(env, k+"="+v)
	}
	return env
}

// ---- Environment ----

// Env reads an environment variable. It needs "env". Secret-looking names
// (SecretLike) also need "env:secrets".
func (c *Scope) Env(name string) (string, error) {
	if !c.Granted(PermEnv) && !c.Granted(PermEnvSecrets) {
		return "", c.requireGrant(PermEnv)
	}
	if SecretLike(name) && !c.Granted(PermEnvSecrets) {
		return "", fmt.Errorf("sdk: variable %q looks like a secret; it needs %q", name, PermEnvSecrets)
	}
	return os.Getenv(name), nil
}

// ---- Database ----

// DB wraps the database the db provider module opened. Modules that store
// data use it through the "db" service instead of importing database/sql.
type DB struct {
	db *sql.DB
}

// NewDB wraps an open database. Only the provider module calls it.
func NewDB(db *sql.DB) *DB { return &DB{db: db} }

// Exec runs a statement and returns the number of rows affected.
func (d *DB) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := d.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Query runs a query and returns each row as a map keyed by column name.
func (d *DB) Query(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// DB returns the "db" service. It needs the "db" capability.
func (c *Scope) DB() (*DB, error) {
	if err := c.requireGrant(PermDB); err != nil {
		return nil, err
	}
	d, ok := c.Service("db").(*DB)
	if !ok || d == nil {
		return nil, errors.New("sdk: no \"db\" service is provided; load a db module")
	}
	return d, nil
}

package sdk

import (
	"io"
	"os/exec"
	"sort"
	"strings"
)

// Command is the safe command handle handed to interpreted modules that
// hold exec. It has no exported Env field, so a module cannot reset the
// environment to nil and make the child inherit the agent's credentials.
type Command struct {
	cmd          *exec.Cmd
	allowSecrets bool
}

// NewCommand wraps a command for an interpreted module. env is the
// already-filtered base environment; allowSecrets is true only when the
// module declared env:secrets.
func NewCommand(cmd *exec.Cmd, env []string, allowSecrets bool) *Command {
	cmd.Env = append([]string(nil), env...)
	return &Command{cmd: cmd, allowSecrets: allowSecrets}
}

func (c *Command) Run() error                      { return c.cmd.Run() }
func (c *Command) Start() error                    { return c.cmd.Start() }
func (c *Command) Wait() error                     { return c.cmd.Wait() }
func (c *Command) Kill() error                     { return c.cmd.Process.Kill() }
func (c *Command) Output() ([]byte, error)         { return c.cmd.Output() }
func (c *Command) CombinedOutput() ([]byte, error) { return c.cmd.CombinedOutput() }
func (c *Command) SetDir(dir string)               { c.cmd.Dir = dir }
func (c *Command) SetStdin(r io.Reader)            { c.cmd.Stdin = r }
func (c *Command) SetStdout(w io.Writer)           { c.cmd.Stdout = w }
func (c *Command) SetStderr(w io.Writer)           { c.cmd.Stderr = w }

// SetEnv replaces the child environment. Credential-looking names are
// dropped unless the module declared env:secrets.
func (c *Command) SetEnv(env []string) {
	c.cmd.Env = c.filter(env)
}

// SetEnvMap is the map form of SetEnv.
func (c *Command) SetEnvMap(env map[string]string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	c.cmd.Env = c.filter(out)
}

// Environ returns a copy of the child environment.
func (c *Command) Environ() []string { return append([]string(nil), c.cmd.Env...) }

func (c *Command) filter(env []string) []string {
	if c.allowSecrets {
		return append([]string(nil), env...)
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == "" || SecretLike(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

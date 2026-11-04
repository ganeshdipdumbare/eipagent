package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"

	"github.com/traefik/yaegi/stdlib"

	"github.com/ganeshdipdumbare/eipagent/engine/symbols"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// Permissions a plugin can declare with `var Permissions = []string{...}`.
// A plugin gets only the stdlib packages (and os symbols) its granted
// permissions allow. This is defense in depth, not a hard boundary: for
// untrusted code run the agent inside a sandbox.
const (
	PermNet     = "net"      // net, net/http, crypto/tls ...
	PermExec    = "exec"     // os/exec, os.StartProcess
	PermFSRead  = "fs:read"  // read files and directories
	PermFSWrite = "fs:write" // create / modify / remove files
	PermEnv     = "env"      // environment variables, minus secret-looking ones
	PermDB      = "db"       // database/sql
	// PermEnvSecrets also exposes variables whose names look like
	// credentials (*_API_KEY, *_TOKEN, *PASSWORD* ...). Only secret
	// providers (secrets_env, secrets_vault) should hold it; every other
	// plugin gets secrets through grants.
	PermEnvSecrets = "env:secrets"
)

var AllPerms = []string{PermNet, PermExec, PermFSRead, PermFSWrite, PermEnv, PermDB, PermEnvSecrets}

// pure packages every plugin may import.
var pure = []string{
	"bufio", "bytes", "cmp", "container/heap", "container/list", "container/ring", "context",
	"crypto", "crypto/aes", "crypto/cipher", "crypto/ecdsa", "crypto/ed25519", "crypto/elliptic",
	"crypto/hmac", "crypto/md5", "crypto/rand", "crypto/rsa", "crypto/sha1", "crypto/sha256",
	"crypto/sha512", "crypto/subtle", "encoding", "encoding/base32", "encoding/base64",
	"encoding/binary", "encoding/csv", "encoding/hex", "encoding/json", "encoding/pem",
	"encoding/xml", "errors", "fmt", "hash", "hash/adler32", "hash/crc32", "hash/fnv", "html",
	"html/template", "io", "log", "log/slog", "maps", "math", "math/big", "math/bits", "math/rand",
	"math/rand/v2", "mime", "net/url", "path", "regexp", "slices", "sort",
	"strconv", "strings", "sync", "sync/atomic", "text/tabwriter", "text/template", "time",
	"unicode", "unicode/utf16", "unicode/utf8", "net/mail", "mime/quotedprintable",
	"io/ioutil", // per symbol: see symPerms
}

var permPkgs = map[string][]string{
	// net/textproto can dial (textproto.Dial), so it needs net too.
	PermNet:     {"net", "net/http", "net/http/httputil", "net/http/cookiejar", "net/http/httptrace", "net/netip", "net/smtp", "net/textproto", "crypto/tls", "crypto/x509", "crypto/x509/pkix"},
	PermExec:    {"os/exec", "os/signal"},
	PermFSRead:  {"io/fs", "path/filepath", "archive/tar", "archive/zip", "compress/gzip", "compress/flate", "compress/zlib"},
	PermFSWrite: {"mime/multipart"}, // Reader.ReadForm spills large parts to temp files
	PermDB:      {"database/sql", "database/sql/driver"},
}

// symPerms gates single symbols of otherwise importable packages: the
// symbol is only in a plugin's table when it holds the permission.
var symPerms = map[string]map[string]string{
	"io/ioutil/ioutil": {"ReadFile": PermFSRead, "ReadDir": PermFSRead, "WriteFile": PermFSWrite, "TempFile": PermFSWrite, "TempDir": PermFSWrite},
	// net gives the network, not the disk: these open host files by path.
	"net/http/http":  {"Dir": PermFSRead, "ServeFile": PermFSRead},
	"crypto/tls/tls": {"LoadX509KeyPair": PermFSRead},
}

// os is split per symbol.
var osSyms = map[string][]string{
	PermEnv:        {"Getenv", "LookupEnv", "Environ", "ExpandEnv", "Hostname", "UserHomeDir", "TempDir", "Getwd"},
	PermEnvSecrets: {"Getenv", "LookupEnv", "Environ", "ExpandEnv"},
	PermFSRead:     {"Open", "ReadFile", "ReadDir", "Stat", "Lstat", "DirFS", "Readlink", "SameFile", "Getwd", "TempDir", "UserHomeDir"},
	PermFSWrite:    {"Create", "CreateTemp", "WriteFile", "Mkdir", "MkdirAll", "MkdirTemp", "Remove", "RemoveAll", "Rename", "OpenFile", "Chmod", "Chtimes", "Truncate"},
	PermExec:       {"StartProcess", "FindProcess", "Getpid", "Executable", "ProcAttr", "Process", "ProcessState", "Signal", "Interrupt", "Kill"},
}

// os symbols that never need a permission (types, errors, flags).
func osAlwaysOK(name string) bool {
	switch name {
	case "Exit", "Setenv", "Unsetenv", "Clearenv", "Chdir", "Chown", "Lchown":
		return false
	}
	if strings.HasPrefix(name, "Err") || strings.HasPrefix(name, "O_") || strings.HasPrefix(name, "Mode") || strings.HasPrefix(name, "Is") || strings.HasPrefix(name, "Path") {
		return true
	}
	switch name {
	// Not NewFile (it wraps any open descriptor of the agent process) and
	// not Stdin (in `eip serve --stdio` it carries the RPC protocol).
	case "File", "FileInfo", "FileMode", "DirEntry", "LinkError", "PathError", "SyscallError", "DevNull", "Args":
		return true
	}
	return false
}

// importPerm says which permission an import needs ("" = pure, "-" = never).
func importPerm(pkg string) string {
	for _, p := range pure {
		if p == pkg {
			return ""
		}
	}
	for perm, pkgs := range permPkgs {
		for _, p := range pkgs {
			if p == pkg {
				return perm
			}
		}
	}
	if pkg == "os" || pkg == SDKImport || pkg == sdkModulePath {
		return ""
	}
	return "-"
}

// Plugins import the SDK as "eipagent/sdk" (SDKImport), a short alias
// that stays the same whatever the Go module path is, so plugin files
// never change when the module moves. The full path works too.
const (
	SDKImport     = "eipagent/sdk"
	sdkModulePath = "github.com/ganeshdipdumbare/eipagent/sdk"
)

// builtinOnly are sdk symbols only built-in (embedded) plugins get.
// AddRedaction would let a plugin blank out words in everyone's audit
// records. Redact* would let it test whether a string it holds is a
// registered secret: any function that redacts is such an oracle, so
// third-party plugins don't get one. Their logs are redacted for them
// (Context.Logf), and sinks are built in or compiled.
var builtinOnly = map[string]bool{"Redact": true, "RedactMap": true, "RedactValue": true, "RedactMessages": true, "RedactInterrupts": true, "RedactThreadState": true, "PluginRenames": true}

// buildSymbols returns the symbol table for a plugin holding perms.
// builtin is true for plugins embedded in the binary. writeDeny lists
// paths holding the agent's own configuration and plugin store, which
// fs:write may not modify.
func buildSymbols(perms []string, builtin bool, writeDeny []string) map[string]map[string]reflect.Value {
	has := map[string]bool{}
	for _, p := range perms {
		has[p] = true
	}
	allowed := map[string]bool{}
	for _, p := range pure {
		allowed[p] = true
	}
	for perm, pkgs := range permPkgs {
		if has[perm] {
			for _, p := range pkgs {
				allowed[p] = true
			}
		}
	}
	out := map[string]map[string]reflect.Value{}
	for key, syms := range stdlib.Symbols {
		i := strings.LastIndex(key, "/")
		if i < 0 {
			continue
		}
		path := key[:i]
		if path == "os" {
			continue
		}
		if !allowed[path] {
			continue
		}
		if gate, ok := symPerms[key]; ok {
			cp := map[string]reflect.Value{}
			for name, v := range syms {
				if need, gated := gate[name]; !gated || has[need] {
					cp[name] = v
				}
			}
			syms = cp
		}
		if !builtin {
			switch path {
			case "path/filepath":
				delete(syms, "Walk")
				delete(syms, "WalkDir")
			case "net/http":
				delete(syms, "Dir")
				delete(syms, "ServeFile")
				delete(syms, "ServeContent")
				delete(syms, "FileServer")
				delete(syms, "FileServerFS")
			}
		}
		out[key] = syms
	}
	// os: filtered per symbol.
	osAllowed := map[string]bool{}
	for perm, names := range osSyms {
		if has[perm] {
			for _, n := range names {
				osAllowed[n] = true
			}
		}
	}
	filtered := map[string]reflect.Value{}
	for name, v := range stdlib.Symbols["os/os"] {
		if osAlwaysOK(name) || osAllowed[name] {
			filtered[name] = v
		}
	}
	guardOSSymbols(filtered, has, writeDeny)
	if has[PermExec] && !builtin {
		filtered["StartProcess"] = reflect.ValueOf(func(name string, argv []string, attr *os.ProcAttr) (*os.Process, error) {
			if attr == nil {
				attr = &os.ProcAttr{Env: childEnv(perms)}
			} else {
				cp := *attr
				cp.Env = sanitizeCmdEnv(cp.Env, perms)
				attr = &cp
			}
			return os.StartProcess(name, argv, attr)
		})
	}
	out["os/os"] = filtered
	guardIoutilSymbols(out, has, writeDeny)
	if has[PermExec] {
		execSyms := map[string]reflect.Value{
			"ErrNotFound": reflect.ValueOf(&exec.ErrNotFound).Elem(),
			"LookPath":    reflect.ValueOf(exec.LookPath),
			"Error":       reflect.ValueOf((*exec.Error)(nil)),
			"ExitError":   reflect.ValueOf((*exec.ExitError)(nil)),
		}
		if builtin {
			execSyms["Command"] = reflect.ValueOf(exec.Command)
			execSyms["CommandContext"] = reflect.ValueOf(exec.CommandContext)
			execSyms["Cmd"] = reflect.ValueOf((*exec.Cmd)(nil))
		} else {
			allowSecrets := false
			for _, p := range perms {
				allowSecrets = allowSecrets || p == PermEnvSecrets
			}
			delete(execSyms, "Cmd")
			execSyms["Command"] = reflect.ValueOf(func(name string, args ...string) *sdk.Command {
				return sdk.NewCommand(exec.Command(name, args...), childEnv(perms), allowSecrets)
			})
			execSyms["CommandContext"] = reflect.ValueOf(func(c context.Context, name string, args ...string) *sdk.Command {
				return sdk.NewCommand(exec.CommandContext(c, name, args...), childEnv(perms), allowSecrets)
			})
		}
		out["os/exec/exec"] = execSyms
	}
	for k, v := range symbols.Symbols {
		if strings.HasPrefix(k, sdkModulePath+"/") {
			// No plugin may add redaction rules; only built-in ones may
			// redact (see builtinOnly).
			cp := make(map[string]reflect.Value, len(v))
			for name, sym := range v {
				if name != "AddRedaction" && (builtin || !builtinOnly[name]) {
					cp[name] = sym
				}
			}
			v = cp
		}
		out[k] = v
		if strings.HasPrefix(k, sdkModulePath+"/") {
			out[SDKImport+"/sdk"] = v // the alias plugins import
		}
	}
	return out
}

// checkImports verifies every import is covered by perms.
func checkImports(name string, imports, perms []string) error {
	has := map[string]bool{}
	for _, p := range perms {
		has[p] = true
	}
	var bad []string
	for _, imp := range imports {
		switch need := importPerm(imp); need {
		case "":
		case "-":
			bad = append(bad, fmt.Sprintf("%s (not available to plugins)", imp))
		default:
			if !has[need] {
				bad = append(bad, fmt.Sprintf("%s (needs permission %q)", imp, need))
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("plugin %s: import not permitted: %s", name, strings.Join(bad, ", "))
	}
	return nil
}

// pluginEnv is the environment an interpreted plugin sees: nothing
// without env, everything but secret-looking variables with env, and
// everything with env:secrets.
func pluginEnv(perms []string) []string {
	has := map[string]bool{}
	for _, p := range perms {
		has[p] = true
	}
	switch {
	case has[PermEnvSecrets]:
		return os.Environ()
	case has[PermEnv]:
		var out []string
		for _, kv := range os.Environ() {
			if name, _, _ := strings.Cut(kv, "="); !SecretLike(name) {
				out = append(out, kv)
			}
		}
		return out
	}
	return []string{}
}

// SecretLike reports whether an environment variable name looks like a
// credential, which plugins with only the env permission don't see.
func SecretLike(name string) bool { return sdk.SecretLike(name) }

// childEnv is the environment an exec-only plugin may pass to a child.
// A plugin without env permission still gets PATH so running a command
// works, but never the agent's credentials.
func childEnv(perms []string) []string {
	env := pluginEnv(perms)
	if len(env) == 0 {
		if p := os.Getenv("PATH"); p != "" {
			env = []string{"PATH=" + p}
		}
	}
	return env
}

func sanitizeCmdEnv(env []string, perms []string) []string {
	hasSecrets := false
	for _, p := range perms {
		if p == PermEnvSecrets {
			hasSecrets = true
		}
	}
	if env == nil {
		return childEnv(perms)
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == "" || (!hasSecrets && SecretLike(name)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if in[x] || in["*"] {
			out = append(out, x)
		}
	}
	return out
}

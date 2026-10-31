package engine

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"

	"github.com/traefik/yaegi/stdlib"

	"eipagent/engine/symbols"
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
	"math/rand/v2", "mime", "mime/multipart", "net/url", "path", "regexp", "slices", "sort",
	"strconv", "strings", "sync", "sync/atomic", "text/tabwriter", "text/template", "time",
	"unicode", "unicode/utf16", "unicode/utf8", "net/mail", "net/textproto", "mime/quotedprintable",
}

var permPkgs = map[string][]string{
	PermNet:    {"net", "net/http", "net/http/httputil", "net/http/cookiejar", "net/http/httptrace", "net/netip", "net/smtp", "crypto/tls", "crypto/x509", "crypto/x509/pkix"},
	PermExec:   {"os/exec", "os/signal"},
	PermFSRead: {"io/fs", "path/filepath", "io/ioutil", "archive/tar", "archive/zip", "compress/gzip", "compress/flate", "compress/zlib"},
	PermDB:     {"database/sql", "database/sql/driver"},
}

// os is split per symbol.
var osSyms = map[string][]string{
	PermEnv:        {"Getenv", "LookupEnv", "Environ", "ExpandEnv", "Hostname", "UserHomeDir", "TempDir", "Getwd"},
	PermEnvSecrets: {"Getenv", "LookupEnv", "Environ", "ExpandEnv"},
	PermFSRead:     {"Open", "ReadFile", "ReadDir", "Stat", "Lstat", "DirFS", "Readlink", "SameFile", "Getwd", "TempDir", "UserHomeDir"},
	PermFSWrite:    {"Create", "CreateTemp", "WriteFile", "Mkdir", "MkdirAll", "MkdirTemp", "Remove", "RemoveAll", "Rename", "OpenFile", "Chmod", "Chtimes", "Truncate", "Symlink", "Link", "CopyFS"},
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
	case "File", "FileInfo", "FileMode", "DirEntry", "LinkError", "PathError", "SyscallError", "DevNull", "Stdin", "Stdout", "Stderr", "NewFile", "Args":
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
	if pkg == "os" || pkg == "eipagent/sdk" {
		return ""
	}
	return "-"
}

// builtinOnly are sdk symbols only built-in (embedded) plugins get.
// AddRedaction would let a plugin blank out words in everyone's audit
// records. Redact* would let it test whether a string it holds is a
// registered secret: any function that redacts is such an oracle, so
// third-party plugins don't get one. Their logs are redacted for them
// (Context.Logf), and sinks are built in or compiled.
var builtinOnly = map[string]bool{"Redact": true, "RedactMap": true, "RedactValue": true}

// buildSymbols returns the symbol table for a plugin holding perms.
// builtin is true for plugins embedded in the binary.
func buildSymbols(perms []string, builtin bool) map[string]map[string]reflect.Value {
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
		if allowed[path] {
			out[key] = syms
		}
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
	if osAllowed["OpenFile"] && !has[PermFSRead] {
		// fs:write alone must not read: OpenFile only for writing.
		filtered["OpenFile"] = reflect.ValueOf(writeOnlyOpenFile)
	}
	out["os/os"] = filtered
	if has[PermExec] {
		out["os/exec/exec"] = map[string]reflect.Value{
			"Command":        reflect.ValueOf(exec.Command),
			"CommandContext": reflect.ValueOf(exec.CommandContext),
			"ErrNotFound":    reflect.ValueOf(&exec.ErrNotFound).Elem(),
			"LookPath":       reflect.ValueOf(exec.LookPath),
			"Cmd":            reflect.ValueOf((*exec.Cmd)(nil)),
			"Error":          reflect.ValueOf((*exec.Error)(nil)),
			"ExitError":      reflect.ValueOf((*exec.ExitError)(nil)),
		}
	}
	for k, v := range symbols.Symbols {
		if strings.HasPrefix(k, "eipagent/sdk/") {
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
	}
	return out
}

// writeOnlyOpenFile is os.OpenFile for plugins with fs:write but not fs:read.
func writeOnlyOpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR) != os.O_WRONLY {
		return nil, fmt.Errorf("open %s: reading files needs the fs:read permission (use os.O_WRONLY)", name)
	}
	return os.OpenFile(name, flag, perm)
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
func SecretLike(name string) bool {
	n := strings.ToUpper(name)
	for _, s := range []string{"_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_PASS", "_PASSWD", "_DSN", "_CREDENTIALS", "_CREDENTIAL", "_PAT"} {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	for _, s := range []string{"PASSWORD", "SECRET", "TOKEN", "APIKEY", "API_KEY", "PRIVATE_KEY", "DATABASE_URL"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
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

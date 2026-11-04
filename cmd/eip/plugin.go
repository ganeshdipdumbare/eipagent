package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/pluginpkg"
)

const pluginUsage = `usage: eip plugin <command> [flags]

authoring:
  init [-template T] [-dir DIR] NAME      scaffold a plugin; asks what it should do (templates: %s)
  validate [-json] PATH...                lint a plugin dir or .go file, and say what it does
  fix PATH...                             rewrite names renamed in v0.4 (sdk.Context, Inject, old services)
  build [-key FILE] [-out DIR] PATH       lint and pack PATH into NAME-VERSION.eip
  inspect [-json] PATH|PKG                show manifest, facts, files, signature
  keygen -out FILE                        make an ed25519 signing key (FILE, FILE.pub)
  pin -plugins DIR [-lock eip.lock]       pin plugin sources by sha256

lifecycle (store: -store DIR, default .eipagent/plugins):
  install [-sha256 X] [-approve] PKG      verify and hold a package (VALIDATED)
  approve NAME VERSION                    approve a version (APPROVED)
  enable NAME [VERSION]                   activate (ACTIVE); the old version is kept on failure
  disable NAME                            deactivate
  update [-sha256 X] [-approve] PKG       install and, if enabled and approved, switch to it
  rollback NAME                           switch back to the previous version
  quarantine -reason R NAME VERSION       pull a version from service until re-approved
  uninstall NAME                          remove every version
  list                                    show installed plugins and states

Signatures: packages must be signed by a key in DIR/trusted_keys (one public
key per line) or -trust FILE, unless -allow-unsigned is given.
`

func pluginCmd(args []string) error {
	if len(args) == 0 {
		fmt.Printf(pluginUsage, strings.Join(pluginpkg.TemplateNames(), ", "))
		return nil
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "init":
		return pluginInit(args)
	case "fix":
		return pluginFix(args)
	case "validate":
		return pluginValidate(args)
	case "build":
		return pluginBuild(args)
	case "inspect":
		return pluginInspect(args)
	case "keygen":
		return pluginKeygen(args)
	case "pin":
		return pluginPin(args)
	case "install", "update", "approve", "enable", "disable", "rollback", "quarantine", "uninstall", "list":
		return pluginLifecycle(sub, args)
	case "help", "-h", "--help":
		fmt.Printf(pluginUsage, strings.Join(pluginpkg.TemplateNames(), ", "))
		return nil
	}
	return fmt.Errorf("unknown plugin command %q (eip plugin help)", sub)
}

func pluginInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	kind := fs.String("template", "", "template: "+strings.Join(pluginpkg.TemplateNames(), ", ")+" (asked when omitted)")
	dir := fs.String("dir", "", "directory to create (default ./NAME)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: eip plugin init [-template tool] [-dir DIR] NAME")
	}
	name := fs.Arg(0)
	if *dir == "" {
		*dir = name
	}
	if *kind == "" {
		*kind = askGoal(name)
	}
	files, err := pluginpkg.Init(*dir, name, *kind)
	for _, f := range files {
		fmt.Println("created", f)
	}
	if err == nil {
		fmt.Printf("next: edit %s, then `eip plugin validate %s`\n", filepath.Join(*dir, name+".go"), *dir)
	}
	return err
}

// askGoal asks what the plugin should do (in a terminal); without one
// it picks a tool plugin, the most common kind.
func askGoal(name string) string {
	if st, err := os.Stdin.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return "tool"
	}
	fmt.Printf("What should %s do?\n", name)
	for i, k := range pluginpkg.GoalOrder {
		fmt.Printf("  %d. %s\n", i+1, pluginpkg.Templates[k])
	}
	fmt.Print("Choose 1-", len(pluginpkg.GoalOrder), " [1]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(pluginpkg.GoalOrder) {
		return pluginpkg.GoalOrder[n-1]
	}
	return "tool"
}

func pluginFix(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: eip plugin fix PATH... (a .go file or a plugin dir)")
	}
	for _, p := range args {
		files := []string{p}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			files, _ = filepath.Glob(filepath.Join(p, "*.go"))
			if mp := filepath.Join(p, pluginpkg.ManifestFile); fileExists(mp) {
				m, err := pluginpkg.ReadManifest(mp)
				if err != nil {
					return err
				}
				if pluginpkg.FixManifest(m) {
					if err := os.WriteFile(mp, m.Encode(), 0o644); err != nil {
						return err
					}
					fmt.Printf("%s: inject -> requires, current service names\n", mp)
				}
			}
		}
		for _, f := range files {
			src, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			out, notes, err := pluginpkg.Fix(src)
			if err != nil {
				return fmt.Errorf("%s: %v", f, err)
			}
			if len(notes) == 0 {
				fmt.Printf("%s: nothing to change\n", f)
				continue
			}
			if err := os.WriteFile(f, out, 0o644); err != nil {
				return err
			}
			fmt.Printf("%s: %s\n", f, strings.Join(notes, ", "))
		}
	}
	return nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func pluginValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print reports as JSON")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: eip plugin validate [-json] PATH...")
	}
	failed := 0
	var reports []*pluginpkg.Report
	for _, p := range fs.Args() {
		src, err := pluginpkg.ReadDir(p)
		if err != nil {
			return err
		}
		r := pluginpkg.Lint(src)
		reports = append(reports, r)
		if !r.OK() {
			failed++
		}
		if !*jsonOut {
			fmt.Printf("%s: %s\n", r.Name, r.State)
			for _, i := range r.Issues {
				fmt.Println("  " + i.String())
			}
			fmt.Println("  " + pluginpkg.Summary(r.Name, r.Facts))
			if src.Manifest == nil && r.Facts != nil {
				fmt.Printf("  suggested %s:\n%s", pluginpkg.ManifestFile, indent(string(pluginpkg.Infer(r.Name, r.Facts).Encode()), "    "))
			}
		}
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(reports)
	}
	if failed > 0 {
		return fmt.Errorf("%d plugin(s) failed validation", failed)
	}
	return nil
}

func pluginBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	keyPath := fs.String("key", "", "ed25519 private key to sign with (eip plugin keygen)")
	out := fs.String("out", ".", "output directory")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: eip plugin build [-key FILE] [-out DIR] PATH")
	}
	src, err := pluginpkg.ReadDir(fs.Arg(0))
	if err != nil {
		return err
	}
	b, rep, err := buildWithKey(src, *keyPath)
	for _, i := range rep.Issues {
		fmt.Println("  " + i.String())
	}
	if err != nil {
		return err
	}
	p := filepath.Join(*out, pluginpkg.FileName(rep.Manifest))
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return err
	}
	pkg, _ := pluginpkg.Open(b)
	signed := "unsigned"
	if pkg.Signature != nil {
		signed = "signed by " + pkg.Signature.KeyID
	}
	fmt.Printf("built %s (%s)\nsha256 %s\n", p, signed, pkg.SHA256)
	return nil
}

func buildWithKey(src *pluginpkg.Source, keyPath string) ([]byte, *pluginpkg.Report, error) {
	if keyPath == "" {
		return pluginpkg.Build(src, nil)
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, &pluginpkg.Report{}, err
	}
	sk, err := pluginpkg.ParsePrivateKey(string(kb))
	if err != nil {
		return nil, &pluginpkg.Report{}, err
	}
	return pluginpkg.Build(src, sk)
}

func pluginInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print as JSON")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: eip plugin inspect [-json] PATH|PKG")
	}
	p := fs.Arg(0)
	var src *pluginpkg.Source
	var pkg *pluginpkg.Package
	if strings.HasSuffix(p, ".eip") {
		var err error
		if pkg, err = pluginpkg.OpenFile(p); err != nil {
			return err
		}
		src = pkg.Source
	} else {
		var err error
		if src, err = pluginpkg.ReadDir(p); err != nil {
			return err
		}
	}
	r := pluginpkg.Lint(src)
	if *jsonOut {
		out := map[string]any{"report": r}
		if pkg != nil {
			out["sha256"], out["signature"] = pkg.SHA256, pkg.Signature
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	m := src.Manifest
	if m == nil && r.Facts != nil {
		m = pluginpkg.Infer(src.Name, r.Facts)
		fmt.Println("(no plugin.json: showing what the source declares)")
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	row := func(k string, v any) { fmt.Fprintf(w, "%s\t%v\n", k, v) }
	row("name", m.Name)
	row("version", m.Version)
	row("description", m.Description)
	row("state", r.State)
	row("permissions", list(m.Permissions))
	row("requires", list(m.Needs()))
	row("provides", list(m.Provides))
	row("tools", list(m.Tools))
	row("secrets", list(m.Secrets))
	row("eipagent", m.EIPAgent)
	if pkg != nil {
		row("package sha256", pkg.SHA256)
		if pkg.Signature != nil {
			row("signature", pkg.Signature.Alg+" key "+pkg.Signature.KeyID)
		} else {
			row("signature", "none")
		}
	}
	names := make([]string, 0, len(src.Files))
	for n := range src.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		row("file "+n, engine.Hash(src.Files[n])[:16]+"…")
	}
	w.Flush()
	fmt.Println(pluginpkg.Summary(src.Name, r.Facts))
	for _, i := range r.Issues {
		fmt.Println("  " + i.String())
	}
	return nil
}

func pluginKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "private key file (public key goes to FILE.pub)")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("usage: eip plugin keygen -out FILE")
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s exists", *out)
	}
	pub, priv, err := pluginpkg.GenerateKey()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(priv), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(*out+".pub", []byte(pub), 0o644); err != nil {
		return err
	}
	pk, _ := pluginpkg.ParsePublicKey(pub)
	fmt.Printf("wrote %s (keep it secret) and %s.pub (key id %s)\nadd the .pub line to <store>/trusted_keys to accept its packages\n", *out, *out, pluginpkg.KeyID(pk))
	return nil
}

func pluginPin(args []string) error {
	fs := flag.NewFlagSet("pin", flag.ExitOnError)
	var dirs multi
	fs.Var(&dirs, "plugins", "plugin directory (repeatable)")
	lockPath := fs.String("lock", "eip.lock", "lock file to write")
	fs.Parse(args)
	lock := &engine.Lock{Plugins: map[string]string{}}
	if err := engine.ReadJSON(*lockPath, lock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if lock.Plugins == nil {
		lock.Plugins = map[string]string{}
	}
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.go"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			name := strings.TrimSuffix(filepath.Base(f), ".go")
			lock.Plugins[name] = engine.Hash(b)
			fmt.Printf("pinned %-20s %s\n", name, lock.Plugins[name][:16])
		}
	}
	return engine.WriteLock(*lockPath, lock)
}

// openStore opens a store with its trusted keys (DIR/trusted_keys plus
// extra key files).
func openStore(dir string, trustFiles []string, allowUnsigned bool) (*pluginpkg.Store, error) {
	var lines []string
	read := func(p string) error {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
				lines = append(lines, l)
			}
		}
		return sc.Err()
	}
	if err := read(filepath.Join(dir, "trusted_keys")); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, t := range trustFiles {
		if err := read(t); err != nil {
			return nil, err
		}
	}
	keys, err := pluginpkg.TrustedKeys(lines...)
	if err != nil {
		return nil, err
	}
	return pluginpkg.OpenStore(dir, pluginpkg.StoreOptions{Verify: pluginpkg.VerifyOptions{TrustedKeys: keys, RequireSignature: !allowUnsigned, AllowUntrustedSignature: allowUnsigned}})
}

func pluginLifecycle(sub string, args []string) error {
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	dir := fs.String("store", ".eipagent/plugins", "plugin store directory")
	var trust multi
	fs.Var(&trust, "trust", "trusted public key file (repeatable)")
	allowUnsigned := fs.Bool("allow-unsigned", false, "accept unsigned packages (dev only)")
	sum := fs.String("sha256", "", "expected package sha256 (pin)")
	approve := fs.Bool("approve", false, "approve right after install")
	by := fs.String("by", os.Getenv("USER"), "who is acting (recorded in state and audit)")
	reason := fs.String("reason", "", "quarantine reason")
	fs.Parse(args)
	st, err := openStore(*dir, trust, *allowUnsigned)
	if err != nil {
		return err
	}
	a := fs.Args()
	need := func(n int, usage string) error {
		if len(a) < n {
			return fmt.Errorf("usage: eip plugin %s %s", sub, usage)
		}
		return nil
	}
	arg := func(i int) string {
		if i < len(a) {
			return a[i]
		}
		return ""
	}
	switch sub {
	case "install", "update":
		if err := need(1, "[-sha256 X] [-approve] PKG"); err != nil {
			return err
		}
		data, err := os.ReadFile(a[0])
		if err != nil {
			return err
		}
		pkg, err := pluginpkg.Open(data)
		if err != nil {
			return err
		}
		if *sum != "" && !strings.EqualFold(*sum, pkg.SHA256) {
			return fmt.Errorf("package sha256 %s does not match -sha256 %s", pkg.SHA256, *sum)
		}
		v, err := st.Install(data, *by)
		if err != nil {
			return err
		}
		name := v.Manifest.Name
		fmt.Printf("installed %s %s: %s (permissions %v)\n", name, v.Version, v.State, list(v.Manifest.Permissions))
		if *approve {
			if err := st.Approve(name, v.Version, *by); err != nil {
				return err
			}
			fmt.Printf("approved %s %s\n", name, v.Version)
		}
		if sub == "update" {
			if p, _ := st.Get(name); p.Active != "" && p.Active != v.Version {
				if err := st.Enable(name, v.Version); err != nil {
					return err
				}
				fmt.Printf("enabled %s %s (rollback target %s)\n", name, v.Version, p.Active)
			}
		}
		return nil
	case "approve":
		if err := need(2, "NAME VERSION"); err != nil {
			return err
		}
		return st.Approve(a[0], a[1], *by)
	case "enable":
		if err := need(1, "NAME [VERSION]"); err != nil {
			return err
		}
		return st.Enable(a[0], arg(1))
	case "disable":
		if err := need(1, "NAME"); err != nil {
			return err
		}
		return st.Disable(a[0])
	case "rollback":
		if err := need(1, "NAME"); err != nil {
			return err
		}
		return st.Rollback(a[0])
	case "quarantine":
		if err := need(2, "-reason R NAME VERSION"); err != nil {
			return err
		}
		return st.Quarantine(a[0], a[1], *reason)
	case "uninstall":
		if err := need(1, "NAME"); err != nil {
			return err
		}
		return st.Uninstall(a[0])
	case "list":
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tVERSION\tSTATE\tSIGNER\tSHA256")
		for _, p := range st.List() {
			vers := make([]string, 0, len(p.Versions))
			for v := range p.Versions {
				vers = append(vers, v)
			}
			sort.Strings(vers)
			for _, v := range vers {
				x := p.Versions[v]
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, v, x.State, x.Signer, x.SHA256[:12])
			}
		}
		return w.Flush()
	}
	return nil
}

func list(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ", ")
}

func indent(s, pre string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = pre + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

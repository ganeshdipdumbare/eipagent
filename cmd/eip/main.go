// Command eip is the eipagent CLI: an interactive REPL, a headless
// one-shot runner, the sandbox-side server, and plugin tooling.
//
//	eip [repl] -model openai:gpt-4.1 -profile eip.json
//	eip run -model ... "summarize README.md"
//	eip serve --stdio ...          (used inside sandboxes by WithRunIn)
//	eip plugin pin -plugins ./plugins -lock eip.lock
//	eip config dump -profile eip.json
//	eip trace view trace.jsonl > trace.html
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"eipagent"
	"eipagent/engine"
	"eipagent/plugins"
	"eipagent/sdk"

	_ "eipagent/drivers/postgres"
)

type common struct {
	model, profile, policy, lock, preset, user string
	dirs                                       multi
	plugins                                    multi
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func (c *common) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.model, "model", "", "provider:model (or EIPAGENT_MODEL)")
	fs.StringVar(&c.profile, "profile", "", "profile JSON")
	fs.StringVar(&c.policy, "policy", "", "admin policy JSON")
	fs.StringVar(&c.lock, "lock", "", "eip.lock with plugin pins")
	fs.StringVar(&c.preset, "preset", "secure", "secure | dev")
	fs.StringVar(&c.user, "user", "", "user id for runs")
	fs.Var(&c.dirs, "plugins", "plugin directory (repeatable)")
	fs.Var(&c.plugins, "use", "load a plugin by name (repeatable)")
}

func (c *common) options() []eipagent.Option {
	var o []eipagent.Option
	if c.model != "" {
		o = append(o, eipagent.WithModel(c.model))
	}
	if c.profile != "" {
		o = append(o, eipagent.WithProfile(c.profile))
	}
	if c.policy != "" {
		o = append(o, eipagent.WithPolicyFile(c.policy))
	}
	if c.lock != "" {
		o = append(o, eipagent.WithLockFile(c.lock))
	}
	for _, d := range c.dirs {
		o = append(o, eipagent.WithPluginDir(d))
	}
	if len(c.plugins) > 0 {
		o = append(o, eipagent.WithPlugins(c.plugins...))
	}
	if c.user != "" {
		o = append(o, eipagent.WithUser(c.user))
	}
	return append(o, eipagent.WithPreset(c.preset))
}

func main() {
	args := os.Args[1:]
	cmd := "repl"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch cmd {
	case "repl":
		err = repl(ctx, args)
	case "run":
		err = run(ctx, args)
	case "serve":
		err = serve(ctx, args)
	case "plugin":
		err = pluginCmd(args)
	case "config":
		err = configCmd(args)
	case "trace":
		err = traceCmd(args)
	case "help", "-h", "--help":
		fmt.Println("usage: eip [repl|run|serve|plugin|config|trace] [flags]")
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "eip:", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c common
	c.flags(fs)
	fs.Bool("stdio", true, "serve JSON-RPC on stdin/stdout")
	fs.Parse(args)
	// stdout carries the protocol; logs go to stderr.
	opts := append(c.options(), eipagent.WithLogger(func(scope, msg string) { fmt.Fprintf(os.Stderr, "[%s] %s\n", scope, msg) }))
	return eipagent.Serve(ctx, os.Stdin, os.Stdout, opts...)
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.flags(fs)
	thread := fs.String("thread", "", "persist the conversation in this thread")
	jsonOut := fs.Bool("json", false, "print the full result as JSON")
	fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return fmt.Errorf("run: no prompt")
	}
	a, err := eipagent.New(c.options()...)
	if err != nil {
		return err
	}
	defer a.Close()
	var s *eipagent.Stream
	if *thread != "" {
		s = a.Thread(*thread).Stream(ctx, prompt)
	} else {
		s = a.Stream(ctx, prompt)
	}
	if !*jsonOut {
		for d := range s.Text() {
			fmt.Print(d)
		}
		fmt.Println()
	}
	res, err := s.Result()
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	for _, in := range res.Interrupts {
		fmt.Printf("needs approval [%s]: %s\n", in.ID, in.Message)
	}
	return nil
}

func repl(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("repl", flag.ExitOnError)
	var c common
	c.flags(fs)
	thread := fs.String("thread", "repl", "thread id")
	fs.Parse(args)
	in := bufio.NewScanner(os.Stdin)
	opts := append(c.options(), eipagent.WithApprover(func(ctx context.Context, it eipagent.Interrupt) (*eipagent.Decision, error) {
		fmt.Printf("\n  ? %s [y/N/reason] ", it.Message)
		if !in.Scan() {
			return nil, nil
		}
		ans := strings.TrimSpace(in.Text())
		if ans == "y" || ans == "yes" {
			d := eipagent.Approve(it.ID)
			return &d, nil
		}
		d := eipagent.Reject(it.ID, ans)
		return &d, nil
	}))
	a, err := eipagent.New(opts...)
	if err != nil {
		return err
	}
	defer a.Close()
	fmt.Printf("eip · model %s · /plugins /load <n> /unload <n> /reload <n> /quit\n", a.Model())
	th := a.Thread(*thread)
	for fmt.Print("> "); in.Scan(); fmt.Print("> ") {
		line := strings.TrimSpace(in.Text())
		cmd, arg, _ := strings.Cut(line, " ")
		var err error
		switch cmd {
		case "":
		case "/quit", "/exit":
			return nil
		case "/plugins":
			for _, s := range a.Status() {
				fmt.Println(" ", s.String())
			}
		case "/load":
			err = a.Engine().Load(engine.PluginSpec{Use: arg})
		case "/reload":
			err = a.Engine().Reload(arg)
		case "/unload":
			err = a.Engine().Unload(arg)
		default:
			s := th.Stream(ctx, line)
			for e := range s.Events() {
				switch e.Type {
				case "text":
					fmt.Print(e.Text)
				case "tool_call":
					fmt.Printf("\n  → %s %s\n", e.ToolCall.Name, sdk.ToJSON(e.ToolCall.Args))
				}
			}
			fmt.Println()
			_, err = s.Result()
		}
		if err != nil {
			fmt.Println("error:", err)
		}
	}
	return nil
}

func pluginCmd(args []string) error {
	if len(args) == 0 || args[0] != "pin" {
		return fmt.Errorf("usage: eip plugin pin -plugins DIR [-lock eip.lock]")
	}
	fs := flag.NewFlagSet("pin", flag.ExitOnError)
	var dirs multi
	fs.Var(&dirs, "plugins", "plugin directory (repeatable)")
	lockPath := fs.String("lock", "eip.lock", "lock file to write")
	fs.Parse(args[1:])
	lock := &engine.Lock{Plugins: map[string]string{}}
	engine.ReadJSON(*lockPath, lock)
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

func configCmd(args []string) error {
	if len(args) == 0 || args[0] != "dump" {
		return fmt.Errorf("usage: eip config dump [flags]")
	}
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	var c common
	c.flags(fs)
	fs.Parse(args[1:])
	a, err := eipagent.New(c.options()...)
	if err != nil {
		return err
	}
	defer a.Close()
	fmt.Printf("model: %s\npolicy: require_pins=%v hot_reload=%v locked=%v\nplugins:\n", a.Model(), a.Engine().Policy().RequirePins, a.Engine().Policy().HotReload, a.Engine().Policy().Locked)
	for _, s := range a.Status() {
		fmt.Println(" ", s.String())
	}
	_ = plugins.FS
	return nil
}

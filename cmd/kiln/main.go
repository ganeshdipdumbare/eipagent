// Command kiln is the kiln CLI: an interactive REPL, a headless
// one-shot runner, an evaluator, trace and audit viewers, and tools for the
// module set.
//
//	kiln [repl] -model openai:gpt-4.1 -profile kiln.json
//	kiln run -model ... "summarize README.md"
//	kiln eval -model ... cases.json   (grade the agent on a dataset; see package evals)
//	kiln config dump -profile kiln.json (show what composes and what each module may use)
//	kiln module list                  (every standard module: provides, permissions)
//	kiln module check [dir]           (static check of module packages; CI gate)
//	kiln module manifest [dir]        (JSON: modules, requires, provides, capabilities)
//	kiln trace view trace.jsonl > trace.html
//	kiln audit verify audit.jsonl
//
// Application modules are compiled into your own program with
// kiln.WithModule; this CLI runs the standard modules only.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/evals"
	"github.com/ganeshdipdumbare/kiln/internal/modulecheck"
	"github.com/ganeshdipdumbare/kiln/modules/std"
	"github.com/ganeshdipdumbare/kiln/sdk"

	_ "github.com/ganeshdipdumbare/kiln/drivers/postgres"
)

type common struct {
	model, profile, policy, user string
	use                          multi
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func (c *common) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.model, "model", "", "provider:model (or KILN_MODEL)")
	fs.StringVar(&c.profile, "profile", "", "profile JSON")
	fs.StringVar(&c.policy, "policy", "", "admin policy JSON")
	fs.StringVar(&c.user, "user", "", "user id for runs")
	fs.Var(&c.use, "use", "run a standard module by name (repeatable; see kiln module list)")
}

func (c *common) options() []kiln.Option {
	var o []kiln.Option
	if c.model != "" {
		o = append(o, kiln.WithModel(c.model))
	}
	if c.profile != "" {
		o = append(o, kiln.WithProfile(c.profile))
	}
	if c.policy != "" {
		o = append(o, kiln.WithPolicyFile(c.policy))
	}
	if len(c.use) > 0 {
		o = append(o, kiln.WithModules(c.use...))
	}
	if c.user != "" {
		o = append(o, kiln.WithUser(c.user))
	}
	return o
}

func main() {
	args := os.Args[1:]
	cmd := "repl"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// A sandbox box runs the same program: serve the agent over JSON-RPC on
	// stdin/stdout, with the profile and policy the host uploaded.
	if kiln.InBox() {
		if err := kiln.ServeBox(ctx, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "kiln:", err)
			os.Exit(1)
		}
		return
	}
	var err error
	switch cmd {
	case "repl":
		err = repl(ctx, args)
	case "run":
		err = run(ctx, args)
	case "eval":
		err = evalCmd(ctx, args)
	case "module":
		err = moduleCmd(args)
	case "config":
		err = configCmd(args)
	case "trace":
		err = traceCmd(args)
	case "audit":
		err = auditCmd(args)
	case "help", "-h", "--help":
		fmt.Println("usage: kiln [repl|run|eval|module|config|trace|audit] [flags]")
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kiln:", err)
		os.Exit(1)
	}
}

func evalCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	var c common
	c.flags(fs)
	parallel := fs.Int("parallel", 4, "cases to run at once")
	jsonOut := fs.Bool("json", false, "print the report as JSON")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("eval: give one dataset file (kiln eval [flags] cases.json)")
	}
	ds, err := evals.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	a, err := kiln.New(c.options()...)
	if err != nil {
		return err
	}
	defer a.Close()
	rep, err := evals.Run(ctx, a, ds, evals.Options{Parallel: *parallel})
	if err != nil {
		return err
	}
	if *jsonOut {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Print(rep.Summary())
	}
	if rep.Failed > 0 {
		return fmt.Errorf("%d of %d cases failed", rep.Failed, rep.Passed+rep.Failed)
	}
	return nil
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
	a, err := kiln.New(c.options()...)
	if err != nil {
		return err
	}
	defer a.Close()
	var s *kiln.Stream
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
	opts := append(c.options(), kiln.WithApprover(func(ctx context.Context, it kiln.Interrupt) (*kiln.Decision, error) {
		fmt.Printf("\n  ? %s [y/N/reason] ", it.Message)
		if !in.Scan() {
			return nil, nil
		}
		ans := strings.TrimSpace(in.Text())
		if ans == "y" || ans == "yes" {
			d := kiln.Approve(it.ID)
			return &d, nil
		}
		d := kiln.Reject(it.ID, ans)
		return &d, nil
	}))
	a, err := kiln.New(opts...)
	if err != nil {
		return err
	}
	defer a.Close()
	fmt.Printf("kiln · model %s · /modules /quit\n", a.Model())
	th := a.Thread(*thread)
	for fmt.Print("> "); in.Scan(); fmt.Print("> ") {
		line := strings.TrimSpace(in.Text())
		cmd, _, _ := strings.Cut(line, " ")
		var err error
		switch cmd {
		case "":
		case "/quit", "/exit":
			return nil
		case "/modules":
			for _, s := range a.Status() {
				fmt.Println(" ", s.String())
			}
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

func configCmd(args []string) error {
	if len(args) == 0 || args[0] != "dump" {
		return fmt.Errorf("usage: kiln config dump [flags]")
	}
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	var c common
	c.flags(fs)
	fs.Parse(args[1:])
	a, err := kiln.New(c.options()...)
	if err != nil {
		return err
	}
	defer a.Close()
	pol := a.Engine().Policy()
	fmt.Printf("model: %s\npolicy: allow=%v deny=%v locked=%v\nmodules (start order):\n", a.Model(), pol.Allow, pol.Deny, pol.Locked)
	for _, s := range a.Status() {
		fmt.Println(" ", s.String())
	}
	return nil
}

// moduleCmd: kiln module list | check [dir] | manifest [dir].
func moduleCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kiln module list | check [dir] | manifest [dir]")
	}
	switch args[0] {
	case "list":
		for _, m := range std.All() {
			perms := strings.Join(m.Permissions, ",")
			if perms == "" {
				perms = "-"
			}
			fmt.Printf("%-22s provides=%-28s requires=%-22s permissions=%s\n", m.Name,
				strings.Join(m.Provides, ","), strings.Join(m.Requires, ","), perms)
		}
		return nil
	case "check", "manifest":
		dir := "modules"
		if len(args) > 1 {
			dir = args[1]
		}
		if args[0] == "manifest" {
			infos, err := modulecheck.Manifest(dir)
			if err != nil {
				return err
			}
			b, _ := json.MarshalIndent(infos, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		fs, err := modulecheck.Check(dir)
		if err != nil {
			return err
		}
		fmt.Print(modulecheck.Format(fs))
		if modulecheck.HasErrors(fs) {
			return fmt.Errorf("module check failed")
		}
		return nil
	}
	return fmt.Errorf("unknown module command %q", args[0])
}

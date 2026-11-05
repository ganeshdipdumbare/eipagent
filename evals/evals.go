// Package evals checks an agent against a dataset of cases: each case is
// a prompt and what the answer and the run must look like. Run it in a
// test, in CI, or with `kiln eval`.
//
//	ds, _ := evals.Load("cases.json")
//	rep, _ := evals.Run(ctx, agent, ds)
//	fmt.Print(rep.Summary())
//
// A dataset file:
//
//	{
//	  "name": "support desk",
//	  "judge_model": "anthropic:claude-haiku-5-5",
//	  "cases": [
//	    {"name": "tracking", "prompt": "Where is order #1077?",
//	     "expect": {"contains": ["shipped"], "tools": ["lookup_order"]}},
//	    {"name": "refund tone", "prompt": "My mug arrived broken.",
//	     "expect": {"judge": "Apologises and offers a refund or replacement."}}
//	  ]
//	}
package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ganeshdipdumbare/kiln"
)

// Dataset is a named list of cases.
type Dataset struct {
	Name string `json:"name"`
	// JudgeModel grades cases with a "judge" expectation (default: the
	// agent's model).
	JudgeModel string `json:"judge_model,omitempty"`
	Cases      []Case `json:"cases"`
}

// Case is one prompt and what must hold for it to pass.
type Case struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	Expect Expect `json:"expect"`
}

// Expect lists the checks for a case. Every check given must pass.
type Expect struct {
	Contains    []string `json:"contains,omitempty"`     // the answer contains each (case-insensitive)
	NotContains []string `json:"not_contains,omitempty"` // ...and none of these
	Regex       string   `json:"regex,omitempty"`        // the answer matches
	Tools       []string `json:"tools,omitempty"`        // these tools were called
	NoTools     []string `json:"no_tools,omitempty"`     // these tools were not called
	MaxSteps    int      `json:"max_steps,omitempty"`    // the run took at most this many model calls
	Judge       string   `json:"judge,omitempty"`        // a rubric a judge model grades the answer against
}

// Result is the outcome of one case.
type Result struct {
	Case     string        `json:"case"`
	Passed   bool          `json:"passed"`
	Failures []string      `json:"failures,omitempty"`
	Answer   string        `json:"answer"`
	Tools    []string      `json:"tools,omitempty"`
	Steps    int           `json:"steps"`
	Tokens   int           `json:"tokens"`
	Duration time.Duration `json:"duration_ns"`
	Error    string        `json:"error,omitempty"`
}

// Report is the outcome of a dataset.
type Report struct {
	Name    string   `json:"name"`
	Passed  int      `json:"passed"`
	Failed  int      `json:"failed"`
	Results []Result `json:"results"`
}

// Load reads a dataset from a JSON file.
func Load(path string) (*Dataset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ds Dataset
	if err := json.Unmarshal(b, &ds); err != nil {
		return nil, fmt.Errorf("evals: %s: %w", path, err)
	}
	if len(ds.Cases) == 0 {
		return nil, fmt.Errorf("evals: %s has no cases", path)
	}
	for i, c := range ds.Cases {
		if c.Prompt == "" {
			return nil, fmt.Errorf("evals: %s: case %d has no prompt", path, i+1)
		}
		if c.Expect.Regex != "" {
			if _, err := regexp.Compile(c.Expect.Regex); err != nil {
				return nil, fmt.Errorf("evals: %s: case %d: %w", path, i+1, err)
			}
		}
	}
	return &ds, nil
}

// Options for Run.
type Options struct {
	Parallel int              // cases run at once (default 4)
	RunOpts  []kiln.RunOption // passed to every run
}

// Run runs every case on the agent and grades it.
func Run(ctx context.Context, a *kiln.Agent, ds *Dataset, opt ...Options) (*Report, error) {
	o := Options{Parallel: 4}
	if len(opt) > 0 {
		o = opt[0]
		if o.Parallel <= 0 {
			o.Parallel = 4
		}
	}
	rep := &Report{Name: ds.Name, Results: make([]Result, len(ds.Cases))}
	sem := make(chan struct{}, o.Parallel)
	var wg sync.WaitGroup
	for i, c := range ds.Cases {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c Case) {
			defer func() { <-sem; wg.Done() }()
			rep.Results[i] = runCase(ctx, a, ds, c, o.RunOpts)
		}(i, c)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, r := range rep.Results {
		if r.Passed {
			rep.Passed++
		} else {
			rep.Failed++
		}
	}
	return rep, nil
}

func runCase(ctx context.Context, a *kiln.Agent, ds *Dataset, c Case, runOpts []kiln.RunOption) Result {
	name := c.Name
	if name == "" {
		name = c.Prompt
	}
	r := Result{Case: name}
	start := time.Now()
	res, err := a.Generate(ctx, c.Prompt, runOpts...)
	r.Duration = time.Since(start)
	if err != nil {
		r.Error = err.Error()
		r.Failures = []string{"run failed: " + err.Error()}
		return r
	}
	r.Answer, r.Steps, r.Tokens = res.Text, len(res.Steps), res.Usage.TotalTokens
	called := map[string]bool{}
	for _, tc := range res.ToolCalls() {
		if !called[tc.Name] {
			r.Tools = append(r.Tools, tc.Name)
		}
		called[tc.Name] = true
	}
	sort.Strings(r.Tools)

	e := c.Expect
	low := strings.ToLower(res.Text)
	for _, s := range e.Contains {
		if !strings.Contains(low, strings.ToLower(s)) {
			r.Failures = append(r.Failures, fmt.Sprintf("answer does not contain %q", s))
		}
	}
	for _, s := range e.NotContains {
		if strings.Contains(low, strings.ToLower(s)) {
			r.Failures = append(r.Failures, fmt.Sprintf("answer contains %q", s))
		}
	}
	if e.Regex != "" && !regexp.MustCompile(e.Regex).MatchString(res.Text) {
		r.Failures = append(r.Failures, fmt.Sprintf("answer does not match /%s/", e.Regex))
	}
	for _, t := range e.Tools {
		if !called[t] {
			r.Failures = append(r.Failures, fmt.Sprintf("tool %s was not called", t))
		}
	}
	for _, t := range e.NoTools {
		if called[t] {
			r.Failures = append(r.Failures, fmt.Sprintf("tool %s was called", t))
		}
	}
	if e.MaxSteps > 0 && len(res.Steps) > e.MaxSteps {
		r.Failures = append(r.Failures, fmt.Sprintf("took %d steps, more than %d", len(res.Steps), e.MaxSteps))
	}
	if e.Judge != "" {
		if why, ok, err := judge(ctx, a, ds.JudgeModel, c.Prompt, res.Text, e.Judge); err != nil {
			r.Failures = append(r.Failures, "judge failed: "+err.Error())
		} else if !ok {
			r.Failures = append(r.Failures, "judge: "+why)
		}
	}
	r.Passed = len(r.Failures) == 0
	return r
}

type verdict struct {
	Pass   bool   `json:"pass" desc:"true when the answer meets the rubric"`
	Reason string `json:"reason" desc:"one sentence on why"`
}

func judge(ctx context.Context, a *kiln.Agent, model, prompt, answer, rubric string) (string, bool, error) {
	opts := []kiln.RunOption{
		kiln.System("You grade an AI assistant's answer against a rubric. Be strict: pass only when the rubric is clearly met."),
		kiln.ToolChoice("none"), kiln.MaxSteps(1),
	}
	if model != "" {
		opts = append(opts, kiln.Model(model))
	}
	q := fmt.Sprintf("Rubric:\n%s\n\nThe user asked:\n%s\n\nThe assistant answered:\n%s", rubric, prompt, answer)
	v, err := kiln.GenerateObject[verdict](ctx, a, q, opts...)
	if err != nil {
		return "", false, err
	}
	return v.Reason, v.Pass, nil
}

// Summary is a short text report: one line per case, then the totals.
func (r *Report) Summary() string {
	var b strings.Builder
	if r.Name != "" {
		fmt.Fprintf(&b, "%s\n", r.Name)
	}
	for _, x := range r.Results {
		mark := "PASS"
		if !x.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "%s  %s (%d steps, %d tokens, %s)\n", mark, x.Case, x.Steps, x.Tokens, x.Duration.Round(time.Millisecond))
		for _, f := range x.Failures {
			fmt.Fprintf(&b, "      - %s\n", f)
		}
	}
	fmt.Fprintf(&b, "%d passed, %d failed\n", r.Passed, r.Failed)
	return b.String()
}

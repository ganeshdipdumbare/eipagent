package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"sort"
	"strings"
	"time"
)

type span struct {
	Trace  string         `json:"trace"`
	ID     string         `json:"id"`
	Parent string         `json:"parent"`
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	Start  time.Time      `json:"start"`
	MS     int64          `json:"ms"`
	Attrs  map[string]any `json:"attrs"`
}

// traceCmd renders a trace_file trace as a static HTML timeline.
func traceCmd(args []string) error {
	if len(args) != 2 || args[0] != "view" {
		return fmt.Errorf("usage: kiln trace view trace.jsonl > trace.html")
	}
	f, err := os.Open(args[1])
	if err != nil {
		return err
	}
	defer f.Close()
	byTrace := map[string][]span{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var s span
		if json.Unmarshal(sc.Bytes(), &s) == nil && s.Trace != "" {
			byTrace[s.Trace] = append(byTrace[s.Trace], s)
		}
	}
	var traces []string
	for t := range byTrace {
		traces = append(traces, t)
	}
	sort.Slice(traces, func(i, j int) bool { return first(byTrace[traces[i]]).Before(first(byTrace[traces[j]])) })

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Agent trace</title>
<style>:root{--bg:#fff;--fg:#1d1d1f;--muted:#6e6e73;--run:#5b6cff;--model:#18a058;--tool:#e08a00;--err:#d03050;--line:#e5e5ea}
@media (prefers-color-scheme:dark){:root{--bg:#141416;--fg:#f2f2f7;--muted:#9a9aa0;--line:#2c2c30}}
body{background:var(--bg);color:var(--fg);font:14px system-ui,sans-serif;margin:0;padding:16px;max-width:1100px}
h1{font-size:20px}h2{font-size:15px;margin:24px 0 8px}.row{display:grid;grid-template-columns:minmax(120px,240px) 1fr 70px;gap:8px;align-items:center;padding:3px 0;border-bottom:1px solid var(--line)}
.bar{height:12px;border-radius:3px;position:relative}.track{position:relative;height:12px}.name{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.model{background:var(--model)}.tool{background:var(--tool)}.run{background:var(--run)}.err{background:var(--err)}.ms{color:var(--muted);text-align:right;font-variant-numeric:tabular-nums}
details{color:var(--muted);font-size:12px}pre{white-space:pre-wrap;word-break:break-word}</style><h1>Agent trace</h1>`)
	for _, t := range traces {
		spans := byTrace[t]
		sort.Slice(spans, func(i, j int) bool { return spans[i].Start.Before(spans[j].Start) })
		t0 := first(spans)
		var total int64 = 1
		for _, s := range spans {
			if end := s.Start.Sub(t0).Milliseconds() + s.MS; end > total {
				total = end
			}
		}
		title := t
		for _, s := range spans {
			if s.Kind == "run" {
				title = fmt.Sprintf("%s · %v · %v steps · %v in / %v out tokens", t0.Format(time.RFC3339), s.Attrs["model"], s.Attrs["steps"], s.Attrs["input_tokens"], s.Attrs["output_tokens"])
			}
		}
		fmt.Fprintf(w, "<h2>%s</h2>", html.EscapeString(title))
		for _, s := range spans {
			cls := s.Kind
			if s.Attrs["error"] != nil {
				cls = "err"
			}
			left := float64(s.Start.Sub(t0).Milliseconds()) / float64(total) * 100
			width := float64(s.MS) / float64(total) * 100
			if width < 0.5 {
				width = 0.5
			}
			attrs, _ := json.MarshalIndent(s.Attrs, "", "  ")
			fmt.Fprintf(w, `<div class="row"><div class="name" title="%s">%s %s</div><div class="track"><div class="bar %s" style="position:absolute;left:%.2f%%;width:%.2f%%"></div></div><div class="ms">%d ms</div></div><details><summary>details</summary><pre>%s</pre></details>`,
				html.EscapeString(s.Name), s.Kind, html.EscapeString(strings.TrimPrefix(s.Name, "run ")), cls, left, width, s.MS, html.EscapeString(string(attrs)))
		}
	}
	return nil
}

func first(spans []span) time.Time {
	t := spans[0].Start
	for _, s := range spans {
		if s.Start.Before(t) {
			t = s.Start
		}
	}
	return t
}

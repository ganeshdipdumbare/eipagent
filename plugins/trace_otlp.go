//go:build ignore

package plugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"eipagent/sdk"
)

// Exports traces over OTLP/HTTP (JSON) to an OpenTelemetry collector or
// any OTLP backend: one trace per run, with a span per model call and
// per tool call. Attributes follow the OpenTelemetry GenAI conventions
// where they exist (gen_ai.*) plus eip.* fields: run id, thread, user,
// plugin, tool, call id. Secret values are redacted.
//
// Failed exports are retried on the next flush; the queue is bounded by
// max_queue (oldest spans dropped first, and logged).
//
// Config: endpoint ("http://localhost:4318"; spans go to /v1/traces),
// headers ({name: value or secret://NAME}), service_name ("eipagent"),
// content (include prompts / results, false), flush_seconds (2),
// max_queue (5000).
//
// Example:
//
//	eipagent.WithPlugin("trace_otlp", map[string]any{
//		"endpoint":     "https://otlp.example.com",
//		"headers":      map[string]any{"x-api-key": "secret://OTLP_KEY"},
//		"service_name": "support-agent",
//	})
//	// Shortcut: eipagent.WithTracing("http://localhost:4318")
var Permissions = []string{"net"}

var Requires = []string{"?secrets"}

type span struct {
	TraceID      string           `json:"traceId"`
	SpanID       string           `json:"spanId"`
	ParentSpanID string           `json:"parentSpanId,omitempty"`
	Name         string           `json:"name"`
	Kind         int              `json:"kind"`
	Start        string           `json:"startTimeUnixNano"`
	End          string           `json:"endTimeUnixNano"`
	Attributes   []map[string]any `json:"attributes"`
	Status       map[string]any   `json:"status,omitempty"`
}

type runSpan struct {
	trace, id string
	start     time.Time
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func attr(k string, v any) map[string]any {
	switch x := v.(type) {
	case int:
		return map[string]any{"key": k, "value": map[string]any{"intValue": strconv.Itoa(x)}}
	case bool:
		return map[string]any{"key": k, "value": map[string]any{"boolValue": x}}
	case string:
		return map[string]any{"key": k, "value": map[string]any{"stringValue": sdk.Redact(x)}}
	}
	return map[string]any{"key": k, "value": map[string]any{"stringValue": sdk.Redact(fmt.Sprint(v))}}
}

func Apply(p *sdk.Plugin) error {
	endpoint := strings.TrimRight(p.ConfigString("endpoint", "http://localhost:4318"), "/")
	headers := p.ConfigMap("headers")
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("trace_otlp: endpoint must be an absolute http(s) URL")
	}
	if u.User != nil {
		return fmt.Errorf("trace_otlp: endpoint must not contain credentials")
	}
	if len(headers) > 0 && u.Scheme != "https" && !otlpLocalHost(u.Hostname()) {
		return fmt.Errorf("trace_otlp: endpoint must be https when headers are configured")
	}
	service := p.ConfigString("service_name", "eipagent")
	content := p.ConfigBool("content", false)
	flushEvery := time.Duration(p.ConfigInt("flush_seconds", 2)) * time.Second
	maxQueue := p.ConfigInt("max_queue", 5000)
	if maxQueue < 1 || flushEvery <= 0 {
		return fmt.Errorf("trace_otlp: max_queue and flush_seconds must be positive")
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: sdk.RefuseRedirect}

	var mu sync.Mutex
	var queue []span
	runs := map[*sdk.RunState]runSpan{}
	push := func(s span) {
		mu.Lock()
		queue = append(queue, s)
		if len(queue) > maxQueue {
			queue = queue[len(queue)-maxQueue:]
		}
		mu.Unlock()
	}
	runOf := func(st *sdk.RunState) runSpan {
		mu.Lock()
		defer mu.Unlock()
		if r, ok := runs[st]; ok {
			return r
		}
		return runSpan{trace: randHex(16)}
	}
	var sending sync.Mutex
	lastErr := ""
	// requeue puts a batch that failed back in front of newer spans (the
	// queue stays bounded by max_queue, oldest dropped first), so a
	// collector outage or a bad header delays traces instead of losing them.
	requeue := func(batch []span, why error) {
		mu.Lock()
		queue = append(batch, queue...)
		lost := 0
		if len(queue) > maxQueue {
			lost = len(queue) - maxQueue
			queue = queue[lost:]
		}
		mu.Unlock()
		if msg := why.Error(); msg != lastErr || lost > 0 {
			lastErr = msg
			p.Logf("trace_otlp: export failed, %d spans kept for retry, %d dropped (queue full): %v", len(batch)-lost, lost, why)
		}
	}
	flush := func() {
		sending.Lock()
		defer sending.Unlock()
		mu.Lock()
		batch := queue
		queue = nil
		mu.Unlock()
		if len(batch) == 0 {
			return
		}
		payload := map[string]any{"resourceSpans": []map[string]any{{
			"resource":   map[string]any{"attributes": []map[string]any{attr("service.name", service)}},
			"scopeSpans": []map[string]any{{"scope": map[string]any{"name": "eipagent", "version": sdk.Version}, "spans": batch}},
		}}}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", endpoint+"/v1/traces", bytes.NewReader(body))
		if err != nil {
			requeue(batch, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			s := ""
			if x, ok := v.(string); ok {
				s = x
			}
			val, err := p.SecretValue(s)
			if err != nil {
				requeue(batch, fmt.Errorf("header %s: %v", k, err))
				return
			}
			req.Header.Set(k, val)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}
		if err != nil {
			requeue(batch, err)
			return
		}
		lastErr = ""
	}
	p.Effect(func() func() {
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			t := time.NewTicker(flushEvery)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					flush()
				}
			}
		}()
		return func() {
			close(stop)
			<-done
			flush()
		}
	})

	p.OnRunStart(func(c context.Context, st *sdk.RunState) error {
		mu.Lock()
		runs[st] = runSpan{trace: randHex(16), id: randHex(8), start: time.Now()}
		mu.Unlock()
		return nil
	})
	p.OnRunEnd(func(c context.Context, st *sdk.RunState, res *sdk.RunResult) error {
		mu.Lock()
		r := runs[st]
		delete(runs, st)
		mu.Unlock()
		if r.id == "" {
			return nil
		}
		provider, _ := sdk.SplitModel(st.Model)
		attrs := []map[string]any{attr("eip.run_id", st.ID), attr("eip.thread", st.Thread), attr("enduser.id", st.User),
			attr("gen_ai.request.model", st.Model), attr("gen_ai.system", provider), attr("eip.steps", len(res.Steps)),
			attr("gen_ai.usage.input_tokens", res.Usage.InputTokens), attr("gen_ai.usage.output_tokens", res.Usage.OutputTokens),
			attr("eip.finish_reason", string(res.FinishReason))}
		if content {
			attrs = append(attrs, attr("eip.output", res.Text))
		}
		push(span{TraceID: r.trace, SpanID: r.id, Name: "agent.run", Kind: 1, Start: nanos(r.start), End: nanos(time.Now()), Attributes: attrs})
		return nil
	})
	p.WrapModel(func(c context.Context, req *sdk.ModelRequest, next sdk.ModelHandler) (*sdk.ModelResponse, error) {
		t := time.Now()
		resp, err := next(c, req)
		r := runOf(req.Run)
		provider, model := sdk.SplitModel(req.Model)
		attrs := []map[string]any{attr("gen_ai.operation.name", "chat"), attr("gen_ai.system", provider), attr("gen_ai.request.model", model), attr("eip.step", req.Step)}
		if req.Run != nil {
			attrs = append(attrs, attr("eip.run_id", req.Run.ID))
		}
		s := span{TraceID: r.trace, SpanID: randHex(8), ParentSpanID: r.id, Name: "chat " + model, Kind: 3, Start: nanos(t), End: nanos(time.Now())}
		if err != nil {
			s.Status = map[string]any{"code": 2, "message": statusMessage(err.Error(), "model error", content)}
		} else {
			attrs = append(attrs, attr("gen_ai.usage.input_tokens", resp.Usage.InputTokens), attr("gen_ai.usage.output_tokens", resp.Usage.OutputTokens),
				attr("eip.tool_calls", len(resp.Message.ToolCalls)))
			if content {
				attrs = append(attrs, attr("eip.output", resp.Message.Content))
			}
		}
		s.Attributes = attrs
		push(s)
		return resp, err
	})
	p.WrapTool(func(c context.Context, req *sdk.ToolRequest, next sdk.ToolHandler) (*sdk.ToolResult, error) {
		t := time.Now()
		res, err := next(c, req)
		r := runOf(req.Run)
		attrs := []map[string]any{attr("gen_ai.operation.name", "execute_tool"), attr("gen_ai.tool.name", req.Call.Name), attr("gen_ai.tool.call.id", req.Call.ID), attr("eip.step", req.Step)}
		if req.Tool != nil {
			attrs = append(attrs, attr("eip.plugin", req.Tool.Plugin))
		}
		if req.Run != nil {
			attrs = append(attrs, attr("eip.run_id", req.Run.ID))
		}
		if req.Decision != nil {
			attrs = append(attrs, attr("eip.decision", req.Decision.Type), attr("eip.decided_by", req.Decision.By))
		}
		s := span{TraceID: r.trace, SpanID: randHex(8), ParentSpanID: r.id, Name: "execute_tool " + req.Call.Name, Kind: 1, Start: nanos(t), End: nanos(time.Now())}
		switch {
		case err != nil:
			s.Status = map[string]any{"code": 2, "message": statusMessage(err.Error(), "tool error", content)}
		case res.Interrupt != nil:
			attrs = append(attrs, attr("eip.interrupted", true))
		case res.IsError:
			s.Status = map[string]any{"code": 2, "message": statusMessage(res.Content, "tool error", content)}
		case content:
			attrs = append(attrs, attr("eip.output", res.Content))
		}
		s.Attributes = attrs
		push(s)
		return res, err
	})
	return nil
}

// statusMessage keeps error text out of spans unless content is on.
func statusMessage(s, fallback string, content bool) string {
	if !content {
		return fallback
	}
	s = sdk.Redact(s)
	if len(s) > 1024 {
		s = s[:1024] + "..."
	}
	return s
}

func otlpLocalHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

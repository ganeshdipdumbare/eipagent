// Command observability ships an agent's audit trail and traces to
// external systems, offline: a fake SIEM receives audit records from
// audit_http (HMAC-signed, hash-chained NDJSON), and a fake OpenTelemetry
// collector receives spans from trace_otlp. Both run in-process.
//
//	go run ./examples/observability
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/ganeshdipdumbare/eipagent"
)

func main() {
	ctx := context.Background()
	os.Setenv("SIEM_HMAC_TOKEN", "shared-hmac-key") // in production: secret:// from Vault

	// The SIEM checks every batch's signature and the hash chain.
	var mu sync.Mutex
	var records []map[string]any
	badSig, prev, broken := 0, "", false
	siem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m := hmac.New(sha256.New, []byte("shared-hmac-key"))
		m.Write(body)
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("X-EIP-Signature") != "sha256="+hex.EncodeToString(m.Sum(nil)) {
			badSig++
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			var rec struct {
				Body map[string]any `json:"body"`
				Prev string         `json:"prev"`
				Hash string         `json:"hash"`
			}
			json.Unmarshal([]byte(line), &rec)
			if prev != "" && rec.Prev != prev {
				broken = true
			}
			prev = rec.Hash
			records = append(records, rec.Body)
		}
	}))
	defer siem.Close()

	// The collector keeps the spans it receives.
	var spans []map[string]any
	otel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []map[string]any `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		defer mu.Unlock()
		for _, rs := range p.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
	}))
	defer otel.Close()

	type CityIn struct {
		City string `json:"city"`
	}
	a, err := eipagent.New(
		eipagent.WithModel("mock:demo"),
		eipagent.WithPlugin("model_mock", map[string]any{"script": []any{
			map[string]any{"tool": "weather", "args": map[string]any{"city": "Oslo"}},
			map[string]any{"text": "It is 4°C and windy in Oslo."},
		}}),
		eipagent.WithTools(eipagent.Tool("weather", "Current weather", func(c context.Context, in CityIn) (string, error) {
			return "4°C, wind 9 m/s", nil
		})),
		eipagent.WithPlugin("secrets_env", nil),
		eipagent.WithPlugin("audit_file", map[string]any{"path": "-"}), // records only go to sinks
		eipagent.WithPlugin("audit_http", map[string]any{"url": siem.URL, "hmac_key": "secret://SIEM_HMAC_TOKEN"}),
		eipagent.WithPlugin("trace_otlp", map[string]any{"endpoint": otel.URL, "service_name": "support-bot"}),
		eipagent.WithUser("u-1001"),
		eipagent.WithLogger(func(string, string) {}),
	)
	check(err)
	res, err := a.Thread("t-weather").Generate(ctx, "What's the weather in Oslo?")
	check(err)
	fmt.Println("agent:", res.Text)
	a.Close() // flushes both exporters

	mu.Lock()
	defer mu.Unlock()
	fmt.Printf("\nSIEM received %d audit records (bad signatures: %d, chain intact: %v)\n", len(records), badSig, !broken)
	for _, r := range records {
		d, _ := r["data"].(map[string]any)
		switch r["type"] {
		case "model.call":
			fmt.Printf("  #%-3v model.call  run=%v provider=%v model=%v user=%v thread=%v tokens=%v/%v\n", r["seq"], d["run"], d["provider"], d["model"], d["user"], d["thread"], d["input_tokens"], d["output_tokens"])
		case "tool.call":
			fmt.Printf("  #%-3v tool.call   run=%v tool=%v call_id=%v outcome=%v args=%v\n", r["seq"], d["run"], d["tool"], d["call_id"], d["outcome"], d["args"])
		case "secret.access":
			fmt.Printf("  #%-3v secret.access plugin=%v secret=%v outcome=%v\n", r["seq"], d["plugin"], d["secret"], d["outcome"])
		default:
			fmt.Printf("  #%-3v %v %v\n", r["seq"], r["type"], d["plugin"])
		}
	}

	fmt.Printf("\nCollector received %d spans:\n", len(spans))
	sort.Slice(spans, func(i, j int) bool {
		return spans[i]["startTimeUnixNano"].(string) < spans[j]["startTimeUnixNano"].(string)
	})
	for _, s := range spans {
		indent := "  "
		if s["parentSpanId"] != nil && s["parentSpanId"] != "" {
			indent = "    └─ "
		}
		var attrs []string
		for _, x := range s["attributes"].([]any) {
			m := x.(map[string]any)
			for _, v := range m["value"].(map[string]any) {
				attrs = append(attrs, fmt.Sprintf("%v=%v", m["key"], v))
			}
		}
		fmt.Printf("%s%s  trace=%s…\n%s   %s\n", indent, s["name"], s["traceId"].(string)[:8], strings.Repeat(" ", len(indent)), strings.Join(attrs, " "))
	}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

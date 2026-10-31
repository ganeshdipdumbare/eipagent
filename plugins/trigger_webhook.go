//go:build ignore

package plugin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"eipagent/sdk"
)

// Starts agent runs from webhooks. Every route requires an HMAC-SHA256
// signature ("X-Signature-256: sha256=<hex>" of the raw body), so only
// trusted senders can trigger work.
//
// Config:
//
//	addr:   listen address (default 127.0.0.1:8787)
//	routes: [{path: "/github", secret: "secret://GH_WEBHOOK_TOKEN", user: "github-bot",
//	          thread: "gh-{repository.full_name}", prompt: "Triage this event: {body}"}]
//
// {body} is the raw JSON; {a.b} is a field of it.
var Inject = []string{"agent"}

var Permissions = []string{"net"}

func Apply(ctx *sdk.Context) error {
	agent := ctx.Service("agent").(*sdk.AgentService)
	addr := ctx.ConfigString("addr", "127.0.0.1:8787")
	mux := http.NewServeMux()
	runCtx, cancelRuns := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, r := range ctx.ConfigList("routes") {
		if err := route(ctx, mux, agent, r, runCtx, &wg); err != nil {
			cancelRuns()
			return err
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	ctx.Logf("listening on %s", ln.Addr())
	ctx.OnDispose(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(c)
		cancelRuns() // stop in-flight runs and wait for them
		wg.Wait()
	})
	return nil
}

func route(ctx *sdk.Context, mux *http.ServeMux, agent *sdk.AgentService, r map[string]any, runCtx context.Context, wg *sync.WaitGroup) error {
	s := func(k string) string { v, _ := r[k].(string); return v }
	path := s("path")
	secret, err := ctx.Secret(s("secret"))
	if err != nil || secret == "" {
		return fmt.Errorf("trigger_webhook %s: a secret is required: %v", path, err)
	}
	user, threadT, promptT := s("user"), s("thread"), s("prompt")
	if promptT == "" {
		promptT = "Handle this webhook event:\n{body}"
	}
	mux.HandleFunc(path, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(want), []byte(req.Header.Get("X-Signature-256"))) {
			ctx.Logf("rejected unsigned request to %s from %s", path, req.RemoteAddr)
			http.Error(w, "bad signature", 401)
			return
		}
		var doc map[string]any
		json.Unmarshal(body, &doc)
		thread := fill(threadT, doc, string(body))
		prompt := fill(promptT, doc, string(body))
		w.WriteHeader(202)
		fmt.Fprintf(w, "{\"accepted\":true,\"thread\":%q}\n", thread)
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := agent.Run(runCtx, thread, user, prompt)
			if err != nil {
				ctx.Logf("webhook %s run failed: %v", path, err)
				return
			}
			ctx.Logf("webhook %s -> thread %s: %.80s", path, thread, res.Text)
		}()
	})
	return nil
}

// fill replaces {body} and {a.b.c} placeholders.
func fill(t string, doc map[string]any, body string) string {
	out := strings.ReplaceAll(t, "{body}", body)
	for {
		i := strings.Index(out, "{")
		j := strings.Index(out, "}")
		if i < 0 || j < i {
			return out
		}
		key := out[i+1 : j]
		var cur any = doc
		for _, part := range strings.Split(key, ".") {
			m, _ := cur.(map[string]any)
			cur = m[part]
		}
		out = out[:i] + fmt.Sprint(cur) + out[j+1:]
	}
}

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
	"time"

	"eipagent/sdk"
)

// hello_webhook listens for webhooks and starts an agent run per request.
// Requests must carry X-Signature: sha256=<hex HMAC of the body>.
//
// Config: addr ("127.0.0.1:8090"), secret ("secret://hello_webhook_WEBHOOK_SECRET"),
// prompt ("Handle this event: ").
var Permissions = []string{"net"}

var Inject = []string{"agent", "?secrets"}

func Apply(ctx *sdk.Context) error {
	agent := ctx.Service("agent").(*sdk.AgentService)
	addr := ctx.ConfigString("addr", "127.0.0.1:8090")
	secretRef := ctx.ConfigString("secret", "secret://hello_webhook_WEBHOOK_SECRET")
	prompt := ctx.ConfigString("prompt", "Handle this event: ")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		key, err := ctx.Secret(secretRef)
		if err != nil {
			http.Error(w, "not configured", 503)
			return
		}
		m := hmac.New(sha256.New, []byte(key))
		m.Write(body)
		if !hmac.Equal([]byte(r.Header.Get("X-Signature")), []byte("sha256="+hex.EncodeToString(m.Sum(nil)))) {
			http.Error(w, "bad signature", 401)
			return
		}
		res, err := agent.Run(r.Context(), "", "webhook:hello_webhook", prompt+string(body))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": res.Text})
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("hello_webhook: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx.Effect(func() func() {
		go srv.Serve(ln)
		return func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(c)
		}
	})
	return nil
}

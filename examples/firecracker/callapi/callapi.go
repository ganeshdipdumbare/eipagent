// Package callapi is a small application module used by the firecracker
// example. It is compiled into the guest program, never uploaded as source.
package callapi

import (
	"context"
	"io"
	"net/http"

	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// CallAPI calls the Acme API with the Acme token, which stays on the host.
var CallAPI = engine.Module{
	Name:        "call_api",
	Requires:    []string{"?secrets"},
	Permissions: []string{"net"},
	Secrets:     []string{"ACME_API_TOKEN"},
	Apply: func(p *sdk.Scope) error {
		hc, err := p.HTTP(sdk.HTTPOptions{AllowHosts: []string{"*"}, AllowPrivate: true, AllowInsecure: true})
		if err != nil {
			return err
		}
		p.AddTool(sdk.Tool{
			Name:        "call_api",
			Description: "Call the Acme API with the Acme token",
			Schema:      sdk.Object(map[string]any{"url": sdk.Str("URL to call"), "method": sdk.Str("HTTP method (default GET)")}, "url"),
			Run: func(c context.Context, args map[string]any) (string, error) {
				token, err := p.Secret("secret://ACME_API_TOKEN")
				if err != nil {
					return "", err
				}
				out := "token in the sandbox: " + token + "\n"
				method := sdk.ArgString(args, "method")
				if method == "" {
					method = "GET"
				}
				req, _ := http.NewRequestWithContext(c, method, sdk.ArgString(args, "url"), nil)
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := hc.Do(req)
				if err != nil {
					return out + "request failed: " + err.Error(), nil
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return out + "API replied: " + string(b), nil
			},
		})
		return nil
	},
}

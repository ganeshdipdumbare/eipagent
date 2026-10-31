//go:build ignore

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"eipagent/sdk"
)

// Resolves secret://NAME from HashiCorp Vault (KV v2), provided as
// "secrets". Values are cached for ttl_seconds and re-read after that, so
// rotating a secret in Vault reaches running plugins without a restart.
// If Vault can't be reached once the cache expires, lookups fail (closed).
//
// NAME maps to <mount>/data/<prefix>/<NAME>, field <field>, unless paths
// maps it explicitly: {"OPENAI_API_KEY": "llm/openai#api_key"}.
//
// Config:
//
//	address:     Vault URL (default $VAULT_ADDR)
//	token_env:   env var holding the token (default "VAULT_TOKEN")
//	token_file:  file holding the token, re-read on every fetch (e.g. a
//	             Vault agent sink); wins over token_env
//	namespace:   X-Vault-Namespace (Vault Enterprise / HCP)
//	mount:       KV v2 mount ("secret"), prefix ("eipagent"), field ("value")
//	paths:       explicit NAME -> "path#field" map
//	ttl_seconds: cache lifetime (300)
//	allow:       glob patterns of names that may be read (default ["*"])
//	grants:      {plugin instance: [names]}: when set, a plugin may read only
//	             the names granted to it (the caller is set by the core)
var Permissions = []string{"net", "env", "fs:read"}

func Apply(ctx *sdk.Context) error {
	addr := strings.TrimRight(ctx.ConfigString("address", os.Getenv("VAULT_ADDR")), "/")
	if addr == "" {
		return fmt.Errorf("secrets_vault: set config address or VAULT_ADDR")
	}
	tokenEnv := ctx.ConfigString("token_env", "VAULT_TOKEN")
	tokenFile := ctx.ConfigString("token_file", "")
	namespace := ctx.ConfigString("namespace", "")
	mount := strings.Trim(ctx.ConfigString("mount", "secret"), "/")
	prefix := strings.Trim(ctx.ConfigString("prefix", "eipagent"), "/")
	field := ctx.ConfigString("field", "value")
	paths := ctx.ConfigMap("paths")
	ttl := time.Duration(ctx.ConfigInt("ttl_seconds", 300)) * time.Second
	allow := ctx.ConfigStrings("allow")
	if len(allow) == 0 {
		allow = []string{"*"}
	}
	grants := ctx.ConfigMap("grants")
	client := &http.Client{Timeout: 10 * time.Second}

	token := func() (string, error) {
		if tokenFile != "" {
			b, err := os.ReadFile(tokenFile)
			if err != nil {
				return "", fmt.Errorf("vault token file: %v", err)
			}
			return strings.TrimSpace(string(b)), nil
		}
		t := os.Getenv(tokenEnv)
		if t == "" {
			return "", fmt.Errorf("vault token: %s is not set", tokenEnv)
		}
		return t, nil
	}
	fetch := func(c context.Context, name string) (string, error) {
		p, f := prefix+"/"+name, field
		if prefix == "" {
			p = name
		}
		if v, ok := paths[name].(string); ok && v != "" {
			p, f = v, field
			if i := strings.LastIndex(v, "#"); i >= 0 {
				p, f = v[:i], v[i+1:]
			}
		}
		tok, err := token()
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(c, "GET", addr+"/v1/"+mount+"/data/"+strings.Trim(p, "/"), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("X-Vault-Token", tok)
		if namespace != "" {
			req.Header.Set("X-Vault-Namespace", namespace)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("vault: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == 404 {
			return "", fmt.Errorf("secret %q not found in vault", name)
		}
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("vault: HTTP %d reading secret %q", resp.StatusCode, name)
		}
		var out struct {
			Data struct {
				Data map[string]any `json:"data"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", fmt.Errorf("vault: bad response for %q", name)
		}
		v, ok := out.Data.Data[f].(string)
		if !ok {
			return "", fmt.Errorf("secret %q has no string field %q in vault", name, f)
		}
		return v, nil
	}

	var mu sync.Mutex
	vals := map[string]string{}
	exp := map[string]time.Time{}
	return ctx.Provide("secrets", &sdk.Secrets{
		Get: func(c context.Context, caller, name string) (string, error) {
			if grants != nil {
				granted := false
				list, _ := grants[caller].([]any)
				for _, g := range list {
					if g == name {
						granted = true
					}
				}
				if !granted {
					return "", fmt.Errorf("secret %q is not granted to %s", name, caller)
				}
			}
			ok := false
			for _, pat := range allow {
				if m, _ := path.Match(pat, name); m {
					ok = true
				}
			}
			if !ok {
				return "", fmt.Errorf("secret %q is not in the secrets_vault allow list", name)
			}
			mu.Lock()
			v, cached := vals[name]
			fresh := cached && time.Now().Before(exp[name])
			mu.Unlock()
			if fresh {
				return v, nil
			}
			v, err := fetch(c, name)
			if err != nil {
				return "", err
			}
			mu.Lock()
			vals[name] = v
			exp[name] = time.Now().Add(ttl)
			mu.Unlock()
			return v, nil
		},
	})
}

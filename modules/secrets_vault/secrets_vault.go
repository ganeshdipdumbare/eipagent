package secrets_vault

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Resolves secret://NAME from HashiCorp Vault (KV v2), provided as
// "secrets". Values are cached for ttl_seconds and re-read after that, so
// rotating a secret in Vault reaches running modules without a restart.
// If Vault can't be reached once the cache expires, lookups fail (closed).
//
// NAME maps to <mount>/data/<prefix>/<NAME>, field <field>, unless paths
// maps it explicitly: {"OPENAI_API_KEY": "model/openai#api_key"}.
//
// Config:
//
//	address:     Vault URL (default $VAULT_ADDR)
//	token_env:   env var holding the token (default "VAULT_TOKEN")
//	token_file:  file holding the token, re-read on every fetch (e.g. a
//	             Vault agent sink); wins over token_env
//	namespace:   X-Vault-Namespace (Vault Enterprise / HCP)
//	mount:       KV v2 mount ("secret"), prefix ("kiln"), field ("value")
//	paths:       explicit NAME -> "path#field" map
//	ttl_seconds: cache lifetime (300)
//	allow_insecure: allow a non-loopback http address (the Vault token is otherwise TLS-only)
//	allow:       glob patterns of names that may be read
//	             (grants or allow is required; there is no read-all default)
//	grants:      {module instance: [names]}: when set, a module may read only
//	             the names granted to it (the caller is set by the core)
//
// Example:
//
//	kiln.WithModuleConfig("secrets_vault", map[string]any{
//		"address":     "https://vault.internal:8200",
//		"mount":       "secret",
//		"prefix":      "kiln",
//		"allow":       []any{"*_API_KEY"},
//		"ttl_seconds": 300,
//	})
var Permissions = []string{"net", "env", "env:secrets", "fs:read"}

// validName keeps secret names to one path segment of safe characters,
// so a name can't climb out of the configured prefix ("../other").
func validName(n string) bool {
	if n == "" || len(n) > 128 || n == "." || n == ".." {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func Apply(p *sdk.Module) error {
	envAddr, err := p.Env("VAULT_ADDR")
	if err != nil {
		return err
	}
	addr := strings.TrimRight(p.ConfigString("address", envAddr), "/")
	if addr == "" {
		return fmt.Errorf("secrets_vault: set config address or VAULT_ADDR")
	}
	tokenEnv := p.ConfigString("token_env", "VAULT_TOKEN")
	tokenFile := p.ConfigString("token_file", "")
	namespace := p.ConfigString("namespace", "")
	mount := strings.Trim(p.ConfigString("mount", "secret"), "/")
	prefix := strings.Trim(p.ConfigString("prefix", "kiln"), "/")
	field := p.ConfigString("field", "value")
	paths := p.ConfigMap("paths")
	ttl := time.Duration(p.ConfigInt("ttl_seconds", 300)) * time.Second
	allow := p.ConfigStrings("allow")
	grants := p.ConfigMap("grants")
	allowInsecure := p.ConfigBool("allow_insecure", false)
	if err := sdk.CheckCredentialURL(addr, true, allowInsecure); err != nil {
		return fmt.Errorf("secrets_vault: %w", err)
	}
	if len(allow) == 0 && grants == nil {
		// Never default to "every module may read everything in the mount".
		return fmt.Errorf("secrets_vault: set grants (per module) or allow (name patterns)")
	}
	if len(allow) == 0 {
		allow = []string{"*"} // grants decide
	}
	au, err := url.Parse(addr)
	if err != nil || au.Hostname() == "" {
		return fmt.Errorf("secrets_vault: address must be an absolute URL")
	}
	hc, err := p.HTTP(sdk.HTTPOptions{
		AllowHosts:    []string{au.Hostname()},
		Timeout:       10 * time.Second,
		AllowPrivate:  true, // Vault is often on a private or loopback address
		AllowInsecure: allowInsecure,
	})
	if err != nil {
		return err
	}

	token := func() (string, error) {
		if tokenFile != "" {
			// Read on every fetch, so a rotated file is picked up. Symlinks
			// are followed first: the broker's directory can't leave itself.
			resolved, err := filepath.EvalSymlinks(tokenFile)
			if err != nil {
				return "", fmt.Errorf("vault token file: %v", err)
			}
			td, err := p.Dir(filepath.Dir(resolved), false)
			if err != nil {
				return "", fmt.Errorf("vault token file: %v", err)
			}
			b, err := td.ReadFile(filepath.Base(resolved))
			td.Close()
			if err != nil {
				return "", fmt.Errorf("vault token file: %v", err)
			}
			t := strings.TrimSpace(string(b))
			p.RegisterSecret(t)
			return t, nil
		}
		t, err := p.Env(tokenEnv)
		if err != nil {
			return "", err
		}
		if t == "" {
			return "", fmt.Errorf("vault token: %s is not set", tokenEnv)
		}
		p.RegisterSecret(t)
		return t, nil
	}
	fetch := func(c context.Context, name string) (string, error) {
		kvPath, f := prefix+"/"+name, field
		if prefix == "" {
			kvPath = name
		}
		if v, ok := paths[name].(string); ok && v != "" {
			kvPath, f = v, field
			if i := strings.LastIndex(v, "#"); i >= 0 {
				kvPath, f = v[:i], v[i+1:]
			}
		}
		tok, err := token()
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(c, "GET", addr+"/v1/"+mount+"/data/"+strings.Trim(kvPath, "/"), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("X-Vault-Token", tok)
		if namespace != "" {
			req.Header.Set("X-Vault-Namespace", namespace)
		}
		resp, err := hc.Do(req)
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
	return p.Provide("secrets", &sdk.Secrets{
		Get: func(c context.Context, caller, name string) (string, error) {
			if !validName(name) {
				return "", fmt.Errorf("invalid secret name %q", name)
			}
			if grants != nil {
				granted := false
				var list []any
				for _, n := range []string{caller} {
					if l, ok := grants[n].([]any); ok {
						list = l
						break
					}
				}
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

// Module is the compiled form of "secrets_vault". modules/std registers it.
var Module = engine.Module{Name: "secrets_vault", Requires: nil, Provides: []string{"secrets"}, Permissions: Permissions, Apply: Apply}

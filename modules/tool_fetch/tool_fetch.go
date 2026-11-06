package tool_fetch

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Fetches web pages for the model. Egress is restricted: only hosts in
// allow_hosts (glob patterns, e.g. "*.example.com"), and never private,
// loopback or link-local addresses (SSRF protection) unless allow_private.
//
// Config: allow_hosts (required; ["*"] = any public host), allow_private
// (false), max_bytes (500000), timeout_seconds (20).
//
// Example:
//
//	tool_fetch.New(map[string]any{
//		"allow_hosts": []any{"*.wikipedia.org", "docs.python.org"},
//		"max_bytes":   200000,
//	})
var Permissions = []string{"net"}

func Apply(p *sdk.Scope) error {
	allow := p.ConfigStrings("allow_hosts")
	if len(allow) == 0 {
		return fmt.Errorf("tool_fetch: config allow_hosts is required")
	}
	private := p.ConfigBool("allow_private", false)
	maxBytes := int64(p.ConfigInt("max_bytes", 500000))
	timeout := time.Duration(p.ConfigInt("timeout_seconds", 20)) * time.Second

	// The destination is the model's choice, so the broker can't know the
	// host in advance: AllowHosts is "*", and Run restricts the host to
	// allow_hosts before every request. The broker refuses redirects and
	// checks each address it dials (AllowPrivate follows allow_private).
	hc, err := p.HTTP(sdk.HTTPOptions{
		AllowHosts:   []string{"*"},
		Timeout:      timeout,
		AllowPrivate: private,
	})
	if err != nil {
		return err
	}
	tags := regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`)
	spaces := regexp.MustCompile(`\n\s*\n+`)
	p.AddTool(sdk.Tool{
		Name:        "http_fetch",
		Description: "Fetch a URL (http/https) and return its text. Allowed hosts: " + strings.Join(allow, ", "),
		Schema:      sdk.Object(map[string]any{"url": sdk.Str("the URL")}, "url"),
		Meta:        map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			u, err := url.Parse(sdk.ArgString(args, "url"))
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return "", fmt.Errorf("only http(s) URLs are allowed")
			}
			if !allowed(allow, u.Hostname()) {
				return "", fmt.Errorf("host %s is not in allow_hosts", u.Hostname())
			}
			req, _ := http.NewRequestWithContext(c, "GET", u.String(), nil)
			req.Header.Set("User-Agent", "kiln-http-fetch/1")
			resp, err := hc.Do(req)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
			text := string(b)
			if strings.Contains(resp.Header.Get("Content-Type"), "html") {
				text = spaces.ReplaceAllString(tags.ReplaceAllString(text, ""), "\n\n")
			}
			return fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, u.String(), strings.TrimSpace(text)), nil
		},
	})
	return nil
}

func allowed(pats []string, host string) bool {
	for _, pat := range pats {
		if ok, _ := path.Match(pat, host); ok {
			return true
		}
	}
	return false
}

// Module is the compiled form of "tool_fetch". modules/std registers it.
var Module = engine.Module{Name: "tool_fetch", Requires: nil, Provides: nil, Permissions: Permissions, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

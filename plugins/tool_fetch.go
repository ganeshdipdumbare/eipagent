//go:build ignore

package plugin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"eipagent/sdk"
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
//	eipagent.WithPlugin("tool_fetch", map[string]any{
//		"allow_hosts": []any{"*.wikipedia.org", "docs.python.org"},
//		"max_bytes":   200000,
//	})
var Permissions = []string{"net"}

func Apply(p *sdk.Plugin) error {
	allow := p.ConfigStrings("allow_hosts")
	if len(allow) == 0 {
		return fmt.Errorf("tool_fetch: config allow_hosts is required")
	}
	private := p.ConfigBool("allow_private", false)
	maxBytes := int64(p.ConfigInt("max_bytes", 500000))
	timeout := time.Duration(p.ConfigInt("timeout_seconds", 20)) * time.Second

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Check the resolved address at connect time, so DNS tricks and
			// redirects can't reach internal services.
			DialContext: func(c context.Context, network, addr string) (net.Conn, error) {
				host, port, _ := net.SplitHostPort(addr)
				ips, err := net.DefaultResolver.LookupIPAddr(c, host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if !private && (ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() || ip.IP.IsUnspecified()) {
						return nil, fmt.Errorf("blocked: %s resolves to a private address", host)
					}
				}
				return dialer.DialContext(c, network, net.JoinHostPort(ips[0].IP.String(), port))
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return fmt.Errorf("too many redirects")
			}
			if !allowed(allow, req.URL.Hostname()) {
				return fmt.Errorf("redirect to %s is not allowed", req.URL.Hostname())
			}
			return nil
		},
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
			req.Header.Set("User-Agent", "eipagent-http-fetch/1")
			resp, err := client.Do(req)
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

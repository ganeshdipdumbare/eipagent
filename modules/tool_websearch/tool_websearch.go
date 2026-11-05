package tool_websearch

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Web search for the model: a web_search tool returning titles, URLs and
// snippets. Pair it with tool_fetch to read the pages it finds.
//
// Config:
//
//	backend:         duckduckgo (default, no key) | tavily | brave | searxng
//	api_key:         literal or secret://NAME (tavily: secret://TAVILY_API_KEY,
//	                 brave: secret://BRAVE_API_KEY by default)
//	base_url:        override the backend URL (required for searxng)
//	max_results:     default 5 (the model may ask for up to 20)
//	timeout_seconds: default 20
//	allow_insecure:  allow http base_url with an api_key
//
// Example:
//
//	kiln.WithModuleConfig("tool_websearch", map[string]any{"backend": "tavily", "max_results": 8})
//	kiln.WithModuleConfig("tool_fetch", map[string]any{"allow_hosts": []any{"*"}}) // to read the results
//	// Shortcut: kiln.WithWebSearch("duckduckgo")
var Permissions = []string{"net"}

var Requires = []string{"?secrets"} // secret:// keys/headers are optional

type hit struct{ Title, URL, Snippet string }

func Apply(p *sdk.Module) error {
	backend := strings.ToLower(p.ConfigString("backend", "duckduckgo"))
	maxResults := p.ConfigInt("max_results", 5)
	allowInsecure := p.ConfigBool("allow_insecure", false)
	var keyRef, base string
	switch backend {
	case "duckduckgo":
		base = "https://html.duckduckgo.com/html/"
	case "tavily":
		keyRef, base = "secret://TAVILY_API_KEY", "https://api.tavily.com/search"
	case "brave":
		keyRef, base = "secret://BRAVE_API_KEY", "https://api.search.brave.com/res/v1/web/search"
	case "searxng":
	default:
		return fmt.Errorf("tool_websearch: unknown backend %q (duckduckgo, tavily, brave, searxng)", backend)
	}
	keyRef = p.ConfigString("api_key", keyRef)
	base = p.ConfigString("base_url", base)
	if base == "" {
		return fmt.Errorf("tool_websearch: config base_url is required for searxng")
	}
	bu, err := url.Parse(base)
	if err != nil || (bu.Scheme != "http" && bu.Scheme != "https") || bu.Host == "" {
		return fmt.Errorf("tool_websearch: base_url must be an absolute http(s) URL")
	}
	_, customBase := p.Config["base_url"]
	hc, err := p.HTTP(sdk.HTTPOptions{
		AllowHosts:    []string{bu.Hostname()},
		Timeout:       time.Duration(p.ConfigInt("timeout_seconds", 20)) * time.Second,
		AllowPrivate:  customBase, // a self-hosted searxng may be on a private address; the default backends are public
		AllowInsecure: allowInsecure || keyRef == "",
	})
	if err != nil {
		return err
	}

	do := func(hr *http.Request) ([]byte, error) {
		hr.Header.Set("User-Agent", "kiln-websearch/1")
		resp, err := hc.Do(hr)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if resp.StatusCode != 200 {
			if len(b) > 300 {
				b = b[:300]
			}
			return nil, fmt.Errorf("%s: HTTP %d: %s", backend, resp.StatusCode, strings.TrimSpace(string(b)))
		}
		return b, nil
	}

	search := func(c context.Context, q string, n int) ([]hit, error) {
		key := ""
		if keyRef != "" {
			k, err := p.SecretValue(keyRef)
			if err != nil {
				return nil, err
			}
			key = k
		}
		if err := sdk.CheckCredentialURL(base, key != "", allowInsecure); err != nil {
			return nil, err
		}
		var hits []hit
		switch backend {
		case "duckduckgo":
			hr, _ := http.NewRequestWithContext(c, "POST", base, strings.NewReader(url.Values{"q": {q}}.Encode()))
			hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			b, err := do(hr)
			if err != nil {
				return nil, err
			}
			hits = parseDDG(string(b))
		case "tavily":
			payload, _ := json.Marshal(map[string]any{"query": q, "max_results": n})
			hr, _ := http.NewRequestWithContext(c, "POST", base, bytes.NewReader(payload))
			hr.Header.Set("Content-Type", "application/json")
			hr.Header.Set("Authorization", "Bearer "+key)
			b, err := do(hr)
			if err != nil {
				return nil, err
			}
			var out struct {
				Results []struct{ Title, URL, Content string } `json:"results"`
			}
			if err := json.Unmarshal(b, &out); err != nil {
				return nil, fmt.Errorf("tavily: %w", err)
			}
			for _, r := range out.Results {
				hits = append(hits, hit{r.Title, r.URL, r.Content})
			}
		case "brave":
			hr, _ := http.NewRequestWithContext(c, "GET", base+"?"+url.Values{"q": {q}, "count": {fmt.Sprint(n)}}.Encode(), nil)
			hr.Header.Set("Accept", "application/json")
			hr.Header.Set("X-Subscription-Token", key)
			b, err := do(hr)
			if err != nil {
				return nil, err
			}
			var out struct {
				Web struct {
					Results []struct{ Title, URL, Description string } `json:"results"`
				} `json:"web"`
			}
			if err := json.Unmarshal(b, &out); err != nil {
				return nil, fmt.Errorf("brave: %w", err)
			}
			for _, r := range out.Web.Results {
				hits = append(hits, hit{r.Title, r.URL, r.Description})
			}
		case "searxng":
			hr, _ := http.NewRequestWithContext(c, "GET", strings.TrimRight(base, "/")+"/search?"+url.Values{"q": {q}, "format": {"json"}}.Encode(), nil)
			b, err := do(hr)
			if err != nil {
				return nil, err
			}
			var out struct {
				Results []struct{ Title, URL, Content string } `json:"results"`
			}
			if err := json.Unmarshal(b, &out); err != nil {
				return nil, fmt.Errorf("searxng: %w", err)
			}
			for _, r := range out.Results {
				hits = append(hits, hit{r.Title, r.URL, r.Content})
			}
		}
		if len(hits) > n {
			hits = hits[:n]
		}
		return hits, nil
	}

	p.AddTool(sdk.Tool{
		Name:        "web_search",
		Description: "Search the web. Returns titles, URLs and snippets of the top results.",
		Schema: sdk.Object(map[string]any{
			"query":       sdk.Str("what to search for"),
			"max_results": map[string]any{"type": "integer", "description": fmt.Sprintf("number of results (default %d, max 20)", maxResults)},
		}, "query"),
		Meta: map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			q := strings.TrimSpace(sdk.ArgString(args, "query"))
			if q == "" {
				return "", fmt.Errorf("query is required")
			}
			n := sdk.ArgInt(args, "max_results", maxResults)
			if n < 1 || n > 20 {
				n = maxResults
			}
			hits, err := search(c, q, n)
			if err != nil {
				return "", err
			}
			if len(hits) == 0 {
				return "No results for: " + q, nil
			}
			var b strings.Builder
			for i, h := range hits {
				fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, h.Title, h.URL)
				if h.Snippet != "" {
					fmt.Fprintf(&b, "   %s\n", h.Snippet)
				}
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	})
	return nil
}

var (
	ddgLink    = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippet = regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	ddgTags    = regexp.MustCompile(`<[^>]+>`)
)

// parseDDG reads DuckDuckGo's no-JavaScript results page.
func parseDDG(page string) []hit {
	links := ddgLink.FindAllStringSubmatch(page, -1)
	snips := ddgSnippet.FindAllStringSubmatch(page, -1)
	clean := func(s string) string { return strings.TrimSpace(html.UnescapeString(ddgTags.ReplaceAllString(s, ""))) }
	var hits []hit
	for i, l := range links {
		href := html.UnescapeString(l[1])
		// Result links go through a redirect: //duckduckgo.com/l/?uddg=<target>
		if u, err := url.Parse(href); err == nil && u.Query().Get("uddg") != "" {
			href = u.Query().Get("uddg")
		}
		if strings.Contains(href, "duckduckgo.com/y.js") { // ads
			continue
		}
		h := hit{Title: clean(l[2]), URL: href}
		if i < len(snips) {
			h.Snippet = clean(snips[i][1])
		}
		hits = append(hits, h)
	}
	return hits
}

// Module is the compiled form of "tool_websearch". modules/std registers it.
var Module = engine.Module{Name: "tool_websearch", Requires: Requires, Provides: nil, Permissions: Permissions, Secrets: []string{"TAVILY_API_KEY", "BRAVE_API_KEY"}, Apply: Apply}

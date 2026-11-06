package kiln

import (
	"net/url"
	"strings"
	"testing"
)

// Encoded separators and dot segments must always be ambiguous, so the
// broker refuses a path whose meaning can change between check and dial.
func FuzzAmbiguousPath(f *testing.F) {
	for _, s := range []string{
		"/v1/orders",
		"/v1/a/../b",
		"/v1/%2e%2e/b",
		"/v1/a%2fb",
		"/v1/a%5cb",
		"//",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		got := ambiguousPath(u)
		want := strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") ||
			strings.Contains(strings.ToLower(u.EscapedPath()), "%5c") ||
			strings.Contains(strings.ToLower(u.EscapedPath()), "%2e")
		for _, part := range strings.Split(u.Path, "/") {
			if part == ".." {
				want = true
			}
		}
		if got != want {
			t.Fatalf("ambiguousPath(%q) = %v, want %v", raw, got, want)
		}
	})
}

// An egress rule may only admit its own host, method and path. This is the
// check that stands between a sandbox placeholder and a real secret.
func FuzzEgressRuleAllows(f *testing.F) {
	rule, err := parseRule("GET api.example.com/orders/*")
	if err != nil {
		f.Fatal(err)
	}
	f.Add("GET", "api.example.com", "/orders/42")
	f.Add("POST", "api.example.com", "/orders/42")
	f.Add("GET", "evil.example.com", "/orders/42")
	f.Add("GET", "api.example.com", "/ordersX")
	f.Fuzz(func(t *testing.T, method, host, p string) {
		if !rule.allows(method, host, p) {
			return
		}
		if method != "GET" {
			t.Fatalf("allowed method %q", method)
		}
		if !strings.EqualFold(host, "api.example.com") {
			t.Fatalf("allowed host %q", host)
		}
		if p != "/orders/" && !strings.HasPrefix(p, "/orders/") {
			t.Fatalf("allowed path %q", p)
		}
	})
}

package sdk

import (
	"net/url"
	"testing"
)

// A URL accepted with a secret must never be plaintext HTTP to a
// non-loopback host.
func FuzzCheckCredentialURL(f *testing.F) {
	for _, s := range []string{
		"https://api.example.com/v1",
		"http://localhost:11434/v1",
		"http://127.0.0.1:8080/",
		"http://api.example.com/",
		"https://user:pass@api.example.com/",
		"file:///etc/passwd",
		"://",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if err := CheckCredentialURL(raw, true, false); err != nil {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("accepted a URL that does not parse: %q: %v", raw, err)
		}
		if u.Scheme == "http" && !IsLoopbackHost(u.Hostname()) {
			t.Fatalf("accepted plaintext credentials for %q", raw)
		}
		if u.User != nil {
			t.Fatalf("accepted a URL with userinfo: %q", raw)
		}
	})
}

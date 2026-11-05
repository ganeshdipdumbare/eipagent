package sdk

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// RefuseRedirect is an http.Client.CheckRedirect that refuses every
// redirect, so a module's credentials (headers, HMAC keys) can never be
// replayed to another host. Interpreted modules should use this instead
// of writing the equivalent function themselves.
func RefuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// IsLoopbackHost reports whether host names the local machine.
func IsLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	if h == "localhost" {
		return true
	}
	if strings.Contains(h, "%") && !strings.HasPrefix(h, "[") {
		return false // do not strip a zone from an unbracketed name
	}
	if i := strings.LastIndex(h, "%"); i >= 0 { // bracketed IPv6 zone
		h = h[:i]
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// IsBlockedIP reports addresses a broker or fetch tool must not dial:
// loopback, RFC1918/ULA, link-local, CGNAT, documentation, multicast,
// reserved and unspecified ranges.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0: // 0.0.0.0/8
			return true
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT
			return true
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
			return true
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15
			return true
		case v4[0] == 198 && v4[1] == 51 && v4[2] == 100: // 198.51.100.0/24
			return true
		case v4[0] == 203 && v4[1] == 0 && v4[2] == 113: // 203.0.113.0/24
			return true
		case v4[0] >= 240: // 240.0.0.0/4 reserved
			return true
		}
	}
	return false
}

// CheckCredentialURL rejects a URL that would send credentials somewhere
// unsafe: embedded userinfo, a non-http(s) scheme, or plaintext HTTP to a
// non-loopback host. Set allowInsecure for a deliberate exception (for
// example a private-network Vault or vLLM endpoint).
func CheckCredentialURL(raw string, hasSecret, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL %s must use http or https", u.Redacted())
	}
	if u.User != nil {
		return fmt.Errorf("URL %s must not contain credentials", u.Redacted())
	}
	if hasSecret && u.Scheme != "https" && !IsLoopbackHost(u.Hostname()) && !allowInsecure {
		return fmt.Errorf("URL %s must use https when credentials are configured (set allow_insecure: true for a private network)", u.Redacted())
	}
	return nil
}

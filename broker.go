package eipagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/ganeshdipdumbare/eipagent/engine"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// broker keeps secrets on the host for an agent running WithRunIn. The
// box's "secrets" service gets placeholders (host.secret); the box's
// HTTP requests are made by the host (host.http), which swaps a
// placeholder for its value only on requests its rules allow, redacts
// secret values out of the response and records every request.
type broker struct {
	host   *Agent
	hosts  map[string][]egressRule // secret name -> where its placeholder may go (RunIn.SecretHosts)
	egress []egressRule            // where the box may go without secrets (RunIn.Egress)
	// callers are the plugin names the box may ask for secrets as: the
	// ones the host put in its profile, plus the plugins it loads on its
	// own. The box names the caller itself, so this keeps a box from
	// claiming another plugin's grants. nil: any (not yet set).
	callers      map[string]bool
	secretGrants map[string]map[string]bool
	allowPrivate bool
	client       *http.Client

	mu    sync.Mutex
	byKey map[string]string       // caller \x00 name -> placeholder
	byPh  map[string]brokerSecret // placeholder -> who asked for what
}

type brokerSecret struct{ caller, name string }

// maxBrokerBody caps a response relayed to the box.
const maxBrokerBody = 32 << 20

// egressRule is one RunIn.SecretHosts or RunIn.Egress entry:
//
//	"api.github.com"                    any method, any path
//	"*.github.com"                      host glob
//	"GET api.github.com/repos/*"        only GET, paths under /repos/
//	"GET|POST api.acme.com/v1/orders"   these methods, this exact path
//
// A path pattern is a path.Match glob; a trailing "*" also matches
// deeper paths ("/repos/*" matches "/repos/a/b").
type egressRule struct {
	methods map[string]bool // nil: any
	host    string
	path    string // "": any
}

func parseRule(s string) (egressRule, error) {
	var r egressRule
	s = strings.TrimSpace(s)
	if m, rest, ok := strings.Cut(s, " "); ok {
		r.methods = map[string]bool{}
		for _, x := range strings.FieldsFunc(m, func(c rune) bool { return c == '|' || c == ',' }) {
			for _, c := range x {
				if c < 'A' || c > 'Z' {
					return r, fmt.Errorf("rule %q: %q is not an HTTP method (write \"GET host/path\")", s, x)
				}
			}
			r.methods[x] = true
		}
		s = strings.TrimSpace(rest)
	}
	host, p, _ := strings.Cut(s, "/")
	if host == "" || strings.ContainsAny(host, " \t") {
		return r, fmt.Errorf("rule %q: no host", s)
	}
	r.host = strings.ToLower(host)
	if p != "" {
		r.path = "/" + p
		if _, err := path.Match(r.path, ""); err != nil {
			return r, fmt.Errorf("rule %q: bad path pattern: %w", s, err)
		}
	}
	return r, nil
}

func parseRules(list []string) ([]egressRule, error) {
	out := make([]egressRule, 0, len(list))
	for _, s := range list {
		r, err := parseRule(s)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (r egressRule) allows(method, host, p string) bool {
	if r.methods != nil && !r.methods[method] {
		return false
	}
	if ok, _ := path.Match(r.host, strings.ToLower(host)); !ok {
		return false
	}
	if r.path == "" {
		return true
	}
	if p == "" {
		p = "/"
	}
	if strings.HasSuffix(r.path, "*") {
		// A trailing * matches the prefix itself and paths below it, but
		// never a sibling that merely starts with the same text
		// ("/orders*" must not admit "/ordersX").
		prefix := strings.TrimSuffix(r.path, "*")
		if p == prefix {
			return true
		}
		if strings.HasSuffix(prefix, "/") {
			return strings.HasPrefix(p, prefix)
		}
		return strings.HasPrefix(p, prefix+"/")
	}
	ok, _ := path.Match(r.path, p)
	return ok
}

func anyAllows(rules []egressRule, method, host, p string) bool {
	for _, r := range rules {
		if r.allows(method, host, p) {
			return true
		}
	}
	return false
}

// requestPath is the decoded, cleaned path used for rule matching and
// logging, so "/a/../b" is matched as "/b" rather than by its raw text.
func requestPath(u *url.URL) string {
	p := path.Clean(u.Path)
	if p == "" || p == "." {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// ambiguousPath reports a path whose meaning can change between the rule
// check and the destination: encoded separators or dot segments.
func ambiguousPath(u *url.URL) bool {
	raw := strings.ToLower(u.EscapedPath())
	if strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") || strings.Contains(raw, "%2e") {
		return true
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func newBroker(host *Agent, ri *RunIn) (*broker, error) {
	b := &broker{host: host, hosts: map[string][]egressRule{}, byKey: map[string]string{}, byPh: map[string]brokerSecret{},
		allowPrivate: ri != nil && ri.AllowPrivateEgress}
	// The broker makes the box's HTTP requests, so it must apply the same
	// SSRF protection as tool_fetch: by default it never dials loopback,
	// RFC1918, link-local or unspecified addresses, even when an egress
	// rule allows the host name.
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dial := func(c context.Context, network, addr string) (net.Conn, error) {
		if b.allowPrivate {
			return dialer.DialContext(c, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(c, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if sdk.IsBlockedIP(ip.IP) {
				return nil, fmt.Errorf("blocked: %s resolves to a private address", host)
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("blocked: %s has no address", host)
		}
		return dialer.DialContext(c, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
	b.client = &http.Client{
		Timeout:   5 * time.Minute,
		Transport: &http.Transport{DialContext: dial},
		// Redirects go back to the box, whose client asks again, so every
		// hop is checked against the rules.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var err error
	if b.egress, err = parseRules(ri.Egress); err != nil {
		return nil, fmt.Errorf("RunIn.Egress: %w", err)
	}
	for name, list := range ri.SecretHosts {
		if b.hosts[name], err = parseRules(list); err != nil {
			return nil, fmt.Errorf("RunIn.SecretHosts[%s]: %w", name, err)
		}
	}
	return b, nil
}

// setCallers records the plugins the box was started with (see callers).
func (b *broker) setSecretGrants(grants map[string][]string) {
	b.secretGrants = map[string]map[string]bool{}
	for name, list := range grants {
		set := map[string]bool{}
		for _, n := range list {
			set[n] = true
		}
		b.secretGrants[sdk.CanonicalPlugin(name)] = set
	}
}

func (b *broker) secretAllowed(caller, name string) bool {
	set, ok := b.secretGrants[sdk.CanonicalPlugin(caller)]
	return ok && set[name]
}

func (b *broker) setCallers(specs []engine.PluginSpec) {
	b.callers = map[string]bool{}
	for _, s := range specs {
		b.callers[sdk.CanonicalPlugin(s.Name())] = true
	}
	for _, use := range autoPlugins {
		b.callers[sdk.CanonicalPlugin(use)] = true
	}
}

func (b *broker) knownCaller(name string) bool {
	return b.callers == nil || b.callers[sdk.CanonicalPlugin(name)]
}

func (b *broker) services() (*sdk.Secrets, *sdk.Audit) {
	if b.host == nil {
		return nil, nil
	}
	snap := b.host.eng.Root().Snapshot()
	s, _ := snap.Services["secrets"].(*sdk.Secrets)
	a, _ := snap.Services["audit"].(*sdk.Audit)
	return s, a
}

func (b *broker) audit(kind string, d map[string]any) {
	if _, a := b.services(); a != nil && a.Record != nil {
		d["writer"], d["where"] = "core", "sandbox"
		a.Record(kind, d)
	}
}

// resolve returns the real value of a secret, checked against the
// grants of the box plugin that asked for it.
func (b *broker) resolve(ctx context.Context, caller, name string) (string, error) {
	s, _ := b.services()
	if s == nil || s.Get == nil {
		return "", errors.New("the host has no \"secrets\" service")
	}
	v, err := s.Get(ctx, caller, name)
	if err == nil {
		sdk.AddRedaction(v)
	}
	return v, err
}

// placeholder answers host.secret. The value is resolved on the host (so
// grants and "not set" errors surface in the box) but never sent.
func (b *broker) placeholder(ctx context.Context, caller, name string) (string, error) {
	var err error
	if b.knownCaller(caller) {
		if !b.secretAllowed(caller, name) {
			err = fmt.Errorf("sandbox: secret %s is not declared by %s", name, caller)
		} else {
			_, err = b.resolve(ctx, caller, name)
		}
	} else {
		err = fmt.Errorf("sandbox: secret %s asked for as %q, which is not a plugin in the box", name, caller)
	}
	d := map[string]any{"plugin": caller, "secret": name, "outcome": "granted", "value": "placeholder"}
	if err != nil {
		d["outcome"], d["error"] = "denied", sdk.Redact(err.Error())
		b.audit("secret.access", d)
		return "", err
	}
	k := caller + "\x00" + name
	b.mu.Lock()
	ph, ok := b.byKey[k]
	if !ok {
		ph = sdk.NewPlaceholder()
		b.byKey[k], b.byPh[ph] = ph, brokerSecret{caller, name}
	}
	b.mu.Unlock()
	b.audit("secret.access", d)
	return ph, nil
}

// fetch answers host.http: it checks the request against the rules,
// swaps placeholders for values, makes the request, redacts the
// response, and records the request (allowed or not) as sandbox.egress.
func (b *broker) fetch(ctx context.Context, req wireHTTPReq) (resp *wireHTTPResp, err error) {
	t := time.Now()
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "GET"
	}
	u, perr := url.Parse(req.URL)
	rec := map[string]any{"method": method, "url": sdk.Redact(req.URL)}
	if req.RunID != "" {
		rec["run"] = req.RunID
	}
	var names []string
	defer func() {
		rec["ms"] = time.Since(t).Milliseconds()
		if len(names) > 0 {
			rec["secrets"] = names
		}
		switch {
		case resp != nil:
			rec["decision"], rec["status"] = "allowed", resp.Status
		case rec["decision"] == nil:
			rec["decision"], rec["error"] = "allowed", sdk.Redact(err.Error()) // allowed, but the call failed
		}
		b.record(rec)
	}()
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		rec["decision"] = "denied"
		err = fmt.Errorf("sandbox egress: bad URL %q", req.URL)
		rec["error"] = err.Error()
		return nil, err
	}
	dest, p := u.Hostname(), requestPath(u)
	rec["host"], rec["path"] = dest, sdk.Redact(p)
	deny := func(e error) (*wireHTTPResp, error) {
		rec["decision"], rec["error"] = "denied", e.Error()
		return nil, e
	}
	var all strings.Builder
	all.WriteString(req.URL)
	for k, vs := range req.Header {
		all.WriteString("\n" + k + ": " + strings.Join(vs, ", "))
	}
	all.Write(req.Body)
	phs := sdk.FindPlaceholders(all.String())
	if ambiguousPath(u) || strings.ContainsAny(u.Path, ";\\") || strings.Contains(strings.ToLower(u.EscapedPath()), "%25") {
		return deny(fmt.Errorf("sandbox egress: ambiguous path %q (encoded separators, dot segments or path parameters)", u.EscapedPath()))
	}
	values := map[string]string{}
	for _, ph := range phs {
		b.mu.Lock()
		sec, ok := b.byPh[ph]
		b.mu.Unlock()
		if !ok {
			return deny(fmt.Errorf("sandbox egress: unknown secret placeholder %s", ph))
		}
		names = append(names, sec.name)
		if !anyAllows(b.hosts[sec.name], method, dest, p) {
			rec["plugin"] = sec.caller
			return deny(fmt.Errorf("sandbox egress: secret %s may not be sent to %s %s%s (RunIn.SecretHosts)", sec.name, method, dest, p))
		}
		v, err := b.resolve(ctx, sec.caller, sec.name)
		if err != nil {
			return deny(err)
		}
		values[ph] = v
	}
	if len(phs) == 0 && !anyAllows(b.egress, method, dest, p) {
		return deny(fmt.Errorf("sandbox egress: %s %s%s is not allowed (RunIn.Egress)", method, dest, p))
	}
	swap := func(s string) string {
		for ph, v := range values {
			s = strings.ReplaceAll(s, ph, v)
		}
		return s
	}
	body := req.Body
	for ph, v := range values {
		body = bytes.ReplaceAll(body, []byte(ph), []byte(v))
	}
	norm := *u
	norm.Path = requestPath(u)
	norm.RawPath = ""
	hr, err := http.NewRequestWithContext(ctx, method, swap(norm.String()), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			if strings.EqualFold(k, "Host") {
				if len(vs) > 0 && !strings.EqualFold(v, u.Host) {
					return deny(fmt.Errorf("sandbox egress: Host header %q does not match URL host %q", v, u.Host))
				}
				continue
			}
			hr.Header.Add(k, swap(v))
		}
	}
	hr.Host = u.Host
	hresp, err := b.client.Do(hr)
	if err != nil {
		return nil, errors.New(sdk.Redact(err.Error()))
	}
	defer hresp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(hresp.Body, maxBrokerBody+1))
	if err != nil {
		return nil, errors.New(sdk.Redact(err.Error()))
	}
	if len(rb) > maxBrokerBody {
		return nil, fmt.Errorf("sandbox egress: response from %s is over %d bytes", dest, maxBrokerBody)
	}
	out := &wireHTTPResp{Status: hresp.StatusCode, Header: map[string][]string{}, Body: []byte(sdk.Redact(string(rb)))}
	for k, vs := range hresp.Header {
		for _, v := range vs {
			out.Header[k] = append(out.Header[k], sdk.Redact(v))
		}
	}
	return out, nil
}

// record writes one sandbox.egress audit record and a host log line.
func (b *broker) record(d map[string]any) {
	line := fmt.Sprintf("sandbox egress %s %v %v%v", d["decision"], d["method"], d["host"], d["path"])
	if st, ok := d["status"]; ok {
		line += fmt.Sprintf(" -> %v", st)
	}
	if n, ok := d["secrets"]; ok {
		line += fmt.Sprintf(" secrets=%v", n)
	}
	if e, ok := d["error"]; ok {
		line += fmt.Sprintf(" (%v)", e)
	}
	if b.host != nil {
		b.host.eng.Root().Logf("%s", line)
	}
	b.audit("sandbox.egress", d)
}

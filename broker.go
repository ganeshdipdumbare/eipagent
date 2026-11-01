package eipagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

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
	client *http.Client

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
	if ok, _ := path.Match(r.path, p); ok {
		return true
	}
	return strings.HasSuffix(r.path, "*") && strings.HasPrefix(p, strings.TrimSuffix(r.path, "*"))
}

func anyAllows(rules []egressRule, method, host, p string) bool {
	for _, r := range rules {
		if r.allows(method, host, p) {
			return true
		}
	}
	return false
}

func newBroker(host *Agent, ri *RunIn) (*broker, error) {
	b := &broker{host: host, hosts: map[string][]egressRule{}, byKey: map[string]string{}, byPh: map[string]brokerSecret{},
		// Redirects go back to the box, whose client asks again, so every
		// hop is checked against the rules.
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
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

func (b *broker) services() (*sdk.Secrets, *sdk.Audit) {
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
	_, err := b.resolve(ctx, caller, name)
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
	if !ok {
		b.audit("secret.access", d)
	}
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
	dest, p := u.Hostname(), u.EscapedPath()
	if p == "" {
		p = "/"
	}
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
	hr, err := http.NewRequestWithContext(ctx, method, swap(req.URL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hr.Header.Add(k, swap(v))
		}
	}
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
	b.host.eng.Root().Logf("%s", line)
	b.audit("sandbox.egress", d)
}

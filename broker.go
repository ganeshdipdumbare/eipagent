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

	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// broker keeps secrets on the host for an agent running WithRunIn. The
// box's "secrets" service gets placeholders (host.secret); the box's
// HTTP requests are made by the host (host.http), which swaps a
// placeholder for its value only on requests to that secret's allowed
// hosts and redacts secret values out of the response.
type broker struct {
	host   *Agent
	hosts  map[string][]string // secret name -> allowed host globs (RunIn.SecretHosts)
	egress []string            // host globs the box may reach without secrets (RunIn.Egress)
	client *http.Client

	mu    sync.Mutex
	byKey map[string]string       // caller \x00 name -> placeholder
	byPh  map[string]brokerSecret // placeholder -> who asked for what
}

type brokerSecret struct{ caller, name string }

// maxBrokerBody caps a response relayed to the box.
const maxBrokerBody = 32 << 20

func newBroker(host *Agent, ri *RunIn) *broker {
	return &broker{host: host, hosts: ri.SecretHosts, egress: ri.Egress, byKey: map[string]string{}, byPh: map[string]brokerSecret{},
		// Redirects go back to the box, whose client asks again, so every
		// hop is checked against the allowlists.
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
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

func hostAllowed(globs []string, host string) bool {
	host = strings.ToLower(host)
	for _, g := range globs {
		if ok, _ := path.Match(strings.ToLower(g), host); ok {
			return true
		}
	}
	return false
}

// fetch answers host.http: it checks the destination, swaps placeholders
// for values, makes the request and redacts the response.
func (b *broker) fetch(ctx context.Context, req wireHTTPReq) (*wireHTTPResp, error) {
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("sandbox egress: bad URL %q", req.URL)
	}
	dest := u.Hostname()
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
			return nil, fmt.Errorf("sandbox egress: unknown secret placeholder %s", ph)
		}
		if !hostAllowed(b.hosts[sec.name], dest) {
			b.audit("secret.egress_denied", map[string]any{"plugin": sec.caller, "secret": sec.name, "host": dest})
			return nil, fmt.Errorf("sandbox egress: secret %s may not be sent to %s (RunIn.SecretHosts)", sec.name, dest)
		}
		v, err := b.resolve(ctx, sec.caller, sec.name)
		if err != nil {
			return nil, err
		}
		values[ph] = v
	}
	if len(phs) == 0 && !hostAllowed(b.egress, dest) {
		b.audit("sandbox.egress_denied", map[string]any{"host": dest})
		return nil, fmt.Errorf("sandbox egress: %s is not allowed (RunIn.Egress)", dest)
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
	hr, err := http.NewRequestWithContext(ctx, req.Method, swap(req.URL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hr.Header.Add(k, swap(v))
		}
	}
	resp, err := b.client.Do(hr)
	if err != nil {
		return nil, errors.New(sdk.Redact(err.Error()))
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxBrokerBody+1))
	if err != nil {
		return nil, errors.New(sdk.Redact(err.Error()))
	}
	if len(rb) > maxBrokerBody {
		return nil, fmt.Errorf("sandbox egress: response from %s is over %d bytes", dest, maxBrokerBody)
	}
	out := &wireHTTPResp{Status: resp.StatusCode, Header: map[string][]string{}, Body: []byte(sdk.Redact(string(rb)))}
	for k, vs := range resp.Header {
		for _, v := range vs {
			out.Header[k] = append(out.Header[k], sdk.Redact(v))
		}
	}
	return out, nil
}

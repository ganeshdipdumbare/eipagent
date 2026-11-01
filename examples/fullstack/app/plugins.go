package app

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ganeshdipdumbare/eipagent/pluginpkg"
)

// Plugins added from the UI go through the same lifecycle a platform
// team would use: the code is linted without running, signed by this
// server, held as VALIDATED until a supervisor approves it, and then
// enabled in the running agent. A new version that fails to start never
// replaces the running one, and the previous version is one click away.

// Sample is a plugin source the UI offers. On the public demo only these
// exact sources are accepted; free-form code needs -allow-plugin-code.
type Sample struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Title string `json:"title"`
	Ask   string `json:"ask"` // what to type in the chat to use it
	Code  string `json:"code"`
}

const shippingHead = `//go:build ignore

package plugin

import (
	"context"
	"strings"

	"eipagent/sdk"
)

`

// Samples: one tool plugin and two updates (the second fails to start,
// to show that a broken update keeps the running version). Each submit
// gets the next version number, whichever sample it is.
var Samples = []Sample{
	{ID: "shipping-v1", Name: "shipping_estimate", Title: "Shipping estimate: standard delivery",
		Ask: "What's the shipping estimate to Berlin?",
		Code: shippingHead + `// Estimates delivery time to a city.
func Apply(p *sdk.Plugin) error {
	p.AddTool(sdk.Tool{
		Name:        "shipping_estimate",
		Description: "Estimate delivery time to a city",
		Schema:      sdk.Object(map[string]any{"city": sdk.Str("destination city")}, "city"),
		Meta:        map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			city := strings.TrimSpace(sdk.ArgString(args, "city"))
			return "Standard delivery to " + city + " takes 3 to 5 business days.", nil
		},
	})
	return nil
}
`},
	{ID: "shipping-v2", Name: "shipping_estimate", Title: "Shipping estimate, update: adds express",
		Ask: "What's the shipping estimate to Berlin?",
		Code: shippingHead + `// Estimates delivery time to a city, with an express option.
func Apply(p *sdk.Plugin) error {
	p.AddTool(sdk.Tool{
		Name:        "shipping_estimate",
		Description: "Estimate delivery time to a city, standard and express",
		Schema:      sdk.Object(map[string]any{"city": sdk.Str("destination city")}, "city"),
		Meta:        map[string]any{"readonly": true},
		Run: func(c context.Context, args map[string]any) (string, error) {
			city := strings.TrimSpace(sdk.ArgString(args, "city"))
			return "Standard delivery to " + city + " takes 3 to 5 business days; express arrives the next business day for $12.", nil
		},
	})
	return nil
}
`},
	{ID: "shipping-v3", Name: "shipping_estimate", Title: "Shipping estimate, broken update (fails to start)",
		Ask: "What's the shipping estimate to Berlin?",
		Code: strings.Replace(shippingHead, "\"context\"\n", "\"context\"\n\t\"errors\"\n", 1) + `// Estimates delivery time from the carrier's API. This version fails to
// start (the carrier isn't configured), so the running version stays.
func Apply(p *sdk.Plugin) error {
	carrier := p.ConfigString("carrier_url", "")
	if carrier == "" {
		return errors.New("shipping_estimate: carrier_url is not configured")
	}
	p.AddTool(sdk.Tool{
		Name: "shipping_estimate",
		Run: func(c context.Context, args map[string]any) (string, error) {
			return strings.TrimSpace(carrier), nil
		},
	})
	return nil
}
`},
}

// signingKey loads (or creates) the key this server signs UI plugins with.
func signingKey(dir string) (ed25519.PrivateKey, string, error) {
	path := filepath.Join(dir, "plugin-signing.key")
	if b, err := os.ReadFile(path); err == nil {
		sk, err := pluginpkg.ParsePrivateKey(string(b))
		if err != nil {
			return nil, "", err
		}
		pub, _ := os.ReadFile(path + ".pub")
		return sk, string(pub), nil
	}
	pub, priv, err := pluginpkg.GenerateKey()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(path, []byte(priv), 0o600); err != nil {
		return nil, "", err
	}
	os.WriteFile(path+".pub", []byte(pub), 0o644)
	sk, _ := pluginpkg.ParsePrivateKey(priv)
	return sk, pub, nil
}

// openPluginStore opens the store UI plugins are installed into.
func openPluginStore(dataDir string) (*pluginpkg.Store, ed25519.PrivateKey, error) {
	sk, pub, err := signingKey(dataDir)
	if err != nil {
		return nil, nil, err
	}
	keys, err := pluginpkg.TrustedKeys(pub)
	if err != nil {
		return nil, nil, err
	}
	st, err := pluginpkg.OpenStore(filepath.Join(dataDir, "plugins"), pluginpkg.StoreOptions{
		Verify: pluginpkg.VerifyOptions{TrustedKeys: keys, RequireSignature: true},
	})
	return st, sk, err
}

// ---- HTTP ----

type pluginView struct {
	Name     string        `json:"name"`
	Active   string        `json:"active,omitempty"`
	Previous string        `json:"previous,omitempty"`
	State    string        `json:"engine_state,omitempty"` // in the running agent
	Versions []versionView `json:"versions"`
}

type versionView struct {
	Version     string `json:"version"`
	State       string `json:"state"`
	Summary     string `json:"summary"`
	InstalledBy string `json:"installed_by,omitempty"`
	ApprovedBy  string `json:"approved_by,omitempty"`
	Error       string `json:"error,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Signer      string `json:"signer,omitempty"`
}

func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request) {
	running := map[string]string{}
	for _, st := range s.agent.Status() {
		running[st.Name] = string(st.State)
	}
	var out []pluginView
	for _, p := range s.plugins.List() {
		pv := pluginView{Name: p.Name, Active: p.Active, Previous: p.Previous, State: running[p.Name]}
		var vers []string
		for v := range p.Versions {
			vers = append(vers, v)
		}
		sort.Strings(vers)
		for _, v := range vers {
			x := p.Versions[v]
			summary := ""
			if pkg, err := s.plugins.Package(p.Name, v); err == nil {
				if fa, err := pluginpkg.Analyze(p.Name+".go", pkg.Code()); err == nil {
					summary = strings.TrimPrefix(pluginpkg.Summary(p.Name, fa), p.Name+": ")
				}
			}
			pv.Versions = append(pv.Versions, versionView{Version: v, State: string(x.State), Summary: summary, InstalledBy: x.InstalledBy,
				ApprovedBy: x.ApprovedBy, Error: x.Error, Reason: x.Reason, Signer: x.Signer})
		}
		out = append(out, pv)
	}
	writeJSON(w, 200, map[string]any{"plugins": out, "samples": Samples, "allow_code": s.cfg.AllowPluginCode})
}

// submitPlugin installs a new plugin, or a new version of one.
func (s *Server) submitPlugin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User   string `json:"user"`
		Name   string `json:"name"`
		Code   string `json:"code"`
		Sample string `json:"sample"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, errors.New("send {\"name\", \"code\"} or {\"sample\"}"))
		return
	}
	if _, ok := users[body.User]; !ok {
		writeErr(w, 401, errors.New("unknown user"))
		return
	}
	if body.Sample != "" {
		for _, sm := range Samples {
			if sm.ID == body.Sample {
				body.Name, body.Code = sm.Name, sm.Code
			}
		}
	}
	if !s.cfg.AllowPluginCode {
		// The public demo runs only the bundled samples: free-form code
		// would run on this server.
		known := false
		for _, sm := range Samples {
			if sm.Name == body.Name && sm.Code == body.Code {
				known = true
			}
		}
		if !known {
			writeErr(w, 403, errors.New("this demo only accepts the sample plugins; run it locally with -allow-plugin-code to write your own"))
			return
		}
	}
	// Demo rule: plugins from the UI get no permissions (no network,
	// files, processes or environment).
	if fa, err := pluginpkg.Analyze(body.Name+".go", []byte(body.Code)); err == nil && len(fa.Permissions) > 0 {
		writeJSON(w, 422, map[string]any{"error": "plugins added from this UI can't declare permissions",
			"summary": pluginpkg.Summary(body.Name, fa)})
		return
	}
	v, rep, err := s.plugins.InstallSource(body.Name, []byte(body.Code), s.signer, body.User)
	resp := map[string]any{}
	if rep != nil {
		resp["issues"] = rep.Issues
		if rep.Facts != nil {
			resp["summary"] = pluginpkg.Summary(body.Name, rep.Facts)
		}
	}
	if err != nil {
		resp["error"] = err.Error()
		writeJSON(w, 422, resp)
		return
	}
	resp["version"], resp["state"] = v.Version, string(v.State)
	writeJSON(w, 200, resp)
}

// pluginAction approves and enables, rolls back or removes a plugin.
// Supervisors only, like approving a refund.
func (s *Server) pluginAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User    string `json:"user"`
		Action  string `json:"action"` // approve | rollback | disable | remove
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, errors.New("send {\"action\", \"user\"}"))
		return
	}
	if u, ok := users[body.User]; !ok || u.Role != "supervisor" {
		writeErr(w, 403, errors.New("only supervisors can approve, roll back or remove plugins"))
		return
	}
	name := r.PathValue("name")
	var err error
	switch body.Action {
	case "approve": // approve this version and switch to it
		if err = s.plugins.Approve(name, body.Version, body.User); err == nil {
			err = s.plugins.Enable(name, body.Version)
		}
	case "rollback":
		err = s.plugins.Rollback(name)
	case "disable":
		err = s.plugins.Disable(name)
	case "remove":
		err = s.plugins.Uninstall(name)
	default:
		writeErr(w, 400, errors.New("action must be approve, rollback, disable or remove"))
		return
	}
	if err != nil {
		writeJSON(w, 422, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

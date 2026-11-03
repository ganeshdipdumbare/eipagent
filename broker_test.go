package eipagent

import (
	"testing"

	"github.com/ganeshdipdumbare/eipagent/engine"
)

func TestEgressRules(t *testing.T) {
	for _, c := range []struct {
		rule, method, host, path string
		ok                       bool
	}{
		{"api.github.com", "POST", "api.github.com", "/anything", true},
		{"api.github.com", "GET", "evil.com", "/", false},
		{"*.github.com", "GET", "API.GitHub.com", "/", true},
		{"GET api.github.com/repos/*", "GET", "api.github.com", "/repos/a/b", true},
		{"GET api.github.com/repos/*", "DELETE", "api.github.com", "/repos/a", false},
		{"GET api.github.com/repos/*", "GET", "api.github.com", "/user", false},
		{"GET|POST api.acme.com/v1/orders", "POST", "api.acme.com", "/v1/orders", true},
		{"GET|POST api.acme.com/v1/orders", "POST", "api.acme.com", "/v1/orders/1", false},
		{"GET,PUT api.acme.com/v1/*/items", "PUT", "api.acme.com", "/v1/42/items", true},
	} {
		r, err := parseRule(c.rule)
		if err != nil {
			t.Fatalf("%s: %v", c.rule, err)
		}
		if got := r.allows(c.method, c.host, c.path); got != c.ok {
			t.Errorf("%q allows %s %s%s = %v, want %v", c.rule, c.method, c.host, c.path, got, c.ok)
		}
	}
	for _, bad := range []string{"get api.github.com", "api.github.com/[", "", "GET /path"} {
		if _, err := parseRule(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestBrokerKnownCallers(t *testing.T) {
	b := &broker{}
	if !b.knownCaller("anything") {
		t.Fatal("before setCallers every caller is accepted")
	}
	b.setCallers([]engine.PluginSpec{{Use: "probe"}, {Use: "tool_fetch", As: "gh"}, {Use: "llm_mock"}})
	for name, ok := range map[string]bool{
		"probe":       true,
		"gh":          true,  // instance name
		"tool_fetch":  false, // only loaded as "gh"
		"model_mock":  true,  // current name of llm_mock
		"llm_mock":    true,
		"loop":        true, // loaded by the box on its own
		"secrets_env": false,
		"":            false,
	} {
		if got := b.knownCaller(name); got != ok {
			t.Errorf("knownCaller(%q) = %v, want %v", name, got, ok)
		}
	}
}

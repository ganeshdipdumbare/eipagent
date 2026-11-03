package eipagent

import (
	"net/url"
	"testing"
)

func TestEgressRulePrefixIsSegmentAware(t *testing.T) {
	r, err := parseRule("GET api.example.com/orders*")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/orders", "/orders/", "/orders/1", "/orders/a/b"} {
		if !r.allows("GET", "api.example.com", p) {
			t.Errorf("%s should match", p)
		}
	}
	for _, p := range []string{"/ordersX", "/orders-evil", "/steal", "/"} {
		if r.allows("GET", "api.example.com", p) {
			t.Errorf("%s should not match", p)
		}
	}
	if r.allows("POST", "api.example.com", "/orders") {
		t.Error("method limit ignored")
	}
}

func TestRequestPathNormalizesDotSegments(t *testing.T) {
	u, _ := url.Parse("https://api.example.com/orders/../steal")
	if got := requestPath(u); got != "/steal" {
		t.Fatalf("requestPath = %q", got)
	}
	if !ambiguousPath(u) {
		t.Fatal("dot segment not reported")
	}
	u, _ = url.Parse("https://api.example.com/orders%2F..%2Fsteal")
	if !ambiguousPath(u) {
		t.Fatal("encoded separator not reported")
	}
	u, _ = url.Parse("https://api.example.com/orders/1")
	if ambiguousPath(u) {
		t.Fatal("plain path reported as ambiguous")
	}
}

package kiln

import (
	"testing"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

func TestHostToolRequiresHostIssuedRun(t *testing.T) {
	r := &remote{
		runs:       map[string]*sdk.RunState{},
		runSeen:    map[string]time.Time{},
		hostIssued: map[string]bool{},
	}
	box := &sdk.RunState{ID: "box-run", User: "attacker", Values: map[string]any{}}
	if !r.setRun(box, false) {
		t.Fatal("setRun box")
	}
	if r.isHostIssued("box-run") {
		t.Fatal("box-minted run must not be host-issued")
	}
	if st := r.runState("box-run"); st == nil || st.User != "attacker" {
		t.Fatalf("box run should still be visible for host.llm: %+v", st)
	}

	host := &sdk.RunState{ID: "host-run", User: "alice", Roles: []string{"support"}, Values: map[string]any{}}
	if !r.setRun(host, true) {
		t.Fatal("setRun host")
	}
	if !r.isHostIssued("host-run") {
		t.Fatal("host-started run must be host-issued")
	}
}

func TestTouchRunRefreshesExpiry(t *testing.T) {
	r := &remote{
		runs:       map[string]*sdk.RunState{},
		runSeen:    map[string]time.Time{},
		hostIssued: map[string]bool{},
	}
	st := &sdk.RunState{ID: "long", User: "alice"}
	if !r.setRun(st, true) {
		t.Fatal("setRun")
	}
	r.mu.Lock()
	r.runSeen["long"] = time.Now().Add(-50 * time.Minute)
	r.mu.Unlock()
	r.touchRun("long")
	if got := r.runState("long"); got == nil {
		t.Fatal("touchRun should refresh runSeen so the run stays live")
	}
	if !r.isHostIssued("long") {
		t.Fatal("host-issued bit must survive touch")
	}
}

func TestExpiredRunLosesHostIssued(t *testing.T) {
	r := &remote{
		runs:       map[string]*sdk.RunState{},
		runSeen:    map[string]time.Time{},
		hostIssued: map[string]bool{},
	}
	st := &sdk.RunState{ID: "old", User: "alice"}
	if !r.setRun(st, true) {
		t.Fatal("setRun")
	}
	// Simulate runState expiry (skip endHostRun; no host engine in this unit test).
	r.mu.Lock()
	delete(r.runs, "old")
	delete(r.runSeen, "old")
	delete(r.hostIssued, "old")
	r.mu.Unlock()
	// Re-register as a box-minted model call would: tools must stay closed.
	if !r.setRun(&sdk.RunState{ID: "old", User: "forged"}, false) {
		t.Fatal("setRun after expiry")
	}
	if r.isHostIssued("old") {
		t.Fatal("re-binding an expired id from the box must not reopen host.tool")
	}
}

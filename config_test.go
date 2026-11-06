package kiln_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/modules/hook_approval"
	"github.com/ganeshdipdumbare/kiln/modules/tool_time"
)

// A standard module passed as a value composes like any application module.
func TestModuleValueComposes(t *testing.T) {
	a, err := kiln.New(kiln.Config{Model: "mock:test", Modules: []kiln.Module{tool_time.New()}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	found := false
	for _, s := range a.Status() {
		if s.Name == "tool_time" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool_time not started: %+v", a.Status())
	}
}

// A declared tool that another module already registered fails the start
// instead of disappearing.
func TestDuplicateToolFailsStart(t *testing.T) {
	mine := kiln.NewTool("current_time", "mine", func(ctx context.Context, in struct{}) (string, error) {
		return "mine", nil
	})
	_, err := kiln.New(kiln.Config{
		Model:   "mock:test",
		Tools:   []kiln.Tool{mine},
		Modules: []kiln.Module{tool_time.New()},
	})
	if err == nil || !strings.Contains(err.Error(), `"current_time" is already registered`) {
		t.Fatalf("want a duplicate-tool error, got %v", err)
	}
}

// Listing one module twice merges its configuration: option order cannot
// change the result, and it is not a second instance.
func TestModuleRowsMerge(t *testing.T) {
	a, err := kiln.New(kiln.Config{
		Model: "mock:test",
		Modules: []kiln.Module{
			hook_approval.New(map[string]any{"interrupt_on": map[string]any{"refund": true}}),
			hook_approval.New(map[string]any{"non_readonly": true}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	n := 0
	for _, s := range a.Status() {
		if s.Module == "hook_approval" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one hook_approval instance, got %d", n)
	}
}

// A supplying module replaces the default of its family in its position.
func TestReplacesDefaultFamily(t *testing.T) {
	a := mockAgent(t, nil, withModuleConfig("threads_file", map[string]any{"dir": t.TempDir()}))
	for _, s := range a.Status() {
		if s.Module == "threads_inmem" {
			t.Fatalf("threads_inmem should have been replaced: %+v", a.Status())
		}
	}
}

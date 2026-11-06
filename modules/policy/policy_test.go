package policy

import "testing"

func TestIsolatedSandbox(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"sandbox:docker", true},
		{"sandbox:microsandbox", true},
		{"sandbox:firecracker", true},
		{"sandbox:local", false},
		{"sandbox:host", false},
		{"sandbox:", false},
		{"", false},
		{"docker", false},
	} {
		if got := isolatedSandbox(tc.name); got != tc.ok {
			t.Errorf("isolatedSandbox(%q) = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

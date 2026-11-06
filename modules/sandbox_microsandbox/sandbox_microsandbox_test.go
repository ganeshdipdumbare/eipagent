package sandbox_microsandbox

import "testing"

func TestOwnedID(t *testing.T) {
	for _, tc := range []struct {
		id string
		ok bool
	}{
		{"kiln-0123456789ab", true},
		{"kiln-", false},
		{"other-0123456789ab", false},
		{"-kiln-0123456789ab", false},
		{"kiln-../../x", false},
	} {
		if got := ownedID(tc.id); got != tc.ok {
			t.Errorf("ownedID(%q) = %v, want %v", tc.id, got, tc.ok)
		}
	}
}

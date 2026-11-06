package trigger_webhook

import "testing"

func TestValidThreadID(t *testing.T) {
	for _, tc := range []struct {
		id string
		ok bool
	}{
		{"", true},
		{"gh-kiln", true},
		{"t-1", true},
		{"owner/repo-42", true},
		{"..", false},
		{"../etc/passwd", false},
		{"/abs", false},
		{"trailing/", false},
		{"bad\nid", false},
		{"bad id", false},
		{"bad\\id", false},
	} {
		if got := validThreadID(tc.id); got != tc.ok {
			t.Errorf("validThreadID(%q) = %v, want %v", tc.id, got, tc.ok)
		}
	}
}

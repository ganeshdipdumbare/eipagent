package main

import "testing"

func TestValidVMID(t *testing.T) {
	for _, tc := range []struct {
		id string
		ok bool
	}{
		{"fc-0123456789ab", true},
		{"fc-0123456789", false},
		{"fc-0123456789zz", false},
		{"../fc-0123456789ab", false},
		{"FC-0123456789ab", false},
	} {
		if got := validVMID(tc.id); got != tc.ok {
			t.Errorf("validVMID(%q) = %v, want %v", tc.id, got, tc.ok)
		}
	}
}

func TestValidTap(t *testing.T) {
	for _, tc := range []struct {
		tap, prefix string
		ok          bool
	}{
		{"fct12", "fct", true},
		{"fct", "fct", false},
		{"fct1;id", "fct", false},
		{"fct1/2", "fct", false},
		{"other1", "fct", false},
	} {
		if got := validTap(tc.tap, tc.prefix); got != tc.ok {
			t.Errorf("validTap(%q, %q) = %v, want %v", tc.tap, tc.prefix, got, tc.ok)
		}
	}
}

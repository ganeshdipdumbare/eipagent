package sqlval

import "testing"

func TestAsStringBytes(t *testing.T) {
	if s, ok := AsString([]byte("hi")); !ok || s != "hi" {
		t.Fatalf("AsString bytes: %q %v", s, ok)
	}
	if s, ok := AsString("hi"); !ok || s != "hi" {
		t.Fatalf("AsString string: %q %v", s, ok)
	}
	if _, ok := AsString(1); ok {
		t.Fatal("AsString int")
	}
	if b, ok := AsBytes("hi"); !ok || string(b) != "hi" {
		t.Fatalf("AsBytes string: %q %v", b, ok)
	}
}

func TestValidIdent(t *testing.T) {
	if !ValidIdent("eip_threads") || ValidIdent("") || ValidIdent("Bad") || ValidIdent(string(make([]byte, 64))) {
		t.Fatal("ValidIdent")
	}
}

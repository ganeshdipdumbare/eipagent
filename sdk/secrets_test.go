package sdk

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRedactCoversEncodingsAndTypedValues(t *testing.T) {
	secret := `pa"ss/word+1==`
	if !AddRedaction(secret) {
		t.Fatal("not registered")
	}
	if AddRedaction("abc") {
		t.Fatal("a 3-byte value can't be redacted and must say so")
	}
	quoted, _ := json.Marshal("token " + secret)
	for name, in := range map[string]string{
		"raw":    "key " + secret,
		"json":   string(quoted),
		"base64": base64.StdEncoding.EncodeToString([]byte(secret)),
	} {
		if out := Redact(in); strings.Contains(out, "pa") && strings.Contains(out, "word") {
			t.Errorf("%s form not redacted: %s", name, out)
		}
	}
	type creds struct {
		User, Pass string
	}
	v := RedactValue(map[string]any{
		"typed":  map[string]string{"k": secret},
		"slice":  []map[string]any{{"k": secret}},
		"struct": creds{"u", secret},
		"err":    errors.New("bad key " + secret),
		"num":    42,
	})
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "word+1") {
		t.Fatalf("typed values leaked: %s", b)
	}
	if !strings.Contains(string(b), `"num":42`) {
		t.Fatalf("numbers must pass through: %s", b)
	}
}

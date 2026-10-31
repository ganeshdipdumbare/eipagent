// Package audit verifies hash-chained audit logs written by audit_jsonl.
package audit

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// Verify checks every line's hash and the chain; it returns the number
// of valid entries, or an error naming the first broken line.
//
// The chain alone detects edits, but someone who can rewrite the whole
// file can also recompute it. For that, configure audit_jsonl with a key
// and use VerifyKeyed, or ship the log off the host with audit_http.
func Verify(path string) (int, error) { return verify(path, nil) }

// VerifyKeyed also checks each line's "mac", an HMAC-SHA256 of its hash
// under the key audit_jsonl was configured with. Without the key, a
// rewritten log can't produce valid macs.
func VerifyKeyed(path string, key []byte) (int, error) {
	if len(key) == 0 {
		return 0, fmt.Errorf("audit: empty key")
	}
	return verify(path, key)
}

func verify(path string, key []byte) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	prev, n := "", 0
	for sc.Scan() {
		n++
		var line struct {
			Body json.RawMessage `json:"body"`
			Prev string          `json:"prev"`
			Hash string          `json:"hash"`
			MAC  string          `json:"mac"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return n - 1, fmt.Errorf("line %d: %v", n, err)
		}
		if line.Prev != prev {
			return n - 1, fmt.Errorf("line %d: chain broken (a line was removed or reordered)", n)
		}
		h := sha256.Sum256(append([]byte(prev), line.Body...))
		if hex.EncodeToString(h[:]) != line.Hash {
			return n - 1, fmt.Errorf("line %d: hash mismatch (the entry was modified)", n)
		}
		if key != nil && !hmac.Equal([]byte(MAC(key, line.Hash)), []byte(line.MAC)) {
			return n - 1, fmt.Errorf("line %d: mac missing or invalid (written without the key, or forged)", n)
		}
		prev = line.Hash
	}
	return n, sc.Err()
}

// MAC is the per-line mac audit_jsonl writes when it has a key.
func MAC(key []byte, hash string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(hash))
	return hex.EncodeToString(m.Sum(nil))
}

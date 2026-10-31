// Package audit verifies hash-chained audit logs written by audit_jsonl.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// Verify checks every line's hash and the chain; it returns the number
// of valid entries, or an error naming the first broken line.
func Verify(path string) (int, error) {
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
		prev = line.Hash
	}
	return n, sc.Err()
}

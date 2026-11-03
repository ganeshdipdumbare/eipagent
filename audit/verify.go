// Package audit verifies hash-chained audit logs written by audit_jsonl.
package audit

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Verify checks every line's hash, sequence number and the chain; it
// returns the number of valid entries, or an error naming the first
// broken line.
//
// The chain alone detects edits, but someone who can rewrite the whole
// file can also recompute it. For that, configure audit_jsonl with a key
// and use VerifyKeyed, or ship the log off the host with audit_http.
func Verify(path string) (int, error) {
	n, _, err := verifyChain(path, nil)
	return n, err
}

// VerifyKeyed also checks each line's "mac", an HMAC-SHA256 of its hash
// under the key audit_jsonl was configured with, and the <log>.head
// checkpoint, which catches a truncated tail. Without the key, a
// rewritten log can't produce valid macs.
//
// The head lives beside the log, so someone who can rewrite both the log
// and the head can still roll the tail back together. Keep a copy of the
// last head (or pass an expected final hash from an external source) for
// that threat.
func VerifyKeyed(path string, key []byte) (int, error) {
	if len(key) == 0 {
		return 0, fmt.Errorf("audit: empty key")
	}
	n, last, err := verifyChain(path, key)
	if err != nil {
		return n, err
	}
	if err := checkHead(path, key, n, last); err != nil {
		return n, err
	}
	return n, nil
}

// MissingHeadError says a keyed log has no <log>.head checkpoint. The
// log still verifies; callers may report this as a warning rather than a
// failure (it means the log was written before head files existed, or
// the head was removed).
type MissingHeadError struct{ Path string }

func (e *MissingHeadError) Error() string {
	return fmt.Sprintf("audit: %s: no head checkpoint (the last entries can't be checked for truncation)", e.Path)
}

// Warning marks this error as advisory.
func (e *MissingHeadError) Warning() {}

// Head is the checkpoint audit_jsonl writes after each append.
type Head struct {
	Seq  int    `json:"seq"`
	Hash string `json:"hash"`
	MAC  string `json:"mac"`
}

// HeadPath is where audit_jsonl keeps the checkpoint for log path.
func HeadPath(path string) string { return path + ".head" }

// HeadMAC is the HMAC over seq|hash under a log's key.
func HeadMAC(key []byte, seq int, hash string) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%d|%s", seq, hash)
	return hex.EncodeToString(m.Sum(nil))
}

func verifyChain(path string, key []byte) (int, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
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
			return n - 1, prev, fmt.Errorf("line %d: %v", n, err)
		}
		var body struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(line.Body, &body); err != nil {
			return n - 1, prev, fmt.Errorf("line %d: body: %v", n, err)
		}
		if body.Seq != n {
			return n - 1, prev, fmt.Errorf("line %d: seq is %d, want %d (an entry was removed or reordered)", n, body.Seq, n)
		}
		if line.Prev != prev {
			return n - 1, prev, fmt.Errorf("line %d: chain broken (a line was removed or reordered)", n)
		}
		h := sha256.Sum256(append([]byte(prev), line.Body...))
		if hex.EncodeToString(h[:]) != line.Hash {
			return n - 1, prev, fmt.Errorf("line %d: hash mismatch (the entry was modified)", n)
		}
		if key != nil && !hmac.Equal([]byte(MAC(key, line.Hash)), []byte(line.MAC)) {
			return n - 1, prev, fmt.Errorf("line %d: mac missing or invalid (written without the key, or forged)", n)
		}
		prev = line.Hash
	}
	return n, prev, sc.Err()
}

func checkHead(path string, key []byte, n int, last string) error {
	hp := HeadPath(path)
	b, err := os.ReadFile(hp)
	if errors.Is(err, os.ErrNotExist) {
		return &MissingHeadError{Path: hp}
	}
	if err != nil {
		return err
	}
	var h Head
	if err := json.Unmarshal(b, &h); err != nil {
		return fmt.Errorf("audit: %s: %v", hp, err)
	}
	if h.Seq != n || h.Hash != last {
		return fmt.Errorf("audit: %s: the log ends at seq %d (%s), but the head says seq %d (%s): entries were removed from the tail", path, n, shortHash(last), h.Seq, shortHash(h.Hash))
	}
	if !hmac.Equal([]byte(HeadMAC(key, h.Seq, h.Hash)), []byte(h.MAC)) {
		return fmt.Errorf("audit: %s: head mac invalid (the checkpoint was forged)", hp)
	}
	return nil
}

// MAC is the per-line mac audit_jsonl writes when it has a key.
func MAC(key []byte, hash string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(hash))
	return hex.EncodeToString(m.Sum(nil))
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

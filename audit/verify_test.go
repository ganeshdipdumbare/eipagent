package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLog writes a valid chain for the given sequence numbers and
// returns the last hash. key may be nil (unkeyed).
func writeLog(t *testing.T, path string, seqs []int, key []byte) string {
	t.Helper()
	prev := ""
	var lines []string
	for _, seq := range seqs {
		body, _ := json.Marshal(map[string]any{"seq": seq, "type": "test", "data": map[string]any{}})
		h := sha256.Sum256(append([]byte(prev), body...))
		hash := hex.EncodeToString(h[:])
		mac := ""
		if key != nil {
			mac = `,"mac":"` + MAC(key, hash) + `"`
		}
		lines = append(lines, `{"body":`+string(body)+`,"prev":"`+prev+`","hash":"`+hash+`"`+mac+`}`)
		prev = hash
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return prev
}

func writeHead(t *testing.T, path string, seq int, hash string, key []byte) {
	t.Helper()
	h := Head{Seq: seq, Hash: hash, MAC: HeadMAC(key, seq, hash)}
	b, _ := json.Marshal(h)
	if err := os.WriteFile(HeadPath(path), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDetectsSeqGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	writeLog(t, path, []int{1, 3}, nil)
	if _, err := Verify(path); err == nil || !strings.Contains(err.Error(), "seq") {
		t.Fatalf("seq gap accepted: %v", err)
	}
}

func TestVerifyKeyedDetectsTruncatedTail(t *testing.T) {
	key := []byte("chain-key-123456789012345678901234")
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	last := writeLog(t, path, []int{1, 2, 3}, key)
	writeHead(t, path, 3, last, key)
	if _, err := VerifyKeyed(path, key); err != nil {
		t.Fatalf("complete log should verify: %v", err)
	}
	// Drop the last line but leave the checkpoint: the prefix is a valid
	// chain, so only the head reveals the truncation.
	b, _ := os.ReadFile(path)
	parts := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if err := os.WriteFile(path, []byte(strings.Join(parts[:2], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path); err != nil {
		t.Fatalf("the remaining prefix is a valid chain: %v", err)
	}
	if _, err := VerifyKeyed(path, key); err == nil || !strings.Contains(err.Error(), "tail") {
		t.Fatalf("truncated tail accepted: %v", err)
	}
}

func TestVerifyKeyedMissingHeadIsWarning(t *testing.T) {
	key := []byte("chain-key-123456789012345678901234")
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	writeLog(t, path, []int{1, 2}, key)
	_, err := VerifyKeyed(path, key)
	var missing *MissingHeadError
	if !errors.As(err, &missing) {
		t.Fatalf("want MissingHeadError, got %v", err)
	}
}

func TestVerifyKeyedRejectsForgedHead(t *testing.T) {
	key := []byte("chain-key-123456789012345678901234")
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	last := writeLog(t, path, []int{1, 2}, key)
	h := Head{Seq: 2, Hash: last, MAC: "00"}
	b, _ := json.Marshal(h)
	if err := os.WriteFile(HeadPath(path), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyKeyed(path, key); err == nil || !strings.Contains(err.Error(), "head mac") {
		t.Fatalf("forged head accepted: %v", err)
	}
}

func TestVerifyUnkeyedIgnoresHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	last := writeLog(t, path, []int{1, 2}, nil)
	// A head without a key is harmless for Verify, which never consults it.
	h := Head{Seq: 2, Hash: last}
	b, _ := json.Marshal(h)
	if err := os.WriteFile(HeadPath(path), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := Verify(path); err != nil || n != 2 {
		t.Fatalf("Verify: %d %v", n, err)
	}
}

func TestHeadMACChangesWithSeq(t *testing.T) {
	key := []byte("k")
	if HeadMAC(key, 1, "h") == HeadMAC(key, 2, "h") {
		t.Fatal("head mac does not bind seq")
	}
}

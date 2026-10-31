//go:build ignore

package plugin

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"

	"eipagent/sdk"
)

// Ships the audit stream to an HTTP endpoint (a SIEM, a log pipeline or a
// control plane), provided as "audit/sink/http". It receives every record
// audit_jsonl writes (already redacted and hash-chained, so the receiver
// can verify the chain) and POSTs them in batches as NDJSON.
//
// Requests carry X-EIP-Signature: sha256=<hex HMAC of the body> when
// hmac_key is set (use a secret:// reference). Failed batches are retried
// with backoff; when the queue is full the oldest records are dropped and
// counted (a gap in seq tells the receiver).
//
// Config: url (required), headers ({name: value or secret://NAME}),
// hmac_key (secret://NAME), batch_size (100), flush_seconds (2),
// max_queue (10000), retries (3), timeout_seconds (10).
var Permissions = []string{"net"}

var Inject = []string{"audit", "?secrets"}

func Apply(ctx *sdk.Context) error {
	url := ctx.ConfigString("url", "")
	if url == "" {
		return fmt.Errorf("audit_http: config url is required")
	}
	headers := ctx.ConfigMap("headers")
	hmacRef := ctx.ConfigString("hmac_key", "")
	batchSize := ctx.ConfigInt("batch_size", 100)
	flushEvery := time.Duration(ctx.ConfigInt("flush_seconds", 2)) * time.Second
	maxQueue := ctx.ConfigInt("max_queue", 10000)
	retries := ctx.ConfigInt("retries", 3)
	if batchSize < 1 || maxQueue < 1 || retries < 0 || flushEvery <= 0 {
		return fmt.Errorf("audit_http: batch_size, max_queue and flush_seconds must be positive, retries not negative")
	}
	if maxQueue < batchSize {
		return fmt.Errorf("audit_http: max_queue (%d) must be at least batch_size (%d)", maxQueue, batchSize)
	}
	client := &http.Client{Timeout: time.Duration(ctx.ConfigInt("timeout_seconds", 10)) * time.Second}

	var mu sync.Mutex
	var queue [][]byte
	dropped := 0
	post := func(body []byte) error {
		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		for k, v := range headers {
			s, _ := v.(string)
			val, err := ctx.Secret(s)
			if err != nil {
				return err
			}
			req.Header.Set(k, val)
		}
		if hmacRef != "" {
			key, err := ctx.Secret(hmacRef)
			if err != nil {
				return err
			}
			m := hmac.New(sha256.New, []byte(key))
			m.Write(body)
			req.Header.Set("X-EIP-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	}
	var sending sync.Mutex
	flush := func() {
		sending.Lock() // one batch in flight, in order
		defer sending.Unlock()
		for {
			// Take the batch out of the queue while it is in flight, so Send's
			// overflow trimming can only drop records that aren't being sent.
			mu.Lock()
			n := len(queue)
			if n > batchSize {
				n = batchSize
			}
			batch := append([][]byte(nil), queue[:n]...)
			queue = queue[n:]
			mu.Unlock()
			if n == 0 {
				return
			}
			var body []byte
			for _, line := range batch {
				body = append(body, line...)
			}
			var err error
			for try := 0; try <= retries; try++ {
				if err = post(body); err == nil {
					break
				}
				time.Sleep(time.Duration(200*(try+1)) * time.Millisecond)
			}
			if err != nil {
				// Put them back in front, in order; if the queue is now over
				// max_queue, the oldest go (and are counted as dropped).
				mu.Lock()
				queue = append(batch, queue...)
				if len(queue) > maxQueue {
					dropped += len(queue) - maxQueue
					queue = queue[len(queue)-maxQueue:]
				}
				mu.Unlock()
				ctx.Logf("audit_http: %d records not delivered yet: %v", n, err)
				return
			}
		}
	}
	ctx.Effect(func() func() {
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			t := time.NewTicker(flushEvery)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					flush()
				}
			}
		}()
		return func() {
			close(stop)
			<-done
			flush()
			mu.Lock()
			n := dropped
			mu.Unlock()
			if n > 0 {
				ctx.Logf("audit_http: dropped %d records (queue full)", n)
			}
		}
	})
	return ctx.Provide("audit/sink/http", &sdk.AuditSink{Send: func(line []byte) {
		cp := append([]byte(nil), line...)
		mu.Lock()
		queue = append(queue, cp)
		if len(queue) > maxQueue {
			dropped += len(queue) - maxQueue
			queue = queue[len(queue)-maxQueue:]
		}
		full := len(queue) >= batchSize
		mu.Unlock()
		if full {
			go flush()
		}
	}})
}

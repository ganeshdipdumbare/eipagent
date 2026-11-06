package audit_http

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Ships the audit stream to an HTTP endpoint (a SIEM, a log pipeline or a
// control plane), provided as "audit/sink/http". It receives every record
// audit_file writes (already redacted and hash-chained, so the receiver
// can verify the chain) and POSTs them in batches as NDJSON.
//
// Requests carry X-KILN-Signature: sha256=<hex HMAC of the body> when
// hmac_key is set (use a secret:// reference). Failed batches are retried
// with backoff; when the queue is full the oldest records are dropped and
// counted (a gap in seq tells the receiver).
//
// Config: url (required), headers ({name: value or secret://NAME}),
// hmac_key (secret://NAME), batch_size (100), flush_seconds (2),
// max_queue (10000), retries (3), timeout_seconds (10).
//
// Example:
//
//	audit_file.New(map[string]any{"path": "-"}) // no local file
//	audit_http.New(map[string]any{
//		"url":      "https://siem.example.com/ingest",
//		"hmac_key": "secret://SIEM_HMAC_KEY",
//		"headers":  map[string]any{"Authorization": "secret://SIEM_TOKEN"},
//	})
var Permissions = []string{"net"}

var Requires = []string{"audit", "?secrets"}

func Apply(p *sdk.Scope) error {
	endpoint := p.ConfigString("url", "")
	if endpoint == "" {
		return fmt.Errorf("audit_http: config url is required")
	}
	headers := p.ConfigMap("headers")
	hmacRef := p.ConfigString("hmac_key", "")
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("audit_http: url must be an absolute http(s) URL")
	}
	if u.User != nil {
		return fmt.Errorf("audit_http: url must not contain credentials")
	}
	// A custom header or an HMAC key is a credential: keep it on TLS.
	if (len(headers) > 0 || hmacRef != "") && u.Scheme != "https" && !localHost(u.Hostname()) {
		return fmt.Errorf("audit_http: url must be https when headers or hmac_key are configured")
	}
	batchSize := p.ConfigInt("batch_size", 100)
	flushEvery := time.Duration(p.ConfigInt("flush_seconds", 2)) * time.Second
	maxQueue := p.ConfigInt("max_queue", 10000)
	retries := p.ConfigInt("retries", 3)
	if batchSize < 1 || maxQueue < 1 || retries < 0 || flushEvery <= 0 {
		return fmt.Errorf("audit_http: batch_size, max_queue and flush_seconds must be positive, retries not negative")
	}
	if maxQueue < batchSize {
		return fmt.Errorf("audit_http: max_queue (%d) must be at least batch_size (%d)", maxQueue, batchSize)
	}
	hc, err := p.HTTP(sdk.HTTPOptions{
		AllowHosts:    []string{u.Hostname()},
		Timeout:       time.Duration(p.ConfigInt("timeout_seconds", 10)) * time.Second,
		AllowPrivate:  true, // operator-named endpoint (a SIEM or pipeline, possibly on a private network)
		AllowInsecure: len(headers) == 0 && hmacRef == "",
	})
	if err != nil {
		return err
	}

	var mu sync.Mutex
	var queue [][]byte
	dropped := 0
	post := func(body []byte) error {
		req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		for k, v := range headers {
			s := ""
			if x, ok := v.(string); ok {
				s = x
			}
			val, err := p.SecretValue(s)
			if err != nil {
				return err
			}
			req.Header.Set(k, val)
		}
		if hmacRef != "" {
			key, err := p.SecretValue(hmacRef)
			if err != nil {
				return err
			}
			m := hmac.New(sha256.New, []byte(key))
			m.Write(body)
			req.Header.Set("X-KILN-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return &httpStatusError{code: resp.StatusCode}
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
				if se, ok := err.(*httpStatusError); ok && !retryableStatus(se.code) {
					// A permanent rejection (auth error, wrong endpoint) must
					// not block every later record behind it: drop this batch
					// and keep the stream moving.
					p.Logf("audit_http: dropping %d records: %v", n, err)
					continue
				}
				// Put them back in front, in order; if the queue is now over
				// max_queue, the oldest go (and are counted as dropped).
				mu.Lock()
				queue = append(batch, queue...)
				if len(queue) > maxQueue {
					dropped += len(queue) - maxQueue
					queue = queue[len(queue)-maxQueue:]
				}
				mu.Unlock()
				p.Logf("audit_http: %d records not delivered yet: %s", n, safeURLError(err))
				return
			}
		}
	}
	wake := make(chan struct{}, 1)
	signal := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	p.Effect(func() func() {
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
				case <-wake:
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
				p.Logf("audit_http: dropped %d records (queue full)", n)
			}
		}
	})
	return p.Provide("audit/sink/http", &sdk.AuditSink{Send: func(line []byte) {
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
			signal()
		}
	}})
}

func localHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func safeURLError(err error) string {
	if ue, ok := err.(*url.Error); ok {
		host := ""
		if u, perr := url.Parse(ue.URL); perr == nil {
			host = u.Hostname()
		}
		return fmt.Sprintf("%s %s: %v", ue.Op, host, ue.Err)
	}
	return sdk.Redact(err.Error())
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

func retryableStatus(code int) bool { return code == 408 || code == 429 || code >= 500 }

// Module is the compiled form of "audit_http". modules/std registers it.
var Module = engine.Module{Name: "audit_http", Requires: Requires, Provides: []string{"audit/sink/http"}, Permissions: Permissions, Apply: Apply}

// New returns the module. Keys in cfg are the module's configuration; see
// the Config* reads in Apply.
func New(cfg ...map[string]any) engine.Module {
	m := Module
	if len(cfg) > 0 {
		m.Config = cfg[0]
	}
	return m
}

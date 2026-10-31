package app

import (
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The demo is public, so every request that starts an agent run goes
// through guard: a body size cap, a per-client rate limit and a cap on
// runs in flight. A real app adds authentication in front of this.

const maxBody = 64 << 10 // bytes per request body

var threadID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type limiter struct {
	mu      sync.Mutex
	perMin  int
	clients map[string][]time.Time // request times in the last minute
	slots   chan struct{}
	proxied bool // trust X-Forwarded-For (only behind a proxy you run)
}

func newLimiter(perMin, maxRuns int, proxied bool) *limiter {
	return &limiter{perMin: perMin, clients: map[string][]time.Time{}, slots: make(chan struct{}, maxRuns), proxied: proxied}
}

func (l *limiter) client(r *http.Request) string {
	if l.proxied {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			return strings.TrimSpace(strings.Split(f, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// allow records a request and reports whether the client is under its
// per-minute budget.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	recent := l.clients[key][:0]
	for _, t := range l.clients[key] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.perMin {
		l.clients[key] = recent
		return false
	}
	l.clients[key] = append(recent, now)
	if len(l.clients) > 10000 { // forget idle clients
		for k, ts := range l.clients {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= time.Minute {
				delete(l.clients, k)
			}
		}
	}
	return true
}

// guard wraps a handler that starts an agent run.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id := r.PathValue("id"); id != "" && !threadID.MatchString(id) {
			writeErr(w, 400, errors.New("bad thread id"))
			return
		}
		if !s.limit.allow(s.limit.client(r)) {
			w.Header().Set("Retry-After", "60")
			writeErr(w, 429, errors.New("too many requests, try again in a minute"))
			return
		}
		select {
		case s.limit.slots <- struct{}{}:
			defer func() { <-s.limit.slots }()
		default:
			w.Header().Set("Retry-After", "5")
			writeErr(w, 503, errors.New("the demo is busy, try again in a few seconds"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		h(w, r)
	}
}

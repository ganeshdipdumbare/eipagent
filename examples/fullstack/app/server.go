package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/audit"
	"github.com/ganeshdipdumbare/kiln/modules/audit_file"
	"github.com/ganeshdipdumbare/kiln/modules/hook_approval"
	"github.com/ganeshdipdumbare/kiln/modules/hook_limits"
	"github.com/ganeshdipdumbare/kiln/modules/hook_pii"
	"github.com/ganeshdipdumbare/kiln/modules/secrets_env"
	"github.com/ganeshdipdumbare/kiln/modules/threads_file"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

//go:embed ui
var uiFS embed.FS

// Config for the support desk.
type Config struct {
	Model       string  // "demo:support" (offline) or e.g. "ollama:qwen3.5:4b"
	DataDir     string  // threads and the audit log live here
	RefundLimit float64 // refunds above this need a supervisor
	RatePerMin  int     // agent runs per client per minute (default 20)
	MaxRuns     int     // agent runs in flight at once (default 8)
	TrustProxy  bool    // use X-Forwarded-For as the client address
}

type Server struct {
	agent      *kiln.Agent
	shop       *Shop
	cfg        Config
	mux        *http.ServeMux
	limit      *limiter
	mu         sync.Mutex
	owners     map[string]string // thread id -> session id
	ownersPath string
	auditMu    sync.Mutex
	auditAt    time.Time
	auditCount int
	auditOK    bool
}

// User identities. A real app takes these from its login session.
var users = map[string]struct{ Name, Role string }{
	"sam": {"Sam Rivera", "agent"},
	"ada": {"Ada Okafor", "supervisor"},
}

const instructions = `You are the support assistant for Acme, an online homeware shop.
Look up the order before acting. Refund only what the customer paid.
Ask for an order number when you need one. Be brief and warm.`

// New builds the agent and the HTTP API.
func New(cfg Config) (*Server, error) {
	if cfg.Model == "" {
		cfg.Model = "demo:support"
	}
	if cfg.RefundLimit == 0 {
		cfg.RefundLimit = 50
	}
	if cfg.RatePerMin <= 0 {
		cfg.RatePerMin = 20
	}
	if cfg.MaxRuns <= 0 {
		cfg.MaxRuns = 8
	}
	shop := NewShop()
	if os.Getenv("MAIL_RELAY_TOKEN") == "" {
		os.Setenv("MAIL_RELAY_TOKEN", "demo-relay-token-5f2c9a") // a stand-in; production reads it from Vault
	}
	// File-backed modules work inside directories they are given; the
	// application creates them before the agent starts.
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "threads"), 0o700); err != nil {
		return nil, err
	}
	cfg2 := kiln.Config{
		Model:        cfg.Model,
		Instructions: instructions,
		MaxSteps:     8,
		Tools:        Tools(shop),
		Modules: []kiln.Module{
			// Business rule: refunds above the limit wait for a supervisor.
			RefundPolicy(cfg.RefundLimit),
			// Email goes out through a module that alone may read the relay token.
			secrets_env.New(map[string]any{"allow": []any{"MAIL_RELAY_TOKEN"}, "grants": map[string]any{"mailer": []any{"MAIL_RELAY_TOKEN"}}}),
			Mailer(shop),
			// Every outgoing email needs a human OK.
			hook_approval.New(map[string]any{"interrupt_on": map[string]any{
				"send_email": map[string]any{"allowed": []any{"approve", "edit", "reject"}, "description": "Send this email to the customer?"},
			}}),
			// Card numbers never reach the model.
			hook_pii.New(map[string]any{"types": []any{"credit_card"}, "strategy": "mask"}),
			hook_limits.New(map[string]any{"tool_calls_run": 12}),
			// Durable threads (pauses survive restarts) and a tamper-evident log.
			threads_file.New(map[string]any{"dir": filepath.Join(cfg.DataDir, "threads")}),
			audit_file.New(map[string]any{"path": filepath.Join(cfg.DataDir, "audit.jsonl")}),
			Inspector(),
		},
		Logger: func(scope, msg string) {},
	}
	if strings.HasPrefix(cfg.Model, "demo:") {
		cfg2.Modules = append(cfg2.Modules, DemoModel())
	}
	agent, err := kiln.New(cfg2)
	if err != nil {
		return nil, err
	}
	s := &Server{agent: agent, shop: shop, cfg: cfg, mux: http.NewServeMux(), limit: newLimiter(cfg.RatePerMin, cfg.MaxRuns, cfg.TrustProxy), owners: map[string]string{}, ownersPath: filepath.Join(cfg.DataDir, "owners.json")}
	if b, err := os.ReadFile(s.ownersPath); err == nil {
		json.Unmarshal(b, &s.owners)
	}
	s.routes()
	return s, nil
}

func (s *Server) Close() error                                     { return s.agent.Close() }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	ui, _ := fs.Sub(uiFS, "ui")
	s.mux.Handle("GET /", http.FileServerFS(ui))
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/threads", s.guard(s.listThreads))
	s.mux.HandleFunc("POST /api/threads", s.guard(s.newThread))
	s.mux.HandleFunc("GET /api/threads/{id}", s.guard(s.getThread))
	s.mux.HandleFunc("POST /api/threads/{id}/messages", s.guard(s.postMessage))
	s.mux.HandleFunc("POST /api/threads/{id}/decisions", s.guard(s.postDecisions))
	s.mux.HandleFunc("GET /api/approvals", s.guard(s.approvals))
	s.mux.HandleFunc("GET /api/audit", s.guard(s.auditLog))
	s.mux.HandleFunc("GET /api/orders", s.guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"orders": s.shop.Orders(), "outbox": s.shop.Outbox()})
	}))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"mode": "live", "model": s.cfg.Model, "refund_limit": s.cfg.RefundLimit})
}

func (s *Server) newThread(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		writeErr(w, 500, err)
		return
	}
	id := "t-" + hex.EncodeToString(b)
	s.mu.Lock()
	s.owners[id] = s.session(w, r)
	s.saveOwnersLocked()
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"id": id})
}

// session is the browser's demo session. It is not a login: it only keeps
// one visitor's conversations away from another visitor's.
const sessionCookie = "kiln_sid"

func (s *Server) session(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && len(c.Value) == 32 {
		if _, err := hex.DecodeString(c.Value); err == nil {
			return c.Value
		}
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	id := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil,
	})
	return id
}

func (s *Server) owned(sid, id string) bool {
	if sid == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, ok := s.owners[id]
	return ok && owner == sid
}

// claim binds an unowned thread to this session; it refuses a thread that
// belongs to another session.
func (s *Server) claim(sid, id string) bool {
	if sid == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, ok := s.owners[id]
	if !ok {
		s.owners[id] = sid
		s.saveOwnersLocked()
		return true
	}
	return owner == sid
}

func (s *Server) saveOwnersLocked() {
	if s.ownersPath == "" || s.ownersPath == "owners.json" {
		return
	}
	b, err := json.Marshal(s.owners)
	if err == nil {
		_ = os.WriteFile(s.ownersPath, b, 0o600)
	}
}

// ---- reading threads ----

type threadView struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Waiting  bool            `json:"waiting"`
	Messages []messageView   `json:"messages,omitempty"`
	Pending  []sdk.Interrupt `json:"pending,omitempty"`
}

type messageView struct {
	Role    string         `json:"role"`
	Text    string         `json:"text,omitempty"`
	Tool    string         `json:"tool,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
	Result  string         `json:"result,omitempty"`
	IsError bool           `json:"is_error,omitempty"`
	CallID  string         `json:"call_id,omitempty"`
}

func (s *Server) view(ctx context.Context, id string, full bool) (threadView, error) {
	th := s.agent.Thread(id)
	msgs, err := th.Messages(ctx)
	if err != nil {
		return threadView{}, err
	}
	pending, _ := th.Pending(ctx)
	v := threadView{ID: id, Waiting: len(pending) > 0, Pending: pending}
	for _, m := range msgs {
		if m.Role == sdk.RoleUser && v.Title == "" {
			v.Title = m.Content
		}
		if !full {
			continue
		}
		switch {
		case m.Role == sdk.RoleTool:
			// attach results to the call they answer
			for i := len(v.Messages) - 1; i >= 0; i-- {
				if v.Messages[i].CallID == m.ToolCallID {
					v.Messages[i].Result, v.Messages[i].IsError = m.Content, m.IsError
					break
				}
			}
		case len(m.ToolCalls) > 0:
			for _, tc := range m.ToolCalls {
				v.Messages = append(v.Messages, messageView{Role: "tool", Tool: tc.Name, Args: tc.Args, CallID: tc.ID})
			}
		default:
			v.Messages = append(v.Messages, messageView{Role: m.Role, Text: m.Content})
		}
	}
	return v, nil
}

func (s *Server) listThreads(w http.ResponseWriter, r *http.Request) {
	sid := s.session(w, r)
	ids, err := s.agent.Threads(r.Context(), "t-")
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	out := []threadView{}
	for _, id := range ids {
		if !s.owned(sid, id) {
			continue
		}
		if v, err := s.view(r.Context(), id, false); err == nil {
			out = append(out, v)
		}
	}
	writeJSON(w, 200, map[string]any{"threads": out})
}

func (s *Server) getThread(w http.ResponseWriter, r *http.Request) {
	if !threadID.MatchString(r.PathValue("id")) {
		writeErr(w, 400, errors.New("bad thread id"))
		return
	}
	if !s.owned(s.session(w, r), r.PathValue("id")) {
		writeErr(w, 404, errors.New("thread not found"))
		return
	}
	v, err := s.view(r.Context(), r.PathValue("id"), true)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, v)
}

// approvals lists every paused run, across all conversations.
func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	sid := s.session(w, r)
	ids, _ := s.agent.Threads(r.Context(), "t-")
	type item struct {
		Thread string `json:"thread"`
		Title  string `json:"title"`
		sdk.Interrupt
	}
	out := []item{}
	for _, id := range ids {
		if !s.owned(sid, id) {
			continue
		}
		v, err := s.view(r.Context(), id, false)
		if err != nil {
			continue
		}
		for _, in := range v.Pending {
			out = append(out, item{Thread: id, Title: v.Title, Interrupt: in})
		}
	}
	writeJSON(w, 200, map[string]any{"approvals": out})
}

// ---- running the agent, streamed as server-sent events ----

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
		User string `json:"user"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" || len(body.Text) > 4000 {
		writeErr(w, 400, errors.New("send {\"text\": \"...\"} (up to 4000 characters)"))
		return
	}
	u, ok := users[body.User]
	if !ok {
		writeErr(w, 401, errors.New("unknown user"))
		return
	}
	if !s.claim(s.session(w, r), r.PathValue("id")) {
		writeErr(w, 403, errors.New("this conversation belongs to another session"))
		return
	}
	stream := s.agent.Thread(r.PathValue("id")).Stream(r.Context(), body.Text, kiln.Run{User: body.User, Roles: []string{u.Role}})
	sendSSE(w, stream)
}

func (s *Server) postDecisions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User      string         `json:"user"`
		Decisions []sdk.Decision `json:"decisions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Decisions) == 0 {
		writeErr(w, 400, errors.New("send {\"decisions\": [...]}"))
		return
	}
	// Demo only: the identity comes from the request so visitors can switch
	// between Sam and Ada. A real app takes it from its login session;
	// never trust a client-sent user for role checks or Decision.By.
	u, ok := users[body.User]
	if !ok || u.Role != "supervisor" {
		writeErr(w, 403, errors.New("only supervisors can decide on approvals"))
		return
	}
	if !s.owned(s.session(w, r), r.PathValue("id")) {
		writeErr(w, 403, errors.New("this conversation belongs to another session"))
		return
	}
	for i := range body.Decisions {
		body.Decisions[i].By = body.User // recorded in the audit log
	}
	stream := s.agent.Thread(r.PathValue("id")).ResumeStream(r.Context(), body.Decisions, kiln.Run{User: body.User, Roles: []string{u.Role}})
	sendSSE(w, stream)
}

// sendSSE forwards every agent event to the browser, then the result.
func sendSSE(w http.ResponseWriter, stream *kiln.Stream) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	for e := range stream.Events() {
		send(e)
	}
	res, err := stream.Result()
	switch {
	case errors.Is(err, kiln.ErrThreadBusy):
		send(map[string]any{"Type": "error", "Text": "Someone else updated this conversation. Reload and try again."})
	case err != nil:
		send(map[string]any{"Type": "error", "Text": err.Error()})
	default:
		send(map[string]any{"Type": "done", "Data": map[string]any{
			"text": res.Text, "finish": res.FinishReason, "interrupts": res.Interrupts, "usage": res.Usage, "steps": len(res.Steps),
		}})
	}
}

// ---- audit feed ----

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.cfg.DataDir, "audit.jsonl")
	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, 200, map[string]any{"entries": []any{}, "verified": true})
		return
	}
	defer f.Close()
	// Read only the tail: the response returns the last 60 records, and
	// scanning a multi-MB log twice per request is a cheap DoS.
	if st, serr := f.Stat(); serr == nil && st.Size() > 1<<20 {
		f.Seek(st.Size()-1<<20, 0)
	}
	var entries []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	first := true
	for sc.Scan() {
		if first && len(entries) == 0 {
			// If we started in the middle of a line, drop that fragment.
			if b := sc.Bytes(); len(b) > 0 && b[0] != '{' {
				first = false
				continue
			}
		}
		first = false
		var line struct {
			Body map[string]any `json:"body"`
			Hash string         `json:"hash"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.Body == nil {
			continue // a damaged line; Verify reports the broken chain
		}
		h := line.Hash
		if len(h) > 10 {
			h = h[:10]
		}
		line.Body["hash"] = h
		entries = append(entries, line.Body)
		if len(entries) > 120 {
			entries = entries[len(entries)-120:]
		}
	}
	// Verification is O(file); cache it briefly so /api/audit cannot be
	// used to re-hash the whole log on every request.
	s.auditMu.Lock()
	if time.Since(s.auditAt) > 10*time.Second {
		s.auditCount, err = audit.Verify(path)
		s.auditOK, s.auditAt = err == nil, time.Now()
	}
	n, ok := s.auditCount, s.auditOK
	s.auditMu.Unlock()
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 { // newest first
		entries[i], entries[j] = entries[j], entries[i]
	}
	if len(entries) > 60 {
		entries = entries[:60]
	}
	writeJSON(w, 200, map[string]any{"entries": entries, "verified": ok, "count": n})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

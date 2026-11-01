package app

import (
	"bufio"
	"context"
	"crypto/ed25519"
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

	"github.com/ganeshdipdumbare/eipagent"
	"github.com/ganeshdipdumbare/eipagent/audit"
	"github.com/ganeshdipdumbare/eipagent/pluginpkg"
	"github.com/ganeshdipdumbare/eipagent/sdk"
)

//go:embed ui
var uiFS embed.FS

// Config for the support desk.
type Config struct {
	Model       string  // "demo:support" (offline) or e.g. "ollama:qwen3.5:4b"
	DataDir     string  // threads and the audit log live here
	RefundLimit float64 // refunds above this need a supervisor
	RatePerMin  int     // agent runs per client per minute (default 20)
	// AllowPluginCode lets the Plugins tab install any code (without
	// permissions). Off: only the bundled sample plugins. Never turn it on
	// for a public deployment: the code runs in this server.
	AllowPluginCode bool
	MaxRuns         int  // agent runs in flight at once (default 8)
	TrustProxy      bool // use X-Forwarded-For as the client address
}

type Server struct {
	agent *eipagent.Agent
	shop  *Shop
	cfg   Config
	mux   *http.ServeMux
	limit *limiter

	plugins *pluginpkg.Store   // plugins added from the UI
	signer  ed25519.PrivateKey // signs them
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
	pstore, signer, err := openPluginStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	opts := []eipagent.Option{
		eipagent.WithModel(cfg.Model),
		eipagent.WithInstructions(instructions),
		eipagent.WithMaxSteps(8),
		eipagent.WithTools(Tools(shop)...),
		// Business rule: refunds above the limit wait for a supervisor.
		eipagent.WithCompiledPlugin(RefundPolicy(cfg.RefundLimit)),
		// Email goes out through a plugin that alone may read the relay token.
		eipagent.WithPlugin("secrets_env", map[string]any{"allow": []any{"MAIL_RELAY_TOKEN"}, "grants": map[string]any{"mailer": []any{"MAIL_RELAY_TOKEN"}}}),
		eipagent.WithCompiledPlugin(Mailer(shop)),
		// Every outgoing email needs a human OK.
		eipagent.WithPlugin("hook_approval", map[string]any{"interrupt_on": map[string]any{
			"send_email": map[string]any{"allowed": []any{"approve", "edit", "reject"}, "description": "Send this email to the customer?"},
		}}),
		// Card numbers never reach the model.
		eipagent.WithPlugin("hook_pii", map[string]any{"types": []any{"credit_card"}, "strategy": "mask"}),
		eipagent.WithPlugin("hook_limits", map[string]any{"tool_calls_run": 12}),
		// Durable threads (pauses survive restarts) and a tamper-evident log.
		eipagent.WithPlugin("threads_file", map[string]any{"dir": filepath.Join(cfg.DataDir, "threads")}),
		eipagent.WithPlugin("audit_file", map[string]any{"path": filepath.Join(cfg.DataDir, "audit.jsonl")}),
		eipagent.WithCompiledPlugin(Inspector()),
		// Plugins added in the Plugins tab, live in this agent.
		eipagent.WithPluginStore(pstore),
		eipagent.WithLogger(func(scope, msg string) {}),
	}
	if strings.HasPrefix(cfg.Model, "demo:") {
		opts = append(opts, eipagent.WithCompiledPlugin(DemoModel()))
	}
	agent, err := eipagent.New(opts...)
	if err != nil {
		return nil, err
	}
	s := &Server{agent: agent, shop: shop, cfg: cfg, mux: http.NewServeMux(), limit: newLimiter(cfg.RatePerMin, cfg.MaxRuns, cfg.TrustProxy),
		plugins: pstore, signer: signer}
	s.routes()
	return s, nil
}

func (s *Server) Close() error                                     { return s.agent.Close() }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	ui, _ := fs.Sub(uiFS, "ui")
	s.mux.Handle("GET /", http.FileServerFS(ui))
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/threads", s.listThreads)
	s.mux.HandleFunc("POST /api/threads", s.newThread)
	s.mux.HandleFunc("GET /api/threads/{id}", s.getThread)
	s.mux.HandleFunc("POST /api/threads/{id}/messages", s.guard(s.postMessage))
	s.mux.HandleFunc("POST /api/threads/{id}/decisions", s.guard(s.postDecisions))
	s.mux.HandleFunc("GET /api/approvals", s.approvals)
	s.mux.HandleFunc("GET /api/plugins", s.listPlugins)
	s.mux.HandleFunc("POST /api/plugins", s.guard(s.submitPlugin))
	s.mux.HandleFunc("POST /api/plugins/{name}", s.guard(s.pluginAction))
	s.mux.HandleFunc("GET /api/audit", s.auditLog)
	s.mux.HandleFunc("GET /api/orders", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"orders": s.shop.Orders(), "outbox": s.shop.Outbox()})
	})
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
	writeJSON(w, 200, map[string]any{"id": "t-" + hex.EncodeToString(b)})
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
	ids, err := s.agent.Threads(r.Context(), "t-")
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	out := []threadView{}
	for _, id := range ids {
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
	v, err := s.view(r.Context(), r.PathValue("id"), true)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, v)
}

// approvals lists every paused run, across all conversations.
func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	ids, _ := s.agent.Threads(r.Context(), "t-")
	type item struct {
		Thread string `json:"thread"`
		Title  string `json:"title"`
		sdk.Interrupt
	}
	out := []item{}
	for _, id := range ids {
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
	stream := s.agent.Thread(r.PathValue("id")).Stream(r.Context(), body.Text, eipagent.AsUser(body.User, u.Role))
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
	for i := range body.Decisions {
		body.Decisions[i].By = body.User // recorded in the audit log
	}
	stream := s.agent.Thread(r.PathValue("id")).ResumeStream(r.Context(), body.Decisions, eipagent.AsUser(body.User, u.Role))
	sendSSE(w, stream)
}

// sendSSE forwards every agent event to the browser, then the result.
func sendSSE(w http.ResponseWriter, stream *eipagent.Stream) {
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
	case errors.Is(err, eipagent.ErrThreadBusy):
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
	var entries []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var line struct {
			Body map[string]any `json:"body"`
			Hash string         `json:"hash"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.Body == nil {
			continue // a damaged line; Verify below reports the broken chain
		}
		h := line.Hash
		if len(h) > 10 {
			h = h[:10]
		}
		line.Body["hash"] = h
		entries = append(entries, line.Body)
	}
	n, verr := audit.Verify(path)
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 { // newest first
		entries[i], entries[j] = entries[j], entries[i]
	}
	if len(entries) > 60 {
		entries = entries[:60]
	}
	writeJSON(w, 200, map[string]any{"entries": entries, "verified": verr == nil, "count": n})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

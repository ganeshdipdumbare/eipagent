// Package sdk is the module-facing API of the kiln core engine.
//
// Everything here is plain structs and func types (no interfaces to
// implement, no generics) so interpreted Yaegi modules can provide and
// consume every type.
package sdk

import (
	"context"
	"errors"
	"fmt"
)

// ---- Conversation ----

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

type Message struct {
	Role       string
	Content    string
	Images     []Image
	Reasoning  string
	ToolCalls  []ToolCall
	ToolCallID string // tool messages: which call this answers
	ToolName   string
	IsError    bool
}

type Image struct {
	MediaType string // image/png ...
	Data      []byte // raw bytes, or
	URL       string // a URL the provider can fetch
}

type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
}

// Usage counts tokens. InputTokens is every input token, cached or not,
// so it is comparable across providers; CacheReadTokens and
// CacheWriteTokens say how many of them were read from or written to the
// provider's prompt cache.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	TotalTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
}

func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + o.InputTokens,
		OutputTokens:     u.OutputTokens + o.OutputTokens,
		TotalTokens:      u.TotalTokens + o.TotalTokens,
		CacheReadTokens:  u.CacheReadTokens + o.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
	}
}

// ---- Tools ----

type Tool struct {
	Name        string
	Description string
	Schema      map[string]any // JSON Schema of the arguments
	Run         func(ctx context.Context, args map[string]any) (string, error)
	// Meta carries hints for middleware/policy: "readonly", "risk", "sandbox"...
	Meta map[string]any
	// Module is the module that registered the tool (set by AddTool).
	Module string
}

type ToolRequest struct {
	Call   ToolCall
	Tool   *Tool // nil when the model called an unknown tool
	Thread string
	User   string
	Step   int
	// Decision is set when a human (or reviewer) already decided on this
	// call during resume; HITL middleware lets approved calls through.
	Decision *Decision
	Run      *RunState
}

type ToolResult struct {
	Content   string
	IsError   bool
	Interrupt *Interrupt // set to pause the run (human-in-the-loop)
}

type ToolHandler func(ctx context.Context, req *ToolRequest) (*ToolResult, error)

// ---- Models ----

type ModelRequest struct {
	Model          string // full "provider:model" string
	Provider       string
	ModelID        string
	System         string
	Messages       []Message
	Tools          []Tool
	ToolChoice     string // "", auto, none, required, or a tool name
	ResponseSchema map[string]any
	ResponseName   string
	Temperature    *float64
	MaxTokens      int
	Extra          map[string]any // provider specific
	// Stream, when set, asks the provider to emit chunks as they arrive.
	// The provider still returns the complete response at the end.
	Stream func(Chunk)
	Thread string
	User   string
	Step   int
	Run    *RunState // the run this call belongs to (nil outside a loop)
}

type Chunk struct {
	Type     string // text | reasoning | tool_call
	Text     string
	ToolCall *ToolCall
}

type ModelResponse struct {
	Message      Message
	Usage        Usage
	FinishReason string // stop | tool_calls | length | refusal | error
}

type ModelHandler func(ctx context.Context, req *ModelRequest) (*ModelResponse, error)

// Provider is a model adapter, provided as the service "model/<name>".
type Provider struct {
	Name     string
	Generate ModelHandler
	// Capabilities: "streaming", "structured_output", "vision", "tools".
	Capabilities map[string]bool
}

// PolicyCheck is provided by the policy module as the "policy" service so
// middleware that makes an extra model call (for example a fallback) can
// apply the admin model allowlist before choosing another model.
type PolicyCheck struct {
	Model func(ctx context.Context, model string) error
}

// ---- Interrupts (human in the loop) ----

type Interrupt struct {
	ID       string
	Kind     string // approval | question
	ToolCall *ToolCall
	Message  string
	Allowed  []string // allowed decisions: approve, edit, reject, respond
	Payload  map[string]any
}

type Decision struct {
	InterruptID string
	Type        string         // approve | edit | reject | respond
	Args        map[string]any // edit: replacement arguments
	Message     string         // reject / respond: text for the model
	By          string         // who decided (audit)
}

// ---- Run state, shared by the facade and the loop module ----

type Event struct {
	Type     string // text | reasoning | tool_call | tool_result | step_start | step_end | interrupt | finish | error
	Step     int
	Text     string
	ToolCall *ToolCall
	Result   string
	Usage    Usage
	Data     map[string]any
}

type RunState struct {
	ID             string // unique per run; set by the loop when empty
	Thread         string
	User           string
	Roles          []string
	Model          string
	System         string
	Messages       []Message // history; the loop appends to it
	MaxSteps       int
	StopOnTools    []string // stop after any of these tools is called
	ToolChoice     string
	ResponseSchema map[string]any
	ResponseName   string
	Temperature    *float64
	MaxTokens      int
	Pending        []Interrupt // interrupts awaiting decisions
	Decisions      []Decision  // supplied on resume
	Values         map[string]any
	Parent         *RunState    // set for subagent runs: budgets and limits follow the parent
	Checkpoint     func() error // optional durable checkpoint; called before tools
	Emit           func(Event)  // never nil inside a loop
	Streaming      bool         // caller wants text deltas (ask providers to stream)
	Tools          []Tool       // extra per-run tools (not registered globally)
}

type Step struct {
	Response    Message
	ToolResults []Message
	Usage       Usage
	Finish      string
}

type RunResult struct {
	Text         string
	Steps        []Step
	Usage        Usage
	Interrupts   []Interrupt
	FinishReason string
	Messages     []Message // full updated history
}

// Loop is the agent loop, provided as the service "loop".
type Loop struct {
	Name string
	Run  func(ctx context.Context, snap *Snapshot, st *RunState) (*RunResult, error)
}

// ---- Persistence services ----

var (
	ErrVersionConflict = errors.New("kiln: thread version conflict")
	ErrNotFound        = errors.New("kiln: not found")
)

type ThreadState struct {
	ID       string
	Version  int64
	Messages []Message
	Pending  []Interrupt
	Values   map[string]any
	Updated  int64 // unix millis
}

// Checkpointer persists thread state, provided as "threads".
// Save must fail with ErrVersionConflict unless the stored version equals
// expected (0 = must not exist); on success st.Version is expected+1.
type Checkpointer struct {
	Load   func(ctx context.Context, id string) (*ThreadState, error) // nil, nil when absent
	Save   func(ctx context.Context, st *ThreadState, expected int64) error
	Delete func(ctx context.Context, id string) error
	List   func(ctx context.Context, prefix string) ([]string, error)
}

type StoreItem struct {
	Namespace []string
	Key       string
	Value     map[string]any
	Updated   int64
}

// Store is long-term key/value memory, provided as "memory".
type Store struct {
	Put    func(ctx context.Context, ns []string, key string, val map[string]any) error
	Get    func(ctx context.Context, ns []string, key string) (*StoreItem, error) // nil, nil when absent
	Search func(ctx context.Context, ns []string, query string, limit int) ([]StoreItem, error)
	Delete func(ctx context.Context, ns []string, key string) error
}

// Skill is a system-prompt fragment.
type Skill struct {
	Name        string
	Description string
	Prompt      string
}

// Secrets resolves secret references, provided as "secrets". It is the
// secret provider interface: env, Vault, KMS or a custom store each
// provide one. caller is the requesting module: modules that look the
// service up get a copy bound to their own identity, so the caller they
// pass is ignored, every resolved value is redacted from audit / trace
// output, and each access is recorded as a "secret.access" audit event.
// Resolve secrets when you use them (not once in Apply) to pick up
// rotation; providers cache and refresh.
type Secrets struct {
	Get func(ctx context.Context, caller, name string) (string, error)
}

// ProviderError is returned by LLM adapters for HTTP failures so retry
// and fallback middleware can tell transient errors apart.
type ProviderError struct {
	Provider  string
	Status    int
	Body      string
	Retryable bool
}

func (e *ProviderError) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300] + "..."
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, b)
}

func NewProviderError(provider string, status int, body string) error {
	return &ProviderError{Provider: provider, Status: status, Body: body, Retryable: status == 429 || status >= 500}
}

// IsRetryable reports whether err is a transient provider error.
func IsRetryable(err error) bool {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return false
}

// FatalError aborts the whole run when returned from a tool handler
// (ordinary tool errors are sent back to the model instead).
type FatalError struct{ Err error }

func (e *FatalError) Error() string { return e.Err.Error() }
func (e *FatalError) Unwrap() error { return e.Err }

func Fatal(err error) error { return &FatalError{Err: err} }

// Approver decides interrupts synchronously, provided as "approver"
// (e.g. a CLI prompt). Returning nil leaves the interrupt pending.
type Approver struct {
	Decide func(ctx context.Context, in Interrupt) (*Decision, error)
}

// Audit records security-relevant events, provided as "audit". The core
// engine also reports module lifecycle (start, fail, close) to it.
type Audit struct {
	Record func(kind string, data map[string]any)
}

// AuditSink receives every audit record as one JSON line (already
// redacted and hash-chained), provided as "audit/sink/<name>". Sinks
// export to a SIEM or control plane (audit_http).
type AuditSink struct {
	Send func(line []byte)
}

// AgentService lets modules start runs (jobs, webhooks, teams), provided
// as "agent" by the facade. Thread runs are checkpointed like any other.
type AgentService struct {
	Run func(ctx context.Context, thread, user, prompt string) (*RunResult, error)
}

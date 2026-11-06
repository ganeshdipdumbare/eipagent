package kiln

import (
	"github.com/ganeshdipdumbare/kiln/engine"
	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Config is everything an agent is made of. It is the only input New takes:
// there are no options to set anywhere else, and field order never matters.
//
//	a, err := kiln.New(kiln.Config{
//		Model:   "openai:gpt-4.1",
//		Modules: []kiln.Module{tool_time.New()},
//	})
type Config struct {
	// Model is "provider:model", e.g. "openai:gpt-4.1". The provider module
	// is added for you; list it in Modules to configure it.
	Model string
	// Instructions is the system prompt.
	Instructions string
	// MaxSteps caps model calls per run (default 20).
	MaxSteps int
	// User and Roles are the default identity for runs (audit, policy, memory).
	User  string
	Roles []string
	// Tools are Go-defined tools available to every run.
	Tools []sdk.Tool
	// Modules is the composition. The default loop, secrets and thread
	// storage are added unless a module of the same family is listed here.
	Modules []Module
	// Policy is the admin policy. nil means SecurePolicy: every module gets
	// exactly the capabilities it declares.
	Policy *engine.Policy
	// Logger receives module log lines.
	Logger func(scope, msg string)
	// SaveEachStep saves thread history before every model call.
	SaveEachStep bool
	// RunIn runs the agent inside a sandbox instead of this process.
	RunIn *RunIn
}

// Run overrides Config defaults for one run. The zero value runs with the
// Config defaults.
type Run struct {
	Model        string
	Instructions string
	MaxSteps     int
	MaxTokens    int
	Temperature  *float64
	ToolChoice   string
	StopOnTools  []string
	History      []sdk.Message
	Images       []sdk.Image
	Tools        []sdk.Tool
	User         string
	Roles        []string
	OnEvent      func(Event)
	// Schema asks for structured output (GenerateObject sets it).
	Schema     map[string]any
	SchemaName string
	// CaptureResult stores the run Result.
	CaptureResult **Result
	// RunID is the host-assigned run id.
	RunID string

	onStep     func(*sdk.RunState) // threads: save before each model call
	checkpoint func() error        // threads: save before tools too
}

// one returns the single optional Run, or the zero value.
func one(run []Run) Run {
	if len(run) > 0 {
		return run[0]
	}
	return Run{}
}

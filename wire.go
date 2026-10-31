package eipagent

import "eipagent/sdk"

// JSON shapes exchanged between a host and an agent inside a sandbox.

type wireTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema,omitempty"`
	Meta        map[string]any `json:"meta,omitempty"`
}

func toWireTools(ts []sdk.Tool) []wireTool {
	out := make([]wireTool, 0, len(ts))
	for _, t := range ts {
		out = append(out, wireTool{t.Name, t.Description, t.Schema, t.Meta})
	}
	return out
}

type wireRun struct {
	RunID          string          `json:"run_id"`
	Thread         string          `json:"thread,omitempty"`
	Prompt         string          `json:"prompt,omitempty"` // thread runs
	User           string          `json:"user,omitempty"`
	Roles          []string        `json:"roles,omitempty"`
	Model          string          `json:"model,omitempty"`
	System         string          `json:"system,omitempty"`
	Messages       []sdk.Message   `json:"messages,omitempty"`
	MaxSteps       int             `json:"max_steps,omitempty"`
	StopOnTools    []string        `json:"stop_on_tools,omitempty"`
	ToolChoice     string          `json:"tool_choice,omitempty"`
	ResponseSchema map[string]any  `json:"response_schema,omitempty"`
	ResponseName   string          `json:"response_name,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Pending        []sdk.Interrupt `json:"pending,omitempty"`
	Decisions      []sdk.Decision  `json:"decisions,omitempty"`
	Values         map[string]any  `json:"values,omitempty"`
	Streaming      bool            `json:"streaming,omitempty"`
}

type wireModelReq struct {
	CallID         string         `json:"call_id"`
	Model          string         `json:"model"`
	System         string         `json:"system,omitempty"`
	Messages       []sdk.Message  `json:"messages"`
	Tools          []wireTool     `json:"tools,omitempty"`
	ToolChoice     string         `json:"tool_choice,omitempty"`
	ResponseSchema map[string]any `json:"response_schema,omitempty"`
	ResponseName   string         `json:"response_name,omitempty"`
	Temperature    *float64       `json:"temperature,omitempty"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	Extra          map[string]any `json:"extra,omitempty"`
	Thread         string         `json:"thread,omitempty"`
	User           string         `json:"user,omitempty"`
	Step           int            `json:"step"`
	Stream         bool           `json:"stream,omitempty"`
}

type wireEvent struct {
	RunID string    `json:"run_id"`
	Event sdk.Event `json:"event"`
}

type wireChunk struct {
	CallID string    `json:"call_id"`
	Chunk  sdk.Chunk `json:"chunk"`
}

type wireToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
	Info sdk.RunInfo    `json:"info"`
}

type wireHello struct {
	Providers []string   `json:"providers"`
	Tools     []wireTool `json:"tools"`
}

type wireThreadState struct {
	Messages []sdk.Message   `json:"messages"`
	Pending  []sdk.Interrupt `json:"pending"`
}

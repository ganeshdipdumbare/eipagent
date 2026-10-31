//go:build ignore

package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"eipagent/sdk"
)

// Anthropic Messages API adapter, provided as "llm/<name>".
//
// Config:
//
//	name:       default "anthropic"
//	base_url:   default https://api.anthropic.com
//	api_key:    literal or secret://NAME (default secret://ANTHROPIC_API_KEY)
//	max_tokens: default 8192 (the API requires one)
//	version:    anthropic-version header (default 2023-06-01)
//
// Structured output uses a tool named after the schema; when the model
// calls it, its input becomes the reply text.
var Permissions = []string{"net"}

func Apply(ctx *sdk.Context) error {
	name := ctx.ConfigString("name", "anthropic")
	base := strings.TrimRight(ctx.ConfigString("base_url", "https://api.anthropic.com"), "/")
	keyRef := ctx.ConfigString("api_key", "secret://ANTHROPIC_API_KEY")
	maxTokens := ctx.ConfigInt("max_tokens", 8192)
	version := ctx.ConfigString("version", "2023-06-01")
	client := &http.Client{Timeout: time.Duration(ctx.ConfigInt("timeout_seconds", 300)) * time.Second}

	return ctx.Provide("llm/"+name, &sdk.Provider{
		Name:         name,
		Capabilities: map[string]bool{"streaming": true, "tools": true, "structured_output": true, "vision": true},
		Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			key, err := ctx.Secret(keyRef)
			if err != nil {
				return nil, err
			}
			body, schemaTool := buildBody(req, maxTokens)
			payload, _ := json.Marshal(body)
			hr, err := http.NewRequestWithContext(c, "POST", base+"/v1/messages", bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			hr.Header.Set("Content-Type", "application/json")
			hr.Header.Set("x-api-key", key)
			hr.Header.Set("anthropic-version", version)
			resp, err := client.Do(hr)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
				return nil, sdk.NewProviderError(name, resp.StatusCode, string(raw))
			}
			var out *sdk.ModelResponse
			if req.Stream != nil {
				out, err = readStream(resp.Body, req.Stream)
			} else {
				var raw []byte
				raw, err = io.ReadAll(resp.Body)
				if err == nil {
					out, err = parseResponse(raw)
				}
			}
			if err != nil {
				return nil, err
			}
			if schemaTool != "" {
				unwrapSchemaTool(out, schemaTool)
			}
			return out, nil
		},
	})
}

func buildBody(req *sdk.ModelRequest, maxTokens int) (map[string]any, string) {
	var msgs []any
	var pendingResults []any
	flush := func() {
		if len(pendingResults) > 0 {
			msgs = append(msgs, map[string]any{"role": "user", "content": pendingResults})
			pendingResults = nil
		}
	}
	for _, m := range req.Messages {
		switch m.Role {
		case sdk.RoleTool:
			pendingResults = append(pendingResults, map[string]any{
				"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content, "is_error": m.IsError,
			})
			continue
		case sdk.RoleSystem:
			continue
		}
		flush()
		var blocks []any
		for _, img := range m.Images {
			src := map[string]any{"type": "url", "url": img.URL}
			if img.URL == "" {
				src = map[string]any{"type": "base64", "media_type": img.MediaType, "data": base64.StdEncoding.EncodeToString(img.Data)}
			}
			blocks = append(blocks, map[string]any{"type": "image", "source": src})
		}
		if m.Content != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
		}
		for _, tc := range m.ToolCalls {
			args := tc.Args
			if args == nil {
				args = map[string]any{}
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": args})
		}
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": "(empty)"})
		}
		msgs = append(msgs, map[string]any{"role": m.Role, "content": blocks})
	}
	flush()

	body := map[string]any{"model": req.ModelID, "messages": msgs, "max_tokens": maxTokens}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if strings.TrimSpace(req.System) != "" {
		body["system"] = req.System
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	var tools []any
	if req.ToolChoice != "none" {
		for _, t := range req.Tools {
			schema := t.Schema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": schema})
		}
	}
	schemaTool := ""
	if req.ResponseSchema != nil {
		schemaTool = req.ResponseName
		if schemaTool == "" {
			schemaTool = "response"
		}
		schemaTool = "final_" + schemaTool
		tools = append(tools, map[string]any{"name": schemaTool, "description": "Return your final answer with this tool.", "input_schema": req.ResponseSchema})
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	switch {
	case req.ToolChoice == "none":
	case schemaTool != "" && len(req.Tools) == 0:
		body["tool_choice"] = map[string]any{"type": "tool", "name": schemaTool}
	case req.ToolChoice == "required":
		body["tool_choice"] = map[string]any{"type": "any"}
	case req.ToolChoice != "" && req.ToolChoice != "auto":
		body["tool_choice"] = map[string]any{"type": "tool", "name": req.ToolChoice}
	}
	for k, v := range req.Extra {
		body[k] = v
	}
	return body, schemaTool
}

// unwrapSchemaTool turns a call to the structured-output tool into text.
func unwrapSchemaTool(r *sdk.ModelResponse, tool string) {
	var keep []sdk.ToolCall
	for _, tc := range r.Message.ToolCalls {
		if tc.Name == tool {
			r.Message.Content = sdk.ToJSON(tc.Args)
			continue
		}
		keep = append(keep, tc)
	}
	r.Message.ToolCalls = keep
	if len(keep) == 0 {
		r.FinishReason = "stop"
	}
}

type block struct {
	Type     string         `json:"type"`
	Text     string         `json:"text"`
	Thinking string         `json:"thinking"`
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Input    map[string]any `json:"input"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func finishOf(r string) string {
	switch r {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	}
	return "stop"
}

func parseResponse(raw []byte) (*sdk.ModelResponse, error) {
	var out struct {
		Content    []block `json:"content"`
		StopReason string  `json:"stop_reason"`
		Usage      usage   `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	msg := sdk.Message{Role: sdk.RoleAssistant}
	var text []string
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "thinking":
			msg.Reasoning += b.Thinking
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, sdk.ToolCall{ID: b.ID, Name: b.Name, Args: b.Input})
		}
	}
	msg.Content = strings.Join(text, "")
	u := sdk.Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens, TotalTokens: out.Usage.InputTokens + out.Usage.OutputTokens}
	return &sdk.ModelResponse{Message: msg, Usage: u, FinishReason: finishOf(out.StopReason)}, nil
}

type streamBlock struct {
	typ, id, name string
	text, json    strings.Builder
}

func readStream(r io.Reader, emit func(sdk.Chunk)) (*sdk.ModelResponse, error) {
	blocks := map[int]*streamBlock{}
	var u sdk.Usage
	stop := ""
	var reasoning strings.Builder
	err := sdk.ReadSSE(r, func(event, data string) error {
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock block  `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				Usage usage `json:"usage"`
			} `json:"message"`
			Usage usage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return err
		}
		switch ev.Type {
		case "error":
			if ev.Error != nil {
				return sdk.NewProviderError("anthropic", 529, ev.Error.Message)
			}
		case "message_start":
			u.InputTokens = ev.Message.Usage.InputTokens
		case "content_block_start":
			blocks[ev.Index] = &streamBlock{typ: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				b = &streamBlock{typ: "text"}
				blocks[ev.Index] = b
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
				emit(sdk.Chunk{Type: "text", Text: ev.Delta.Text})
			case "thinking_delta":
				reasoning.WriteString(ev.Delta.Thinking)
				emit(sdk.Chunk{Type: "reasoning", Text: ev.Delta.Thinking})
			case "input_json_delta":
				b.json.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
			u.OutputTokens = ev.Usage.OutputTokens
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var idx []int
	for i := range blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	msg := sdk.Message{Role: sdk.RoleAssistant, Reasoning: reasoning.String()}
	for _, i := range idx {
		b := blocks[i]
		switch b.typ {
		case "text":
			msg.Content += b.text.String()
		case "tool_use":
			args := map[string]any{}
			if s := strings.TrimSpace(b.json.String()); s != "" {
				json.Unmarshal([]byte(s), &args)
			}
			tc := sdk.ToolCall{ID: b.id, Name: b.name, Args: args}
			msg.ToolCalls = append(msg.ToolCalls, tc)
			emit(sdk.Chunk{Type: "tool_call", ToolCall: &tc})
		}
	}
	u.TotalTokens = u.InputTokens + u.OutputTokens
	return &sdk.ModelResponse{Message: msg, Usage: u, FinishReason: finishOf(stop)}, nil
}

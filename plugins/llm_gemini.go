//go:build ignore

package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"eipagent/sdk"
)

// Google Gemini adapter (generateContent / streamGenerateContent),
// provided as "llm/<name>".
//
// Config:
//
//	name:     default "gemini"
//	base_url: default https://generativelanguage.googleapis.com/v1beta
//	api_key:  literal or secret://NAME (default secret://GEMINI_API_KEY)
var Permissions = []string{"net"}

func Apply(ctx *sdk.Context) error {
	name := ctx.ConfigString("name", "gemini")
	base := strings.TrimRight(ctx.ConfigString("base_url", "https://generativelanguage.googleapis.com/v1beta"), "/")
	keyRef := ctx.ConfigString("api_key", "secret://GEMINI_API_KEY")
	client := &http.Client{Timeout: time.Duration(ctx.ConfigInt("timeout_seconds", 300)) * time.Second}

	return ctx.Provide("llm/"+name, &sdk.Provider{
		Name:         name,
		Capabilities: map[string]bool{"streaming": true, "tools": true, "structured_output": true, "vision": true},
		Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			key, err := ctx.Secret(keyRef)
			if err != nil {
				return nil, err
			}
			payload, _ := json.Marshal(buildBody(req))
			url := base + "/models/" + req.ModelID + ":generateContent"
			if req.Stream != nil {
				url = base + "/models/" + req.ModelID + ":streamGenerateContent?alt=sse"
			}
			hr, err := http.NewRequestWithContext(c, "POST", url, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			hr.Header.Set("Content-Type", "application/json")
			hr.Header.Set("x-goog-api-key", key)
			resp, err := client.Do(hr)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
				return nil, sdk.NewProviderError(name, resp.StatusCode, string(raw))
			}
			acc := &accum{}
			if req.Stream != nil {
				err = sdk.ReadSSE(resp.Body, func(event, data string) error {
					var chunk gResp
					if err := json.Unmarshal([]byte(data), &chunk); err != nil {
						return err
					}
					acc.add(chunk, req.Stream)
					return nil
				})
			} else {
				var chunk gResp
				raw, rerr := io.ReadAll(resp.Body)
				if rerr != nil {
					return nil, rerr
				}
				if err = json.Unmarshal(raw, &chunk); err == nil {
					acc.add(chunk, nil)
				}
			}
			if err != nil {
				return nil, err
			}
			return acc.response(), nil
		},
	})
}

func buildBody(req *sdk.ModelRequest) map[string]any {
	var contents []any
	for _, m := range req.Messages {
		var parts []any
		role := "user"
		switch m.Role {
		case sdk.RoleSystem:
			continue
		case sdk.RoleAssistant:
			role = "model"
		case sdk.RoleTool:
			parts = append(parts, map[string]any{"functionResponse": map[string]any{
				"name": m.ToolName, "response": map[string]any{"content": m.Content, "is_error": m.IsError},
			}})
		}
		for _, img := range m.Images {
			if img.URL != "" {
				parts = append(parts, map[string]any{"fileData": map[string]any{"mimeType": img.MediaType, "fileUri": img.URL}})
			} else {
				parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": img.MediaType, "data": base64.StdEncoding.EncodeToString(img.Data)}})
			}
		}
		if m.Content != "" && m.Role != sdk.RoleTool {
			parts = append(parts, map[string]any{"text": m.Content})
		}
		for _, tc := range m.ToolCalls {
			args := tc.Args
			if args == nil {
				args = map[string]any{}
			}
			parts = append(parts, map[string]any{"functionCall": map[string]any{"name": tc.Name, "args": args}})
		}
		if len(parts) == 0 {
			continue
		}
		// Consecutive tool results belong in one user turn.
		if n := len(contents); n > 0 && m.Role == sdk.RoleTool {
			prev := contents[n-1].(map[string]any)
			if prev["role"] == "user" && prev["tool"] == true {
				prev["parts"] = append(prev["parts"].([]any), parts...)
				continue
			}
		}
		c := map[string]any{"role": role, "parts": parts}
		if m.Role == sdk.RoleTool {
			c["tool"] = true
		}
		contents = append(contents, c)
	}
	for _, c := range contents {
		delete(c.(map[string]any), "tool")
	}
	body := map[string]any{"contents": contents}
	if strings.TrimSpace(req.System) != "" {
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": req.System}}}
	}
	if len(req.Tools) > 0 && req.ToolChoice != "none" {
		var decls []any
		for _, t := range req.Tools {
			d := map[string]any{"name": t.Name, "description": t.Description}
			if t.Schema != nil {
				d["parametersJsonSchema"] = t.Schema
			}
			decls = append(decls, d)
		}
		body["tools"] = []any{map[string]any{"functionDeclarations": decls}}
		switch req.ToolChoice {
		case "", "auto":
		case "required":
			body["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}
		default:
			body["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY", "allowedFunctionNames": []any{req.ToolChoice}}}
		}
	}
	gen := map[string]any{}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		gen["maxOutputTokens"] = req.MaxTokens
	}
	if req.ResponseSchema != nil {
		gen["responseMimeType"] = "application/json"
		gen["responseJsonSchema"] = req.ResponseSchema
	}
	if len(gen) > 0 {
		body["generationConfig"] = gen
	}
	for k, v := range req.Extra {
		body[k] = v
	}
	return body
}

type gPart struct {
	Text         string `json:"text"`
	Thought      bool   `json:"thought"`
	FunctionCall *struct {
		ID   string         `json:"id"`
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"functionCall"`
}

type gResp struct {
	Candidates []struct {
		Content struct {
			Parts []gPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

type accum struct {
	text, reasoning strings.Builder
	calls           []sdk.ToolCall
	finish          string
	usage           sdk.Usage
}

func (a *accum) add(r gResp, emit func(sdk.Chunk)) {
	if r.UsageMetadata != nil {
		a.usage = sdk.Usage{InputTokens: r.UsageMetadata.PromptTokenCount, OutputTokens: r.UsageMetadata.CandidatesTokenCount, TotalTokens: r.UsageMetadata.TotalTokenCount}
	}
	if len(r.Candidates) == 0 {
		return
	}
	cand := r.Candidates[0]
	if cand.FinishReason != "" {
		a.finish = cand.FinishReason
	}
	for _, p := range cand.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			id := p.FunctionCall.ID
			if id == "" {
				id = fmt.Sprintf("gem_%d", len(a.calls))
			}
			tc := sdk.ToolCall{ID: id, Name: p.FunctionCall.Name, Args: p.FunctionCall.Args}
			a.calls = append(a.calls, tc)
			if emit != nil {
				emit(sdk.Chunk{Type: "tool_call", ToolCall: &tc})
			}
		case p.Thought:
			a.reasoning.WriteString(p.Text)
			if emit != nil {
				emit(sdk.Chunk{Type: "reasoning", Text: p.Text})
			}
		case p.Text != "":
			a.text.WriteString(p.Text)
			if emit != nil {
				emit(sdk.Chunk{Type: "text", Text: p.Text})
			}
		}
	}
}

func (a *accum) response() *sdk.ModelResponse {
	finish := "stop"
	switch {
	case len(a.calls) > 0:
		finish = "tool_calls"
	case a.finish == "MAX_TOKENS":
		finish = "length"
	}
	msg := sdk.Message{Role: sdk.RoleAssistant, Content: a.text.String(), Reasoning: a.reasoning.String(), ToolCalls: a.calls}
	return &sdk.ModelResponse{Message: msg, Usage: a.usage, FinishReason: finish}
}

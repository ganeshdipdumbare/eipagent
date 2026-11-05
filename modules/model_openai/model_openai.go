package model_openai

import "github.com/ganeshdipdumbare/kiln/engine"

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// OpenAI-compatible chat completions adapter: OpenAI, DeepSeek, Ollama,
// vLLM, OpenRouter, Groq... Provided as "model/<name>".
//
// Config:
//
//	name:        service suffix / model prefix (default "openai")
//	base_url:    default https://api.openai.com/v1
//	api_key:     literal or secret://NAME (default secret://OPENAI_API_KEY; "" for none)
//	structured:  json_schema | json_object (how ResponseSchema is sent)
//	headers:     extra HTTP headers; values may be secret://NAME
//	timeout_seconds: default 120
//	allow_insecure: allow a non-loopback http base_url when a key/header is set
//
// Example:
//
//	kiln.WithModel("openai:gpt-4.1") // also deepseek:, ollama:, openrouter:, groq:
//	// Any other OpenAI-compatible server, e.g. vLLM, then WithModel("vllm:qwen3-32b"):
//	kiln.WithModuleConfig("model_openai", map[string]any{"name": "vllm", "base_url": "http://gpu-box:8000/v1", "api_key": ""})
var Permissions = []string{"net"}

var Requires = []string{"?secrets"} // secret:// keys/headers are optional

func Apply(p *sdk.Module) error {
	name := p.ConfigString("name", "openai")
	base := strings.TrimRight(p.ConfigString("base_url", "https://api.openai.com/v1"), "/")
	keyRef := "secret://OPENAI_API_KEY"
	if v, ok := p.Config["api_key"].(string); ok {
		keyRef = v
	}
	structured := p.ConfigString("structured", "json_schema")
	headers := p.ConfigMap("headers")
	allowInsecure := p.ConfigBool("allow_insecure", false)
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("model_openai: base_url must be an absolute http(s) URL")
	}
	hc, err := p.HTTP(sdk.HTTPOptions{
		AllowHosts:    []string{u.Hostname()},
		Timeout:       time.Duration(p.ConfigInt("timeout_seconds", 120)) * time.Second,
		AllowPrivate:  true, // operator-named endpoint: local servers (Ollama, vLLM) are common
		AllowInsecure: allowInsecure || (keyRef == "" && len(headers) == 0),
	})
	if err != nil {
		return err
	}

	return p.Provide("model/"+name, &sdk.Provider{
		Name:         name,
		Capabilities: map[string]bool{"streaming": true, "tools": true, "structured_output": true, "vision": true},
		Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			key, err := p.SecretValue(keyRef)
			if err != nil {
				return nil, err
			}
			body := buildBody(req, structured)
			payload, _ := json.Marshal(body)
			hr, err := http.NewRequestWithContext(c, "POST", base+"/chat/completions", bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			hr.Header.Set("Content-Type", "application/json")
			if key != "" {
				hr.Header.Set("Authorization", "Bearer "+key)
			}
			for k, v := range headers {
				s, ok := v.(string)
				if !ok {
					continue
				}
				val, err := p.SecretValue(s)
				if err != nil {
					return nil, err
				}
				hr.Header.Set(k, val)
			}
			if err := sdk.CheckCredentialURL(hr.URL.String(), key != "" || len(headers) > 0, allowInsecure); err != nil {
				return nil, err
			}
			resp, err := hc.Do(hr)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
				return nil, sdk.NewProviderError(name, resp.StatusCode, string(raw))
			}
			if req.Stream != nil {
				return readStream(resp.Body, req.Stream)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}
			return parseResponse(raw)
		},
	})
}

func buildBody(req *sdk.ModelRequest, structured string) map[string]any {
	var wire []any
	sys := req.System
	if req.ResponseSchema != nil && structured == "json_object" {
		sys += "\n\nReply with only a JSON object matching this JSON Schema:\n" + sdk.ToJSON(req.ResponseSchema)
	}
	if strings.TrimSpace(sys) != "" {
		wire = append(wire, map[string]any{"role": "system", "content": strings.TrimSpace(sys)})
	}
	for _, m := range req.Messages {
		w := map[string]any{"role": m.Role}
		switch {
		case m.Role == sdk.RoleTool:
			w["tool_call_id"] = m.ToolCallID
			w["content"] = m.Content
		case len(m.Images) > 0:
			parts := []any{}
			if m.Content != "" {
				parts = append(parts, map[string]any{"type": "text", "text": m.Content})
			}
			for _, img := range m.Images {
				url := img.URL
				if url == "" {
					url = "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
			w["content"] = parts
		default:
			w["content"] = m.Content
		}
		if len(m.ToolCalls) > 0 {
			var calls []any
			for _, tc := range m.ToolCalls {
				args := []byte("{}")
				if tc.Args != nil {
					args, _ = json.Marshal(tc.Args)
				}
				calls = append(calls, map[string]any{
					"id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": string(args)},
				})
			}
			w["tool_calls"] = calls
			if m.Content == "" {
				w["content"] = nil
			}
		}
		wire = append(wire, w)
	}
	body := map[string]any{"model": req.ModelID, "messages": wire}
	if len(req.Tools) > 0 && req.ToolChoice != "none" {
		var defs []any
		for _, t := range req.Tools {
			params := t.Schema
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			defs = append(defs, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": params,
			}})
		}
		body["tools"] = defs
		switch req.ToolChoice {
		case "", "auto":
		case "required", "none":
			body["tool_choice"] = req.ToolChoice
		default:
			body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": req.ToolChoice}}
		}
	}
	if req.ResponseSchema != nil {
		if structured == "json_object" {
			body["response_format"] = map[string]any{"type": "json_object"}
		} else {
			n := req.ResponseName
			if n == "" {
				n = "response"
			}
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": n, "schema": req.ResponseSchema}}
		}
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Stream != nil {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	// hook_thinking / hook_cache settings (see those modules).
	if t, ok := req.Extra["kiln.thinking"].(map[string]any); ok {
		if e, _ := t["effort"].(string); e != "" {
			body["reasoning_effort"] = e
		}
	}
	if cc, ok := req.Extra["kiln.cache"].(map[string]any); ok {
		if k, _ := cc["key"].(string); k != "" {
			body["prompt_cache_key"] = k
		}
	}
	for k, v := range req.Extra {
		if strings.HasPrefix(k, "kiln.") {
			continue
		}
		body[k] = v
	}
	return body
}

type wireCall struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireMsg struct {
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content"`
	ToolCalls        []wireCall `json:"tool_calls"`
}

type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type wireResp struct {
	Choices []struct {
		Message      wireMsg `json:"message"`
		Delta        wireMsg `json:"delta"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

func toCalls(ws []wireCall) []sdk.ToolCall {
	var out []sdk.ToolCall
	for _, w := range ws {
		tc := sdk.ToolCall{ID: w.ID, Name: w.Function.Name, Args: map[string]any{}}
		if strings.TrimSpace(w.Function.Arguments) != "" {
			json.Unmarshal([]byte(w.Function.Arguments), &tc.Args)
		}
		out = append(out, tc)
	}
	return out
}

func finishOf(r string) string {
	switch r {
	case "tool_calls", "function_call":
		return "tool_calls"
	case "length":
		return "length"
	case "content_filter":
		return "refusal"
	}
	return "stop"
}

func usageOf(u *wireUsage) sdk.Usage {
	if u == nil {
		return sdk.Usage{}
	}
	out := sdk.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	return out
}

func parseResponse(raw []byte) (*sdk.ModelResponse, error) {
	var out wireResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}
	ch := out.Choices[0]
	msg := sdk.Message{Role: sdk.RoleAssistant, Content: ch.Message.Content, Reasoning: ch.Message.ReasoningContent, ToolCalls: toCalls(ch.Message.ToolCalls)}
	return &sdk.ModelResponse{Message: msg, Usage: usageOf(out.Usage), FinishReason: finishOf(ch.FinishReason)}, nil
}

func readStream(r io.Reader, emit func(sdk.Chunk)) (*sdk.ModelResponse, error) {
	var text, reasoning strings.Builder
	calls := map[int]*wireCall{}
	finish := ""
	var usage *wireUsage
	err := sdk.ReadSSE(r, func(event, data string) error {
		if data == "[DONE]" {
			return nil
		}
		var ev wireResp
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return err
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		for _, ch := range ev.Choices {
			if ch.Delta.Content != "" {
				text.WriteString(ch.Delta.Content)
				emit(sdk.Chunk{Type: "text", Text: ch.Delta.Content})
			}
			if ch.Delta.ReasoningContent != "" {
				reasoning.WriteString(ch.Delta.ReasoningContent)
				emit(sdk.Chunk{Type: "reasoning", Text: ch.Delta.ReasoningContent})
			}
			for _, d := range ch.Delta.ToolCalls {
				cur := calls[d.Index]
				if cur == nil {
					cur = &wireCall{Index: d.Index}
					calls[d.Index] = cur
				}
				if d.ID != "" {
					cur.ID = d.ID
				}
				if d.Function.Name != "" {
					cur.Function.Name += d.Function.Name
				}
				cur.Function.Arguments += d.Function.Arguments
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var idx []int
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var ws []wireCall
	for _, i := range idx {
		ws = append(ws, *calls[i])
	}
	msg := sdk.Message{Role: sdk.RoleAssistant, Content: text.String(), Reasoning: reasoning.String(), ToolCalls: toCalls(ws)}
	for i := range msg.ToolCalls {
		c := msg.ToolCalls[i]
		emit(sdk.Chunk{Type: "tool_call", ToolCall: &c})
	}
	return &sdk.ModelResponse{Message: msg, Usage: usageOf(usage), FinishReason: finishOf(finish)}, nil
}

// Module is the compiled form of "model_openai". modules/std registers it.
var Module = engine.Module{Name: "model_openai", Requires: Requires, Provides: []string{"model/*"}, Permissions: Permissions, Secrets: []string{"OPENAI_API_KEY"}, Apply: Apply}

//go:build ignore

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"eipagent/sdk"
)

// hello_model is a model provider for an OpenAI-compatible chat API,
// registered as "model/hello_model": use models as "hello_model:<model>".
// Text only; see model_openai for tools and streaming.
//
// Config: base_url (required), api_key ("secret://hello_model_API_KEY").
var Permissions = []string{"net"}

var Requires = []string{"?secrets"}

func Apply(p *sdk.Plugin) error {
	base := strings.TrimRight(p.ConfigString("base_url", ""), "/")
	if base == "" {
		return fmt.Errorf("hello_model: config base_url is required")
	}
	keyRef := p.ConfigString("api_key", "secret://hello_model_API_KEY")
	client := &http.Client{Timeout: 120 * time.Second}
	return p.Provide("model/hello_model", &sdk.Provider{
		Name: "hello_model",
		Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			key, err := p.Secret(keyRef) // resolved per call: picks up rotation
			if err != nil {
				return nil, err
			}
			var msgs []map[string]string
			if req.System != "" {
				msgs = append(msgs, map[string]string{"role": "system", "content": req.System})
			}
			for _, m := range req.Messages {
				msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
			}
			body, _ := json.Marshal(struct {
				Model    string              `json:"model"`
				Messages []map[string]string `json:"messages"`
			}{req.ModelID, msgs})
			hr, err := http.NewRequestWithContext(c, "POST", base+"/chat/completions", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			hr.Header.Set("Content-Type", "application/json")
			hr.Header.Set("Authorization", "Bearer "+key)
			resp, err := client.Do(hr)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // bounded: a broken upstream can't exhaust memory
			if resp.StatusCode != 200 {
				return nil, sdk.NewProviderError("hello_model", resp.StatusCode, string(b))
			}
			var out struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(b, &out); err != nil || len(out.Choices) == 0 {
				return nil, fmt.Errorf("hello_model: unexpected response")
			}
			return &sdk.ModelResponse{Message: sdk.Message{Role: sdk.RoleAssistant, Content: out.Choices[0].Message.Content}, FinishReason: "stop"}, nil
		},
	})
}

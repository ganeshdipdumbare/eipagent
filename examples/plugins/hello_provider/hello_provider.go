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

// hello_provider is an LLM provider for an OpenAI-compatible chat API,
// registered as "llm/hello_provider": use models as "hello_provider:<model>".
// Text only; see llm_openai for tools and streaming.
//
// Config: base_url (required), api_key ("secret://hello_provider_API_KEY").
var Permissions = []string{"net"}

var Inject = []string{"?secrets"}

func Apply(ctx *sdk.Context) error {
	base := strings.TrimRight(ctx.ConfigString("base_url", ""), "/")
	if base == "" {
		return fmt.Errorf("hello_provider: config base_url is required")
	}
	keyRef := ctx.ConfigString("api_key", "secret://hello_provider_API_KEY")
	client := &http.Client{Timeout: 120 * time.Second}
	return ctx.Provide("llm/hello_provider", &sdk.Provider{
		Name: "hello_provider",
		Generate: func(c context.Context, req *sdk.ModelRequest) (*sdk.ModelResponse, error) {
			key, err := ctx.Secret(keyRef) // resolved per call: picks up rotation
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
				return nil, sdk.NewProviderError("hello_provider", resp.StatusCode, string(b))
			}
			var out struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(b, &out); err != nil || len(out.Choices) == 0 {
				return nil, fmt.Errorf("hello_provider: unexpected response")
			}
			return &sdk.ModelResponse{Message: sdk.Message{Role: sdk.RoleAssistant, Content: out.Choices[0].Message.Content}, FinishReason: "stop"}, nil
		},
	})
}

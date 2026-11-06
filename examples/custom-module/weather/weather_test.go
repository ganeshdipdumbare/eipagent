package weather

import (
	"context"
	"strings"
	"testing"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/modules/model_mock"
)

func TestModuleIsConfigured(t *testing.T) {
	a, err := kiln.New(kiln.Config{
		Model: "mock:test",
		Modules: []kiln.Module{
			model_mock.New(map[string]any{"script": []any{
				map[string]any{"tool": "weather", "args": map[string]any{"city": "Oslo"}},
				map[string]any{"text": "done"},
			}}),
			New(map[string]any{"unit": "fahrenheit"}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	res, err := a.Generate(context.Background(), "weather in Oslo?")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) == 0 || len(res.Steps[0].ToolResults) == 0 {
		t.Fatalf("no tool result in %d steps", len(res.Steps))
	}
	if got := res.Steps[0].ToolResults[0].Content; !strings.Contains(got, "fahrenheit") {
		t.Fatalf("got %q", got)
	}
}

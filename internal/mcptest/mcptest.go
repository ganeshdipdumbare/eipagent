// Package mcptest is a minimal MCP server implementation for tests.
package mcptest

import "fmt"

// Handle answers one JSON-RPC message (nil for notifications).
func Handle(req map[string]any) map[string]any {
	id, ok := req["id"]
	if !ok {
		return nil
	}
	params, _ := req["params"].(map[string]any)
	var result any
	switch req["method"] {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "test", "version": "1"}}
	case "tools/list":
		if params["cursor"] == nil {
			result = map[string]any{"nextCursor": "p2", "tools": []any{
				map[string]any{"name": "echo", "description": "Echo text", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}, "annotations": map[string]any{"readOnlyHint": true}},
			}}
		} else {
			result = map[string]any{"tools": []any{
				map[string]any{"name": "add", "description": "Add numbers", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"}}}},
			}}
		}
	case "tools/call":
		args, _ := params["arguments"].(map[string]any)
		switch params["name"] {
		case "echo":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(args["text"])}}}
		case "add":
			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(a + b)}}}
		default:
			result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unknown tool"}}}
		}
	default:
		return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "method not found"}}
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

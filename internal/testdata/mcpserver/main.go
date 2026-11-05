// Command mcpserver is a tiny MCP server used by tests. It speaks
// newline-delimited JSON-RPC on stdio.
package main

import (
	"bufio"
	"encoding/json"
	"os"

	"github.com/ganeshdipdumbare/kiln/internal/mcptest"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req map[string]any
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		if resp := mcptest.Handle(req); resp != nil {
			out.Encode(resp)
		}
	}
}

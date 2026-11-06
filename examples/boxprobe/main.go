// boxprobe is a program made to run inside a sandbox box. It registers
// application modules (internal/boxtest) and serves the agent over
// JSON-RPC; the host starts it with KILN_BOX=1. Run it directly with
// no Config.RunIn and it does nothing.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/internal/boxtest"
)

func main() {
	if !kiln.InBox() {
		fmt.Fprintln(os.Stderr, "boxprobe runs inside a sandbox box (KILN_BOX=1); start it with Config.RunIn")
		os.Exit(2)
	}
	if err := kiln.ServeBox(context.Background(), os.Stdin, os.Stdout,
		kiln.Config{Modules: []kiln.Module{boxtest.Probe, boxtest.Forge}}); err != nil {
		fmt.Fprintln(os.Stderr, "boxprobe:", err)
		os.Exit(1)
	}
}

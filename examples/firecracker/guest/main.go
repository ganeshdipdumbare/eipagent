// guest is the program that runs inside the Firecracker microVM. It
// registers the application modules the agent uses in the guest and serves
// the agent over JSON-RPC; the host starts it with KILN_BOX=1.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ganeshdipdumbare/kiln"
	"github.com/ganeshdipdumbare/kiln/examples/firecracker/callapi"
)

func main() {
	if !kiln.InBox() {
		fmt.Fprintln(os.Stderr, "guest runs inside a sandbox box (KILN_BOX=1)")
		os.Exit(2)
	}
	if err := kiln.ServeBox(context.Background(), os.Stdin, os.Stdout, kiln.Config{Modules: []kiln.Module{callapi.CallAPI}}); err != nil {
		fmt.Fprintln(os.Stderr, "guest:", err)
		os.Exit(1)
	}
}

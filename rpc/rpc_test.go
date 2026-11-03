package rpc

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// A message with stray output in front of it on the same line (a print
// without a trailing newline) is still answered.
func TestStrayOutputBeforeMessage(t *testing.T) {
	boxR, hostW := io.Pipe()
	hostR, boxW := io.Pipe()
	host := New(hostR, hostW, nil)
	box := New(boxR, boxW, nil)
	box.Handle("ping", func(context.Context, json.RawMessage) (any, error) { return "pong", nil })
	go host.Serve()
	go func() {
		// The box prints without a newline before its reply goes out.
		boxW.Write([]byte("progress 50%..."))
		box.Serve()
	}()
	defer host.Close()
	defer box.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out string
	if err := host.Call(ctx, "ping", nil, &out); err != nil || out != "pong" {
		t.Fatalf("call after stray output: %q %v", out, err)
	}
	for _, line := range []string{"plain log line", `{"not":"a message"`, ""} {
		var m msg
		if decode([]byte(line), &m) {
			t.Errorf("%q decoded as a message", line)
		}
	}
}

package eipagent

import (
	"context"
	"iter"

	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// Stream is a run in progress. Range over Events or Text, then call
// Result; breaking out early is fine, the run still completes.
type Stream struct {
	events chan sdk.Event
	done   chan struct{}
	res    *Result
	err    error
}

// Stream starts a run and returns immediately.
func (a *Agent) Stream(ctx context.Context, prompt string, opts ...RunOption) *Stream {
	return startStream(func(o RunOption) (*Result, error) { return a.Generate(ctx, prompt, append(opts, o)...) })
}

func startStream(run func(RunOption) (*Result, error)) *Stream {
	s := &Stream{events: make(chan sdk.Event, 64), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer close(s.events)
		s.res, s.err = run(OnEvent(func(e sdk.Event) { s.events <- e }))
	}()
	return s
}

// Events yields every event: text and reasoning deltas, tool calls and
// results, step boundaries, interrupts, finish.
func (s *Stream) Events() iter.Seq[sdk.Event] {
	return func(yield func(sdk.Event) bool) {
		for e := range s.events {
			if !yield(e) {
				go s.drain()
				return
			}
		}
	}
}

// Text yields only the assistant's text deltas.
func (s *Stream) Text() iter.Seq[string] {
	return func(yield func(string) bool) {
		for e := range s.Events() {
			if e.Type == "text" && !yield(e.Text) {
				return
			}
		}
	}
}

func (s *Stream) drain() {
	for range s.events {
	}
}

// Result waits for the run to finish.
func (s *Stream) Result() (*Result, error) {
	go s.drain()
	<-s.done
	return s.res, s.err
}

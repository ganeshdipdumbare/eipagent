package eipagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ganeshdipdumbare/eipagent/sdk"
)

// Thread is a persistent conversation stored by the "checkpointer"
// service. Any process sharing the checkpointer (e.g. Postgres) can
// continue it, so agents stay stateless.
type Thread struct {
	a  *Agent
	id string
}

func (a *Agent) Thread(id string) *Thread { return &Thread{a: a, id: id} }

func (t *Thread) ID() string { return t.id }

func (t *Thread) cp() (*sdk.Checkpointer, error) {
	cp, ok := t.a.eng.Root().Snapshot().Services["checkpointer"].(*sdk.Checkpointer)
	if !ok {
		return nil, ErrNoCheckpoint
	}
	return cp, nil
}

func (t *Thread) load(ctx context.Context) (*sdk.Checkpointer, *sdk.ThreadState, error) {
	cp, err := t.cp()
	if err != nil {
		return nil, nil, err
	}
	st, err := cp.Load(ctx, t.id)
	if err != nil {
		return nil, nil, err
	}
	if st == nil {
		st = &sdk.ThreadState{ID: t.id, Values: map[string]any{}}
	}
	return cp, st, nil
}

// Generate adds a user message and runs the agent on the thread.
func (t *Thread) Generate(ctx context.Context, prompt string, opts ...RunOption) (*Result, error) {
	if t.a.remote != nil {
		return t.a.remote.thread(ctx, t.id, prompt, nil, opts)
	}
	cp, ts, err := t.load(ctx)
	if err != nil {
		return nil, err
	}
	if len(ts.Pending) > 0 {
		return nil, fmt.Errorf("eipagent: thread %s is waiting on %d interrupt(s); call Resume", t.id, len(ts.Pending))
	}
	ro := t.a.runOpts(opts)
	st := t.a.newState(ro)
	st.Thread, st.Values = t.id, ts.Values
	st.Messages = append(append([]sdk.Message{}, ts.Messages...), ro.history...)
	st.Messages = append(st.Messages, sdk.Message{Role: sdk.RoleUser, Content: prompt, Images: ro.images})
	return t.runAndSave(ctx, cp, ts, st, ro)
}

// Stream is Generate with streaming events.
func (t *Thread) Stream(ctx context.Context, prompt string, opts ...RunOption) *Stream {
	return startStream(func(o RunOption) (*Result, error) { return t.Generate(ctx, prompt, append(opts, o)...) })
}

// Resume answers the thread's pending interrupts and continues the run.
func (t *Thread) Resume(ctx context.Context, decisions ...sdk.Decision) (*Result, error) {
	return t.ResumeWith(ctx, decisions)
}

// ResumeWith is Resume with run options (identity, events, limits).
func (t *Thread) ResumeWith(ctx context.Context, decisions []sdk.Decision, opts ...RunOption) (*Result, error) {
	if t.a.remote != nil {
		return t.a.remote.thread(ctx, t.id, "", decisions, opts)
	}
	cp, ts, err := t.load(ctx)
	if err != nil {
		return nil, err
	}
	if len(ts.Pending) == 0 {
		return nil, fmt.Errorf("eipagent: thread %s has nothing to resume", t.id)
	}
	ro := t.a.runOpts(opts)
	st := t.a.newState(ro)
	st.Thread, st.Values = t.id, ts.Values
	st.Messages = append([]sdk.Message{}, ts.Messages...)
	st.Pending, st.Decisions = ts.Pending, decisions
	return t.runAndSave(ctx, cp, ts, st, ro)
}

// ResumeStream is ResumeWith with streaming events.
func (t *Thread) ResumeStream(ctx context.Context, decisions []sdk.Decision, opts ...RunOption) *Stream {
	return startStream(func(o RunOption) (*Result, error) { return t.ResumeWith(ctx, decisions, append(opts, o)...) })
}

func (t *Thread) runAndSave(ctx context.Context, cp *sdk.Checkpointer, ts *sdk.ThreadState, st *sdk.RunState, ro *runOptions) (*Result, error) {
	res, err := t.a.run(ctx, st, ro)
	if err != nil {
		return nil, err
	}
	next := &sdk.ThreadState{ID: t.id, Messages: res.Messages, Pending: res.Interrupts, Values: st.Values, Updated: time.Now().UnixMilli()}
	if err := cp.Save(ctx, next, ts.Version); err != nil {
		if errors.Is(err, sdk.ErrVersionConflict) {
			return nil, ErrThreadBusy
		}
		return nil, err
	}
	res.Thread = t.id
	return res, nil
}

// Messages returns the stored history.
func (t *Thread) Messages(ctx context.Context) ([]sdk.Message, error) {
	if t.a.remote != nil {
		ts, err := t.a.remote.threadState(ctx, t.id)
		if err != nil {
			return nil, err
		}
		return ts.Messages, nil
	}
	_, ts, err := t.load(ctx)
	if err != nil {
		return nil, err
	}
	return ts.Messages, nil
}

// Pending returns interrupts waiting for a decision.
func (t *Thread) Pending(ctx context.Context) ([]sdk.Interrupt, error) {
	if t.a.remote != nil {
		ts, err := t.a.remote.threadState(ctx, t.id)
		if err != nil {
			return nil, err
		}
		return ts.Pending, nil
	}
	_, ts, err := t.load(ctx)
	if err != nil {
		return nil, err
	}
	return ts.Pending, nil
}

func (t *Thread) Delete(ctx context.Context) error {
	cp, err := t.cp()
	if err != nil {
		return err
	}
	return cp.Delete(ctx, t.id)
}

// Threads lists stored thread IDs with the given prefix ("" = all).
func (a *Agent) Threads(ctx context.Context, prefix string) ([]string, error) {
	cp, err := (&Thread{a: a}).cp()
	if err != nil {
		return nil, err
	}
	return cp.List(ctx, prefix)
}

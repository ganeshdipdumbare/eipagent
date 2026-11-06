package kiln

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ganeshdipdumbare/kiln/sdk"
)

// Thread is a persistent conversation stored by the "threads" service.
// Any process sharing that storage (e.g. Postgres via threads_sql) can
// continue it, so agents stay stateless.
type Thread struct {
	a  *Agent
	id string
}

func (a *Agent) Thread(id string) *Thread { return &Thread{a: a, id: id} }

func (t *Thread) ID() string { return t.id }

func (t *Thread) cp() (*sdk.Checkpointer, error) {
	cp, ok := t.a.eng.Root().Snapshot().Services["threads"].(*sdk.Checkpointer)
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
func (t *Thread) Generate(ctx context.Context, prompt string, run ...Run) (*Result, error) {
	if t.a.remote != nil {
		return t.a.remote.thread(ctx, t.id, prompt, nil, one(run))
	}
	cp, ts, err := t.load(ctx)
	if err != nil {
		return nil, err
	}
	if len(ts.Pending) > 0 {
		return nil, fmt.Errorf("kiln: thread %s is waiting on %d interrupt(s); call Resume", t.id, len(ts.Pending))
	}
	r := one(run)
	st := t.a.newState(r)
	st.Thread, st.Values = t.id, ts.Values
	st.Messages = append(append([]sdk.Message{}, ts.Messages...), r.History...)
	st.Messages = append(st.Messages, sdk.Message{Role: sdk.RoleUser, Content: prompt, Images: r.Images})
	return t.runAndSave(ctx, cp, ts, st, r)
}

// Stream is Generate with streaming events.
func (t *Thread) Stream(ctx context.Context, prompt string, run ...Run) *Stream {
	return startStream(func(onEvent func(sdk.Event)) (*Result, error) {
		r := one(run)
		r.OnEvent = onEvent
		return t.Generate(ctx, prompt, r)
	})
}

// Resume answers the thread's pending interrupts and continues the run.
func (t *Thread) Resume(ctx context.Context, decisions []sdk.Decision, run ...Run) (*Result, error) {
	if t.a.remote != nil {
		return t.a.remote.thread(ctx, t.id, "", decisions, one(run))
	}
	cp, ts, err := t.load(ctx)
	if err != nil {
		return nil, err
	}
	if len(ts.Pending) == 0 {
		return nil, fmt.Errorf("kiln: thread %s has nothing to resume", t.id)
	}
	r := one(run)
	st := t.a.newState(r)
	st.Thread, st.Values = t.id, ts.Values
	st.Messages = append([]sdk.Message{}, ts.Messages...)
	st.Pending, st.Decisions = ts.Pending, decisions
	return t.runAndSave(ctx, cp, ts, st, r)
}

// ResumeStream is Resume with streaming events.
func (t *Thread) ResumeStream(ctx context.Context, decisions []sdk.Decision, run ...Run) *Stream {
	return startStream(func(onEvent func(sdk.Event)) (*Result, error) {
		r := one(run)
		r.OnEvent = onEvent
		return t.Resume(ctx, decisions, r)
	})
}

func (t *Thread) runAndSave(ctx context.Context, cp *sdk.Checkpointer, ts *sdk.ThreadState, st *sdk.RunState, r Run) (*Result, error) {
	version := ts.Version
	var stepErr error
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	checkpoint := func() error {
		if stepErr != nil {
			return stepErr
		}
		cur := &sdk.ThreadState{ID: t.id, Messages: append([]sdk.Message(nil), st.Messages...), Values: st.Values, Updated: time.Now().UnixMilli()}
		if err := cp.Save(ctx, cur, version); err != nil {
			stepErr = err
			cancel() // stop before the next model or tool call
			return err
		}
		version++
		return nil
	}
	if t.a.saveEachStep {
		// Before each model call the history is whole (every tool call has
		// its result), so a run cut short can be continued from here. The
		// loop also calls Checkpoint immediately before executing tools, so
		// a failed save can stop before a side effect.
		r.onStep = func(st *sdk.RunState) { _ = checkpoint() }
		r.checkpoint = checkpoint
	}
	res, err := t.a.exec(runCtx, st, r)
	if stepErr != nil {
		err = stepErr
	}
	if err != nil {
		if errors.Is(err, sdk.ErrVersionConflict) {
			return nil, ErrThreadBusy
		}
		return nil, err
	}
	next := &sdk.ThreadState{ID: t.id, Messages: res.Messages, Pending: res.Interrupts, Values: st.Values, Updated: time.Now().UnixMilli()}
	if err := cp.Save(ctx, next, version); err != nil {
		if errors.Is(err, sdk.ErrVersionConflict) {
			return nil, ErrThreadBusy
		}
		return nil, err
	}
	res.Thread = t.id
	return res, nil
}

// Continue runs the agent on the thread's stored history without a new
// message: for a run that was cut short (see Config.SaveEachStep), or after
// editing the history elsewhere. It fails when the last message is
// already the model's answer.
func (t *Thread) Continue(ctx context.Context, run ...Run) (*Result, error) {
	if t.a.remote != nil {
		return nil, errors.New("kiln: Thread.Continue is not supported with Config.RunIn")
	}
	cp, ts, err := t.load(ctx)
	if err != nil {
		return nil, err
	}
	if len(ts.Pending) > 0 {
		return nil, fmt.Errorf("kiln: thread %s is waiting on %d interrupt(s); call Resume", t.id, len(ts.Pending))
	}
	if n := len(ts.Messages); n == 0 || (ts.Messages[n-1].Role == sdk.RoleAssistant && len(ts.Messages[n-1].ToolCalls) == 0) {
		return nil, fmt.Errorf("kiln: thread %s has nothing to continue", t.id)
	}
	r := one(run)
	st := t.a.newState(r)
	st.Thread, st.Values = t.id, ts.Values
	st.Messages = append(append([]sdk.Message{}, ts.Messages...), r.History...)
	return t.runAndSave(ctx, cp, ts, st, r)
}

// Fork copies the thread (history, pending approvals and values) to a
// new thread, so you can try a different direction without changing this
// one. It fails if newID already exists.
func (t *Thread) Fork(ctx context.Context, newID string) (*Thread, error) {
	if t.a.remote != nil {
		return nil, errors.New("kiln: Thread.Fork is not supported with Config.RunIn")
	}
	cp, ts, err := t.load(ctx)
	if err != nil {
		return nil, err
	}
	if ts.Version == 0 {
		return nil, fmt.Errorf("kiln: thread %s does not exist", t.id)
	}
	vals := map[string]any{}
	for k, v := range ts.Values {
		vals[k] = v
	}
	cp2 := &sdk.ThreadState{ID: newID, Messages: append([]sdk.Message(nil), ts.Messages...),
		Pending: append([]sdk.Interrupt(nil), ts.Pending...), Values: vals, Updated: time.Now().UnixMilli()}
	if err := cp.Save(ctx, cp2, 0); err != nil {
		if errors.Is(err, sdk.ErrVersionConflict) {
			return nil, fmt.Errorf("kiln: thread %s already exists", newID)
		}
		return nil, err
	}
	return t.a.Thread(newID), nil
}

// Messages returns the thread's stored history.
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

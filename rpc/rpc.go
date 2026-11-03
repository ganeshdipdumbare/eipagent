// Package rpc is a small bidirectional JSON-RPC 2.0 connection over a
// byte stream (newline-delimited). Both sides can call and notify; it
// connects a host to an agent running inside a sandbox.
package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

type Handler func(ctx context.Context, params json.RawMessage) (any, error)

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

var ErrClosed = errors.New("rpc: connection closed")

type Conn struct {
	r        io.Reader
	w        io.Writer
	closer   func()
	wmu      sync.Mutex
	mu       sync.Mutex
	seq      int64
	pending  map[int64]chan *msg
	handlers map[string]Handler
	done     chan struct{}
	err      error
	ctx      context.Context
	cancel   context.CancelFunc
}

// New creates a connection; call Serve to start reading.
func New(r io.Reader, w io.Writer, closer func()) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &Conn{r: r, w: w, closer: closer, pending: map[int64]chan *msg{}, handlers: map[string]Handler{}, done: make(chan struct{}), ctx: ctx, cancel: cancel}
}

// Handle registers a method (requests and notifications).
func (c *Conn) Handle(method string, h Handler) {
	c.mu.Lock()
	c.handlers[method] = h
	c.mu.Unlock()
}

func (c *Conn) write(m *msg) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// Call sends a request and decodes the result into out (may be nil).
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.seq++
	id := c.seq
	ch := make(chan *msg, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.write(&msg{ID: &id, Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-c.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Notify sends a notification (no reply).
func (c *Conn) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(&msg{Method: method, Params: raw})
}

// Serve reads until the stream ends. Requests run concurrently;
// notifications run in arrival order and must not block.
func (c *Conn) Serve() error {
	sc := bufio.NewScanner(c.r)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var m msg
		if !decode(sc.Bytes(), &m) {
			continue // ignore stray output
		}
		if m.Method == "" && m.ID != nil { // response
			c.mu.Lock()
			ch := c.pending[*m.ID]
			c.mu.Unlock()
			if ch != nil {
				ch <- &m
			}
			continue
		}
		c.mu.Lock()
		h := c.handlers[m.Method]
		c.mu.Unlock()
		if m.ID == nil { // notifications run in order, on the read loop
			if h != nil {
				h(c.ctx, m.Params)
			}
			continue
		}
		go func(m msg) {
			if h == nil {
				if m.ID != nil {
					c.write(&msg{ID: m.ID, Error: &Error{Code: -32601, Message: "method not found: " + m.Method}})
				}
				return
			}
			res, err := h(c.ctx, m.Params)
			if m.ID == nil {
				return
			}
			if err != nil {
				c.write(&msg{ID: m.ID, Error: &Error{Code: -32000, Message: err.Error()}})
				return
			}
			raw, merr := json.Marshal(res)
			if merr != nil {
				c.write(&msg{ID: m.ID, Error: &Error{Code: -32603, Message: fmt.Sprintf("marshal: %v", merr)}})
				return
			}
			c.write(&msg{ID: m.ID, Result: raw})
		}(m)
	}
	c.err = sc.Err()
	c.Close()
	return c.err
}

// msgStart is how every message this package writes begins.
var msgStart = []byte(`{"jsonrpc":`)

// decode reads one line. Output printed without a trailing newline
// ends up in front of the next message on the same line; the message
// is still read from where it starts, so its caller isn't left waiting.
func decode(line []byte, m *msg) bool {
	if json.Unmarshal(line, m) == nil {
		return true
	}
	if i := bytes.Index(line, msgStart); i > 0 {
		*m = msg{}
		return json.Unmarshal(line[i:], m) == nil
	}
	return false
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Close() {
	c.mu.Lock()
	select {
	case <-c.done:
	default:
		close(c.done)
		c.cancel()
		if c.closer != nil {
			c.closer()
		}
	}
	c.mu.Unlock()
}

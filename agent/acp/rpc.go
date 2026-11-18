// Package acp runs an agent CLI as an Agent Client Protocol server (stdio
// JSON-RPC) for one chat session (P6). The kimi and antigravity agents use
// it when their backend option is "acp": modes, model and thinking level
// switch inside the running session, permission requests and questions go
// to the chat, and /stop cancels the turn without ending the process.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// callTimeout bounds a request and its answer; session/prompt waits for the
// whole turn instead (see call).
var callTimeout = 30 * time.Second

// writeTimeout bounds one write: a server that stops reading must not hang
// the caller.
var writeTimeout = 30 * time.Second

var errClosed = errors.New("acp: agent process exited")

// rpcError is a JSON-RPC error answer.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Code) }

type rpcReply struct {
	result json.RawMessage
	err    error
}

type rpcClient struct {
	w       io.Writer
	writeMu sync.Mutex
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcReply
	done    chan struct{} // closed when the reader stops
	// Set before readLoop starts.
	onNotify  func(method string, params json.RawMessage)
	onRequest func(id json.RawMessage, method string, params json.RawMessage)
	// onBroken is called once when a write does not finish in time: the
	// owner stops the process (a late message could still go out).
	onBroken   func()
	brokenOnce sync.Once
}

func newRPCClient(w io.Writer) *rpcClient {
	return &rpcClient{w: w, pending: map[int64]chan rpcReply{}, done: make(chan struct{})}
}

func (c *rpcClient) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

// writeBounded writes v within writeTimeout or ctx, whichever ends first;
// a write that does not finish breaks the connection (onBroken).
func (c *rpcClient) writeBounded(ctx context.Context, v any) error {
	errc := make(chan error, 1)
	go func() { errc <- c.write(v) }()
	timer := time.NewTimer(writeTimeout)
	defer timer.Stop()
	broken := func() {
		c.brokenOnce.Do(func() {
			slog.Warn("acp: a write did not finish; stopping the agent process")
			if c.onBroken != nil {
				go c.onBroken()
			}
		})
	}
	select {
	case err := <-errc:
		return err
	case <-c.done:
		return errClosed
	case <-timer.C:
		broken()
		return errors.New("acp: write timed out")
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			broken() // a deadline, not the session closing
		}
		return ctx.Err()
	}
}

// call sends a request and waits for its answer: at most callTimeout, or as
// long as ctx allows when wait is false (session/prompt lasts a turn).
func (c *rpcClient) call(ctx context.Context, method string, params any, bounded bool) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
	}
	if err := c.writeBounded(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("acp %s: %w", method, err)
	}
	return c.await(ctx, method, ch)
}

func (c *rpcClient) await(ctx context.Context, method string, ch chan rpcReply) (json.RawMessage, error) {
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("acp %s: %w", method, r.err)
		}
		return r.result, nil
	case <-c.done:
		return nil, fmt.Errorf("acp %s: %w", method, errClosed)
	case <-ctx.Done():
		return nil, fmt.Errorf("acp %s: %w", method, ctx.Err())
	}
}

// start writes a request now (so it is ordered before anything written
// later) and returns a function that waits for its answer.
func (c *rpcClient) start(ctx context.Context, method string, params any) (func(ctx context.Context) (json.RawMessage, error), error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	if err := c.writeBounded(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		forget()
		return nil, fmt.Errorf("acp %s: %w", method, err)
	}
	return func(ctx context.Context) (json.RawMessage, error) {
		defer forget()
		return c.await(ctx, method, ch)
	}, nil
}

func (c *rpcClient) notify(ctx context.Context, method string, params any) error {
	return c.writeBounded(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *rpcClient) respond(ctx context.Context, id json.RawMessage, result any) error {
	return c.writeBounded(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *rpcClient) respondError(id json.RawMessage, code int, msg string) {
	_ = c.writeBounded(context.Background(), map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

// readLoop dispatches answers, server requests and notifications until r
// ends.
func (c *rpcClient) readLoop(r io.Reader) {
	defer close(c.done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
		switch {
		case msg.Method != "" && hasID:
			if c.onRequest != nil {
				c.onRequest(msg.ID, msg.Method, msg.Params)
			} else {
				c.respondError(msg.ID, -32601, "not supported")
			}
		case msg.Method != "":
			if c.onNotify != nil {
				c.onNotify(msg.Method, msg.Params)
			}
		case hasID:
			var id int64
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			reply := rpcReply{result: msg.Result}
			if msg.Error != nil {
				reply.err = msg.Error
			}
			select {
			case ch <- reply:
			default:
			}
		}
	}
	if err := sc.Err(); err != nil {
		slog.Warn("acp: read failed", "error", err)
	}
}

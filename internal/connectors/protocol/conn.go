// Package protocol owns trebi-connector/1: JSON-RPC 2.0, one object per
// line, over the stdin and stdout of one adapter process (Part D). The
// daemon side is Conn. The typed params and results of every message live
// in messages.go, and the wire fixtures in testdata/contract.
package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
)

// Error is a JSON-RPC error with the typed data of Part D.6.
type Error struct {
	Code       string
	Message    string
	RetryAfter time.Duration
	rpcCode    int
}

// Conn is the daemon end of one adapter session. Run reads the adapter
// output; Call and Notify write to it.
type Conn struct {
	w       io.Writer
	r       io.Reader
	notify  NotifyFunc
	badLine func(error)

	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan response
	closed  chan struct{}
	err     error
}

// NotifyFunc receives each notification from the adapter. An empty method
// means a line that is not JSON-RPC 2.0, when the Conn has no WithBadLine.
// It runs on the read loop, so it must not block on a Call of the same Conn.
type NotifyFunc func(method string, params json.RawMessage)

// Option configures a Conn.
type Option func(*Conn)

// WithBadLine gives each stdout line that is not JSON-RPC 2.0 to fn, as an
// error that wraps ErrNotJSONRPC and holds the start of the line. The
// notify func then does not get the empty method. fn runs on the read loop.
func WithBadLine(fn func(error)) Option {
	return func(c *Conn) { c.badLine = fn }
}

// Message is one JSON-RPC 2.0 object on the wire.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type errorData struct {
	Code         string `json:"code"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
}

type response struct {
	result json.RawMessage
	err    error
}

// Error codes of Part D.6 (data.code).
const (
	CodeTransient    = "transient"
	CodeRateLimited  = "rate_limited"
	CodeAuthRequired = "auth_required"
	CodeNotFound     = "not_found"
	CodeInvalid      = "invalid"
	CodeUnsupported  = "unsupported"
	CodePermanent    = "permanent"
)

// JSON-RPC 2.0 numeric codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcServerError    = -32000
)

// MaxLine is the largest line on the wire (1 MiB).
const MaxLine = 1 << 20

var (
	// ErrClosed is a call on a Conn whose reader ended.
	ErrClosed = errors.New("connector session closed")
	// ErrLineTooLong is a line over MaxLine.
	ErrLineTooLong = errors.New("connector line over 1 MiB")
	// ErrNotJSONRPC is a stdout line that is not a JSON-RPC 2.0 message.
	ErrNotJSONRPC = errors.New("stdout line is not JSON-RPC 2.0")
)

// badLineShown is the part of a bad line that an ErrNotJSONRPC error holds.
const badLineShown = 200

// NewConn builds a Conn. notify may be nil.
func NewConn(r io.Reader, w io.Writer, notify NotifyFunc, opts ...Option) *Conn {
	c := &Conn{
		r: r, w: w, notify: notify,
		pending: map[int64]chan response{},
		closed:  make(chan struct{}),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Run reads adapter output until EOF, a framing error, or ctx ends. It
// answers any adapter request with method-not-found, because the adapter
// does not send requests. Each pending Call fails with ErrClosed after Run.
func (c *Conn) Run(ctx context.Context) error {
	lines := make(chan []byte)
	errc := make(chan error, 1)
	go func() { // ends at EOF or a read error; the caller closes the reader
		defer close(lines)
		sc := bufio.NewScanner(c.r)
		sc.Buffer(make([]byte, 0, 64*1024), MaxLine)
		for sc.Scan() {
			select {
			case lines <- bytes.Clone(sc.Bytes()):
			case <-ctx.Done():
				return
			}
		}
		if err := sc.Err(); errors.Is(err, bufio.ErrTooLong) {
			errc <- ErrLineTooLong
		} else if err != nil {
			errc <- err
		}
	}()
	var err error
loop:
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break loop
		case line, ok := <-lines:
			if !ok {
				select {
				case err = <-errc:
				default:
				}
				break loop
			}
			c.dispatch(line)
		}
	}
	c.close(err)
	return err
}

// Done is closed when Run ends.
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Err returns the reason Run ended, or nil while it runs or after EOF.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Call sends one request and waits for its answer. The method timeout of
// Part D.1 applies on top of ctx. A JSON-RPC error returns as *Error.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	if d := Timeout(method); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	ch := make(chan response, 1)
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return ErrClosed
	default:
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	if err := c.write(Message{JSONRPC: "2.0", ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: raw}); err != nil {
		return err
	}
	var res response
	select {
	case res = <-ch:
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.closed:
		// Run dispatches every line before it closes, so an answer read before EOF is already in ch.
		select {
		case res = <-ch:
		default:
			return ErrClosed
		}
	}
	if res.err != nil {
		return res.err
	}
	if result == nil || len(res.result) == 0 {
		return nil
	}
	if err := json.Unmarshal(res.result, result); err != nil {
		return fmt.Errorf("%s result: %w", method, err)
	}
	return nil
}

// Notify sends one notification.
func (c *Conn) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(Message{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *Conn) write(m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(b)
	return err
}

func (c *Conn) dispatch(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var m Message
	if err := json.Unmarshal(line, &m); err != nil || m.JSONRPC != "2.0" {
		switch {
		case c.badLine != nil:
			shown := line[:min(len(line), badLineShown)]
			c.badLine(fmt.Errorf("%w: %q", ErrNotJSONRPC, shown))
		case c.notify != nil:
			c.notify("", nil) // a parse error; the owner counts it
		}
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		_ = c.write(Message{JSONRPC: "2.0", ID: m.ID, Error: &wireError{ //nolint:errcheck // best effort; the adapter broke the protocol
			Code: rpcMethodNotFound, Message: "the daemon takes no requests",
		}})
	case m.Method != "":
		if c.notify != nil {
			c.notify(m.Method, m.Params)
		}
	case len(m.ID) > 0:
		id, err := strconv.ParseInt(string(m.ID), 10, 64)
		if err != nil {
			return
		}
		c.mu.Lock()
		ch, ok := c.pending[id]
		c.mu.Unlock()
		if !ok {
			return
		}
		if m.Error != nil {
			ch <- response{err: m.Error.typed()}
			return
		}
		ch <- response{result: m.Result}
	}
}

func (c *Conn) close(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return
	default:
	}
	c.err = err
	close(c.closed)
}

// NewError builds a typed error.
func NewError(code, msg string) *Error { return &Error{Code: code, Message: msg} }

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// Is matches another *Error by code, so errors.Is(err, &Error{Code: …})
// works.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// Wire returns the JSON-RPC error object of e.
func (e *Error) Wire() json.RawMessage {
	code := e.rpcCode
	if code == 0 {
		code = rpcServerError
		if e.Code == CodeInvalid {
			code = rpcInvalidParams
		}
		if e.Code == CodeUnsupported {
			code = rpcMethodNotFound
		}
	}
	d := errorData{Code: e.Code, RetryAfterMS: e.RetryAfter.Milliseconds()}
	data, _ := json.Marshal(d)
	b, _ := json.Marshal(wireError{Code: code, Message: e.Message, Data: data})
	return b
}

// CodeOf returns the typed code of err, or "" when err is not an *Error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// typed maps the wire error to *Error. An error with no typed data maps by
// its JSON-RPC code.
func (w *wireError) typed() *Error {
	e := &Error{Message: w.Message, rpcCode: w.Code}
	var d errorData
	if len(w.Data) > 0 && json.Unmarshal(w.Data, &d) == nil && d.Code != "" {
		e.Code = d.Code
		e.RetryAfter = time.Duration(d.RetryAfterMS) * time.Millisecond
		return e
	}
	switch w.Code {
	case rpcMethodNotFound:
		e.Code = CodeUnsupported
	case rpcInvalidParams, rpcInvalidRequest, rpcParseError:
		e.Code = CodeInvalid
	default:
		e.Code = CodePermanent
	}
	return e
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return json.RawMessage(`{}`), nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(params)
}

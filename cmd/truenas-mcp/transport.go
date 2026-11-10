package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

// lineTransport is the MCP stdio transport: newline-delimited JSON-RPC
// messages on in and out. It is used instead of the SDK's StdioTransport,
// which ends the session on the first line that is not valid JSON or is over
// its size limit. Here such a line gets a JSON-RPC error reply (-32700 or
// -32600, with "id": null) and the session goes on.
type lineTransport struct {
	in           io.Reader
	out          io.Writer
	maxLineBytes int
	debug        bool
}

func (t *lineTransport) Connect(context.Context) (sdk.Connection, error) {
	c := &lineConn{
		in:       t.in,
		out:      t.out,
		debug:    t.debug,
		incoming: make(chan msgOrErr),
		closed:   make(chan struct{}),
	}
	go c.readLoop(mcp.NewLineReader(t.in, t.maxLineBytes), t.maxLineBytes)
	return c, nil
}

// errStdoutWrite marks a failure to write to stdout, after which no response
// can reach the client.
var errStdoutWrite = errors.New("failed to write to stdout")

type msgOrErr struct {
	msg jsonrpc.Message
	err error
}

type lineConn struct {
	in    io.Reader
	out   io.Writer
	debug bool

	writeMu sync.Mutex

	// calls holds the ids of requests read but not yet answered; true marks
	// one the client cancelled. MCP says a cancelled request gets no reply,
	// but the SDK still sends one (an error), so Write drops it.
	callsMu sync.Mutex
	calls   map[jsonrpc.ID]bool

	incoming  chan msgOrErr
	closeOnce sync.Once
	closed    chan struct{}
}

// readLoop reads and decodes messages until the input ends or fails, or a
// reply to a malformed line cannot be written. It runs in its own goroutine
// so that Close can unblock Read.
func (c *lineConn) readLoop(lines *mcp.LineReader, maxLineBytes int) {
	for {
		line, err := lines.ReadLine()
		var msg jsonrpc.Message
		switch {
		case errors.Is(err, mcp.ErrLineTooLong):
			log.Printf("Rejected a request longer than %d bytes", maxLineBytes)
			err = c.writeError(nil, jsonrpc.CodeInvalidRequest, fmt.Sprintf("Request too large: the limit is %d bytes", maxLineBytes))
		case errors.Is(err, io.EOF):
		case err != nil:
			err = fmt.Errorf("stdin error: %w", err)
		default:
			msg, err = c.decode(line)
		}
		if err != nil {
			// After Close (which closes the input) the read error is a
			// consequence, not the cause, of the connection ending.
			select {
			case <-c.closed:
			default:
				c.deliver(msgOrErr{err: err})
			}
			return
		}
		if msg != nil && !c.deliver(msgOrErr{msg: msg}) {
			return
		}
	}
}

// decode parses one line. It returns a nil message for a line that was
// answered with an error or is dropped, and an error only if that answer
// could not be written.
func (c *lineConn) decode(line []byte) (jsonrpc.Message, error) {
	if c.debug {
		log.Printf("[STDIN] %s", truenas.RedactJSONForLog(line))
	}
	if !json.Valid(line) {
		var v interface{}
		err := json.Unmarshal(line, &v)
		if c.debug {
			log.Printf("Parse error: %v", err)
		}
		return nil, c.writeError(nil, jsonrpc.CodeParseError, fmt.Sprintf("Parse error: %v", err))
	}
	msg, err := jsonrpc.DecodeMessage(line)
	if err != nil {
		if c.debug {
			log.Printf("Invalid request: %v", err)
		}
		return nil, c.writeError(requestID(line), jsonrpc.CodeInvalidRequest, fmt.Sprintf("Invalid Request: %v", err))
	}
	if req, ok := msg.(*jsonrpc.Request); ok {
		// A message without an id (or with "id": null, which MCP does not
		// allow) is a notification: it gets no reply and must not run
		// anything. Only notifications/* methods are notifications in MCP.
		if !req.IsCall() && !strings.HasPrefix(req.Method, "notifications/") {
			if c.debug {
				log.Printf("Ignoring %s sent without an id (a notification)", redact.Text(req.Method))
			}
			return nil, nil
		}
		if c.debug {
			log.Printf("Handling method: %s (id: %v)", redact.Text(req.Method), req.ID.Raw())
		}
		c.track(req)
	}
	return msg, nil
}

// track records a call as unanswered, and marks the call named by a
// notifications/cancelled as cancelled if it is still unanswered.
func (c *lineConn) track(req *jsonrpc.Request) {
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	if req.IsCall() {
		if c.calls == nil {
			c.calls = map[jsonrpc.ID]bool{}
		}
		if _, dup := c.calls[req.ID]; !dup {
			c.calls[req.ID] = false
		}
		return
	}
	if req.Method != "notifications/cancelled" {
		return
	}
	var p struct {
		RequestID interface{} `json:"requestId"`
	}
	if json.Unmarshal(req.Params, &p) != nil {
		return
	}
	if id, err := jsonrpc.MakeID(p.RequestID); err == nil && id.IsValid() {
		if _, ok := c.calls[id]; ok {
			c.calls[id] = true
		}
	}
}

// answered removes a call from the unanswered set and reports whether the
// client had cancelled it.
func (c *lineConn) answered(id jsonrpc.ID) (cancelled bool) {
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	cancelled = c.calls[id]
	delete(c.calls, id)
	return cancelled
}

// requestID returns the id of a JSON-RPC message if it is an object with a
// valid id (a string or a number), and nil otherwise.
func requestID(line []byte) interface{} {
	var obj struct {
		ID interface{} `json:"id"`
	}
	if json.Unmarshal(line, &obj) != nil {
		return nil
	}
	switch id := obj.ID.(type) {
	case string, float64:
		return id
	}
	return nil
}

func (c *lineConn) deliver(m msgOrErr) bool {
	select {
	case c.incoming <- m:
		return true
	case <-c.closed:
		return false
	}
}

func (c *lineConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case m := <-c.incoming:
		return m.msg, m.err
	case <-c.closed:
		return nil, io.EOF
	}
}

func (c *lineConn) Write(_ context.Context, msg jsonrpc.Message) error {
	if resp, ok := msg.(*jsonrpc.Response); ok && c.answered(resp.ID) {
		if c.debug {
			log.Printf("Dropping the reply to cancelled request %v", resp.ID.Raw())
		}
		return nil
	}
	if resp, ok := msg.(*jsonrpc.Response); ok {
		redactResponseError(resp)
	}
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}
	return c.writeLine(data)
}

// redactResponseError applies redact.Text to a JSON-RPC error message, and
// redacts error.data when it is present, so a client-supplied name that
// looks like a secret (an unknown tool or method) never leaves unmasked.
func redactResponseError(resp *jsonrpc.Response) {
	if resp == nil || resp.Error == nil {
		return
	}
	we, ok := resp.Error.(*jsonrpc.Error)
	if !ok {
		we = &jsonrpc.Error{Message: resp.Error.Error()}
		var wrapped *jsonrpc.Error
		if errors.As(resp.Error, &wrapped) {
			we.Code = wrapped.Code
			we.Data = wrapped.Data
		}
		resp.Error = we
	}
	we.Message = redact.Text(we.Message)
	if len(we.Data) > 0 {
		we.Data = redactErrorData(we.Data)
	}
}

// redactErrorData redacts a JSON-RPC error.data value: a JSON string is
// treated as free text, and any other JSON is redacted structurally.
func redactErrorData(raw json.RawMessage) json.RawMessage {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return raw
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return json.RawMessage(redact.Text(string(raw)))
		}
		b, err := json.Marshal(redact.Text(s))
		if err != nil {
			return json.RawMessage(`"` + redact.Marker + `"`)
		}
		return b
	}
	return redact.JSON(raw)
}

// writeError sends an error reply for a line the SDK never sees. A nil id is
// sent as "id": null, as JSON-RPC 2.0 requires when the id is unknown.
func (c *lineConn) writeError(id interface{}, code int, message string) error {
	data, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]interface{}{"code": code, "message": redact.Text(message)},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}
	return c.writeLine(data)
}

func (c *lineConn) writeLine(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.debug {
		log.Printf("[STDOUT] %s", truenas.RedactJSONForLog(data))
	}
	// One write per message, so a line is never split by another writer.
	if _, err := c.out.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("%w: %w", errStdoutWrite, err)
	}
	return nil
}

// Close stops the connection. It closes the input if it can be closed, so
// that a read blocked on it returns; stdout is left open.
func (c *lineConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		if closer, ok := c.in.(io.Closer); ok {
			closer.Close()
		}
	})
	return nil
}

func (c *lineConn) SessionID() string { return "" }

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
)

// replyTimeout bounds every wait for the server; it is long enough for
// -race on a slow machine.
const replyTimeout = 10 * time.Second

// rpcClient plays the MCP client on the other end of the server's stdin and
// stdout. The server answers requests concurrently and drops the replies of
// calls still running when its input ends, so a test sends a request and
// reads its reply before it closes the input.
type rpcClient struct {
	t     *testing.T
	in    io.WriteCloser
	lines chan []byte
}

func newRPCClient(t *testing.T, in io.WriteCloser, out io.Reader) *rpcClient {
	c := &rpcClient{t: t, in: in, lines: make(chan []byte, 16)}
	go func() {
		defer close(c.lines)
		r := mcp.NewLineReader(out, 1<<30)
		for {
			line, err := r.ReadLine()
			if err != nil {
				return
			}
			c.lines <- bytes.Clone(line)
		}
	}()
	return c
}

// serverRun is a StdioHandler.Run in progress.
type serverRun struct {
	t    *testing.T
	in   io.Closer
	done chan error
	once sync.Once
	err  error
}

// result waits for Run to return, without closing its input, and returns
// its error. It fails the test if Run does not return.
func (r *serverRun) result() error {
	r.t.Helper()
	r.once.Do(func() {
		select {
		case r.err = <-r.done:
		case <-time.After(replyTimeout):
			r.t.Fatal("Run did not return")
		}
	})
	return r.err
}

// wait closes the server's input (the client disconnects) and returns what
// Run returned.
func (r *serverRun) wait() error {
	r.t.Helper()
	r.in.Close()
	return r.result()
}

// session runs h on pipes. wrapOut, if set, wraps the server's stdout.
func session(t *testing.T, h *StdioHandler, wrapOut ...func(io.Writer) io.Writer) (*rpcClient, *serverRun) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h.in, h.out = inR, outW
	for _, w := range wrapOut {
		h.out = w(h.out)
	}
	run := &serverRun{t: t, in: inW, done: make(chan error, 1)}
	go func() {
		err := h.Run(context.Background())
		outW.Close()
		run.done <- err
	}()
	t.Cleanup(func() { inW.Close() })
	return newRPCClient(t, inW, outR), run
}

func newTestHandler(reg mcp.ToolRegistry, debug bool) *StdioHandler {
	return newStdioHandler(reg, debug, nil, nil)
}

func rpcLine(t *testing.T, v map[string]interface{}) string {
	t.Helper()
	v["jsonrpc"] = "2.0"
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// sendRaw writes s to the server's input. It reports whether the write
// succeeded: it fails once the server has stopped reading.
func (c *rpcClient) sendRaw(s string) bool {
	_, err := io.WriteString(c.in, s)
	return err == nil
}

func (c *rpcClient) send(v map[string]interface{}) {
	c.t.Helper()
	if !c.sendRaw(rpcLine(c.t, v)) {
		c.t.Fatalf("server stopped reading before %v", v["method"])
	}
}

// recvLine returns the next stdout line, checked to be a JSON-RPC 2.0
// response with exactly one of result and error.
func (c *rpcClient) recvLine() ([]byte, map[string]interface{}) {
	c.t.Helper()
	select {
	case line, ok := <-c.lines:
		if !ok {
			c.t.Fatal("stdout ended while a reply was expected")
		}
		return line, checkJSONRPCResponse(c.t, line)
	case <-time.After(replyTimeout):
		c.t.Fatal("no reply from the server")
	}
	return nil, nil
}

func (c *rpcClient) recv() map[string]interface{} {
	c.t.Helper()
	_, m := c.recvLine()
	return m
}

// call sends a request and returns its reply, which must be the next line.
func (c *rpcClient) call(id interface{}, method string, params interface{}) map[string]interface{} {
	c.t.Helper()
	req := map[string]interface{}{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	c.send(req)
	m := c.recv()
	if m == nil {
		c.t.Fatalf("no reply to %s", method)
	}
	if m["id"] != id {
		c.t.Fatalf("reply to %s has id %v, want %v: %v", method, m["id"], id, m)
	}
	return m
}

func initParams(version string) map[string]interface{} {
	return map[string]interface{}{
		"protocolVersion": version,
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "test-client", "version": "1.0"},
	}
}

// initialize performs the handshake and returns the initialize result.
func (c *rpcClient) initialize() map[string]interface{} {
	c.t.Helper()
	m := c.call("init", "initialize", initParams("2025-11-25"))
	c.send(map[string]interface{}{"method": "notifications/initialized"})
	res, _ := m["result"].(map[string]interface{})
	if res == nil {
		c.t.Fatalf("initialize failed: %v", m)
	}
	return res
}

// noMoreOutput checks the server wrote nothing after the replies read so
// far. Call it after wait.
func (c *rpcClient) noMoreOutput() {
	c.t.Helper()
	for line := range c.lines {
		c.t.Errorf("unexpected output: %.200s", line)
	}
}

func checkJSONRPCResponse(t *testing.T, line []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("stdout line is not JSON (%v): %.200q", err, line)
	}
	if m["jsonrpc"] != "2.0" {
		t.Fatalf("stdout line is not JSON-RPC 2.0: %.200q", line)
	}
	_, hasResult := m["result"]
	_, hasError := m["error"]
	if hasResult == hasError {
		t.Fatalf("stdout line must have exactly one of result and error: %.200q", line)
	}
	return m
}

func errorCode(m map[string]interface{}) float64 {
	e, _ := m["error"].(map[string]interface{})
	code, _ := e["code"].(float64)
	return code
}

// toolText returns the text of a tools/call result and its isError flag.
func toolText(t *testing.T, m map[string]interface{}) (string, bool) {
	t.Helper()
	res, _ := m["result"].(map[string]interface{})
	content, _ := res["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("tools/call result = %v; want one content block", m)
	}
	block, _ := content[0].(map[string]interface{})
	if block["type"] != "text" {
		t.Fatalf("content block = %v; want text", block)
	}
	text, _ := block["text"].(string)
	isError, _ := res["isError"].(bool)
	return text, isError
}

// assertIDNull checks the raw JSON of a reply has an "id" member that is
// null (json.Unmarshal into a map alone cannot tell null from absent).
func assertIDNull(t *testing.T, line []byte) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		t.Fatalf("reply is not JSON: %v", err)
	}
	if id, ok := raw["id"]; !ok || string(id) != "null" {
		t.Errorf("reply = %s; want \"id\":null", line)
	}
}

// syncBuffer is a bytes.Buffer safe for the log package's writes from the
// server's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func toolArgs(name string, args map[string]interface{}) map[string]interface{} {
	p := map[string]interface{}{"name": name}
	if args != nil {
		p["arguments"] = args
	}
	return p
}

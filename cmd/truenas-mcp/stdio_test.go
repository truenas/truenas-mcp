package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
)

// largeResultBytes is a tool result big enough to exceed the old 64 KiB
// scanner limit, but under the 100 KiB client output cap, so protocol
// tests can still check that a large reply reaches stdout in full.
const largeResultBytes = 80 << 10

// fakeRegistry echoes the size of its "data" argument and returns a large
// result, and logs from inside the tool as real tools do.
type fakeRegistry struct{ resultBytes int }

func (f fakeRegistry) ListTools() []mcp.Tool {
	return []mcp.Tool{{Name: "echo", Description: "echo", InputSchema: map[string]interface{}{"type": "object"}}}
}

func (f fakeRegistry) CallTool(_ context.Context, name string, args map[string]interface{}) (string, error) {
	log.Printf("calling tool %s", name)
	data, _ := args["data"].(string)
	return fmt.Sprintf("got %d bytes;%s", len(data), strings.Repeat("r", f.resultBytes)), nil
}

// runStdioSession drives a session covering every response path: requests,
// notifications, a large request and result, an unknown method and garbage.
// Each reply is read before the next request is sent, so a notification
// that got a reply would show up as the wrong reply to the next request.
func runStdioSession(t *testing.T, c *rpcClient) {
	t.Helper()
	big := strings.Repeat("d", 300*1024) // 300 KiB, over bufio.Scanner's 64 KiB limit
	c.call(float64(1), "initialize", initParams("2025-11-25"))
	c.send(map[string]interface{}{"method": "notifications/initialized"})
	c.call(float64(2), "tools/list", nil)
	text, _ := toolText(t, c.call(float64(3), "tools/call", toolArgs("echo", map[string]interface{}{"data": big})))
	if !strings.HasPrefix(text, "got 307200 bytes;") || len(text) < largeResultBytes {
		t.Errorf("large request/response did not round-trip: %d bytes, prefix %.40q", len(text), text)
	}
	if m := c.call(float64(4), "no/such/method", nil); errorCode(m) != -32601 {
		t.Errorf("unknown method: %v; want -32601", m)
	}
	c.sendRaw("this is not json\n")
	line, m := c.recvLine()
	if errorCode(m) != -32700 {
		t.Errorf("garbage line: %v; want -32700", m)
	}
	assertIDNull(t, line)
	c.call(float64(5), "tools/list", nil)
}

func TestStdioOnlyProtocolReachesStdout(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	c, run := session(t, newTestHandler(fakeRegistry{resultBytes: largeResultBytes}, true))
	runStdioSession(t, c)
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.noMoreOutput()
	for _, want := range []string{"[STDIN]", "[STDOUT]", "calling tool echo", "Parse error"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log is missing %q", want)
		}
	}
}

func TestStdioLargeRequestDoesNotStopServer(t *testing.T) {
	// A request over 64 KiB used to end the loop (bufio.ErrTooLong), taking
	// the server down. Here the last request follows 4 MiB of padding.
	huge := strings.Repeat("h", 4<<20)
	c, run := session(t, newTestHandler(fakeRegistry{}, false))
	c.initialize()
	text, _ := toolText(t, c.call(float64(1), "tools/call", toolArgs("echo", map[string]interface{}{"data": huge})))
	if !strings.HasPrefix(text, fmt.Sprintf("got %d bytes;", len(huge))) {
		t.Errorf("tool saw a truncated argument: %.40q", text)
	}
	c.call(float64(2), "tools/list", nil)
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// failAfter accepts n writes, then fails every write.
type failAfter struct {
	mu sync.Mutex
	n  int
	w  io.Writer
}

func (f *failAfter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n <= 0 {
		return 0, errors.New("broken pipe")
	}
	f.n--
	return f.w.Write(p)
}

// callCountingRegistry counts tool calls.
type callCountingRegistry struct {
	fakeRegistry
	calls atomic.Int32
}

func (c *callCountingRegistry) CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	c.calls.Add(1)
	return c.fakeRegistry.CallTool(ctx, name, args)
}

func TestStdioWriteErrorStopsServer(t *testing.T) {
	// A broken stdout means no response can be delivered: Run must return the
	// error and stop reading, so no later request (which may have side
	// effects) is executed.
	call := map[string]interface{}{"id": 1, "method": "tools/call", "params": toolArgs("echo", nil)}
	for name, tc := range map[string]struct {
		failing   string // the line whose reply cannot be written
		wantCalls int32  // tools executed: only a call whose reply failed
	}{
		"response":       {rpcLine(t, call), 1},
		"parse error":    {"{bad\n", 0},
		"oversize reply": {strings.Repeat("z", 300) + "\n", 0},
	} {
		t.Run(name, func(t *testing.T) {
			reg := &callCountingRegistry{}
			h := newTestHandler(reg, false)
			h.maxLineBytes = 200
			// Only the initialize reply gets through.
			c, run := session(t, h, func(w io.Writer) io.Writer { return &failAfter{n: 1, w: w} })
			c.initialize()
			c.sendRaw(tc.failing)
			err := run.result()
			if err == nil || !strings.Contains(err.Error(), "broken pipe") || !errors.Is(err, errStdoutWrite) {
				t.Fatalf("Run = %v; want a stdout write error", err)
			}
			if c.sendRaw(rpcLine(t, call)) {
				t.Error("the server still reads its input after stdout failed")
			}
			if got := reg.calls.Load(); got != tc.wantCalls {
				t.Errorf("%d tool calls were executed; want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestStdioOversizedRequestIsRejected(t *testing.T) {
	// A line over the limit gets an error response, and the next request
	// is still served. (Uses a handler with a small limit.)
	h := newTestHandler(fakeRegistry{}, false)
	h.maxLineBytes = 50
	c, run := session(t, h)
	c.sendRaw(strings.Repeat("z", 100) + "\n")
	line, m := c.recvLine()
	if errorCode(m) != -32600 {
		t.Errorf("oversized line: %v; want -32600", m)
	}
	// JSON-RPC 2.0: an error reply to a request with an unknown id carries
	// "id": null, not a missing member.
	assertIDNull(t, line)
	c.call(float64(7), "ping", nil)
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestStdioParseErrorHasNullID(t *testing.T) {
	c, run := session(t, newTestHandler(fakeRegistry{}, false))
	c.sendRaw("{not json\n")
	line, m := c.recvLine()
	if errorCode(m) != -32700 {
		t.Errorf("response = %v; want -32700", m)
	}
	assertIDNull(t, line)
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.noMoreOutput()
}

// A message that is JSON but not a JSON-RPC message is an invalid request,
// answered with its id when it has a usable one.
func TestStdioInvalidRequest(t *testing.T) {
	c, run := session(t, newTestHandler(fakeRegistry{}, false))
	c.sendRaw(`{"jsonrpc":"1.0","id":5,"method":"ping"}` + "\n")
	if m := c.recv(); errorCode(m) != -32600 || m["id"] != float64(5) {
		t.Errorf("wrong version tag: %v; want -32600 with id 5", m)
	}
	c.sendRaw(`[{"jsonrpc":"2.0","id":6,"method":"ping"}]` + "\n")
	line, m := c.recvLine()
	if errorCode(m) != -32600 {
		t.Errorf("batch: %v; want -32600", m)
	}
	assertIDNull(t, line)
	c.call(float64(7), "ping", nil)
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestStdioResponseKeepsFalsyIDs(t *testing.T) {
	// ids 0 and "" are real ids and must be echoed, not turned into null.
	c, run := session(t, newTestHandler(fakeRegistry{}, false))
	c.initialize()
	c.call(float64(0), "tools/list", nil)
	c.call("", "tools/list", nil)
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestStdioRequestsWithoutIDAreNotifications(t *testing.T) {
	// Requests without an id (or with "id": null) are notifications: no
	// reply, and the tool must not run. The valid requests after them
	// (including ids 0 and "") are still served, and each is the next reply.
	call := toolArgs("echo", map[string]interface{}{"data": "x"})
	for _, debug := range []bool{false, true} {
		reg := &callCountingRegistry{}
		var logs syncBuffer
		log.SetOutput(&logs)
		c, run := session(t, newTestHandler(reg, debug))
		c.call(float64(9), "initialize", initParams("2025-11-25"))
		c.send(map[string]interface{}{"method": "initialize", "params": initParams("2025-11-25")})
		c.send(map[string]interface{}{"method": "tools/list"})
		c.send(map[string]interface{}{"method": "tools/call", "params": call})
		c.send(map[string]interface{}{"id": nil, "method": "tools/call", "params": call})
		c.send(map[string]interface{}{"method": "notifications/initialized"})
		c.send(map[string]interface{}{"method": "no/such/method"})
		c.call(float64(0), "tools/call", call)
		c.call("", "tools/list", nil)
		err := run.wait()
		log.SetOutput(os.Stderr)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		c.noMoreOutput()
		if got := reg.calls.Load(); got != 1 {
			t.Errorf("debug=%v: the tool ran %d times; want 1 (only the call with id 0)", debug, got)
		}
		if got := strings.Count(logs.String(), "sent without an id"); debug && got != 5 || !debug && got != 0 {
			t.Errorf("debug=%v: %d ignore log lines", debug, got)
		}
	}
}

// TestStdioHelperProcess is not a test: it is the child process of
// TestStdioDefaultLoggingGoesToStderr, which runs the real stdio loop on the
// real stdin and stdout with the logging setup main uses.
func TestStdioHelperProcess(t *testing.T) {
	if os.Getenv("TRUENAS_MCP_STDIO_HELPER") != "1" {
		t.Skip("helper process")
	}
	configureLogging()
	log.Println("log line before serving")
	h := NewStdioHandler(fakeRegistry{resultBytes: largeResultBytes}, true)
	if err := h.Run(context.Background()); err != nil {
		log.Printf("Run: %v", err)
		os.Exit(1)
	}
	log.Println("log line after serving")
	os.Exit(0)
}

func TestStdioDefaultLoggingGoesToStderr(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestStdioHelperProcess$")
	cmd.Env = append(os.Environ(), "TRUENAS_MCP_STDIO_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW := io.Pipe()
	var stdout, stderr syncBuffer
	cmd.Stdout, cmd.Stderr = io.MultiWriter(stdoutW, &stdout), &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := newRPCClient(t, stdin, stdoutR)
	runStdioSession(t, c)
	stdin.Close()
	err = cmd.Wait()
	stdoutW.Close()
	if err != nil {
		t.Fatalf("helper: %v\nstderr: %.2000s", err, stderr.String())
	}
	c.noMoreOutput()
	for _, want := range []string{"log line before serving", "log line after serving", "[STDIN]", "[STDOUT]", "calling tool echo"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr is missing %q", want)
		}
		if strings.Contains(stdout.String(), want) {
			t.Errorf("stdout contains log text %q", want)
		}
	}
}

// repeatReader yields n copies of b without holding them in memory.
type repeatReader struct {
	b byte
	n int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = r.b
	}
	r.n -= len(p)
	return len(p), nil
}

// sizedRequest streams one tools/list request line of exactly size bytes
// (excluding the newline), padded with an unknown member that the decoder
// skips, so the test does not allocate a second copy of the padding.
func sizedRequest(id, size int) io.Reader {
	prefix := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","pad":"`, id)
	const suffix = `"}`
	return io.MultiReader(
		strings.NewReader(prefix),
		&repeatReader{b: 'z', n: size - len(prefix) - len(suffix)},
		strings.NewReader(suffix+"\n"),
	)
}

// TestStdioRealLimitThroughRun uses the real 64 MiB limit: a request of
// exactly the limit is served, one byte more is rejected, and the server
// keeps going. It is the memory-heavy test: the line buffer grows to the
// limit (peak about 160 MiB, about 480 MiB under -race); -short skips it.
func TestStdioRealLimitThroughRun(t *testing.T) {
	if testing.Short() {
		t.Skip("streams about 128 MiB")
	}
	c, run := session(t, newTestHandler(fakeRegistry{}, false))
	c.initialize()
	if _, err := io.Copy(c.in, sizedRequest(1, mcp.MaxLineBytes)); err != nil {
		t.Fatal(err)
	}
	if m := c.recv(); m["id"] != float64(1) || m["error"] != nil || m["result"] == nil {
		t.Errorf("request of exactly the limit: %v", m)
	}
	if _, err := io.Copy(c.in, sizedRequest(2, mcp.MaxLineBytes+1)); err != nil {
		t.Fatal(err)
	}
	line, m := c.recvLine()
	if errorCode(m) != -32600 {
		t.Errorf("request one byte over the limit: %v", m)
	}
	assertIDNull(t, line)
	c.call(float64(3), "tools/list", nil)
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// ping is answered with an empty result object (MCP basic/utilities/ping),
// with or without params, before and after initialize; a ping without an
// id is a notification and gets no reply.
func TestStdioPing(t *testing.T) {
	c, run := session(t, newTestHandler(fakeRegistry{}, false))
	check := func(id interface{}, params interface{}) {
		t.Helper()
		req := map[string]interface{}{"id": id, "method": "ping"}
		if params != nil {
			req["params"] = params
		}
		c.send(req)
		line, m := c.recvLine()
		if m["id"] != id {
			t.Errorf("ping %v: reply id %v", id, m["id"])
		}
		if res, ok := m["result"].(map[string]interface{}); !ok || len(res) != 0 {
			t.Errorf("ping %v: result = %#v, want {}", id, m["result"])
		}
		if !strings.Contains(string(line), `"result":{}`) {
			t.Errorf("ping %v: wire response %s, want \"result\":{}", id, line)
		}
	}
	check(float64(1), nil)
	c.send(map[string]interface{}{"method": "ping"})
	c.call(float64(2), "initialize", initParams("2025-11-25"))
	check("p", map[string]interface{}{})
	check(float64(0), map[string]interface{}{"_meta": map[string]interface{}{"k": "v"}})
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.noMoreOutput()
}

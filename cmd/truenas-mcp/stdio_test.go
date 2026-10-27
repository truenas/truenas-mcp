package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
)

// fakeRegistry echoes the size of its "data" argument and returns a large
// result, and logs from inside the tool as real tools do.
type fakeRegistry struct{ resultBytes int }

func (f fakeRegistry) ListTools() []mcp.Tool {
	return []mcp.Tool{{Name: "echo", Description: "echo", InputSchema: map[string]interface{}{"type": "object"}}}
}

func (f fakeRegistry) CallTool(name string, args map[string]interface{}) (string, error) {
	log.Printf("calling tool %s", name)
	data, _ := args["data"].(string)
	return fmt.Sprintf("got %d bytes;%s", len(data), strings.Repeat("r", f.resultBytes)), nil
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

// stdioSession is a request stream covering every response path: requests,
// notifications, a large request and result, an unknown method and garbage.
func stdioSession(t *testing.T) (input string, wantIDs []float64) {
	t.Helper()
	big := strings.Repeat("d", 300*1024) // 300 KiB, over bufio.Scanner's 64 KiB limit
	input += rpcLine(t, map[string]interface{}{"id": 1, "method": "initialize"})
	input += rpcLine(t, map[string]interface{}{"method": "notifications/initialized"})
	input += rpcLine(t, map[string]interface{}{"id": 2, "method": "tools/list"})
	input += rpcLine(t, map[string]interface{}{"id": 3, "method": "tools/call", "params": map[string]interface{}{
		"name": "echo", "arguments": map[string]interface{}{"data": big}}})
	input += rpcLine(t, map[string]interface{}{"id": 4, "method": "no/such/method"})
	input += "this is not json\n"
	input += rpcLine(t, map[string]interface{}{"id": 5, "method": "tools/list"})
	return input, []float64{1, 2, 3, 4, 5}
}

// checkOnlyJSONRPC asserts every line of stdout is a JSON-RPC 2.0 response
// and returns them.
func checkOnlyJSONRPC(t *testing.T, stdout []byte) []map[string]interface{} {
	t.Helper()
	var msgs []map[string]interface{}
	for i, line := range strings.Split(strings.TrimSuffix(string(stdout), "\n"), "\n") {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout line %d is not JSON (%v): %.200q", i+1, err, line)
		}
		if m["jsonrpc"] != "2.0" {
			t.Fatalf("stdout line %d is not JSON-RPC 2.0: %.200q", i+1, line)
		}
		_, hasResult := m["result"]
		_, hasError := m["error"]
		if hasResult == hasError {
			t.Fatalf("stdout line %d must have exactly one of result and error: %.200q", i+1, line)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func checkSession(t *testing.T, msgs []map[string]interface{}, wantIDs []float64) {
	t.Helper()
	// Responses to 1-5, plus the parse error for the garbage line (id null).
	if len(msgs) != len(wantIDs)+1 {
		t.Fatalf("got %d responses, want %d", len(msgs), len(wantIDs)+1)
	}
	byID := map[float64]map[string]interface{}{}
	for _, m := range msgs {
		if id, ok := m["id"].(float64); ok {
			byID[id] = m
		} else if e, _ := m["error"].(map[string]interface{}); e == nil || e["code"] != float64(-32700) {
			t.Errorf("response without id should be a parse error: %v", m)
		}
	}
	for _, id := range wantIDs {
		if byID[id] == nil {
			t.Errorf("no response for id %v", id)
		}
	}
	result := byID[3]["result"].(map[string]interface{})
	text := result["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	if !strings.HasPrefix(text, "got 307200 bytes;") || len(text) < 256<<10 {
		t.Errorf("large request/response did not round-trip: %d bytes, prefix %.40q", len(text), text)
	}
}

func TestStdioOnlyProtocolReachesStdout(t *testing.T) {
	input, wantIDs := stdioSession(t)
	var stdout bytes.Buffer
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	h := newStdioHandler(fakeRegistry{resultBytes: 256 << 10}, true, strings.NewReader(input), &stdout)
	if err := h.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	checkSession(t, checkOnlyJSONRPC(t, stdout.Bytes()), wantIDs)
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
	input := rpcLine(t, map[string]interface{}{"id": 1, "method": "tools/call", "params": map[string]interface{}{
		"name": "echo", "arguments": map[string]interface{}{"data": huge}}}) +
		rpcLine(t, map[string]interface{}{"id": 2, "method": "tools/list"})
	var stdout bytes.Buffer
	if err := newStdioHandler(fakeRegistry{}, false, strings.NewReader(input), &stdout).Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := checkOnlyJSONRPC(t, stdout.Bytes())
	if len(msgs) != 2 || msgs[0]["id"] != float64(1) || msgs[1]["id"] != float64(2) {
		t.Fatalf("responses = %d, first %v", len(msgs), msgs[0]["id"])
	}
	text := msgs[0]["result"].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	if !strings.HasPrefix(text, fmt.Sprintf("got %d bytes;", len(huge))) {
		t.Errorf("tool saw a truncated argument: %.40q", text)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestStdioWriteErrorIsReported(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	h := newStdioHandler(fakeRegistry{}, false, strings.NewReader(rpcLine(t, map[string]interface{}{"id": 1, "method": "tools/list"})), failWriter{})
	if err := h.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(logs.String(), "broken pipe") {
		t.Errorf("write failure was not logged: %q", logs.String())
	}
}

func TestStdioOversizedRequestIsRejected(t *testing.T) {
	// A line over the limit gets an error response, and the next request
	// is still served. (Uses a handler with a small limit.)
	in := strings.Repeat("z", 100) + "\n" + rpcLine(t, map[string]interface{}{"id": 7, "method": "tools/list"})
	var stdout bytes.Buffer
	h := newStdioHandler(fakeRegistry{}, false, strings.NewReader(in), &stdout)
	h.stdin = mcp.NewLineReader(strings.NewReader(in), 50)
	if err := h.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := checkOnlyJSONRPC(t, stdout.Bytes())
	if len(msgs) != 2 {
		t.Fatalf("got %d responses", len(msgs))
	}
	if e, _ := msgs[0]["error"].(map[string]interface{}); e == nil || e["code"] != float64(-32600) {
		t.Errorf("first response = %v; want -32600", msgs[0])
	}
	if msgs[1]["id"] != float64(7) {
		t.Errorf("second response = %v", msgs[1])
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
	h := NewStdioHandler(fakeRegistry{resultBytes: 256 << 10}, true)
	if err := h.Run(); err != nil {
		log.Printf("Run: %v", err)
		os.Exit(1)
	}
	log.Println("log line after serving")
	os.Exit(0)
}

func TestStdioDefaultLoggingGoesToStderr(t *testing.T) {
	input, wantIDs := stdioSession(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestStdioHelperProcess$")
	cmd.Env = append(os.Environ(), "TRUENAS_MCP_STDIO_HELPER=1")
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v\nstderr: %.2000s", err, stderr.String())
	}
	checkSession(t, checkOnlyJSONRPC(t, stdout.Bytes()), wantIDs)
	for _, want := range []string{"log line before serving", "log line after serving", "[STDIN]", "[STDOUT]", "calling tool echo"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr is missing %q", want)
		}
		if strings.Contains(stdout.String(), want) {
			t.Errorf("stdout contains log text %q", want)
		}
	}
}

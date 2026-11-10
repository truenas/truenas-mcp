package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
)

// secretRegistry returns tool output and errors that carry fake secrets.
type secretRegistry struct {
	result string
	err    error
}

func (r secretRegistry) ListTools() []mcp.Tool {
	return []mcp.Tool{{Name: "query_directory_services", Description: "fake", InputSchema: map[string]interface{}{"type": "object"}}}
}

func (r secretRegistry) CallTool(context.Context, string, map[string]interface{}) (string, error) {
	return r.result, r.err
}

// callText calls the tool over the wire and returns the reply's text, so
// redaction is checked on what actually reaches stdout.
func callText(t *testing.T, reg mcp.ToolRegistry) (string, bool) {
	t.Helper()
	c, run := session(t, newTestHandler(reg, false))
	c.initialize()
	text, isError := toolText(t, c.call(float64(1), "tools/call", toolArgs("query_directory_services", nil)))
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return text, isError
}

func TestToolsCallRedactsResult(t *testing.T) {
	text, _ := callText(t, secretRegistry{result: "{\n  \"bindpw\": \"fake-bind-pw\",\n  \"binddn\": \"cn=admin\"\n}"})
	if strings.Contains(text, "fake-bind-pw") || !strings.Contains(text, redact.Marker) || !strings.Contains(text, "cn=admin") {
		t.Errorf("result not redacted as expected:\n%s", text)
	}
}

func TestToolsCallRedactsError(t *testing.T) {
	err := errors.New("API error: [EINVAL] {'bindpw': 'fake-bind-pw'}\n-----BEGIN PRIVATE KEY-----\nFAKEFAKE\n-----END PRIVATE KEY-----")
	text, isError := callText(t, secretRegistry{err: err})
	if !isError || strings.Contains(text, "fake-bind-pw") || strings.Contains(text, "FAKEFAKE") || !strings.HasPrefix(text, "Error: API error") {
		t.Errorf("error not redacted as expected:\n%s", text)
	}
}

// TestStdioRedactsUnknownToolAndMethodNames covers JSON-RPC errors the SDK
// builds itself: an unknown tool or method name is echoed in the error
// message, so a name that looks like a secret must be redacted on the way
// out. The reply stays a protocol error with the request id; the secret
// never appears anywhere on stdout.
func TestStdioRedactsUnknownToolAndMethodNames(t *testing.T) {
	const secret = "s3cret"
	var stdout syncBuffer
	c, run := session(t, newTestHandler(fakeRegistry{}, false), func(w io.Writer) io.Writer {
		return io.MultiWriter(w, &stdout)
	})
	c.initialize()

	const toolID = float64(7)
	toolReply := c.call(toolID, "tools/call", toolArgs("password="+secret, nil))
	if errorCode(toolReply) != -32602 {
		t.Errorf("unknown tool: %v; want -32602", toolReply)
	}
	if toolReply["id"] != toolID {
		t.Errorf("unknown tool reply id = %v; want %v", toolReply["id"], toolID)
	}

	const methodID = float64(8)
	methodReply := c.call(methodID, "password="+secret, nil)
	if methodReply["error"] == nil {
		t.Errorf("unknown method: %v; want an error", methodReply)
	}
	if methodReply["id"] != methodID {
		t.Errorf("unknown method reply id = %v; want %v", methodReply["id"], methodID)
	}

	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.noMoreOutput()
	out := stdout.String()
	if strings.Contains(out, secret) {
		t.Errorf("stdout leaked %q:\n%s", secret, out)
	}
	if !strings.Contains(out, redact.Marker) {
		t.Errorf("stdout has no redaction marker:\n%s", out)
	}
}

// TestWriteErrorRedactsMessage covers error replies the transport builds
// itself (bad JSON, oversized lines): the message is redacted before it
// is written, the JSON-RPC code and id are kept, and the secret never
// appears on stdout.
func TestWriteErrorRedactsMessage(t *testing.T) {
	const secret = "s3cret"
	var out bytes.Buffer
	c := &lineConn{out: &out}
	if err := c.writeError(float64(1), jsonrpc.CodeParseError, "Parse error: password="+secret); err != nil {
		t.Fatalf("writeError: %v", err)
	}
	got := out.String()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &m); err != nil {
		t.Fatalf("reply is not JSON: %v\n%s", err, got)
	}
	if errorCode(m) != -32700 {
		t.Errorf("writeError: %v; want -32700", m)
	}
	if m["id"] != float64(1) {
		t.Errorf("writeError id = %v; want 1", m["id"])
	}
	if strings.Contains(got, secret) {
		t.Errorf("stdout leaked %q:\n%s", secret, got)
	}
	if !strings.Contains(got, redact.Marker) {
		t.Errorf("stdout has no redaction marker:\n%s", got)
	}
}

func TestRedactErrorData(t *testing.T) {
	const secret = "s3cret"
	got := string(redactErrorData(json.RawMessage(`"password=` + secret + `"`)))
	if strings.Contains(got, secret) || !strings.Contains(got, redact.Marker) {
		t.Errorf("string data not redacted: %s", got)
	}
	got = string(redactErrorData(json.RawMessage(`{"password":"` + secret + `"}`)))
	if strings.Contains(got, secret) || !strings.Contains(got, redact.Marker) {
		t.Errorf("object data not redacted: %s", got)
	}
}

// TestDebugLogsRedactMethodName covers the debug-only stderr line that
// logs the method being handled: a client-supplied name that looks like
// a secret must be redacted, and the secret must not appear on stderr.
func TestDebugLogsRedactMethodName(t *testing.T) {
	const secret = "s3cret"
	var logs syncBuffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	c, run := session(t, newTestHandler(fakeRegistry{}, true))
	c.initialize()
	if m := c.call(float64(1), "tools/call", toolArgs("password="+secret, nil)); errorCode(m) != -32602 {
		t.Errorf("unknown tool: %v; want -32602", m)
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "Handling method:") {
		t.Errorf("debug log is missing the Handling method line:\n%s", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("debug log leaked %q:\n%s", secret, out)
	}
	if !strings.Contains(out, redact.Marker) {
		t.Errorf("debug log has no redaction marker:\n%s", out)
	}
}

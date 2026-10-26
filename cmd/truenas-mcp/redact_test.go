package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
)

// secretRegistry returns tool output and errors that carry fake secrets.
type secretRegistry struct {
	result string
	err    error
}

func (r secretRegistry) ListTools() []mcp.Tool { return nil }

func (r secretRegistry) CallTool(string, map[string]interface{}) (string, error) {
	return r.result, r.err
}

func callText(t *testing.T, reg mcp.ToolRegistry) mcp.ToolCallResult {
	t.Helper()
	h := NewStdioHandler(reg, false)
	resp := h.handleToolsCall(&mcp.Request{JSONRPC: "2.0", ID: 1, Method: "tools/call",
		Params: map[string]interface{}{"name": "query_directory_services"}})
	res, ok := resp.Result.(mcp.ToolCallResult)
	if !ok || len(res.Content) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	return res
}

func TestToolsCallRedactsResult(t *testing.T) {
	res := callText(t, secretRegistry{result: "{\n  \"bindpw\": \"fake-bind-pw\",\n  \"binddn\": \"cn=admin\"\n}"})
	text := res.Content[0].Text
	if strings.Contains(text, "fake-bind-pw") || !strings.Contains(text, redact.Marker) || !strings.Contains(text, "cn=admin") {
		t.Errorf("result not redacted as expected:\n%s", text)
	}
}

func TestToolsCallRedactsError(t *testing.T) {
	err := errors.New("API error: [EINVAL] {'bindpw': 'fake-bind-pw'}\n-----BEGIN PRIVATE KEY-----\nFAKEFAKE\n-----END PRIVATE KEY-----")
	res := callText(t, secretRegistry{err: err})
	text := res.Content[0].Text
	if !res.IsError || strings.Contains(text, "fake-bind-pw") || strings.Contains(text, "FAKEFAKE") || !strings.HasPrefix(text, "Error: API error") {
		t.Errorf("error not redacted as expected:\n%s", text)
	}
}

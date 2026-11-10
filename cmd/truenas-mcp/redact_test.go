package main

import (
	"context"
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

package main

import (
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/tools"
)

// An unknown argument comes back as an MCP tool error (isError), not a
// protocol error, so the model sees it and can correct the call.
func TestToolsCallRejectsUnknownArgument(t *testing.T) {
	c, run := session(t, newTestHandler(tools.NewRegistry(nil, nil), false))
	c.initialize()
	m := c.call(float64(1), "tools/call", toolArgs("query_pools", map[string]interface{}{"padding": "fake-value"}))
	if m["error"] != nil {
		t.Fatalf("protocol error: %v", m["error"])
	}
	text, isError := toolText(t, m)
	if !isError || !strings.Contains(text, `unknown argument "padding"`) || strings.Contains(text, "fake-value") {
		t.Errorf("text = %s (isError %v)", text, isError)
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

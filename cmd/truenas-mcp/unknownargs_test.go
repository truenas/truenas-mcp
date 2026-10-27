package main

import (
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tools"
)

// An unknown argument comes back as an MCP tool error (isError), not a
// protocol error, so the model sees it and can correct the call.
func TestToolsCallRejectsUnknownArgument(t *testing.T) {
	h := NewStdioHandler(tools.NewRegistry(nil, nil), false)
	resp := h.handleToolsCall(&mcp.Request{JSONRPC: "2.0", ID: 1, Method: "tools/call",
		Params: map[string]interface{}{"name": "query_pools", "arguments": map[string]interface{}{"padding": "fake-value"}}})
	if resp.Error != nil {
		t.Fatalf("protocol error: %+v", resp.Error)
	}
	res, ok := resp.Result.(mcp.ToolCallResult)
	if !ok || len(res.Content) != 1 || !res.IsError {
		t.Fatalf("unexpected response: %+v", resp)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, `unknown argument "padding"`) || strings.Contains(text, "fake-value") {
		t.Errorf("text = %s", text)
	}
}

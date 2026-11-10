package mcp

import "context"

// Tool is a tool definition as listed to MCP clients.
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
	Annotations *ToolAnnotations       `json:"annotations,omitempty"`
}

// ToolAnnotations are behavioural hints for clients (MCP 2025-03-26+).
// All fields are always sent: destructiveHint defaults to true in the spec.
type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

// ToolRegistry lists and runs the tools the server exposes. CallTool may be
// called concurrently; ctx is cancelled when the client cancels the call or
// the session ends.
type ToolRegistry interface {
	ListTools() []Tool
	CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error)
}

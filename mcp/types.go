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

// ReplyError is a tool error whose result content is Reply, a JSON object,
// instead of the error text. A refused destructive call uses it to return
// the reason together with a fresh preview (and, when the operation can
// proceed, a fresh confirmation token). Error returns the reason only.
type ReplyError struct {
	Message string
	Reply   string
}

func (e *ReplyError) Error() string { return e.Message }

// ConfirmationTokens is implemented by registries that issue confirmation
// tokens. IsConfirmationToken reports whether tok is a token the registry
// issued that is still live; the server lets only such a value through
// redaction, as the top-level confirmation_token of a tool result.
type ConfirmationTokens interface {
	IsConfirmationToken(tok string) bool
}

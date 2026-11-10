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

// Elicitation is the MCP elicitation state for one tools/call. The server
// fills it from the client's declared capabilities and, on a retry, from
// the elicitation response and echoed request state.
type Elicitation struct {
	// SupportsForm is true when the client declared the form elicitation
	// capability (explicitly, or as the default when elicitation is
	// declared without a mode).
	SupportsForm bool
	// RequestState is the opaque state echoed from a previous input-required
	// result. It is not a confirmation_token and cannot be used as one.
	RequestState string
	// HasResponse is true when this call is a retry carrying an
	// elicitation response.
	HasResponse bool
	// Action is the elicitation result action: "accept", "decline", or
	// "cancel". Empty when HasResponse is false.
	Action string
	// Approved is the required boolean field, and is only meaningful when
	// Action is "accept".
	Approved bool
	// ResponseErr, when set, means the elicitation response could not be
	// read (unexpected shape or an elicitation error). Nothing is executed.
	ResponseErr error
}

// ElicitPrompt is returned when a destructive tool should ask the user to
// confirm through MCP elicitation. It is not an error and does not execute
// anything. The server turns it into an InputRequests result; Content must
// stay empty on that result.
type ElicitPrompt struct {
	// Message is a short, already-redacted summary of the preview (tool,
	// target, main warnings) shown to the user.
	Message string
	// State is an opaque handle the client must echo as RequestState. It
	// is not a confirmation_token.
	State string
}

// ToolCallerElicit is implemented by registries that can use MCP
// elicitation as the primary confirmation path for destructive tools.
// CallToolElicit has the same validation and policy as CallTool. When
// prompt is non-nil, the call executed nothing and the server should
// return the prompt as an input request. Clients without elicitation, and
// callers of CallTool, keep the confirmation-token flow.
type ToolCallerElicit interface {
	CallToolElicit(ctx context.Context, name string, args map[string]interface{}, e Elicitation) (result string, prompt *ElicitPrompt, err error)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/tasks"
)

const (
	// maxConcurrentToolCalls bounds the tool calls running at once; further
	// calls wait for a slot. A call abandoned after cancellation or its
	// deadline keeps its slot until its handler returns.
	maxConcurrentToolCalls = 8
	// toolCallTimeout is the overall deadline of one tool call, including
	// the wait for a slot. It is above what the slowest tools need: a few
	// sequential middleware calls of up to 120 s each.
	toolCallTimeout = 10 * time.Minute
	// maxToolOutputBytes is the cap on a tool result or error sent to the
	// MCP client, in bytes (100 KiB). Larger output is redacted first,
	// then cut on a rune boundary, and a notice is appended so the total
	// stays within the cap.
	maxToolOutputBytes = 100 * 1024
	// toolOutputTruncationNotice is appended when a redacted tool result
	// or error exceeds maxToolOutputBytes. It tells the model the output
	// was cut and how to narrow the query.
	toolOutputTruncationNotice = "\n\n[truncated] Output exceeded 100 KiB and was cut. Narrow the query with limit, select, or filters and call again."
)

// StdioHandler serves MCP over newline-delimited JSON-RPC on stdin/stdout.
type StdioHandler struct {
	registry     mcp.ToolRegistry
	debug        bool
	in           io.Reader
	out          io.Writer
	maxLineBytes int
	callTimeout  time.Duration
	callSlots    chan struct{}
}

// NewStdioHandler serves the protocol on the process's stdin and stdout.
func NewStdioHandler(registry mcp.ToolRegistry, debug bool) *StdioHandler {
	return newStdioHandler(registry, debug, os.Stdin, os.Stdout)
}

func newStdioHandler(registry mcp.ToolRegistry, debug bool, in io.Reader, out io.Writer) *StdioHandler {
	return &StdioHandler{
		registry:     registry,
		debug:        debug,
		in:           in,
		out:          out,
		maxLineBytes: mcp.MaxLineBytes,
		callTimeout:  toolCallTimeout,
		callSlots:    make(chan struct{}, maxConcurrentToolCalls),
	}
}

// Run serves one session until the input ends or stdout fails. Requests
// are handled concurrently. When the input ends, calls still in flight are
// cancelled and get no reply.
func (h *StdioHandler) Run(ctx context.Context) error {
	if h.debug {
		log.Println("Starting stdio handler...")
	}
	return h.newServer().Run(ctx, &lineTransport{in: h.in, out: h.out, maxLineBytes: h.maxLineBytes, debug: h.debug})
}

// toolChecker is implemented by registries that know tools the policy
// hides from ListTools (tools.Registry).
type toolChecker interface {
	HasTool(name string) bool
}

func (h *StdioHandler) newServer() *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "truenas-mcp", Version: Version}, &sdk.ServerOptions{
		// Only tools, without list-change notifications: the tool list is
		// fixed at startup. This also drops the SDK's default logging
		// capability, which the server does not implement.
		Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
	})
	listed := map[string]bool{}
	for _, t := range h.registry.ListTools() {
		listed[t.Name] = true
		server.AddTool(sdkTool(t), func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return h.callTool(ctx, req.Params)
		})
	}
	// A tool the policy hides is not listed, so the SDK would answer a call
	// to it as an unknown tool. Pass such calls to the registry instead,
	// which refuses them with the reason (read-only mode, allow or deny
	// list) as a tool error, as it did before. Names the registry does not
	// know still get the SDK's -32602.
	checker, _ := h.registry.(toolChecker)
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "tools/call" && checker != nil {
				if p, ok := req.GetParams().(*sdk.CallToolParamsRaw); ok && !listed[p.Name] && checker.HasTool(p.Name) {
					return h.callTool(ctx, p)
				}
			}
			return next(ctx, method, req)
		}
	})
	return server
}

// sdkTool converts a tool definition. The schema is passed through as is,
// and all four annotations are always sent.
func sdkTool(t mcp.Tool) *sdk.Tool {
	st := &sdk.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	if a := t.Annotations; a != nil {
		destructive, openWorld := a.DestructiveHint, a.OpenWorldHint
		st.Annotations = &sdk.ToolAnnotations{
			ReadOnlyHint:    a.ReadOnlyHint,
			DestructiveHint: &destructive,
			IdempotentHint:  a.IdempotentHint,
			OpenWorldHint:   &openWorld,
		}
	}
	return st
}

func (h *StdioHandler) callTool(ctx context.Context, p *sdk.CallToolParamsRaw) (*sdk.CallToolResult, error) {
	var args map[string]interface{}
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "Invalid params: arguments must be an object"}
		}
	}
	return h.runTool(ctx, p.Name, args)
}

// runTool runs a tool under the concurrency limit and the call deadline.
// It returns as soon as ctx is cancelled (notifications/cancelled, or the
// session ending), even if the handler is still running.
func (h *StdioHandler) runTool(ctx context.Context, name string, args map[string]interface{}) (*sdk.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, h.callTimeout)
	defer cancel()

	select {
	case h.callSlots <- struct{}{}:
	case <-ctx.Done():
		return h.interrupted(ctx, name, false)
	}
	type outcome struct {
		result string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() { <-h.callSlots }()
		result, err := h.registry.CallTool(ctx, name, args)
		done <- outcome{result, err}
	}()

	select {
	case o := <-done:
		return toolResult(o.result, o.err), nil
	case <-ctx.Done():
		return h.interrupted(ctx, name, true)
	}
}

// interrupted ends a call whose ctx is done. A cancelled call returns the
// cancellation as an error (the SDK sends no tool result for it); a call
// past its deadline gets a tool error, saying whether it had started.
func (h *StdioHandler) interrupted(ctx context.Context, name string, started bool) (*sdk.CallToolResult, error) {
	if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		return nil, ctx.Err()
	}
	if !started {
		return toolResult("", fmt.Errorf("tool %s was not run: too many tool calls were in progress for %s; retry later", name, h.callTimeout)), nil
	}
	return toolResult("", fmt.Errorf("tool %s did not finish within %s; it may still complete on TrueNAS, so check the system state before retrying", name, h.callTimeout)), nil
}

// toolResult builds the tools/call result. Every result and error is
// redacted before it reaches the client: TrueNAS returns secrets
// (passwords, keys, tokens) in many query results. The redacted text is
// then cut to maxToolOutputBytes so a secret cannot survive as a
// fragment at the cut.
func toolResult(result string, err error) *sdk.CallToolResult {
	if err != nil {
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: capToolOutput("Error: " + redact.Text(err.Error()))}},
			IsError: true,
		}
	}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: capToolOutput(redact.ToolOutput(result))}}}
}

// capToolOutput cuts already-redacted text to at most maxToolOutputBytes.
// When s is larger, it is cut on a rune boundary and the truncation
// notice is appended; the notice is included in the cap.
func capToolOutput(s string) string {
	if len(s) <= maxToolOutputBytes {
		return s
	}
	budget := maxToolOutputBytes - len(toolOutputTruncationNotice)
	if budget < 0 {
		budget = 0
	}
	cut, _ := tasks.TruncateText(s, budget)
	return cut + toolOutputTruncationNotice
}

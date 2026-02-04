package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/truenas/truenas-mcp/mcp"
)

// StdioHandler manages stdin/stdout communication
type StdioHandler struct {
	stdin       *bufio.Scanner
	stdoutMutex sync.Mutex
	debug       bool
}

// NewStdioHandler creates a new stdio handler
func NewStdioHandler(debug bool) *StdioHandler {
	return &StdioHandler{
		stdin: bufio.NewScanner(os.Stdin),
		debug: debug,
	}
}

// ReadRequest reads a JSON-RPC request from stdin
func (h *StdioHandler) ReadRequest() (*mcp.Request, error) {
	if !h.stdin.Scan() {
		if err := h.stdin.Err(); err != nil {
			return nil, fmt.Errorf("stdin read error: %w", err)
		}
		return nil, io.EOF
	}

	line := h.stdin.Bytes()
	if h.debug {
		log.Printf("[STDIN] %s", string(line))
	}

	var req mcp.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return nil, fmt.Errorf("failed to parse JSON-RPC request: %w", err)
	}

	return &req, nil
}

// WriteResponse writes a JSON-RPC response to stdout
func (h *StdioHandler) WriteResponse(resp *mcp.Response) error {
	h.stdoutMutex.Lock()
	defer h.stdoutMutex.Unlock()

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}

	if h.debug {
		log.Printf("[STDOUT] %s", string(data))
	}

	_, err = fmt.Fprintf(os.Stdout, "%s\n", data)
	if err != nil {
		return fmt.Errorf("failed to write to stdout: %w", err)
	}

	return nil
}

// WriteError writes a JSON-RPC error response to stdout
func (h *StdioHandler) WriteError(id interface{}, code int, message string) error {
	resp := &mcp.Response{
		JSONRPC: "2.0",
		ID:      id,
		Error: &mcp.Error{
			Code:    code,
			Message: message,
		},
	}
	return h.WriteResponse(resp)
}

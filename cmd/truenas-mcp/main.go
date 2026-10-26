package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/tools"
	"github.com/truenas/truenas-mcp/truenas"
)

var (
	truenasURL = flag.String("truenas-url", "", "TrueNAS hostname or WebSocket URL (e.g., 'truenas.local' or 'wss://10.0.0.1/api/current')")
	apiUser    = flag.String("api-user", "", "User that owns the API key, sent to auth.login_ex (required; env TRUENAS_API_USER)")
	apiKey     = flag.String("api-key", "", "TrueNAS API key for middleware authentication")
	insecure   = flag.Bool("insecure", false, "Disable TLS certificate verification (UNSAFE: allows man-in-the-middle attacks)")
	tlsCA      = flag.String("tls-ca", "", "Path to a PEM certificate to trust (e.g., the TrueNAS self-signed certificate)")
	versionFlg = flag.Bool("version", false, "Print version and exit")
	debug      = flag.Bool("debug", false, "Enable debug logging")
)

// Version is the release version, injected at build time via
// -ldflags "-X main.Version=...". Builds without injection report "dev".
var Version = "dev"

// checkRequired reports any required setting that is missing.
func checkRequired(truenasURL, apiKey, apiUser string) error {
	var missing []string
	if truenasURL == "" {
		missing = append(missing, "--truenas-url (TRUENAS_URL)")
	}
	if apiKey == "" {
		missing = append(missing, "--api-key (TRUENAS_API_KEY)")
	}
	if apiUser == "" {
		missing = append(missing, "--api-user (TRUENAS_API_USER, the user that owns the API key)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	return nil
}

func main() {
	flag.Parse()

	if *versionFlg {
		fmt.Printf("truenas-mcp version %s\n", Version)
		os.Exit(0)
	}

	// Get configuration from flags or environment variables
	if *truenasURL == "" {
		*truenasURL = os.Getenv("TRUENAS_URL")
	}
	if *apiKey == "" {
		*apiKey = os.Getenv("TRUENAS_API_KEY")
	}
	if *apiUser == "" {
		*apiUser = os.Getenv("TRUENAS_API_USER")
	}

	if err := checkRequired(*truenasURL, *apiKey, *apiUser); err != nil {
		log.Fatal(err)
	}

	// TRUENAS_MCP_DEBUG=1 is equivalent to --debug; either enables verbose
	// (credential-redacted) request/response logging everywhere
	if truenas.DebugLogging() {
		*debug = true
	}
	truenas.SetDebugLogging(*debug)

	if *tlsCA == "" {
		*tlsCA = os.Getenv("TRUENAS_TLS_CA")
	}
	if !*insecure {
		switch strings.ToLower(os.Getenv("TRUENAS_INSECURE")) {
		case "", "0", "false", "no", "off":
		default:
			*insecure = true
		}
	}

	// Configure TLS: certificates are verified by default. TrueNAS ships a
	// self-signed certificate, so users must either trust it via --tls-ca
	// or explicitly opt out of verification with --insecure.
	tlsConfig, err := truenas.NewTLSConfig(*insecure, *tlsCA)
	if err != nil {
		log.Fatalf("Failed to configure TLS: %v", err)
	}
	if *insecure {
		log.Println("WARNING: TLS certificate verification disabled - the connection is vulnerable to man-in-the-middle attacks; prefer --tls-ca with the server's certificate")
	}

	// Create TrueNAS client
	client, err := truenas.NewClientWithOptions(*truenasURL, *apiKey, tlsConfig, truenas.Options{
		APIUser: *apiUser,
	})
	if err != nil {
		log.Fatalf("Failed to create TrueNAS client: %v", err)
	}
	defer client.Close()

	// Authenticate with TrueNAS middleware
	if err := client.Authenticate(); err != nil {
		log.Fatalf("Failed to authenticate with TrueNAS: %v", err)
	}
	log.Println("Successfully authenticated with TrueNAS middleware")

	// Create task manager
	taskConfig := tasks.PollerConfig{
		PollInterval:    5 * time.Second,
		MaxPollAttempts: 0, // Unlimited
		CleanupInterval: 1 * time.Minute,
	}
	taskManager := tasks.NewManager(client, taskConfig)
	taskManager.Start()
	defer taskManager.Shutdown()

	// Create tool registry
	registry := tools.NewRegistry(client, taskManager)

	// Start stdio handler
	handler := NewStdioHandler(registry, *debug)
	if err := handler.Run(); err != nil {
		log.Fatalf("Stdio handler error: %v", err)
	}
}

// StdioHandler manages stdio communication for MCP protocol
type StdioHandler struct {
	registry    mcp.ToolRegistry
	stdin       *bufio.Scanner
	stdoutMutex sync.Mutex
	debug       bool
}

func NewStdioHandler(registry mcp.ToolRegistry, debug bool) *StdioHandler {
	return &StdioHandler{
		registry: registry,
		stdin:    bufio.NewScanner(os.Stdin),
		debug:    debug,
	}
}

func (h *StdioHandler) Run() error {
	if h.debug {
		log.Println("Starting stdio handler...")
	}

	for h.stdin.Scan() {
		line := h.stdin.Bytes()
		if h.debug {
			log.Printf("[STDIN] %s", truenas.RedactJSONForLog(line))
		}

		var req mcp.Request
		if err := json.Unmarshal(line, &req); err != nil {
			if h.debug {
				log.Printf("Parse error: %v", err)
			}
			h.sendError(nil, -32700, fmt.Sprintf("Parse error: %v", err))
			continue
		}

		if h.debug {
			log.Printf("Handling method: %s (id: %v)", req.Method, req.ID)
		}

		resp := h.handleRequest(&req)
		// Only send response if not nil (notifications don't get responses)
		if resp != nil {
			if err := h.sendResponse(resp); err != nil {
				log.Printf("Failed to send response: %v", err)
			}
		}
	}

	if err := h.stdin.Err(); err != nil {
		return fmt.Errorf("stdin error: %w", err)
	}

	return nil
}

func (h *StdioHandler) handleRequest(req *mcp.Request) *mcp.Response {
	switch req.Method {
	case "initialize":
		return h.handleInitialize(req)
	case "notifications/initialized":
		// This is a notification from the client after initialization
		// Notifications don't require a response
		return nil
	case "tools/list":
		return h.handleToolsList(req)
	case "tools/call":
		return h.handleToolsCall(req)
	default:
		// Only return error if this is a request (has an ID)
		if req.ID != nil {
			return h.createErrorResponse(req.ID, -32601, "Method not found")
		}
		// For notifications, no response needed
		return nil
	}
}

func (h *StdioHandler) handleInitialize(req *mcp.Request) *mcp.Response {
	result := mcp.InitializeResult{
		ProtocolVersion: "2024-11-05",
		ServerInfo: mcp.ServerInfo{
			Name:    "truenas-mcp",
			Version: Version,
		},
		Capabilities: mcp.Capabilities{
			Tools: map[string]interface{}{
				"listChanged": false,
			},
		},
	}

	return &mcp.Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
}

func (h *StdioHandler) handleToolsList(req *mcp.Request) *mcp.Response {
	tools := h.registry.ListTools()
	result := mcp.ToolsListResult{
		Tools: tools,
	}

	return &mcp.Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
}

func (h *StdioHandler) handleToolsCall(req *mcp.Request) *mcp.Response {
	// Extract tool call parameters
	var params mcp.ToolCallParams
	paramsBytes, err := json.Marshal(req.Params)
	if err != nil {
		return h.createErrorResponse(req.ID, -32602, fmt.Sprintf("Invalid params: %v", err))
	}

	if err := json.Unmarshal(paramsBytes, &params); err != nil {
		return h.createErrorResponse(req.ID, -32602, fmt.Sprintf("Invalid params: %v", err))
	}

	// Call the tool
	result, err := h.registry.CallTool(params.Name, params.Arguments)
	if err != nil {
		return &mcp.Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: mcp.ToolCallResult{
				Content: []mcp.ContentBlock{
					{
						Type: "text",
						Text: fmt.Sprintf("Error: %v", err),
					},
				},
				IsError: true,
			},
		}
	}

	return &mcp.Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: mcp.ToolCallResult{
			Content: []mcp.ContentBlock{
				{
					Type: "text",
					Text: result,
				},
			},
		},
	}
}

func (h *StdioHandler) createErrorResponse(id interface{}, code int, message string) *mcp.Response {
	return &mcp.Response{
		JSONRPC: "2.0",
		ID:      id,
		Error: &mcp.Error{
			Code:    code,
			Message: message,
		},
	}
}

func (h *StdioHandler) sendResponse(resp *mcp.Response) error {
	h.stdoutMutex.Lock()
	defer h.stdoutMutex.Unlock()

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}

	if h.debug {
		log.Printf("[STDOUT] %s", truenas.RedactJSONForLog(data))
	}

	fmt.Printf("%s\n", data)
	return nil
}

func (h *StdioHandler) sendError(id interface{}, code int, message string) {
	resp := h.createErrorResponse(id, code, message)
	if err := h.sendResponse(resp); err != nil {
		log.Printf("Failed to send error response: %v", err)
	}
}

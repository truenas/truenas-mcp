package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

type SSEServer struct {
	registry   ToolRegistry
	listenAddr string
	apiKey     string
	clients    sync.Map // clientID -> chan Response
}

type clientConnection struct {
	id       string
	messages chan Response
	done     chan struct{}
}

func NewSSEServer(registry ToolRegistry, listenAddr string, apiKey string) *SSEServer {
	return &SSEServer{
		registry:   registry,
		listenAddr: listenAddr,
		apiKey:     apiKey,
	}
}

func (s *SSEServer) Run() error {
	mux := http.NewServeMux()

	// SSE endpoint - server sends messages to client
	mux.HandleFunc("/sse", s.handleSSE)

	// Messages endpoint - client sends messages to server
	mux.HandleFunc("/messages", s.handleMessages)

	// Health check endpoint
	mux.HandleFunc("/health", s.handleHealth)

	server := &http.Server{
		Addr:         s.listenAddr,
		Handler:      s.corsMiddleware(s.authMiddleware(mux)),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // No write timeout for SSE streaming
		IdleTimeout:  120 * time.Second,
		// Enable TCP keepalive for long-lived SSE connections
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetKeepAlive(true)
				tc.SetKeepAlivePeriod(30 * time.Second)
			}
			return ctx
		},
	}

	log.Printf("SSE server listening on %s", s.listenAddr)
	return server.ListenAndServe()
}

func (s *SSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	// Verify SSE support
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Create client connection
	clientID := fmt.Sprintf("client-%d", time.Now().UnixNano())
	client := &clientConnection{
		id:       clientID,
		messages: make(chan Response, 100), // Increase buffer for large responses
		done:     make(chan struct{}),
	}

	s.clients.Store(clientID, client)
	defer func() {
		s.clients.Delete(clientID)
		close(client.done)
	}()

	log.Printf("Client connected: %s", clientID)

	// Send initial endpoint event
	endpointEvent := fmt.Sprintf("event: endpoint\ndata: /messages\n\n")
	if _, err := fmt.Fprint(w, endpointEvent); err != nil {
		log.Printf("Error sending endpoint event: %v", err)
		return
	}
	flusher.Flush()

	// Stream messages to client
	for {
		select {
		case <-r.Context().Done():
			log.Printf("Client disconnected: %s", clientID)
			return
		case msg := <-client.messages:
			data, err := json.Marshal(msg)
			if err != nil {
				log.Printf("Error marshaling message: %v", err)
				continue
			}

			// Send SSE message
			event := fmt.Sprintf("event: message\ndata: %s\n\n", data)
			if _, err := fmt.Fprint(w, event); err != nil {
				log.Printf("Error sending message: %v", err)
				return
			}
			flusher.Flush()
		}
	}
}

func (s *SSEServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Parse JSON-RPC request
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		s.sendErrorToAllClients(nil, -32700, "Parse error", err.Error())
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Process request
	s.handleRequest(&req)

	// Return 202 Accepted (response will be sent via SSE)
	w.WriteHeader(http.StatusAccepted)
}

func (s *SSEServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "healthy",
		"version": "0.1.0",
	})
}

func (s *SSEServer) handleRequest(req *Request) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "tools/list":
		s.handleToolsList(req)
	case "tools/call":
		s.handleToolsCall(req)
	default:
		s.sendErrorToAllClients(req.ID, -32601, "Method not found", fmt.Sprintf("Unknown method: %s", req.Method))
	}
}

func (s *SSEServer) handleInitialize(req *Request) {
	result := InitializeResult{
		ProtocolVersion: "2024-11-05",
		ServerInfo: ServerInfo{
			Name:    "truenas-mcp",
			Version: "0.1.0",
		},
		Capabilities: Capabilities{
			Tools: map[string]interface{}{},
		},
	}
	s.sendResponseToAllClients(req.ID, result)
}

func (s *SSEServer) handleToolsList(req *Request) {
	tools := s.registry.ListTools()
	result := ToolsListResult{
		Tools: tools,
	}
	s.sendResponseToAllClients(req.ID, result)
}

func (s *SSEServer) handleToolsCall(req *Request) {
	// Extract tool call params
	paramsJSON, err := json.Marshal(req.Params)
	if err != nil {
		s.sendErrorToAllClients(req.ID, -32602, "Invalid params", err.Error())
		return
	}

	var params ToolCallParams
	if err := json.Unmarshal(paramsJSON, &params); err != nil {
		s.sendErrorToAllClients(req.ID, -32602, "Invalid params", err.Error())
		return
	}

	// Call the tool
	resultText, err := s.registry.CallTool(params.Name, params.Arguments)
	if err != nil {
		result := ToolCallResult{
			Content: []ContentBlock{
				{
					Type: "text",
					Text: fmt.Sprintf("Error: %v", err),
				},
			},
			IsError: true,
		}
		s.sendResponseToAllClients(req.ID, result)
		return
	}

	result := ToolCallResult{
		Content: []ContentBlock{
			{
				Type: "text",
				Text: resultText,
			},
		},
	}
	s.sendResponseToAllClients(req.ID, result)
}

func (s *SSEServer) sendResponseToAllClients(id interface{}, result interface{}) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	s.broadcastResponse(resp)
}

func (s *SSEServer) sendErrorToAllClients(id interface{}, code int, message string, data interface{}) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Error: &Error{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
	s.broadcastResponse(resp)
}

func (s *SSEServer) broadcastResponse(resp Response) {
	s.clients.Range(func(key, value interface{}) bool {
		client := value.(*clientConnection)
		select {
		case client.messages <- resp:
			// Successfully queued
		case <-client.done:
			// Client disconnected, skip
		case <-time.After(30 * time.Second):
			// Increase timeout to 30s to accommodate large responses
			log.Printf("Timeout queueing message for client %s", client.id)
		}
		return true
	})
}

// CORS middleware
func (s *SSEServer) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Authentication middleware
func (s *SSEServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health endpoint
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// If no API key is configured, allow all requests
		if s.apiKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Check Authorization header
		authHeader := r.Header.Get("Authorization")
		expectedAuth := fmt.Sprintf("Bearer %s", s.apiKey)

		if authHeader != expectedAuth {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

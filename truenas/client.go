package truenas

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	socketPath     string
	apiKey         string
	conn           *websocket.Conn
	requestID      atomic.Uint64
	authenticated  bool
}

type ConnectRequest struct {
	Msg     string   `json:"msg"`
	Version string   `json:"version"`
	Support []string `json:"support"`
}

type ConnectResponse struct {
	Msg     string `json:"msg"`
	Session string `json:"session"`
}

type APIRequest struct {
	ID     string        `json:"id"`
	Msg    string        `json:"msg"`
	Method string        `json:"method"`
	Params []interface{} `json:"params,omitempty"`
}

type APIResponse struct {
	ID     string          `json:"id"`
	Msg    string          `json:"msg"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *APIError       `json:"error,omitempty"`
}

type APIError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Trace   interface{} `json:"trace,omitempty"` // Can be string or object
}

func NewClient(socketPath, apiKey string) (*Client, error) {
	return &Client{
		socketPath: socketPath,
		apiKey:     apiKey,
	}, nil
}

func (c *Client) connect() error {
	if c.conn != nil {
		return nil
	}

	// Create dialer for Unix socket
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
	}

	// Connect WebSocket over Unix socket
	wsDialer := websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial("unix", c.socketPath)
		},
		HandshakeTimeout: 10 * time.Second,
	}

	// TrueNAS middleware WebSocket endpoint
	conn, _, err := wsDialer.Dial("ws://localhost/websocket", nil)
	if err != nil {
		return fmt.Errorf("failed to connect to websocket: %w", err)
	}

	c.conn = conn
	c.authenticated = false

	// Send connect message as per TrueNAS WebSocket protocol
	connectMsg := ConnectRequest{
		Msg:     "connect",
		Version: "1",
		Support: []string{"1"},
	}

	log.Printf("Sending connect message: %+v", connectMsg)
	if err := c.conn.WriteJSON(connectMsg); err != nil {
		c.conn = nil
		return fmt.Errorf("failed to send connect message: %w", err)
	}

	// Read connect response
	var connectResp ConnectResponse
	if err := c.conn.ReadJSON(&connectResp); err != nil {
		c.conn = nil
		return fmt.Errorf("failed to read connect response: %w", err)
	}

	log.Printf("Received connect response: %+v", connectResp)

	if connectResp.Msg != "connected" {
		c.conn = nil
		return fmt.Errorf("unexpected connect response: %s", connectResp.Msg)
	}

	return nil
}

func (c *Client) Authenticate() error {
	if err := c.connect(); err != nil {
		return err
	}

	log.Println("Authenticating with TrueNAS middleware...")

	// Call auth.login_with_api_key
	result, err := c.Call("auth.login_with_api_key", c.apiKey)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// The result should be true if authentication succeeded
	var success bool
	if err := json.Unmarshal(result, &success); err != nil {
		return fmt.Errorf("failed to parse authentication response: %w", err)
	}

	if !success {
		return fmt.Errorf("authentication returned false")
	}

	c.authenticated = true
	log.Println("TrueNAS middleware authentication successful")
	return nil
}

func (c *Client) Call(method string, params ...interface{}) (json.RawMessage, error) {
	if err := c.connect(); err != nil {
		return nil, err
	}

	id := fmt.Sprintf("%d", c.requestID.Add(1))

	req := APIRequest{
		ID:     id,
		Msg:    "method",
		Method: method,
		Params: params,
	}

	reqJSON, _ := json.Marshal(req)
	log.Printf("Sending request: %s", string(reqJSON))

	if err := c.conn.WriteJSON(req); err != nil {
		c.conn = nil // Reset connection on error
		c.authenticated = false
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Read response
	var resp APIResponse
	if err := c.conn.ReadJSON(&resp); err != nil {
		c.conn = nil // Reset connection on error
		c.authenticated = false
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	respJSON, _ := json.Marshal(resp)
	log.Printf("Received response: %s", string(respJSON))

	// Check for explicit failure message
	if resp.Msg == "failed" {
		if resp.Error != nil {
			return nil, formatAPIError(resp.Error)
		}
		return nil, fmt.Errorf("API call failed with no error details")
	}

	if resp.Error != nil {
		return nil, formatAPIError(resp.Error)
	}

	log.Printf("Result length: %d bytes", len(resp.Result))

	return resp.Result, nil
}

func (c *Client) Close() error {
	c.authenticated = false
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// formatAPIError formats an API error into a readable error message
func formatAPIError(apiErr *APIError) error {
	errMsg := fmt.Sprintf("API error: %s (code %d)", apiErr.Message, apiErr.Code)
	if apiErr.Trace != nil {
		// Try to format trace if it's available
		if traceStr, ok := apiErr.Trace.(string); ok && traceStr != "" {
			errMsg = fmt.Sprintf("%s\nTrace: %s", errMsg, traceStr)
		}
	}
	return fmt.Errorf("%s", errMsg)
}

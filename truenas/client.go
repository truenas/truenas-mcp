package truenas

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	endpoint  string
	apiKey    string
	tlsConfig *tls.Config

	// connMu protects conn and authenticated; also gates connect/authenticate
	connMu        sync.Mutex
	conn          *websocket.Conn
	authenticated bool

	// writeMu protects concurrent WebSocket writes
	writeMu sync.Mutex

	// pending maps request ID -> response channel for concurrent request multiplexing
	pendingMu sync.Mutex
	pending   map[string]chan *responseResult

	requestID atomic.Uint64
}

type responseResult struct {
	resp *APIResponse
	err  error
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

func NewClient(endpoint, apiKey string, tlsConfig *tls.Config) (*Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("apiKey cannot be empty")
	}
	return &Client{
		endpoint:  endpoint,
		apiKey:    apiKey,
		tlsConfig: tlsConfig,
		pending:   make(map[string]chan *responseResult),
	}, nil
}

// connect establishes the WebSocket connection and starts the read loop.
// Must be called with connMu held.
func (c *Client) connect() error {
	if c.conn != nil {
		return nil
	}

	urls, err := c.buildConnectionURLs()
	if err != nil {
		return err
	}

	wsDialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  c.tlsConfig, // Always use TLS config (only wss:// allowed)
		ReadBufferSize:   65536,       // 64KB read buffer to handle large messages
		WriteBufferSize:  65536,       // 64KB write buffer to handle large messages
	}

	var lastErr error
	for _, url := range urls {
		log.Printf("Connecting to %s...", url)
		conn, _, err := wsDialer.Dial(url, nil)
		if err != nil {
			log.Printf("Connection failed: %v", err)
			lastErr = err
			continue
		}

		// Set read limit to handle large responses (e.g., large upgrade summaries)
		conn.SetReadLimit(10 * 1024 * 1024) // 10MB

		// Send connect message as per TrueNAS WebSocket protocol
		connectMsg := ConnectRequest{
			Msg:     "connect",
			Version: "1",
			Support: []string{"1"},
		}
		log.Printf("Sending connect message: %+v", connectMsg)
		if err := conn.WriteJSON(connectMsg); err != nil {
			conn.Close()
			lastErr = fmt.Errorf("failed to send connect message: %w", err)
			continue
		}

		// Read connect response directly (before read loop starts)
		var connectResp ConnectResponse
		if err := conn.ReadJSON(&connectResp); err != nil {
			conn.Close()
			lastErr = fmt.Errorf("failed to read connect response: %w", err)
			continue
		}
		log.Printf("Received connect response: %+v", connectResp)

		if connectResp.Msg != "connected" {
			conn.Close()
			lastErr = fmt.Errorf("unexpected connect response: %s", connectResp.Msg)
			continue
		}

		c.conn = conn
		c.authenticated = false

		// Start the read loop to multiplex concurrent responses
		go c.readLoop(conn)

		log.Printf("Successfully connected via %s", url)
		return nil
	}

	return fmt.Errorf("all connection attempts failed: %w", lastErr)
}

// readLoop reads all WebSocket responses and routes them to the waiting callers
// via the pending map. Runs as a goroutine for the lifetime of the connection.
func (c *Client) readLoop(conn *websocket.Conn) {
	for {
		var resp APIResponse
		if err := conn.ReadJSON(&resp); err != nil {
			// Connection dropped - fail all pending requests
			c.failAllPending(fmt.Errorf("failed to read response: %w", err))

			// Reset connection state if it's still this connection
			c.connMu.Lock()
			if c.conn == conn {
				c.conn = nil
				c.authenticated = false
			}
			c.connMu.Unlock()
			return
		}

		respJSON, _ := json.Marshal(resp)
		log.Printf("Received response: %s", string(respJSON))
		log.Printf("Result length: %d bytes", len(resp.Result))

		// Route response to the waiting caller
		c.pendingMu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.pendingMu.Unlock()

		if ok {
			ch <- &responseResult{resp: &resp}
		} else if resp.ID != "" {
			log.Printf("Warning: received response for unknown request ID %s (may have timed out)", resp.ID)
		}
	}
}

// failAllPending delivers an error to all in-flight requests (called on disconnect)
func (c *Client) failAllPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan *responseResult)
	c.pendingMu.Unlock()

	for _, ch := range pending {
		ch <- &responseResult{err: err}
	}
}

// buildConnectionURLs returns URLs to try in order
func (c *Client) buildConnectionURLs() ([]string, error) {
	// SECURITY: Reject ws:// URLs entirely - TrueNAS will revoke API keys used over unencrypted connections
	if strings.HasPrefix(c.endpoint, "ws://") {
		return nil, fmt.Errorf("SECURITY ERROR: ws:// (unencrypted) connections are not allowed. TrueNAS will revoke API keys used over ws://. Use wss:// instead")
	}

	// If full wss:// URL provided, use it
	if strings.HasPrefix(c.endpoint, "wss://") {
		return []string{c.endpoint}, nil
	}

	// Otherwise, ONLY use wss:// (secure connection required for API key authentication)
	hostname := c.endpoint
	// Remove port if specified (we'll add the correct port)
	if idx := strings.LastIndex(hostname, ":"); idx != -1 {
		hostname = hostname[:idx]
	}

	return []string{fmt.Sprintf("wss://%s:443/websocket", hostname)}, nil
}

func (c *Client) Authenticate() error {
	// Ensure connected before authenticating
	c.connMu.Lock()
	err := c.connect()
	c.connMu.Unlock()
	if err != nil {
		return err
	}

	log.Println("Authenticating with TrueNAS middleware...")

	// Call auth.login_with_api_key
	result, err := c.callRaw("auth.login_with_api_key", c.apiKey)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	var success bool
	if err := json.Unmarshal(result, &success); err != nil {
		return fmt.Errorf("failed to parse authentication response: %w", err)
	}

	if !success {
		return fmt.Errorf("authentication returned false")
	}

	c.connMu.Lock()
	c.authenticated = true
	c.connMu.Unlock()

	log.Println("TrueNAS middleware authentication successful")
	return nil
}

func (c *Client) Call(method string, params ...interface{}) (json.RawMessage, error) {
	// Ensure connected and authenticated (serialized to prevent concurrent reconnects)
	c.connMu.Lock()
	if err := c.connect(); err != nil {
		c.connMu.Unlock()
		return nil, err
	}
	needsAuth := !c.authenticated
	c.connMu.Unlock()

	if needsAuth {
		if err := c.Authenticate(); err != nil {
			return nil, fmt.Errorf("re-authentication failed: %w", err)
		}
	}

	return c.callRaw(method, params...)
}

// callRaw sends a request and waits for its response via the pending map.
// Safe for concurrent use.
func (c *Client) callRaw(method string, params ...interface{}) (json.RawMessage, error) {
	var lastErr error

	// Try up to 2 times (initial attempt + 1 retry on connection error)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			log.Printf("Retrying request after connection error (attempt %d/2)...", attempt+1)
			c.connMu.Lock()
			if err := c.connect(); err != nil {
				c.connMu.Unlock()
				return nil, fmt.Errorf("reconnection failed: %w", err)
			}
			c.connMu.Unlock()
			if err := c.Authenticate(); err != nil {
				return nil, fmt.Errorf("re-authentication failed: %w", err)
			}
		}

		// Snapshot the connection under the lock to avoid nil dereference
		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()

		if conn == nil {
			lastErr = fmt.Errorf("not connected")
			if attempt == 0 {
				// Try to reconnect
				c.connMu.Lock()
				if err := c.connect(); err != nil {
					c.connMu.Unlock()
					return nil, fmt.Errorf("reconnection failed: %w", err)
				}
				c.connMu.Unlock()
				if err := c.Authenticate(); err != nil {
					return nil, fmt.Errorf("re-authentication failed: %w", err)
				}
				continue
			}
			return nil, lastErr
		}

		id := fmt.Sprintf("%d", c.requestID.Add(1))

		// Register the response channel BEFORE writing, to avoid a race where
		// the response arrives before we add the channel to the pending map.
		ch := make(chan *responseResult, 1)
		c.pendingMu.Lock()
		c.pending[id] = ch
		c.pendingMu.Unlock()

		req := APIRequest{
			ID:     id,
			Msg:    "method",
			Method: method,
			Params: params,
		}

		reqJSON, _ := json.Marshal(req)
		log.Printf("Sending request: %s", string(reqJSON))

		// writeMu ensures only one goroutine writes to the WebSocket at a time
		c.writeMu.Lock()
		err := conn.WriteJSON(req)
		c.writeMu.Unlock()

		if err != nil {
			// Remove our pending channel since we failed to send
			c.pendingMu.Lock()
			delete(c.pending, id)
			c.pendingMu.Unlock()

			// Clear the connection if it's still this one
			c.connMu.Lock()
			if c.conn == conn {
				c.conn = nil
				c.authenticated = false
			}
			c.connMu.Unlock()

			lastErr = fmt.Errorf("failed to send request: %w", err)
			if isConnectionError(err) && attempt == 0 {
				continue
			}
			return nil, lastErr
		}

		// Wait for the response router to deliver our response
		select {
		case result := <-ch:
			if result.err != nil {
				lastErr = result.err
				if isConnectionError(result.err) && attempt == 0 {
					continue
				}
				return nil, result.err
			}

			resp := result.resp

			if resp.Msg == "failed" {
				if resp.Error != nil {
					return nil, formatAPIErrorWithContext(resp.Error, method, params)
				}
				return nil, fmt.Errorf("API call failed with no error details")
			}

			if resp.Error != nil {
				return nil, formatAPIErrorWithContext(resp.Error, method, params)
			}

			return resp.Result, nil

		case <-time.After(120 * time.Second):
			// Timeout - clean up pending entry
			c.pendingMu.Lock()
			delete(c.pending, id)
			c.pendingMu.Unlock()
			return nil, fmt.Errorf("request timed out after 120 seconds (method: %s)", method)
		}
	}

	return nil, lastErr
}

// isConnectionError checks if an error is a connection-related error that should trigger a retry
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "failed to read response")
}

func (c *Client) Close() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
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
		if traceStr, ok := apiErr.Trace.(string); ok && traceStr != "" {
			errMsg = fmt.Sprintf("%s\nTrace: %s", errMsg, traceStr)
		} else {
			if traceJSON, err := json.MarshalIndent(apiErr.Trace, "", "  "); err == nil {
				errMsg = fmt.Sprintf("%s\nTrace: %s", errMsg, string(traceJSON))
			}
		}
	}
	return fmt.Errorf("%s", errMsg)
}

// formatAPIErrorWithContext formats API error with request context for debugging
func formatAPIErrorWithContext(apiErr *APIError, method string, params []interface{}) error {
	errMsg := fmt.Sprintf("API error: %s (code %d)", apiErr.Message, apiErr.Code)

	errMsg = fmt.Sprintf("%s\n\nRequest:\n  Method: %s", errMsg, method)

	if len(params) > 0 {
		if paramsJSON, err := json.MarshalIndent(params, "  ", "  "); err == nil {
			errMsg = fmt.Sprintf("%s\n  Params: %s", errMsg, string(paramsJSON))
		}
	}

	if apiErr.Trace != nil {
		if traceStr, ok := apiErr.Trace.(string); ok && traceStr != "" {
			errMsg = fmt.Sprintf("%s\n\nTrace: %s", errMsg, traceStr)
		} else {
			if traceJSON, err := json.MarshalIndent(apiErr.Trace, "", "  "); err == nil {
				errMsg = fmt.Sprintf("%s\n\nTrace: %s", errMsg, string(traceJSON))
			}
		}
	}

	return fmt.Errorf("%s", errMsg)
}

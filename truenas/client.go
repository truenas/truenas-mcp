package truenas

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	endpoint      string
	apiKey        string
	tlsConfig     *tls.Config
	conn          *websocket.Conn
	requestID     atomic.Uint64
	authenticated bool
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
	}, nil
}

func (c *Client) connect() error {
	if c.conn != nil {
		return nil
	}

	// Build connection URLs - will return error if ws:// is specified
	urls, err := c.buildConnectionURLs()
	if err != nil {
		return err
	}

	wsDialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  c.tlsConfig, // Always use TLS config (only wss:// allowed)
	}

	var lastErr error
	for _, url := range urls {
		log.Printf("Connecting to %s...", url)
		conn, _, err := wsDialer.Dial(url, nil)
		if err != nil {
			log.Printf("Connection failed: %v", err)
			lastErr = err
			continue // Try next URL
		}

		c.conn = conn
		c.authenticated = false

		// Set read limit to handle large responses (e.g., large upgrade summaries)
		// Default is 512KB, set to 10MB to handle large responses from TrueNAS
		c.conn.SetReadLimit(10 * 1024 * 1024) // 10MB

		// Send connect message as per TrueNAS WebSocket protocol
		connectMsg := ConnectRequest{
			Msg:     "connect",
			Version: "1",
			Support: []string{"1"},
		}

		log.Printf("Sending connect message: %+v", connectMsg)
		if err := c.conn.WriteJSON(connectMsg); err != nil {
			c.conn.Close()
			c.conn = nil
			lastErr = fmt.Errorf("failed to send connect message: %w", err)
			continue
		}

		// Read connect response
		var connectResp ConnectResponse
		if err := c.conn.ReadJSON(&connectResp); err != nil {
			c.conn.Close()
			c.conn = nil
			lastErr = fmt.Errorf("failed to read connect response: %w", err)
			continue
		}

		log.Printf("Received connect response: %+v", connectResp)

		if connectResp.Msg != "connected" {
			c.conn.Close()
			c.conn = nil
			lastErr = fmt.Errorf("unexpected connect response: %s", connectResp.Msg)
			continue
		}

		log.Printf("Successfully connected via %s", url)
		return nil
	}

	return fmt.Errorf("all connection attempts failed: %w", lastErr)
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
	if err := c.connect(); err != nil {
		return err
	}

	log.Println("Authenticating with TrueNAS middleware...")

	// Call auth.login_with_api_key using raw call (bypass auth check)
	result, err := c.callRaw("auth.login_with_api_key", c.apiKey)
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
	// Ensure we're connected
	if err := c.connect(); err != nil {
		return nil, err
	}

	// Ensure we're authenticated (will re-authenticate if connection was reset)
	if !c.authenticated {
		if err := c.Authenticate(); err != nil {
			return nil, fmt.Errorf("re-authentication failed: %w", err)
		}
	}

	return c.callRaw(method, params...)
}

// callRaw performs the actual API call without authentication check
// Automatically retries once on connection errors
func (c *Client) callRaw(method string, params ...interface{}) (json.RawMessage, error) {
	var lastErr error

	// Try up to 2 times (initial attempt + 1 retry on connection error)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			log.Printf("Retrying request after connection error (attempt %d/2)...", attempt+1)
			// Reconnect and re-authenticate before retry
			if err := c.connect(); err != nil {
				return nil, fmt.Errorf("reconnection failed: %w", err)
			}
			if !c.authenticated {
				if err := c.Authenticate(); err != nil {
					return nil, fmt.Errorf("re-authentication failed: %w", err)
				}
			}
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
			lastErr = fmt.Errorf("failed to send request: %w", err)

			// Only retry if this looks like a connection error
			if isConnectionError(err) && attempt == 0 {
				continue // Retry
			}
			return nil, lastErr
		}

		// Read response
		var resp APIResponse
		if err := c.conn.ReadJSON(&resp); err != nil {
			c.conn = nil // Reset connection on error
			c.authenticated = false
			lastErr = fmt.Errorf("failed to read response: %w", err)

			// Only retry if this looks like a connection error
			if isConnectionError(err) && attempt == 0 {
				continue // Retry
			}
			return nil, lastErr
		}

		respJSON, _ := json.Marshal(resp)
		log.Printf("Received response: %s", string(respJSON))

		// Check for explicit failure message
		if resp.Msg == "failed" {
			if resp.Error != nil {
				return nil, formatAPIErrorWithContext(resp.Error, method, params)
			}
			return nil, fmt.Errorf("API call failed with no error details")
		}

		if resp.Error != nil {
			return nil, formatAPIErrorWithContext(resp.Error, method, params)
		}

		log.Printf("Result length: %d bytes", len(resp.Result))

		return resp.Result, nil
	}

	return nil, lastErr
}

// isConnectionError checks if an error is a connection-related error that should trigger a retry
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Check for common connection errors
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "i/o timeout")
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
		// Try to format trace - can be string or object
		if traceStr, ok := apiErr.Trace.(string); ok && traceStr != "" {
			errMsg = fmt.Sprintf("%s\nTrace: %s", errMsg, traceStr)
		} else {
			// If it's not a string, marshal it as JSON
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

	// Add request context for debugging
	errMsg = fmt.Sprintf("%s\n\nRequest:\n  Method: %s", errMsg, method)

	// Format params (usually just one element - the actual payload)
	if len(params) > 0 {
		if paramsJSON, err := json.MarshalIndent(params, "  ", "  "); err == nil {
			errMsg = fmt.Sprintf("%s\n  Params: %s", errMsg, string(paramsJSON))
		}
	}

	// Add trace if available
	if apiErr.Trace != nil {
		// Try to format trace - can be string or object
		if traceStr, ok := apiErr.Trace.(string); ok && traceStr != "" {
			errMsg = fmt.Sprintf("%s\n\nTrace: %s", errMsg, traceStr)
		} else {
			// If it's not a string, marshal it as JSON
			if traceJSON, err := json.MarshalIndent(apiErr.Trace, "", "  "); err == nil {
				errMsg = fmt.Sprintf("%s\n\nTrace: %s", errMsg, string(traceJSON))
			}
		}
	}

	return fmt.Errorf("%s", errMsg)
}

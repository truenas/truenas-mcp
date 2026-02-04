package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
)

// Proxy manages the stdio-to-SSE bridge
type Proxy struct {
	config       *Config
	sseClient    *mcp.SSEClient
	httpClient   *http.Client
	stdio        *StdioHandler
	pendingReqs  sync.Map // map[interface{}]chan *mcp.Response
	messagesURL  string
	shutdownChan chan struct{}
	shutdownOnce sync.Once
	wg           sync.WaitGroup
	stdinClosed  atomic.Bool
	activeReqs   atomic.Int32
}

// NewProxy creates a new proxy instance
func NewProxy(config *Config) *Proxy {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: config.Insecure,
		},
	}

	return &Proxy{
		config: config,
		httpClient: &http.Client{
			Timeout:   config.Timeout,
			Transport: transport,
		},
		stdio:        NewStdioHandler(config.Debug),
		sseClient:    mcp.NewSSEClient(config.Debug),
		shutdownChan: make(chan struct{}),
	}
}

// Run starts the proxy
func (p *Proxy) Run() error {
	// Set up SSE handlers
	p.sseClient.SetEndpointHandler(p.handleEndpoint)
	p.sseClient.SetMessageHandler(p.handleSSEMessage)

	// Connect to SSE endpoint
	sseURL := p.config.ServerURL + "/sse"
	if p.config.Debug {
		log.Printf("[PROXY] Connecting to SSE endpoint: %s", sseURL)
	}

	if err := p.sseClient.Connect(sseURL, p.config.APIKey); err != nil {
		return fmt.Errorf("failed to connect to SSE endpoint: %w", err)
	}

	// Start request timeout cleaner
	p.wg.Add(1)
	go p.timeoutCleaner()

	// Start stdin reader
	p.wg.Add(1)
	go p.stdinReader()

	// Wait for shutdown
	<-p.shutdownChan
	p.wg.Wait()

	return nil
}

// Shutdown gracefully stops the proxy
func (p *Proxy) Shutdown() {
	p.shutdownOnce.Do(func() {
		if p.config.Debug {
			log.Printf("[PROXY] Shutting down...")
		}
		close(p.shutdownChan)
		if err := p.sseClient.Close(); err != nil {
			log.Printf("Error closing SSE client: %v", err)
		}
	})
}

// handleEndpoint is called when the SSE endpoint URL is received
func (p *Proxy) handleEndpoint(url string) {
	// Only accept the first endpoint event (should be the /messages path)
	// Ignore subsequent events that might be responses
	if p.messagesURL != "" {
		if p.config.Debug {
			log.Printf("[PROXY] Ignoring duplicate endpoint event: %s", url)
		}
		return
	}

	// Construct full URL from server base URL and endpoint path
	p.messagesURL = p.config.ServerURL + url
	if p.config.Debug {
		log.Printf("[PROXY] Messages endpoint: %s", p.messagesURL)
	}
}

// handleSSEMessage is called when a message is received via SSE
func (p *Proxy) handleSSEMessage(resp *mcp.Response) {
	if p.config.Debug {
		log.Printf("[PROXY] Received response for request ID: %v", resp.ID)
	}

	// Find pending request
	if pending, ok := p.pendingReqs.LoadAndDelete(resp.ID); ok {
		ch := pending.(chan *mcp.Response)
		select {
		case ch <- resp:
			// Response delivered
		default:
			// Channel full or closed
			if p.config.Debug {
				log.Printf("[PROXY] Failed to deliver response for ID %v", resp.ID)
			}
		}
	} else {
		if p.config.Debug {
			log.Printf("[PROXY] No pending request for ID %v", resp.ID)
		}
	}
}

// stdinReader reads requests from stdin and sends them to the server
func (p *Proxy) stdinReader() {
	defer p.wg.Done()

	if p.config.Debug {
		log.Printf("[PROXY] Stdin reader started")
	}

	for {
		select {
		case <-p.shutdownChan:
			return
		default:
		}

		if p.config.Debug {
			log.Printf("[PROXY] Waiting for stdin...")
		}

		req, err := p.stdio.ReadRequest()
		if err != nil {
			if err == io.EOF {
				if p.config.Debug {
					log.Printf("[PROXY] Stdin closed, waiting for pending requests to complete")
				}
				p.stdinClosed.Store(true)
				// Check if there are pending requests
				if p.activeReqs.Load() == 0 {
					if p.config.Debug {
						log.Printf("[PROXY] No pending requests, shutting down")
					}
					p.Shutdown()
				}
				return
			}

			log.Printf("Error reading from stdin: %v", err)
			if err := p.stdio.WriteError(nil, -32700, "Parse error"); err != nil {
				log.Printf("Failed to write error response: %v", err)
			}
			continue
		}

		// Handle request
		if p.config.Debug {
			log.Printf("[PROXY] Received request ID=%v method=%s", req.ID, req.Method)
		}

		// Check if this is a notification (no ID means no response expected)
		if req.ID == nil {
			if p.config.Debug {
				log.Printf("[PROXY] Notification (no response needed): %s", req.Method)
			}
			// Just forward to server, don't wait for response
			p.wg.Add(1)
			go p.sendRequestNoResponse(req)
			continue
		}

		// Increment activeReqs BEFORE starting goroutine to avoid race condition
		p.activeReqs.Add(1)
		p.wg.Add(1)
		go p.handleRequest(req)
	}
}

// handleRequest processes a single request
func (p *Proxy) handleRequest(req *mcp.Request) {
	defer func() {
		p.activeReqs.Add(-1)
		p.wg.Done()

		// If stdin is closed and no more active requests, shutdown
		if p.stdinClosed.Load() && p.activeReqs.Load() == 0 {
			if p.config.Debug {
				log.Printf("[PROXY] All requests completed, shutting down")
			}
			p.Shutdown()
		}
	}()

	if p.config.Debug {
		log.Printf("[PROXY] Handling request ID=%v", req.ID)
	}

	// Wait for messages endpoint
	if p.messagesURL == "" {
		timeout := time.After(5 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-timeout:
				if err := p.stdio.WriteError(req.ID, -32603, "Timeout waiting for server endpoint"); err != nil {
					log.Printf("Failed to write error: %v", err)
				}
				return
			case <-ticker.C:
				if p.messagesURL != "" {
					goto ready
				}
			}
		}
	}

ready:
	// Create response channel
	respChan := make(chan *mcp.Response, 1)
	p.pendingReqs.Store(req.ID, respChan)

	// Send request to server
	if err := p.sendRequest(req); err != nil {
		p.pendingReqs.Delete(req.ID)
		if err := p.stdio.WriteError(req.ID, -32603, fmt.Sprintf("Failed to send request: %v", err)); err != nil {
			log.Printf("Failed to write error: %v", err)
		}
		return
	}

	// Wait for response with timeout
	select {
	case resp := <-respChan:
		if err := p.stdio.WriteResponse(resp); err != nil {
			log.Printf("Failed to write response: %v", err)
		}
	case <-time.After(p.config.Timeout):
		p.pendingReqs.Delete(req.ID)
		if err := p.stdio.WriteError(req.ID, -32603, "Request timeout"); err != nil {
			log.Printf("Failed to write timeout error: %v", err)
		}
	case <-p.shutdownChan:
		p.pendingReqs.Delete(req.ID)
	}
}

// sendRequestNoResponse sends a notification to the server without expecting a response
func (p *Proxy) sendRequestNoResponse(req *mcp.Request) {
	defer p.wg.Done()

	// Wait for messages endpoint
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for p.messagesURL == "" {
		select {
		case <-timeout:
			log.Printf("Timeout waiting for server endpoint")
			return
		case <-ticker.C:
		}
	}

	if err := p.sendRequest(req); err != nil {
		log.Printf("Failed to send notification: %v", err)
	}
}

// sendRequest sends a request to the server's /messages endpoint with retry logic
func (p *Proxy) sendRequest(req *mcp.Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	maxRetries := 3
	retryDelay := 1 * time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if p.config.Debug {
				log.Printf("[PROXY] Retry attempt %d/%d after %v delay", attempt, maxRetries, retryDelay)
			}
			time.Sleep(retryDelay)
			retryDelay *= 2 // Exponential backoff
		}

		if p.config.Debug {
			log.Printf("[PROXY] Sending request to %s (attempt %d/%d)", p.messagesURL, attempt+1, maxRetries+1)
		}

		httpReq, err := http.NewRequest("POST", p.messagesURL, bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("failed to create HTTP request: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")
		if p.config.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.config.APIKey)
		}

		resp, err := p.httpClient.Do(httpReq)
		if err != nil {
			if attempt < maxRetries {
				if p.config.Debug {
					log.Printf("[PROXY] Request failed: %v, will retry...", err)
				}
				continue
			}
			return fmt.Errorf("failed to send request after %d attempts: %w", maxRetries+1, err)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
			return nil
		}

		// If it's a connection error or server error, retry
		if resp.StatusCode >= 500 && attempt < maxRetries {
			if p.config.Debug {
				log.Printf("[PROXY] Server error (status %d), will retry...", resp.StatusCode)
			}
			continue
		}

		return fmt.Errorf("server returned status %d: %s", resp.StatusCode, string(body))
	}

	return fmt.Errorf("failed after %d attempts", maxRetries+1)
}

// timeoutCleaner periodically cleans up timed-out requests
func (p *Proxy) timeoutCleaner() {
	defer p.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.shutdownChan:
			return
		case <-ticker.C:
			// Cleanup is handled by request timeout goroutines
		}
	}
}

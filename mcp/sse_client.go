package mcp

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/r3labs/sse/v2"
)

// SSEClient manages connection to an MCP SSE server
type SSEClient struct {
	client          *sse.Client
	onMessage       func(*Response)
	onEndpoint      func(string)
	connected       atomic.Bool
	debugLog        bool
	url             string
	apiKey          string
	reconnecting    atomic.Bool
	shutdownChan    chan struct{}
	shutdownOnce    sync.Once
	subscriptionsMu sync.Mutex
}

// NewSSEClient creates a new SSE client
func NewSSEClient(debugLog bool) *SSEClient {
	return &SSEClient{
		debugLog:     debugLog,
		shutdownChan: make(chan struct{}),
	}
}

// Connect establishes connection to the SSE endpoint
func (c *SSEClient) Connect(url, apiKey string) error {
	c.url = url
	c.apiKey = apiKey
	return c.connect()
}

func (c *SSEClient) connect() error {
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()

	client := sse.NewClient(c.url)

	// Add authorization header
	if c.apiKey != "" {
		client.Headers = map[string]string{
			"Authorization": "Bearer " + c.apiKey,
		}
	}

	// Don't log connection attempts by default
	client.Connection.Transport = &http.Transport{}

	// Set connection callback to monitor disconnections
	debugLog := c.debugLog
	client.OnDisconnect(func(client *sse.Client) {
		if debugLog {
			log.Printf("[SSE] Disconnected from server")
		}
	})

	c.client = client
	c.connected.Store(true)

	// Subscribe to endpoint event in a goroutine (non-blocking)
	go func() {
		if c.debugLog {
			log.Printf("[SSE] Starting endpoint subscription...")
		}
		err := client.Subscribe("endpoint", func(msg *sse.Event) {
			if c.debugLog {
				log.Printf("[SSE] Received endpoint event: %s", string(msg.Data))
			}
			if c.onEndpoint != nil {
				c.onEndpoint(string(msg.Data))
			}
		})
		if err != nil {
			log.Printf("[SSE] Endpoint subscription error: %v", err)
			c.connected.Store(false)
			c.scheduleReconnect()
		}
	}()

	// Subscribe to message event in a goroutine (non-blocking)
	go func() {
		if c.debugLog {
			log.Printf("[SSE] Starting message subscription...")
		}
		err := client.Subscribe("message", func(msg *sse.Event) {
			if c.debugLog {
				log.Printf("[SSE] Received message event: %s", string(msg.Data))
			}

			if c.onMessage != nil {
				var resp Response
				if err := json.Unmarshal(msg.Data, &resp); err != nil {
					// Ignore non-JSON messages (like endpoint paths that might leak through)
					if c.debugLog {
						log.Printf("Skipping non-JSON SSE message: %s", string(msg.Data))
					}
					return
				}
				c.onMessage(&resp)
			}
		})
		if err != nil {
			log.Printf("[SSE] Message subscription error: %v", err)
			c.connected.Store(false)
			c.scheduleReconnect()
		}
	}()

	// Give subscriptions time to start
	if c.debugLog {
		log.Printf("[SSE] Subscriptions started")
	}

	return nil
}

// scheduleReconnect attempts to reconnect after a delay
func (c *SSEClient) scheduleReconnect() {
	// Only one reconnection attempt at a time
	if !c.reconnecting.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer c.reconnecting.Store(false)

		backoff := 1 * time.Second
		maxBackoff := 30 * time.Second

		for {
			select {
			case <-c.shutdownChan:
				return
			case <-time.After(backoff):
				if c.debugLog {
					log.Printf("[SSE] Attempting to reconnect...")
				}

				if err := c.connect(); err != nil {
					if c.debugLog {
						log.Printf("[SSE] Reconnection failed: %v", err)
					}
					// Exponential backoff
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
					continue
				}

				log.Printf("[SSE] Reconnected successfully")
				return
			}
		}
	}()
}


// SetMessageHandler sets the callback for message events
func (c *SSEClient) SetMessageHandler(handler func(*Response)) {
	c.onMessage = handler
}

// SetEndpointHandler sets the callback for endpoint events
func (c *SSEClient) SetEndpointHandler(handler func(string)) {
	c.onEndpoint = handler
}

// IsConnected returns true if client is connected
func (c *SSEClient) IsConnected() bool {
	return c.connected.Load()
}

// Close disconnects the client
func (c *SSEClient) Close() error {
	c.shutdownOnce.Do(func() {
		close(c.shutdownChan)
	})
	c.connected.Store(false)
	if c.client != nil {
		c.client.Unsubscribe(make(chan *sse.Event))
	}
	return nil
}

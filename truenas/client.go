package truenas

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// debugLogging controls whether full request/response bodies are written to
// the log. Off by default: normal operation logs only metadata (method, id,
// result size). Enabled via the TRUENAS_MCP_DEBUG environment variable or the
// --debug flag (see SetDebugLogging). Even when enabled, credential-bearing
// values are redacted before logging.
var debugLogging atomic.Bool

func init() {
	switch strings.ToLower(os.Getenv("TRUENAS_MCP_DEBUG")) {
	case "", "0", "false", "no", "off":
	default:
		debugLogging.Store(true)
	}
}

// SetDebugLogging enables or disables verbose request/response body logging.
func SetDebugLogging(enabled bool) { debugLogging.Store(enabled) }

// DebugLogging reports whether verbose body logging is enabled.
func DebugLogging() bool { return debugLogging.Load() }

type Client struct {
	endpoint  string
	apiKey    string
	apiUser   string
	proxy     string
	tlsConfig *tls.Config

	// authMu serializes connecting and logging in, so concurrent callers
	// share a single auth.login_ex per connection.
	authMu sync.Mutex

	// connMu protects conn, authedConn and authGen.
	connMu sync.Mutex
	conn   *websocket.Conn
	// authedConn is the connection auth.login_ex last succeeded on. Requests
	// are only sent once authedConn == conn, so a reconnect is never mistaken
	// for an authenticated session.
	authedConn *websocket.Conn
	// authGen counts successful logins, so a stale ENOTAUTHENTICATED reply
	// only discards the login it was sent under.
	authGen uint64

	// Keepalive: a ping every pingInterval; the connection is dropped when
	// nothing (not even a pong) arrives within pongWait.
	pingInterval time.Duration
	pongWait     time.Duration

	legacyWarning sync.Once

	// writeMu protects concurrent WebSocket writes
	writeMu sync.Mutex

	// pending maps request ID -> in-flight request, for concurrent request
	// multiplexing. Each entry records the connection it was written to, so
	// a dying connection only fails its own requests.
	pendingMu sync.Mutex
	pending   map[string]*pendingCall

	requestID atomic.Uint64
}

type pendingCall struct {
	conn *websocket.Conn
	ch   chan *responseResult
}

const (
	callTimeout         = 120 * time.Second
	loginTimeout        = 30 * time.Second
	defaultPingInterval = 30 * time.Second
	defaultPongWait     = 75 * time.Second
)

type responseResult struct {
	resp *rpcResponse
	err  error
}

// apiPath is TrueNAS's versioned JSON-RPC 2.0 endpoint, available since
// TrueNAS 25.04 (the oldest supported release). It replaces the legacy
// /websocket protocol, which is deprecated and planned for removal in
// TrueNAS 28.
const apiPath = "/api/current"

// legacyPath is the old DDP-style endpoint. URLs naming it are rewritten to
// apiPath so existing configurations keep working.
const legacyPath = "/websocket"

// errNotSent marks a request that never reached the socket. Only such
// requests are retried: once written, the server may already have acted on it.
var errNotSent = errors.New("request not sent")

// rpcRequest is a JSON-RPC 2.0 request. Params is always sent, as an array.
type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      uint64        `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

// rpcResponse is a JSON-RPC 2.0 response, or a server notification (which
// carries a method and no id).
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    *rpcErrorData `json:"data,omitempty"`
}

// rpcErrorData is the TrueNAS middleware's error detail.
type rpcErrorData struct {
	Error   int         `json:"error"`
	Errname string      `json:"errname"`
	Reason  string      `json:"reason"`
	Trace   interface{} `json:"trace,omitempty"`
	Extra   interface{} `json:"extra,omitempty"`
}

type APIError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Trace   interface{} `json:"trace,omitempty"` // Can be string or object
	Extra   interface{} `json:"extra,omitempty"` // Validation details, e.g. [[field, reason, errno]]
}

// toAPIError maps a JSON-RPC error onto the APIError the client formats.
// TrueNAS puts the useful detail (errno, errname, reason) in data.
func (e *rpcError) toAPIError() *APIError {
	out := &APIError{Code: e.Code, Message: e.Message}
	if d := e.Data; d != nil {
		if d.Error != 0 {
			out.Code = d.Error
		}
		if d.Reason != "" {
			out.Message = d.Reason
		}
		if d.Errname != "" && !strings.Contains(out.Message, d.Errname) {
			out.Message = "[" + d.Errname + "] " + out.Message
		}
		out.Trace = d.Trace
		out.Extra = d.Extra
	}
	return out
}

// notAuthenticated reports whether the server refused a call because the
// session is not logged in; such a call was not executed.
func (e *rpcError) notAuthenticated() bool {
	return e.Data != nil && e.Data.Errname == "ENOTAUTHENTICATED"
}

// responseKey normalizes a JSON-RPC id (number or string) to a pending-map
// key. Notifications (no id) and responses with a null id yield "".
func responseKey(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var n uint64
	if json.Unmarshal(raw, &n) == nil {
		return strconv.FormatUint(n, 10)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// Options configures a Client beyond the endpoint, key and TLS settings.
type Options struct {
	// APIUser is the user that owns the API key, sent to auth.login_ex.
	// Required.
	APIUser string
	// Proxy overrides TRUENAS_PROXY, HTTPS_PROXY and ALL_PROXY for the
	// TrueNAS connection (NO_PROXY still applies); "none" disables
	// proxying. Optional.
	Proxy string
}

func NewClientWithOptions(endpoint, apiKey string, tlsConfig *tls.Config, opts Options) (*Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("apiKey cannot be empty")
	}
	if opts.APIUser == "" {
		return nil, fmt.Errorf("API user cannot be empty: it must name the user that owns the API key")
	}
	return &Client{
		endpoint:  endpoint,
		apiKey:    apiKey,
		apiUser:   opts.APIUser,
		proxy:     strings.TrimSpace(opts.Proxy),
		tlsConfig: tlsConfig,
		pending:   make(map[string]*pendingCall),

		pingInterval: defaultPingInterval,
		pongWait:     defaultPongWait,
	}, nil
}

// tlsHint suggests a fix for a certificate verification failure, or returns
// "" if err is not one.
func tlsHint(err error, host string) string {
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		fix := "connect using a host name listed in the certificate, or install a TrueNAS certificate whose subject alternative names include " + host
		if net.ParseIP(host) != nil {
			fix = "connect using a host name listed in the certificate instead of the IP address, or install a TrueNAS certificate with IP " + host + " in its subject alternative names"
		}
		return "the TrueNAS certificate is not valid for " + host + ": " + fix + "; --tls-ca cannot fix a name mismatch, and --insecure (which disables verification at the cost of man-in-the-middle protection) is the last resort"
	}
	if strings.Contains(err.Error(), "x509:") || strings.Contains(err.Error(), "certificate") {
		return "TrueNAS uses a self-signed certificate by default: pass --tls-ca <cert.pem> to trust it, or --insecure to disable verification at the cost of man-in-the-middle protection"
	}
	return ""
}

// connect establishes the WebSocket connection and starts the read loop.
// Must be called with connMu held.
func (c *Client) connect() error {
	if c.conn != nil {
		return nil
	}

	u, err := c.endpointURL()
	if err != nil {
		return err
	}

	wsDialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  c.tlsConfig, // Always use TLS config (only wss:// allowed)
		ReadBufferSize:   65536,       // 64KB read buffer to handle large messages
		WriteBufferSize:  65536,       // 64KB write buffer to handle large messages
	}
	proxy, err := c.proxyFor(u)
	if err != nil {
		return err
	}
	via := ""
	if proxy != nil {
		// TLS still runs end to end inside the proxy tunnel.
		wsDialer.Proxy = http.ProxyURL(dialerProxy(proxy.url))
		via = " via proxy " + proxy.String()
	}

	log.Printf("Connecting to %s%s...", u.Redacted(), via)
	conn, resp, err := wsDialer.Dial(u.String(), nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			if u.Path == apiPath {
				err = fmt.Errorf("%w (HTTP 404 for %s: TrueNAS 25.04 or later required)", err, u.Redacted())
			} else {
				err = fmt.Errorf("%w (HTTP 404 for %s: check the path; the JSON-RPC endpoint is %s, and TrueNAS 25.04 or later required)", err, u.Redacted(), apiPath)
			}
		} else if hint := tlsHint(err, u.Hostname()); hint != "" {
			err = fmt.Errorf("%w (%s)", err, hint)
		} else if opErr := (*net.OpError)(nil); proxy != nil && errors.As(err, &opErr) && opErr.Op == "dial" {
			// Through a proxy, the only TCP dial is to the proxy itself.
			err = fmt.Errorf("connecting via proxy %s from %s: proxy unreachable: %w", proxy, proxy.source, err)
		}
		log.Printf("Connection failed: %v", err)
		return fmt.Errorf("connection failed: %w", err)
	}

	// Set read limit to handle large responses (e.g., large upgrade summaries)
	conn.SetReadLimit(10 * 1024 * 1024) // 10MB

	// JSON-RPC 2.0 has no connect handshake: the socket is ready once upgraded.
	c.conn = conn
	c.authedConn = nil

	// Keepalive: any inbound frame or pong extends the read deadline, so a
	// half-open connection is noticed instead of every call timing out.
	pongWait := c.pongWait
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	done := make(chan struct{})
	go c.keepalive(conn, done)

	// Start the read loop to multiplex concurrent responses
	go c.readLoop(conn, done)

	log.Printf("Successfully connected to %s%s", u.Redacted(), via)
	return nil
}

// keepalive pings conn until done is closed. WriteControl may be called
// concurrently with other writes.
func (c *Client) keepalive(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return // readLoop notices the broken connection
			}
		}
	}
}

// readLoop reads all WebSocket responses and routes them to the waiting callers
// via the pending map. Runs as a goroutine for the lifetime of the connection.
func (c *Client) readLoop(conn *websocket.Conn, done chan struct{}) {
	defer close(done)
	for {
		var resp rpcResponse
		err := conn.ReadJSON(&resp)
		if err == nil {
			conn.SetReadDeadline(time.Now().Add(c.pongWait))
		}
		if err != nil {
			// Clear the connection BEFORE failing waiters, so their retry
			// reconnects instead of reusing this dead socket.
			c.connMu.Lock()
			if c.conn == conn {
				c.conn = nil
				c.authedConn = nil
			}
			c.connMu.Unlock()
			conn.Close()
			c.failPending(conn, fmt.Errorf("failed to read response: %w", err))
			return
		}

		key := responseKey(resp.ID)
		if key == "" && resp.Error != nil {
			// A null id means the server could not parse a frame. It keeps
			// serving the connection and answers every other request, so
			// only fail a request when it is the only one in flight here.
			err := fmt.Errorf("API error without a request id: %s (code %d)",
				c.scrubSecrets(resp.Error.toAPIError().Message), resp.Error.Code)
			log.Printf("Received %v", err)
			if call := c.takeOnlyPending(conn); call != nil {
				call.ch <- &responseResult{err: err}
			}
			continue
		}
		if key == "" {
			// Server notification (no id): nothing is waiting for it.
			if debugLogging.Load() {
				log.Printf("Ignoring server notification: method=%s", resp.Method)
			}
			continue
		}

		if debugLogging.Load() {
			// Full bodies only in debug mode, and even then with known
			// credential keys masked and any server echo of the API key
			// scrubbed - responses can carry secrets (e.g. job arguments,
			// app configs) that the server is not guaranteed to redact.
			respJSON, _ := json.Marshal(resp)
			log.Printf("Received response: %s", c.scrubSecrets(string(RedactJSONForLog(respJSON))))
		} else {
			log.Printf("Received response: id=%s result=%d bytes", key, len(resp.Result))
		}

		// Route response to the waiting caller
		c.pendingMu.Lock()
		call, ok := c.pending[key]
		if ok {
			delete(c.pending, key)
		}
		c.pendingMu.Unlock()

		if ok {
			call.ch <- &responseResult{resp: &resp}
		} else {
			log.Printf("Warning: received response for unknown request ID %s (may have timed out)", key)
		}
	}
}

// failPending delivers err to every request in flight on conn (called when
// conn breaks). Requests on other connections are left alone.
func (c *Client) failPending(conn *websocket.Conn, err error) {
	c.pendingMu.Lock()
	var failed []*pendingCall
	for key, call := range c.pending {
		if call.conn == conn {
			failed = append(failed, call)
			delete(c.pending, key)
		}
	}
	c.pendingMu.Unlock()

	for _, call := range failed {
		call.ch <- &responseResult{err: err}
	}
}

// takeOnlyPending removes and returns the request in flight on conn if it is
// the only one, and returns nil otherwise.
func (c *Client) takeOnlyPending(conn *websocket.Conn) *pendingCall {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	var only *pendingCall
	var onlyKey string
	for key, call := range c.pending {
		if call.conn != conn {
			continue
		}
		if only != nil {
			return nil
		}
		only, onlyKey = call, key
	}
	if only != nil {
		delete(c.pending, onlyKey)
	}
	return only
}

// errUserinfo rejects credentials embedded in the URL. The error never
// echoes the input, which would leak them.
var errUserinfo = errors.New("TrueNAS URL must not contain user info (user:password@); pass the API key with --api-key and the user with --api-user")

// endpointURL returns the JSON-RPC 2.0 endpoint to dial. A bare host (name,
// IPv4, IPv6, optionally bracketed and with a port) becomes
// wss://<host>:<port or 443>/api/current. A wss:// URL is used as given,
// except that trailing slashes are dropped and an empty path or the legacy
// /websocket path is replaced with /api/current, so existing configurations
// keep working. Errors never quote input that could hold credentials.
func (c *Client) endpointURL() (*url.URL, error) {
	e := strings.TrimSpace(c.endpoint)
	if i := strings.Index(e, "://"); i >= 0 {
		switch strings.ToLower(e[:i]) {
		case "wss":
		case "ws":
			// SECURITY: TrueNAS revokes API keys used over unencrypted connections
			return nil, fmt.Errorf("SECURITY ERROR: ws:// (unencrypted) connections are not allowed. TrueNAS will revoke API keys used over ws://. Use wss:// instead")
		default:
			return nil, fmt.Errorf("unsupported URL scheme: pass a hostname or a wss:// URL")
		}
		u, err := url.Parse("wss" + e[i:])
		if err != nil {
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				err = urlErr.Err // drop the URL itself from the message
			}
			if strings.Contains(e, "@") {
				return nil, errUserinfo
			}
			return nil, fmt.Errorf("invalid TrueNAS URL: %w", err)
		}
		if u.User != nil {
			return nil, errUserinfo
		}
		if u.Hostname() == "" {
			return nil, fmt.Errorf("TrueNAS URL has no host")
		}
		u.Path, u.RawPath = strings.TrimRight(u.Path, "/"), ""
		switch u.Path {
		case "":
			u.Path = apiPath
		case legacyPath:
			c.legacyWarning.Do(func() {
				log.Printf("Warning: %s is the legacy protocol (deprecated, planned for removal in TrueNAS 28); using %s instead", legacyPath, apiPath)
			})
			u.Path = apiPath
		}
		return u, nil
	}
	if strings.Contains(e, "@") {
		return nil, errUserinfo
	}
	// From here on e holds no userinfo, so quoting it is safe.
	if strings.ContainsAny(e, "/?#") {
		return nil, fmt.Errorf("invalid TrueNAS host %q: a bare host cannot include a path; pass a full wss://host[:port]%s URL instead", e, apiPath)
	}
	host, port := e, "443"
	if h, p, err := net.SplitHostPort(e); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(e, "[") && strings.HasSuffix(e, "]") {
		host = e[1 : len(e)-1] // bracketed IPv6 without a port
	} // otherwise a name or an unbracketed IPv6 address, without a port
	if host == "" || strings.ContainsAny(host, "[]") {
		return nil, fmt.Errorf("invalid TrueNAS host %q: no host", e)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("invalid TrueNAS host %q: bad port %q", e, port)
	}
	return &url.URL{Scheme: "wss", Host: net.JoinHostPort(host, port), Path: apiPath}, nil
}

// Authenticate connects if needed and logs in with auth.login_ex.
func (c *Client) Authenticate() error {
	_, _, err := c.session(true)
	return err
}

// authedSession returns the current connection and its login generation if
// it is logged in.
func (c *Client) authedSession() (*websocket.Conn, uint64) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn != nil && c.authedConn == c.conn {
		return c.conn, c.authGen
	}
	return nil, 0
}

// forgetSession marks conn as no longer logged in, unless it has logged in
// again since login generation gen.
func (c *Client) forgetSession(conn *websocket.Conn, gen uint64) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.authedConn == conn && c.authGen == gen {
		c.authedConn = nil
	}
}

// session returns a logged-in connection, connecting and logging in first
// if needed. authMu serializes this, so concurrent callers share one login.
// force logs in again even if the current connection is already logged in.
func (c *Client) session(force bool) (*websocket.Conn, uint64, error) {
	if !force {
		if conn, gen := c.authedSession(); conn != nil {
			return conn, gen, nil
		}
	}
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if !force {
		// Another caller may have logged in while we waited.
		if conn, gen := c.authedSession(); conn != nil {
			return conn, gen, nil
		}
	}

	for attempt := 0; ; attempt++ {
		c.connMu.Lock()
		err := c.connect()
		conn := c.conn
		c.connMu.Unlock()
		if err != nil {
			return nil, 0, err
		}

		err = c.login(conn)
		c.connMu.Lock()
		current := c.conn == conn
		gen := c.authGen
		if err == nil && current {
			c.authGen++
			gen = c.authGen
			c.authedConn = conn
		}
		c.connMu.Unlock()
		if err == nil && current {
			log.Println("TrueNAS middleware authentication successful")
			return conn, gen, nil
		}
		if err == nil {
			err = errors.New("authentication failed: connection lost during login")
		}
		// The connection dropped during login. Logging in again on a new
		// connection is safe: nothing else has been sent on it.
		if !current && attempt == 0 {
			log.Printf("Connection lost during login, reconnecting: %v", err)
			continue
		}
		return nil, 0, err
	}
}

// login sends auth.login_ex on conn.
func (c *Client) login(conn *websocket.Conn) error {
	log.Println("Authenticating with TrueNAS middleware...")

	// auth.login_ex replaces auth.login_with_api_key, which is deprecated and
	// planned for removal in TrueNAS 28. API_KEY_PLAIN sends the key over the
	// TLS socket.
	params := []interface{}{map[string]interface{}{
		"mechanism": "API_KEY_PLAIN",
		"username":  c.apiUser,
		"api_key":   c.apiKey,
	}}
	resp, err := c.roundTrip(conn, "auth.login_ex", params, loginTimeout)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("authentication failed: %w", c.formatAPIErrorWithContext(resp.Error.toAPIError(), "auth.login_ex", params))
	}

	var result struct {
		ResponseType string `json:"response_type"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("failed to parse authentication response: %w", err)
	}
	if result.ResponseType != "SUCCESS" {
		if result.ResponseType == "AUTH_ERR" {
			return fmt.Errorf("authentication failed: response_type=%s (check that --api-user / TRUENAS_API_USER names the user that owns the API key)", result.ResponseType)
		}
		return fmt.Errorf("authentication failed: response_type=%s", result.ResponseType)
	}
	return nil
}

// Call invokes a middleware method, connecting and logging in first if
// needed. A request is retried once only when it is known not to have run:
// it never reached the socket, or the server refused it as not
// authenticated (then after logging in again). Safe for concurrent use.
func (c *Client) Call(method string, params ...interface{}) (json.RawMessage, error) {
	if params == nil {
		params = []interface{}{} // JSON-RPC: always send params as an array
	}
	for attempt := 0; ; attempt++ {
		retry := attempt == 0
		conn, gen, err := c.session(false)
		if err != nil {
			return nil, err
		}

		resp, err := c.roundTrip(conn, method, params, callTimeout)
		if err != nil {
			if retry && errors.Is(err, errNotSent) {
				log.Printf("Retrying %s on a new connection: %v", method, err)
				continue
			}
			return nil, err
		}
		if resp.Error != nil {
			if retry && resp.Error.notAuthenticated() {
				log.Printf("Session not authenticated; logging in again and retrying %s", method)
				c.forgetSession(conn, gen)
				continue
			}
			return nil, c.formatAPIErrorWithContext(resp.Error.toAPIError(), method, params)
		}
		return resp.Result, nil
	}
}

// sensitiveKeyFragments match JSON object keys whose values must never be
// written to logs or error messages (e.g. bindpw in directoryservices.update).
var sensitiveKeyFragments = []string{"password", "passwd", "bindpw", "secret", "api_key", "apikey", "token", "credential"}

func isSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, fragment := range sensitiveKeyFragments {
		if strings.Contains(k, fragment) {
			return true
		}
	}
	return false
}

// redactValue returns a deep copy of v with values under sensitive keys masked.
func redactValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			if isSensitiveKey(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = redactValue(val)
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = redactValue(val)
		}
		return out
	default:
		return v
	}
}

// RedactJSONForLog returns raw with values under credential-bearing keys
// (password, bindpw, secret, ...) replaced by "[REDACTED]". Intended for
// sanitizing JSON messages before writing them to logs. Returns raw unchanged
// if it is not valid JSON.
func RedactJSONForLog(raw []byte) []byte {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return raw
	}
	return out
}

// scrubSecrets removes the client's API key from server-supplied text (error
// messages, traces, response bodies) in case the server echoes it back.
func (c *Client) scrubSecrets(s string) string {
	if c.apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.apiKey, "[REDACTED]")
}

// redactParams returns a copy of params that is safe for logging. Auth methods
// carry credentials (e.g. auth.login_ex), so every
// parameter is masked; for all other methods, values under sensitive keys are
// masked. The original params are never modified.
func redactParams(method string, params []interface{}) []interface{} {
	if strings.HasPrefix(method, "auth.") {
		redacted := make([]interface{}, len(params))
		for i := range redacted {
			redacted[i] = "[REDACTED]"
		}
		return redacted
	}
	redacted := make([]interface{}, len(params))
	for i, p := range params {
		redacted[i] = redactValue(p)
	}
	return redacted
}

// roundTrip sends one request on conn and waits up to timeout for its
// response via the pending map. It never retries. A failed write is wrapped
// in errNotSent; a request that cannot be encoded is never written.
func (c *Client) roundTrip(conn *websocket.Conn, method string, params []interface{}, timeout time.Duration) (*rpcResponse, error) {
	id := c.requestID.Add(1)
	key := strconv.FormatUint(id, 10)

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	// Encode before touching the socket: a marshal error must not send an
	// empty frame or tear down a healthy connection.
	frame, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to encode request for %s: %w", method, err)
	}

	// Register the response channel BEFORE writing, to avoid a race where
	// the response arrives before we add the channel to the pending map.
	ch := make(chan *responseResult, 1)
	c.pendingMu.Lock()
	c.pending[key] = &pendingCall{conn: conn, ch: ch}
	c.pendingMu.Unlock()

	if debugLogging.Load() {
		logReq := req
		logReq.Params = redactParams(method, req.Params)
		reqJSON, _ := json.Marshal(logReq)
		log.Printf("Sending request: %s", string(reqJSON))
	} else {
		log.Printf("Sending request: id=%s method=%s", key, method)
	}

	// writeMu ensures only one goroutine writes to the WebSocket at a time
	c.writeMu.Lock()
	err = conn.WriteMessage(websocket.TextMessage, frame)
	c.writeMu.Unlock()

	if err != nil {
		// Remove our pending channel since we failed to send
		c.pendingMu.Lock()
		delete(c.pending, key)
		c.pendingMu.Unlock()

		// Clear the connection if it's still this one
		c.connMu.Lock()
		if c.conn == conn {
			c.conn = nil
			c.authedConn = nil
		}
		c.connMu.Unlock()
		conn.Close()

		return nil, fmt.Errorf("failed to send request: %w: %w", errNotSent, err)
	}

	// Wait for the response router to deliver our response
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-ch:
		if result.err != nil {
			return nil, result.err
		}
		return result.resp, nil
	case <-timer.C:
		// Timeout - clean up pending entry
		c.pendingMu.Lock()
		delete(c.pending, key)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("request timed out after %s (method: %s)", timeout, method)
	}
}

func (c *Client) Close() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	c.authedConn = nil
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// formatAPIErrorWithContext formats API error with request context for
// debugging. Our own params are redacted, and the server-supplied message and
// trace are scrubbed of the API key in case the server echoes it back.
func (c *Client) formatAPIErrorWithContext(apiErr *APIError, method string, params []interface{}) error {
	errMsg := fmt.Sprintf("API error: %s (code %d)", c.scrubSecrets(apiErr.Message), apiErr.Code)

	errMsg = fmt.Sprintf("%s\n\nRequest:\n  Method: %s", errMsg, method)

	if len(params) > 0 {
		if paramsJSON, err := json.MarshalIndent(redactParams(method, params), "  ", "  "); err == nil {
			errMsg = fmt.Sprintf("%s\n  Params: %s", errMsg, string(paramsJSON))
		}
	}

	if apiErr.Extra != nil {
		if extraJSON, err := json.MarshalIndent(redactValue(apiErr.Extra), "", "  "); err == nil {
			errMsg = fmt.Sprintf("%s\n\nDetails: %s", errMsg, c.scrubSecrets(string(extraJSON)))
		}
	}

	if apiErr.Trace != nil {
		if traceStr, ok := apiErr.Trace.(string); ok && traceStr != "" {
			errMsg = fmt.Sprintf("%s\n\nTrace: %s", errMsg, c.scrubSecrets(traceStr))
		} else {
			if traceJSON, err := json.MarshalIndent(redactValue(apiErr.Trace), "", "  "); err == nil {
				errMsg = fmt.Sprintf("%s\n\nTrace: %s", errMsg, c.scrubSecrets(string(traceJSON)))
			}
		}
	}

	return fmt.Errorf("%s", errMsg)
}

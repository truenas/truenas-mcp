package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/truenas/truenas-mcp/internal/safego"
	"github.com/truenas/truenas-mcp/redact"
)

// shellPath is the external WebSocket path TrueNAS nginx proxies to
// middlewared's /_shell ShellApplication handler on both 25.10
// (stable/goldeye) and master (etc_files/local/nginx/nginx.conf.mako).
// nginx does not expose /_shell directly over HTTPS/WSS.
const shellPath = "/websocket/shell"

const (
	shellTokenTTLSeconds = 60
	shellDialTimeout     = 10 * time.Second
	shellHandshakeWait   = 15 * time.Second
	shellWriteTimeout    = 10 * time.Second
	shellCloseTimeout    = 1 * time.Second
	// shellMaxFrameBytes bounds a single WebSocket frame from the shell PTY.
	shellMaxFrameBytes = 1 << 20
	// shellMaxErrorText bounds server-supplied error text quoted in errors.
	shellMaxErrorText = 512
)

// ShellAuthError is returned when TrueNAS's /_shell handler rejects the
// authentication handshake with {"msg": "failed", "error": ...}.
type ShellAuthError struct {
	Code   int
	Reason string
}

func (e *ShellAuthError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("TrueNAS rejected VM serial console authentication: %s (code %d)", e.Reason, e.Code)
	}
	return fmt.Sprintf("TrueNAS rejected VM serial console authentication: %s", e.Reason)
}

// ShellConn is an authenticated WebSocket connection to a TrueNAS
// ShellApplication session (such as a VM serial console).
type ShellConn struct {
	conn      *websocket.Conn
	shellID   string
	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
}

// ID returns the middleware shell session UUID from the connected handshake
// reply. It is used with core.resize_shell and should not be exposed as an
// external handle.
func (s *ShellConn) ID() string {
	if s == nil {
		return ""
	}
	return s.shellID
}

// ReadFrame reads the next WebSocket frame from the shell PTY.
func (s *ShellConn) ReadFrame() (int, []byte, error) {
	return s.conn.ReadMessage()
}

// WriteInput writes raw terminal input bytes to the shell PTY.
// ShellWorkerThread.writer passes msg.data directly to os.write(master_fd, ...),
// which requires Python bytes and therefore WebSocket binary frames.
func (s *ShellConn) WriteInput(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(shellWriteTimeout)); err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, data)
}

// Close terminates the shell WebSocket connection and stops keepalive pings.
func (s *ShellConn) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		// Send a best-effort close control frame so middleware tears down
		// ShellWorkerThread promptly; WriteControl is safe concurrently.
		_ = s.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(shellCloseTimeout),
		)
		err = s.conn.Close()
	})
	return err
}

func (s *ShellConn) keepalive(interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("shell keepalive panicked: %s\n%s", safego.Describe(r), debug.Stack())
		}
	}()
	if interval <= 0 {
		interval = defaultPingInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(shellWriteTimeout)); err != nil {
				return
			}
		}
	}
}

// OpenVMShell mints a single-use, origin-matched auth token via
// auth.generate_token, dials TrueNAS's /websocket/shell endpoint with the
// client's TLS and proxy configuration, and completes the VM console
// handshake for vmID. The auth token is never logged or returned.
func (c *Client) OpenVMShell(ctx context.Context, vmID int) (*ShellConn, error) {
	if vmID < 1 {
		return nil, fmt.Errorf("vmID must be a positive integer")
	}
	token, err := c.generateShellToken(ctx)
	if err != nil {
		return nil, err
	}
	scrub := func(s string) string {
		if token != "" {
			s = strings.ReplaceAll(s, token, redact.Marker)
		}
		return c.scrubSecrets(s)
	}

	ws, err := c.endpointURL()
	if err != nil {
		return nil, err
	}
	target := &url.URL{Scheme: "wss", Host: ws.Host, Path: shellPath}

	wsDialer := websocket.Dialer{
		HandshakeTimeout: shellDialTimeout,
		TLSClientConfig:  c.tlsConfig.Clone(),
		ReadBufferSize:   65536,
		WriteBufferSize:  65536,
	}
	proxy, err := c.proxyFor(target)
	if err != nil {
		return nil, err
	}
	via := ""
	if proxy != nil {
		wsDialer.Proxy = http.ProxyURL(dialerProxy(proxy.url))
		via = " via proxy " + proxy.String()
	}

	log.Printf("Opening VM serial console WebSocket at %s%s (vm_id=%d)", logURL(target), via, vmID)
	conn, resp, err := safeDial(ctx, &wsDialer, target.String())
	if err != nil {
		return nil, c.formatShellDialError(target, proxy, resp, err, scrub)
	}
	conn.SetReadLimit(shellMaxFrameBytes)

	shellID, err := completeVMShellHandshake(ctx, conn, token, vmID, scrub)
	if err != nil {
		conn.Close()
		return nil, err
	}

	// Clear handshake deadlines before handing the socket to the session reader.
	conn.SetReadDeadline(time.Time{})
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to reset shell write deadline: %w", err)
	}

	sc := &ShellConn{
		conn:    conn,
		shellID: shellID,
		done:    make(chan struct{}),
	}
	go sc.keepalive(c.pingInterval)
	return sc, nil
}

// generateShellToken requests a short-lived, single-use, origin-matched token
// with empty attributes (auth.get_token_for_shell_application rejects any
// token carrying attributes).
func (c *Client) generateShellToken(ctx context.Context) (string, error) {
	raw, err := c.Call(ctx, "auth.generate_token", shellTokenTTLSeconds, map[string]interface{}{}, true, true)
	if err != nil {
		return "", fmt.Errorf("failed to generate shell authentication token: %w", err)
	}
	var token string
	if err := json.Unmarshal(raw, &token); err != nil || strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("TrueNAS returned an unusable shell authentication token")
	}
	return token, nil
}

func (c *Client) formatShellDialError(target *url.URL, proxy *proxyChoice, resp *http.Response, err error, scrub func(string) string) error {
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("VM serial console endpoint %s was not found on TrueNAS (HTTP 404)", logURL(target))
	}
	if hint := tlsHint(err, target.Hostname()); hint != "" {
		return fmt.Errorf("failed to connect to VM serial console at %s: %s (%s)", logURL(target), scrub(err.Error()), hint)
	}
	if proxy != nil && proxyDialFailed(err) {
		return fmt.Errorf("connecting to VM serial console via proxy %s from %s: proxy unreachable: %s", proxy, proxy.source, scrub(err.Error()))
	}
	if proxy != nil {
		if reason, ok := socksReplyFailure(err); ok {
			return socksReplyError(proxy, target, reason)
		}
	}
	return fmt.Errorf("failed to connect to VM serial console at %s: %s", logURL(target), scrub(err.Error()))
}

// completeVMShellHandshake sends {"token": ..., "options": {"vm_id": vmID}}
// and reads the ShellApplication reply ("connected" or "failed").
func completeVMShellHandshake(ctx context.Context, conn *websocket.Conn, token string, vmID int, scrub func(string) string) (string, error) {
	deadline := time.Now().Add(shellHandshakeWait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return "", fmt.Errorf("failed to set shell handshake write deadline: %w", err)
	}
	conn.SetReadDeadline(deadline)

	// Abort blocking socket reads/writes promptly if ctx is cancelled.
	cancelDone := make(chan struct{})
	defer close(cancelDone)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-cancelDone:
		}
	}()

	handshake, err := json.Marshal(map[string]interface{}{
		"token": token,
		"options": map[string]interface{}{
			"vm_id": vmID,
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to encode VM serial console handshake")
	}
	if err := conn.WriteMessage(websocket.TextMessage, handshake); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("VM serial console handshake cancelled: %w", ctx.Err())
		}
		return "", fmt.Errorf("failed to send VM serial console handshake: %s", scrub(err.Error()))
	}

	_, replyBytes, err := conn.ReadMessage()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("VM serial console handshake cancelled: %w", ctx.Err())
		}
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			return "", fmt.Errorf("TrueNAS closed the VM serial console connection during handshake (code %d): verify the VM exists, is running, and the account has shell privileges", closeErr.Code)
		}
		return "", fmt.Errorf("failed to read VM serial console handshake response: %s", scrub(err.Error()))
	}

	var reply struct {
		Msg   string `json:"msg"`
		ID    string `json:"id"`
		Error *struct {
			Error  int    `json:"error"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(replyBytes, &reply); err != nil {
		return "", fmt.Errorf("unexpected VM serial console handshake response from TrueNAS")
	}

	switch reply.Msg {
	case "connected":
		if strings.TrimSpace(reply.ID) == "" {
			return "", fmt.Errorf("TrueNAS VM serial console handshake succeeded without a shell id")
		}
		return reply.ID, nil
	case "failed":
		reason := "authentication failed"
		code := 0
		if reply.Error != nil {
			code = reply.Error.Error
			if r := strings.TrimSpace(scrub(reply.Error.Reason)); r != "" {
				if len(r) > shellMaxErrorText {
					r = r[:shellMaxErrorText] + "..."
				}
				reason = r
			}
		}
		return "", &ShellAuthError{Code: code, Reason: reason}
	default:
		return "", fmt.Errorf("unexpected VM serial console handshake message %q", scrub(reply.Msg))
	}
}

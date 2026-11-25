package truenas

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/url"
	"strings"
)

// ParseEndpoint returns the JSON-RPC endpoint URL for a TrueNAS host or
// wss:// URL, as the client would dial it. Errors are *EndpointError.
func ParseEndpoint(endpoint string) (*url.URL, error) {
	c := &Client{endpoint: endpoint}
	return c.endpointURL()
}

// Session is a single connection to TrueNAS with no automatic login,
// reconnect or retry: each Call is sent once, on this connection, and the
// caller logs in with auth.login_ex itself. It is meant for one-shot
// tools such as setup, not for the MCP server.
type Session struct {
	c *Client
}

// Dial connects to endpoint (a host or wss:// URL, as for the client)
// without logging in. proxy is as Options.Proxy. A failure to connect is a
// *ConnectError; an unusable endpoint is an *EndpointError.
func Dial(ctx context.Context, endpoint string, tlsConfig *tls.Config, proxy string) (*Session, error) {
	c := &Client{
		endpoint:     endpoint,
		proxy:        strings.TrimSpace(proxy),
		tlsConfig:    tlsConfig,
		pending:      make(map[string]*pendingCall),
		pingInterval: defaultPingInterval,
		pongWait:     defaultPongWait,
	}
	c.connMu.Lock()
	err := c.connect(ctx)
	c.connMu.Unlock()
	if err != nil {
		return nil, err
	}
	return &Session{c: c}, nil
}

// AddSecret registers a value (a password, OTP or API key) that is
// replaced by redact.Marker wherever server-supplied text is logged or
// returned in an error.
func (s *Session) AddSecret(v string) {
	if v == "" {
		return
	}
	s.c.secretsMu.Lock()
	s.c.extraSecrets = append(s.c.extraSecrets, v)
	s.c.secretsMu.Unlock()
}

// Call sends one request and waits for its response. A middleware error is
// returned as an *APICallError whose Code is the errno when TrueNAS sends
// one. Parameters of auth.* methods are masked in logs and errors.
func (s *Session) Call(ctx context.Context, method string, params ...interface{}) (json.RawMessage, error) {
	if params == nil {
		params = []interface{}{}
	}
	s.c.connMu.Lock()
	conn := s.c.conn
	s.c.connMu.Unlock()
	if conn == nil {
		return nil, &ConnectError{Err: errSessionClosed}
	}
	resp, err := s.c.roundTrip(ctx, conn, method, params, callTimeout)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, s.c.formatAPIErrorWithContext(resp.Error.toAPIError(), method, params)
	}
	return resp.Result, nil
}

// Close closes the connection.
func (s *Session) Close() error { return s.c.Close() }

type sessionError string

func (e sessionError) Error() string { return string(e) }

const errSessionClosed = sessionError("the connection to TrueNAS was closed")

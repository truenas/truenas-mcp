package truenas

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// closedAddr returns a loopback host:port nothing is listening on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// A failure to establish the connection is a *ConnectError, whatever the
// cause, so callers can tell it from a rejected login.
func TestConnectFailuresAreConnectErrors(t *testing.T) {
	clearProxyEnv(t)
	f := newFakeTrueNAS(t, nil)
	for _, tc := range []struct {
		name     string
		endpoint string
		tls      *tls.Config
		proxy    string
		want     string
	}{
		{"refused", closedAddr(t), &tls.Config{}, "none", "connection refused"},
		// The fake's certificate is not trusted by the empty tls.Config.
		{"untrusted certificate", f.wsURL("127.0.0.1"), &tls.Config{}, "none", "certificate"},
		{"proxy unreachable", "truenas.example", &tls.Config{}, "http://" + closedAddr(t), "proxy unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClientWithOptions(tc.endpoint, testAPIKey, tc.tls, Options{APIUser: testAPIUser, Proxy: tc.proxy})
			if err != nil {
				t.Fatal(err)
			}
			err = c.Authenticate()
			var ce *ConnectError
			if !errors.As(err, &ce) {
				t.Fatalf("Authenticate error %v (%T) is not a *ConnectError", err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "connection failed:") || strings.Contains(strings.ToLower(err.Error()), "authenticat") {
				t.Errorf("connection error is worded as an authentication failure: %v", err)
			}
		})
	}
	if n := f.logins(t); n != 0 {
		t.Errorf("a login was attempted over a connection that failed TLS verification (%d)", n)
	}
}

func TestDialPanicIsConnectError(t *testing.T) {
	orig := wsDial
	wsDial = func(*websocket.Dialer, string) (*websocket.Conn, *http.Response, error) { panic("injected") }
	t.Cleanup(func() { wsDial = orig })
	clearProxyEnv(t)
	c, _ := NewClientWithOptions("wss://truenas.invalid", testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser, Proxy: "none"})
	var ce *ConnectError
	if err := c.Authenticate(); !errors.As(err, &ce) {
		t.Errorf("Authenticate error %v (%T) is not a *ConnectError", err, err)
	}
}

// A rejected login on an established connection is not a ConnectError.
func TestAuthFailureIsNotConnectError(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.loginType = "AUTH_ERR" })
	c := insecureClient(t, f, Options{})
	err := c.Authenticate()
	if err == nil {
		t.Fatal("login succeeded")
	}
	var ce *ConnectError
	if errors.As(err, &ce) {
		t.Errorf("auth failure reported as a connection failure: %v", err)
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("auth error not worded as one: %v", err)
	}
}

// HTTPS_PROXY=none (or ALL_PROXY=none) is refused with a pointer to the
// real opt-out instead of being taken as a proxy host called "none".
func TestProxyEnvNoneIsRefused(t *testing.T) {
	target := &url.URL{Scheme: "wss", Host: "truenas.example:443"}
	for _, tc := range []struct {
		env     map[string]string
		option  string
		wantErr string // "" = connects directly
	}{
		{env: map[string]string{"HTTPS_PROXY": "none"}, wantErr: "HTTPS_PROXY=none"},
		{env: map[string]string{"https_proxy": "NONE"}, wantErr: "https_proxy=NONE"},
		{env: map[string]string{"ALL_PROXY": "None"}, wantErr: "ALL_PROXY=None"},
		// The documented opt-out still wins over, and silences, the variable.
		{env: map[string]string{"HTTPS_PROXY": "none", "TRUENAS_PROXY": "none"}},
		{env: map[string]string{"HTTPS_PROXY": "none"}, option: "none"},
		// A proxy set explicitly takes precedence and is used as usual.
		{env: map[string]string{"HTTPS_PROXY": "none"}, option: "http://corp:3128"},
		// Like any inherited setting, it is not checked for hosts that bypass the proxy.
		{env: map[string]string{"HTTPS_PROXY": "none", "NO_PROXY": ".example"}},
		// Only the bare word is special: this is a proxy host named "none".
		{env: map[string]string{"HTTPS_PROXY": "none:3128"}},
	} {
		clearProxyEnv(t)
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		c := &Client{proxy: tc.option}
		_, err := c.proxyFor(target)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("env %v option %q: unexpected error %v", tc.env, tc.option, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("env %v: accepted", tc.env)
			continue
		}
		for _, want := range []string{tc.wantErr, "TRUENAS_PROXY=none", "--proxy none"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("env %v: error %q lacks %q", tc.env, err, want)
			}
		}
	}

	// It is caught at startup, not on first connect.
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "none")
	c, _ := NewClientWithOptions("truenas.example", testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser})
	if err := c.ValidateProxy(); err == nil || !strings.Contains(err.Error(), "TRUENAS_PROXY=none") {
		t.Errorf("ValidateProxy = %v", err)
	}
}

// "invalid proxy URL" says why, and never repeats credentials or host names.
func TestInvalidProxyURLGivesReason(t *testing.T) {
	target := &url.URL{Scheme: "wss", Host: "truenas.example:443"}
	for _, tc := range []struct {
		raw    string
		reason string
	}{
		{"http://alice:s3cretpw@[bad", "unterminated ["},
		{"http://alice:s3cretpw@corphost:portx", "port is not a number"},
		{"http://alice:s3cretpw@corphost:1:2", "port is not a number"},
		{"http://alice:s3%zzretpw@corphost:3128", "percent-escape"},
		{"http://ali ce:s3cretpw@corphost:3128", "invalid user info"},
		{"http://alice:s3cretpw@corp host:3128", "invalid character in the host name"},
		{"http://alice:s3cretpw@:3128", "no host name"},
		{"socks5h://alice:s3cretpw@", "no host name"},
		{"co rp:3128", "invalid character in the host name"}, // scheme defaults to http
		{"http://alice:s3\x7fcretpw@corphost:3128", "control character"},
	} {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", tc.raw)
		c := &Client{}
		_, err := c.proxyFor(target)
		if err == nil {
			t.Errorf("%q accepted", tc.raw)
			continue
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "invalid proxy URL in HTTPS_PROXY: ") || !strings.Contains(msg, tc.reason) {
			t.Errorf("%q: error %q lacks reason %q", tc.raw, msg, tc.reason)
		}
		for _, leak := range []string{"alice", "s3", "cretpw", "corp", "portx", "zz"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%q: error echoes %q: %s", tc.raw, leak, msg)
			}
		}
	}
}

// A bad host or URL is an *EndpointError with a reason, caught by
// ValidateEndpoint, and never echoes credentials.
func TestInvalidEndpointGivesReason(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		reason   string
	}{
		{"https://truenas.example", "https:// scheme is not accepted; use the host name alone (for example truenas.local) or a wss://host[:port] URL"},
		{"http://truenas.example", "http:// scheme is not accepted"},
		{"https://alice:s3cretpw@truenas.example", "https:// scheme is not accepted"},
		{"ftp://alice:s3cretpw@truenas.example", `unsupported URL scheme "ftp"`},
		{"alice:s3cretpw@host://x", "not a host name or a wss://host[:port] URL"},
		{"ws://alice:s3cretpw@truenas.example", "ws:// (unencrypted)"},
		{"wss://alice:s3cretpw@truenas.example", "user info"},
		{"alice:s3cretpw@truenas.example:99999", "user info"},
		{"wss://truenas.example:abc", "malformed URL"},
		{"wss://", "no host"},
		{"truenas.example:99999", "bad port"},
		{"truenas.example:", "bad port"},
		{":8443", "no host name"},
		{"truenas.example/api/current", "a bare host cannot include a path"},
	} {
		c, err := NewClientWithOptions(tc.endpoint, testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser})
		if err != nil {
			t.Fatal(err)
		}
		err = c.ValidateEndpoint()
		var ee *EndpointError
		if !errors.As(err, &ee) {
			t.Errorf("%q: ValidateEndpoint = %v (%T), want *EndpointError", tc.endpoint, err, err)
			continue
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "invalid TrueNAS host or URL: ") || !strings.Contains(msg, tc.reason) {
			t.Errorf("%q: error %q lacks prefix or reason %q", tc.endpoint, msg, tc.reason)
		}
		for _, leak := range []string{"alice", "s3cretpw"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%q: error echoes credentials: %s", tc.endpoint, msg)
			}
		}
		// Connecting reports the same error, not a connection failure.
		var ce *ConnectError
		if cerr := c.Authenticate(); !errors.As(cerr, &ee) || errors.As(cerr, &ce) {
			t.Errorf("%q: Authenticate = %v", tc.endpoint, cerr)
		}
	}
	// Valid values pass.
	for _, ok := range []string{"truenas.example", "truenas.example:8443", "[::1]:8443", "wss://truenas.example/api/current"} {
		c, _ := NewClientWithOptions(ok, testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser})
		if err := c.ValidateEndpoint(); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
}

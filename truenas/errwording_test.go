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
		{"http://alice:s3cretpw@corphost:1:2", "not a host name or bracketed IPv6 address"},
		{"http://alice:s3cretpw@corphost:3128:3128", "not a host name or bracketed IPv6 address"},
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
		{"truenas.example:443:443", "not a host name or IPv6 address"},
		{"wss://truenas.example:443:443", "not a host name or bracketed IPv6 address"},
		{"wss://truenas.example:0", "bad port"},
		{"truenas.example:", "bad port"},
		{":8443", "no host name"},
		{"truenas.example/api/current", "a bare host cannot include a path, query or fragment"},
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

// logsOf captures the log for the rest of the test.
func logsOf(t *testing.T) *syncBuffer {
	t.Helper()
	b := &syncBuffer{}
	captureLog(t, b)
	return b
}

func TestLogURLDropsUserInfoQueryAndFragment(t *testing.T) {
	u, err := url.Parse("wss://alice:s3cretpw@truenas.example:8443/api/current?token=qsecret&x=1#fsecret")
	if err != nil {
		t.Fatal(err)
	}
	got := logURL(u)
	if got != "wss://truenas.example:8443/api/current" {
		t.Errorf("logURL = %q", got)
	}
	// The URL itself is untouched: the dial still uses it.
	if !strings.Contains(u.String(), "qsecret") {
		t.Errorf("logURL modified its argument: %s", u)
	}
}

// --truenas-url may carry a token in its query or fragment: it is not logged
// when connecting, nor in the 404 error text.
func TestEndpointQueryAndFragmentAreNotLogged(t *testing.T) {
	clearProxyEnv(t)
	secrets := []string{"qsecret", "fsecret", "token="}

	t.Run("connecting", func(t *testing.T) {
		f := newFakeTrueNAS(t, nil)
		logs := logsOf(t)
		c, err := NewClientWithOptions(f.wsURL("127.0.0.1")+"?token=qsecret#fsecret", testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser, Proxy: "none"})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.Authenticate(); err != nil {
			t.Fatal(err)
		}
		out := logs.String()
		for _, want := range []string{"Connecting to wss://127.0.0.1:", "Successfully connected to wss://127.0.0.1:"} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %q:\n%s", want, out)
			}
		}
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("log contains %q:\n%s", s, out)
			}
		}
	})

	t.Run("404", func(t *testing.T) {
		orig := wsDial
		t.Cleanup(func() { wsDial = orig })
		wsDial = func(_ *websocket.Dialer, urlStr string) (*websocket.Conn, *http.Response, error) {
			return nil, &http.Response{StatusCode: http.StatusNotFound}, websocket.ErrBadHandshake
		}
		for _, endpoint := range []string{
			"wss://truenas.example?token=qsecret#fsecret",                      // default path
			"wss://truenas.example/other/path?token=qsecret#fsecret",           // custom path
			"wss://truenas.example:8443/api/current?token=qsecret&a=b#fsecret", // explicit path
		} {
			logs := logsOf(t)
			c, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser, Proxy: "none"})
			if err != nil {
				t.Fatal(err)
			}
			err = c.Authenticate()
			if err == nil || !strings.Contains(err.Error(), "HTTP 404 for wss://truenas.example") {
				t.Fatalf("%q: error = %v", endpoint, err)
			}
			for _, s := range secrets {
				if strings.Contains(err.Error(), s) || strings.Contains(logs.String(), s) {
					t.Errorf("%q: %q appears in the error or log: %v\n%s", endpoint, s, err, logs)
				}
			}
		}
	})
}

// A connection that drops during login is retried on a new connection. If
// that fails to connect, the error says a login was attempted; a first
// connect that fails says none was.
func TestReconnectFailureAfterDroppedLoginSaysLoginWasAttempted(t *testing.T) {
	clearProxyEnv(t)
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.dropLogins = 1 })
	orig := wsDial
	t.Cleanup(func() { wsDial = orig })
	dials := 0
	wsDial = func(d *websocket.Dialer, urlStr string) (*websocket.Conn, *http.Response, error) {
		dials++
		if dials > 1 {
			return nil, nil, errors.New("dial tcp 192.0.2.1:443: connect: network is unreachable")
		}
		return orig(d, urlStr)
	}
	c, err := NewClientWithOptions(f.wsURL("127.0.0.1"), testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Authenticate()
	var ce *ConnectError
	if !errors.As(err, &ce) || !ce.LoginAttempted {
		t.Fatalf("Authenticate = %v; want a *ConnectError with LoginAttempted", err)
	}
	if !strings.Contains(err.Error(), "network is unreachable") {
		t.Errorf("error lost its cause: %v", err)
	}
	if n := f.logins(t); n != 1 {
		t.Errorf("%d logins sent; want the one that dropped", n)
	}

	// A first connection that fails is not a login attempt.
	dials = 1
	c2, _ := NewClientWithOptions(f.wsURL("127.0.0.1"), testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser, Proxy: "none"})
	err = c2.Authenticate()
	if !errors.As(err, &ce) || ce.LoginAttempted {
		t.Errorf("first connect failure = %v; want a *ConnectError without LoginAttempted", err)
	}
}

// A connect failure at startup is reported once: Authenticate returns it for
// the caller to report, and does not log it as well. Later connects, whose
// errors reach no startup report, are logged.
func TestConnectFailureIsLoggedOnce(t *testing.T) {
	clearProxyEnv(t)
	logs := logsOf(t)
	c, err := NewClientWithOptions(closedAddr(t), testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Authenticate()
	if err == nil {
		t.Fatal("Authenticate succeeded")
	}
	if strings.Contains(logs.String(), "connection refused") || strings.Contains(logs.String(), "Connection failed") {
		t.Errorf("Authenticate logged the failure it returns:\n%s", logs)
	}

	if _, err := c.SessionGeneration(); err == nil {
		t.Fatal("SessionGeneration succeeded")
	}
	if n := strings.Count(logs.String(), "Connection failed"); n != 1 {
		t.Errorf("a failed connect after startup was logged %d times, want 1:\n%s", n, logs)
	}
}

// corp:3128:3128 is rejected for the same reason whatever else the proxy
// setting holds: scheme, credentials or none.
func TestExtraColonProxyReasonIsConsistent(t *testing.T) {
	target := &url.URL{Scheme: "wss", Host: "truenas.example:443"}
	const want = "invalid proxy URL in HTTPS_PROXY: the host is not a host name or bracketed IPv6 address"
	for _, raw := range []string{
		"corp:3128:3128",
		"alice:s3cretpw@corp:3128:3128",
		"alice@corp:3128:3128",
		"http://corp:3128:3128",
		"http://alice:s3cretpw@corp:3128:3128",
		"https://alice:s3cretpw@corp:3128:3128",
		"socks5://corp:3128:3128",
		"socks5://alice:s3cretpw@corp:3128:3128",
		"socks5h://alice:s3cretpw@corp:3128:3128/path",
		"socks5://alice:s3cretpw@corp:3128:3128?x=1",
		"socks5://corp:1:2:3",
	} {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", raw)
		_, err := (&Client{}).proxyFor(target)
		if err == nil || err.Error() != want {
			t.Errorf("%q: error = %v; want %q", raw, err, want)
		}
	}
	// A single colon with a bad port, and bracketed IPv6, keep their own reasons.
	for raw, reason := range map[string]string{
		"http://corp:portx":                  "the port is not a number",
		"socks5://alice:s3cretpw@corp:portx": "the port is not a number",
		"http://alice:s3cretpw@[::1]:portx":  "the port is not a number",
	} {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", raw)
		_, err := (&Client{}).proxyFor(target)
		if err == nil || !strings.HasSuffix(err.Error(), ": "+reason) {
			t.Errorf("%q: error = %v; want reason %q", raw, err, reason)
		}
	}
}

// Only something that looks like a scheme is echoed back, and never user
// info or other input.
func TestUnsupportedSchemeEchoIsRestricted(t *testing.T) {
	const generic = "invalid TrueNAS host or URL: not a host name or a wss://host[:port] URL"
	for _, tc := range []struct {
		endpoint string
		want     string // the full error
	}{
		{"ftp://truenas.example", `invalid TrueNAS host or URL: unsupported URL scheme "ftp": use the host name alone or a wss://host[:port] URL`},
		{"FTP://truenas.example", `invalid TrueNAS host or URL: unsupported URL scheme "FTP": use the host name alone or a wss://host[:port] URL`},
		{"a+b-c.d1://truenas.example", `invalid TrueNAS host or URL: unsupported URL scheme "a+b-c.d1": use the host name alone or a wss://host[:port] URL`},
		{"a234567890123456://truenas.example", `invalid TrueNAS host or URL: unsupported URL scheme "a234567890123456": use the host name alone or a wss://host[:port] URL`}, // 16 characters
		{"a2345678901234567://truenas.example", generic}, // 17
		{"1ftp://truenas.example", generic},              // starts with a digit
		{"+ftp://truenas.example", generic},
		{"fт://truenas.example", generic},
		{"f\tp://truenas.example", generic},
		{"alice:s3cretpw@host://x", generic},
		{"alice:s3cretpw://truenas.example", generic},
		{"s3cretpw@alice://truenas.example", generic},
		{"ftp://alice:s3cretpw@truenas.example", `invalid TrueNAS host or URL: unsupported URL scheme "ftp": use the host name alone or a wss://host[:port] URL`},
		{"://truenas.example", generic},
	} {
		c, err := NewClientWithOptions(tc.endpoint, testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser})
		if err != nil {
			t.Fatal(err)
		}
		err = c.ValidateEndpoint()
		if err == nil || err.Error() != tc.want {
			t.Errorf("%q: error = %v; want %q", tc.endpoint, err, tc.want)
			continue
		}
		for _, leak := range []string{"alice", "s3cretpw"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%q: error echoes %q", tc.endpoint, leak)
			}
		}
	}
}

// TestEndpointErrorsNeverEchoQueryFragmentOrUserinfo checks that no error from
// the endpoint (ValidateEndpoint or a connect attempt) quotes the secret parts
// of the input, so the README claim that the URL is logged without query or
// fragment holds for errors too.
func TestEndpointErrorsNeverEchoQueryFragmentOrUserinfo(t *testing.T) {
	clearProxyEnv(t)
	logs := logsOf(t)
	const secret = "SECRETTOKEN"
	for _, endpoint := range []string{
		"h.test?token=" + secret,
		"h.test/path?token=" + secret + "#frag" + secret,
		"h.test#" + secret,
		"h.test;token=" + secret,
		"h.test:443;token=" + secret,
		"h.test\\token=" + secret,
		"h.test,token=" + secret,
		"h.test&token=" + secret,
		"h.test:443,token=" + secret,
		"h.test:443?token=" + secret,
		"[::1]x;token=" + secret,
		"wss://h.test:443,token=" + secret,
		"wss://h.test;token=" + secret + "/api/current",
		"wss://h.test,token=" + secret,
		"wss://h.test&token=" + secret,
		"wss://h.test:443;token=" + secret,
		"h.test:99999?token=" + secret,
		"h.test:443:443?token=" + secret,
		"user:" + secret + "@h.test",
		"user:" + secret + "@h.test?token=" + secret,
		"wss://user:" + secret + "@h.test/api/current?token=" + secret,
		"wss://h.test:0/api/current?token=" + secret + "#" + secret,
		"wss://h.test:443:443/?token=" + secret,
		"wss://?token=" + secret,
		"wss://h.test:/?token=" + secret,
		"ws://h.test?token=" + secret,
		"https://h.test?token=" + secret,
		"ftp://h.test?token=" + secret,
		"x" + secret + ":y@z://h.test?token=" + secret,
	} {
		c, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser})
		if err != nil {
			t.Fatal(err)
		}
		verr := c.ValidateEndpoint()
		if verr == nil {
			t.Errorf("%q: ValidateEndpoint = nil, want an error", endpoint)
			continue
		}
		if strings.Contains(verr.Error(), secret) || strings.Contains(verr.Error(), "token=") {
			t.Errorf("%q: error echoes the query, fragment or userinfo: %v", endpoint, verr)
		}
		_, cerr := c.Call("core.ping")
		if cerr != nil && (strings.Contains(cerr.Error(), secret) || strings.Contains(cerr.Error(), "token=")) {
			t.Errorf("%q: connect error echoes the query, fragment or userinfo: %v", endpoint, cerr)
		}
	}

	// A valid URL with a query that fails to connect: neither the error nor
	// the log carries the query, fragment or userinfo.
	c2, err := NewClientWithOptions("wss://"+closedAddr(t)+"/api/current?token="+secret+"#"+secret, testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Call("core.ping"); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("connect error = %v, want an error without the secret", err)
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "token=") {
		t.Errorf("log echoes the query, fragment or userinfo:\n%s", logs.String())
	}

	// A bare host still names the host, and only the host.
	c, _ := NewClientWithOptions("h.test/path?token="+secret, testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser})
	if err := c.ValidateEndpoint(); err == nil || !strings.Contains(err.Error(), `"h.test"`) {
		t.Errorf("error should quote the host alone, got %v", err)
	}
}

func TestBareHostErrorCutsAtEverySeparator(t *testing.T) {
	clearProxyEnv(t)
	for endpoint, want := range map[string]string{
		"h.test;token=X":     `"h.test": a bare host cannot include a path, query or fragment`,
		"h.test:8443;x=y":    `"h.test:8443": a bare host cannot include a path, query or fragment`,
		"h.test\\x":          `"h.test": a bare host cannot include a path, query or fragment`,
		"h.test?a=b":         `"h.test": a bare host cannot include a path, query or fragment`,
		"h.test#f":           `"h.test": a bare host cannot include a path, query or fragment`,
		"h.test,token=X":     `"h.test": not a host name or IPv6 address`,
		"h.test:443,token=X": "bad port: not a number",
	} {
		c, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser})
		if err != nil {
			t.Fatal(err)
		}
		err = c.ValidateEndpoint()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want it to contain %q", endpoint, err, want)
		}
	}
}

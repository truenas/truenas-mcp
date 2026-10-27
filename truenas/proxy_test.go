package truenas

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// testProxy is an HTTP CONNECT or SOCKS5 proxy that sends every tunnel to
// the fake TrueNAS, whatever host was asked for, and records what the
// client requested. It plays the part of a proxy (e.g. userspace Tailscale)
// that can reach hosts the client cannot.
type testProxy struct {
	net.Listener
	backend  string // address every tunnel is connected to
	user     string // required credentials, if set
	password string

	mu       sync.Mutex
	targets  []string // host:port the client asked for
	addrType byte     // SOCKS5 address type of the last request
}

func (p *testProxy) url(scheme string, withAuth bool) string {
	u := url.URL{Scheme: scheme, Host: p.Addr().String()}
	if withAuth {
		u.User = url.UserPassword(p.user, p.password)
	}
	return u.String()
}

func (p *testProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

func (p *testProxy) socksAddrType() byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addrType
}

func startProxy(t *testing.T, f *fakeTrueNAS, serve func(*testProxy, net.Conn)) *testProxy {
	t.Helper()
	return startProxyAuth(t, f, serve, "proxyuser", "proxysecret7")
}

// startProxyAuth starts a proxy that requires user and password.
func startProxyAuth(t *testing.T, f *fakeTrueNAS, serve func(*testProxy, net.Conn), user, password string) *testProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &testProxy{Listener: l, backend: f.Listener.Addr().String(), user: user, password: password}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serve(p, conn)
		}
	}()
	return p
}

func (p *testProxy) record(target string) {
	p.mu.Lock()
	p.targets = append(p.targets, target)
	p.mu.Unlock()
}

// tunnel connects client to the backend and copies both ways.
func (p *testProxy) tunnel(client net.Conn, buffered io.Reader) {
	backend, err := net.Dial("tcp", p.backend)
	if err != nil {
		client.Close()
		return
	}
	go func() {
		io.Copy(backend, buffered)
		backend.Close()
	}()
	io.Copy(client, backend)
	client.Close()
}

func serveHTTPConnect(p *testProxy, conn net.Conn) {
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		conn.Close()
		return
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.password))
	if req.Header.Get("Proxy-Authorization") != want {
		io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
		conn.Close()
		return
	}
	p.record(req.Host)
	io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	p.tunnel(conn, br)
}

func serveSOCKS5(p *testProxy, conn net.Conn) {
	br := bufio.NewReader(conn)
	fail := func() { conn.Close() }
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 5 {
		fail()
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil || !bytes.Contains(methods, []byte{2}) {
		conn.Write([]byte{5, 0xff}) // username/password required
		fail()
		return
	}
	conn.Write([]byte{5, 2})
	// RFC 1929 username/password
	readField := func() string {
		n, _ := br.ReadByte()
		b := make([]byte, n)
		io.ReadFull(br, b)
		return string(b)
	}
	if v, _ := br.ReadByte(); v != 1 {
		fail()
		return
	}
	if readField() != p.user || readField() != p.password {
		conn.Write([]byte{1, 1})
		fail()
		return
	}
	conn.Write([]byte{1, 0})

	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[1] != 1 {
		fail()
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		io.ReadFull(br, b)
		host = net.IP(b).String()
	case 3:
		host = readField()
	case 4:
		b := make([]byte, 16)
		io.ReadFull(br, b)
		host = net.IP(b).String()
	}
	port := make([]byte, 2)
	io.ReadFull(br, port)
	p.mu.Lock()
	p.addrType = req[3]
	p.mu.Unlock()
	p.record(net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))))
	conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	p.tunnel(conn, br)
}

// nonLoopbackIP returns an IPv4 address of this machine that is not
// loopback, or skips the test.
func nonLoopbackIP(t *testing.T) string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return ""
}

// forwardTo listens on addr and forwards every connection to backend,
// returning the address it listens on.
func forwardTo(t *testing.T, addr, backend string) string {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	t.Cleanup(func() { l.Close() })
	p := &testProxy{backend: backend}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go p.tunnel(conn, conn)
		}
	}()
	return l.Addr().String()
}

// clearProxyEnv unsets every proxy variable for the test.
func clearProxyEnv(t *testing.T) {
	for _, name := range []string{"TRUENAS_PROXY", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
}

// proxiedClient talks to the fake as https://example.com:<port>, a name the
// fake's certificate covers but that only the test proxies route to it. The
// certificate is verified, so TLS must run end to end through the proxy.
func proxiedClient(t *testing.T, f *fakeTrueNAS, host string, opts Options) (*Client, *syncBuffer) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(f.Certificate())
	return proxiedClientTLS(t, f, host, opts, &tls.Config{RootCAs: pool})
}

func proxiedClientTLS(t *testing.T, f *fakeTrueNAS, host string, opts Options, cfg *tls.Config) (*Client, *syncBuffer) {
	t.Helper()
	opts.APIUser = testAPIUser
	c, err := NewClientWithOptions(f.wsURL(host), testAPIKey, cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	buf := &syncBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() {
		c.Close()
		log.SetOutput(os.Stderr)
	})
	return c, buf
}

// syncBuffer is a log destination that is safe to read while server and
// client goroutines are still logging to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func assertNoLeak(t *testing.T, p *testProxy, err error, logs fmt.Stringer) {
	t.Helper()
	for _, secret := range []string{p.user, p.password} {
		if secret == "" {
			continue
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("proxy credential %q in error: %v", secret, err)
		}
		if strings.Contains(logs.String(), secret) {
			t.Errorf("proxy credential %q in logs:\n%s", secret, logs.String())
		}
	}
}

func TestHTTPConnectProxy(t *testing.T) {
	clearProxyEnv(t)
	f := newFakeTrueNAS(t, nil)
	p := startProxy(t, f, serveHTTPConnect)
	t.Setenv("HTTPS_PROXY", p.url("http", true))
	c, logs := proxiedClient(t, f, "example.com", Options{})

	_, err := c.Call("system.info")
	if err != nil {
		t.Fatalf("Call through the HTTP proxy: %v", err)
	}
	port := strconv.Itoa(f.Listener.Addr().(*net.TCPAddr).Port)
	if got := p.seen(); len(got) != 1 || got[0] != "example.com:"+port {
		t.Errorf("proxy saw CONNECT %v, want example.com:%s", got, port)
	}
	if !strings.Contains(logs.String(), "via proxy http://redacted@"+p.Addr().String()) {
		t.Errorf("proxy not logged (redacted):\n%s", logs.String())
	}
	assertNoLeak(t, p, err, logs)
}

func TestSOCKS5hProxyFromAllProxy(t *testing.T) {
	clearProxyEnv(t)
	f := newFakeTrueNAS(t, nil)
	p := startProxy(t, f, serveSOCKS5)
	t.Setenv("ALL_PROXY", p.url("socks5h", true))
	c, logs := proxiedClient(t, f, "example.com", Options{})

	_, err := c.Call("system.info")
	if err != nil {
		t.Fatalf("Call through the SOCKS5 proxy: %v", err)
	}
	if got := p.seen(); len(got) != 1 || !strings.HasPrefix(got[0], "example.com:") {
		t.Errorf("proxy saw %v, want example.com", got)
	}
	if at := p.socksAddrType(); at != 3 {
		t.Errorf("SOCKS5 address type %d, want 3 (domain name, resolved by the proxy)", at)
	}
	assertNoLeak(t, p, err, logs)
}

func TestProxyOptionOverridesEnvironment(t *testing.T) {
	clearProxyEnv(t)
	f := newFakeTrueNAS(t, nil)
	p := startProxy(t, f, serveSOCKS5)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	c, logs := proxiedClient(t, f, "example.com", Options{Proxy: p.url("socks5", true)})

	if _, err := c.Call("system.info"); err != nil {
		t.Fatalf("Call through --proxy: %v", err)
	}
	if len(p.seen()) != 1 {
		t.Errorf("--proxy not used")
	}
	assertNoLeak(t, p, nil, logs)
}

func TestNoProxyBypassesProxy(t *testing.T) {
	clearProxyEnv(t)
	f := newFakeTrueNAS(t, nil)
	p := startProxy(t, f, serveHTTPConnect)
	t.Setenv("HTTPS_PROXY", p.url("http", true))
	// Loopback addresses are always reached directly, so serve the fake on
	// a non-loopback address of this machine to see NO_PROXY at work.
	ip := nonLoopbackIP(t)
	direct := forwardTo(t, net.JoinHostPort(ip, "0"), p.backend)
	t.Setenv("NO_PROXY", "other.example,"+ip)
	c, err := NewClientWithOptions("wss://"+direct+apiPath, testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	logs := &syncBuffer{}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)
	if _, err := c.Call("system.info"); err != nil {
		t.Fatalf("direct Call for a NO_PROXY host: %v", err)
	}
	if got := p.seen(); len(got) != 0 {
		t.Errorf("proxy used for a NO_PROXY host: %v", got)
	}
	if strings.Contains(logs.String(), "via proxy") {
		t.Errorf("logs claim a proxy for a NO_PROXY host:\n%s", logs.String())
	}

	// Loopback addresses are always direct, as with net/http.
	c2 := insecureClient(t, f, Options{Proxy: p.url("http", true)})
	if _, err := c2.Call("system.info"); err != nil {
		t.Fatal(err)
	}
	if got := p.seen(); len(got) != 0 {
		t.Errorf("proxy used for loopback: %v", got)
	}
}

func TestProxyCredentialsNotLeaked(t *testing.T) {
	clearProxyEnv(t)
	f := newFakeTrueNAS(t, nil)
	httpProxy := startProxy(t, f, serveHTTPConnect)
	socksProxy := startProxy(t, f, serveSOCKS5)
	wrong := func(p *testProxy, scheme string) string {
		u := url.URL{Scheme: scheme, Host: p.Addr().String(), User: url.UserPassword(p.user, p.password+"x")}
		return u.String()
	}
	for _, tc := range []struct {
		p     *testProxy
		proxy string
	}{
		{httpProxy, wrong(httpProxy, "http")},             // rejected with 407
		{socksProxy, wrong(socksProxy, "socks5h")},        // rejected by RFC 1929 auth
		{httpProxy, "http://proxyuser:proxysecret7@[bad"}, // unparseable
		{httpProxy, "https://proxyuser:proxysecret7@proxy.example:3128"},
		{httpProxy, "ftp://proxyuser:proxysecret7@proxy.example:21"},
	} {
		c, logs := proxiedClient(t, f, "example.com", Options{Proxy: tc.proxy})
		err := c.Authenticate()
		if err == nil {
			t.Errorf("connected with bad proxy settings %d", len(tc.proxy))
			continue
		}
		assertNoLeak(t, tc.p, err, logs)
		assertNoLeak(t, &testProxy{user: "proxyuser", password: "proxysecret7x"}, err, logs)
	}
}

func TestProxyForSelection(t *testing.T) {
	target, _ := url.Parse("wss://truenas.example:443/api/current")
	loopback, _ := url.Parse("wss://127.0.0.1:443/api/current")
	for _, tc := range []struct {
		env     map[string]string
		option  string
		target  *url.URL // default target
		want    string   // "" = direct
		wantErr string   // substring of the error
	}{
		{env: nil, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1"}, want: "http://a:1"},
		{env: map[string]string{"https_proxy": "a:1"}, want: "http://a:1"}, // scheme defaults to http
		{env: map[string]string{"HTTPS_PROXY": "http://a"}, want: "http://a:80"},
		{env: map[string]string{"HTTP_PROXY": "http://a:1"}, want: ""}, // wss is HTTPS traffic
		{env: map[string]string{"ALL_PROXY": "socks5h://b:2"}, want: "socks5h://b:2"},
		{env: map[string]string{"all_proxy": "SOCKS5://b"}, want: "socks5://b:1080"},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "ALL_PROXY": "socks5h://b:2"}, want: "http://a:1"},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "TRUENAS_PROXY": "socks5h://t:4"}, want: "socks5h://t:4"},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "TRUENAS_PROXY": "socks5h://t:4"}, option: "socks5h://c:3", want: "socks5h://c:3"},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1"}, option: "socks5h://c:3", want: "socks5h://c:3"},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1"}, option: "none", want: ""},
		{env: map[string]string{"ALL_PROXY": "https://a:1", "TRUENAS_PROXY": "NONE"}, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "NO_PROXY": "truenas.example"}, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "no_proxy": ".example"}, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "NO_PROXY": "*"}, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "NO_PROXY": "other.example"}, want: "http://a:1"},
		{env: map[string]string{"NO_PROXY": "truenas.example"}, option: "socks5h://c:3", want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://u:pw@a:1/x?token=tk#f"}, want: "http://redacted@a:1"},
		// Unusable settings for hosts that bypass the proxy connect directly.
		{env: map[string]string{"HTTPS_PROXY": "https://u:pw@corp:3128", "NO_PROXY": "truenas.example"}, want: ""},
		{env: map[string]string{"ALL_PROXY": "socks4://corp:1080", "NO_PROXY": "*"}, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://u:pw@[bad", "NO_PROXY": ".example"}, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "https://u:pw@corp:3128"}, target: loopback, want: ""},
		{option: "socks4://u:pw@corp:1080", target: loopback, want: ""},
		// ...and fail, naming only the source, when the proxy would be used.
		{env: map[string]string{"HTTPS_PROXY": "https://u:pw@corp:3128"}, wantErr: "unsupported proxy scheme https in HTTPS_PROXY"},
		{env: map[string]string{"ALL_PROXY": "socks4://u:pw@corp:1080"}, wantErr: `unsupported proxy scheme "socks4" in ALL_PROXY`},
		{env: map[string]string{"TRUENAS_PROXY": "http://u:pw@[bad"}, wantErr: "invalid proxy URL in TRUENAS_PROXY"},
		{option: "socks4://u:pw@corp:1080", wantErr: `unsupported proxy scheme "socks4" in --proxy`},
		{option: "http://u:pw@corp:99999", wantErr: "invalid proxy port in --proxy"},
		{option: "http://corp:08080", want: "http://corp:8080"}, // ports are normalised
		{option: "socks5h://[::1]:01055", want: "socks5h://[::1]:1055"},
		{option: "http://corp:3128:3128", wantErr: "invalid proxy URL in --proxy"},
		{option: "socks5h://corp:0", wantErr: "invalid proxy port in --proxy"},
		{env: map[string]string{"HTTPS_PROXY": "corp:70000"}, wantErr: "invalid proxy port in HTTPS_PROXY"},
	} {
		clearProxyEnv(t)
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		tgt := tc.target
		if tgt == nil {
			tgt = target
		}
		c := &Client{proxy: tc.option}
		p, err := c.proxyFor(tgt)
		got := ""
		if p != nil {
			got = p.String()
		}
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("env %v option %q: got %q, %v; want error %q", tc.env, tc.option, got, err, tc.wantErr)
			} else if strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "corp") {
				t.Errorf("error echoes the proxy URL: %v", err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("env %v option %q target %s: got %q, %v; want %q", tc.env, tc.option, tgt.Host, got, err, tc.want)
		}
	}
}

func TestUnsupportedProxySchemeEchoIsCapped(t *testing.T) {
	target := &url.URL{Scheme: "wss", Host: "truenas.example:443"}
	for _, tc := range []struct {
		option string
		want   string // a substring of the error
		absent string // must not appear
	}{
		{"socks4://h:1", `unsupported proxy scheme "socks4" in --proxy`, ""},
		{"a23456789012345://h:1", `unsupported proxy scheme "a23456789012345" in --proxy`, ""},  // 15 chars after the first: allowed
		{"a2345678901234567://h:1", "unsupported proxy scheme in --proxy", "a2345678901234567"}, // 18 chars: too long
		{"averyverylongschemenamethatkeepsgoing+token.X://h:1", "unsupported proxy scheme in --proxy", "token"},
	} {
		clearProxyEnv(t)
		_, err := (&Client{proxy: tc.option}).proxyFor(target)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want it to contain %q", tc.option, err, tc.want)
			continue
		}
		if tc.absent != "" && strings.Contains(err.Error(), tc.absent) {
			t.Errorf("%q: error echoes the scheme: %v", tc.option, err)
		}
	}
}

func TestValidateProxy(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		env      map[string]string
		option   string
		wantErr  string
	}{
		{endpoint: "truenas.example", env: map[string]string{"HTTPS_PROXY": "https://u:pw@corp:3128"}, wantErr: "HTTPS_PROXY"},
		{endpoint: "truenas.example", env: map[string]string{"TRUENAS_PROXY": "socks4://u:pw@corp:1080"}, wantErr: "TRUENAS_PROXY"},
		{endpoint: "truenas.example", option: "ftp://u:pw@corp", wantErr: "--proxy"},
		{endpoint: "192.0.2.10", env: map[string]string{"ALL_PROXY": "http://u:pw@[bad"}, wantErr: "ALL_PROXY"},
		{endpoint: "truenas.example", env: map[string]string{"HTTPS_PROXY": "https://u:pw@corp:3128", "NO_PROXY": "truenas.example"}},
		{endpoint: "192.0.2.10", env: map[string]string{"ALL_PROXY": "socks4://corp:1080", "NO_PROXY": "192.0.2.0/24"}},
		{endpoint: "127.0.0.1", env: map[string]string{"HTTPS_PROXY": "https://u:pw@corp:3128"}},
		{endpoint: "truenas.example", env: map[string]string{"HTTPS_PROXY": "https://u:pw@corp:3128"}, option: "none"},
		{endpoint: "truenas.example", env: map[string]string{"HTTPS_PROXY": "socks5h://u:pw@corp:1055"}},
	} {
		clearProxyEnv(t)
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		c, err := NewClientWithOptions(tc.endpoint, testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser, Proxy: tc.option})
		if err != nil {
			t.Fatal(err)
		}
		err = c.ValidateProxy()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s env %v option %q: unexpected error %v", tc.endpoint, tc.env, tc.option, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s env %v option %q: got %v, want error naming %s", tc.endpoint, tc.env, tc.option, err, tc.wantErr)
		case err != nil && (strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "corp")):
			t.Errorf("error echoes the proxy URL: %v", err)
		}
	}
}

func TestUnreachableProxyIsNamed(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := l.Addr().String()
	l.Close()
	for _, tc := range []struct{ env, scheme string }{
		{"HTTPS_PROXY", "http"},
		{"ALL_PROXY", "socks5h"},
	} {
		clearProxyEnv(t)
		t.Setenv(tc.env, tc.scheme+"://proxyuser:proxysecret7@"+dead)
		c, logs := proxiedClient(t, f, "example.com", Options{})
		err := c.Authenticate()
		want := "connecting via proxy " + tc.scheme + "://redacted@" + dead + " from " + tc.env + ": proxy unreachable: "
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want it to contain %q", tc.env, err, want)
		}
		assertNoLeak(t, &testProxy{user: "proxyuser", password: "proxysecret7"}, err, logs)
	}
}

func TestHTTPProxyUserWithoutPassword(t *testing.T) {
	clearProxyEnv(t)
	f := newFakeTrueNAS(t, nil)
	p := startProxyAuth(t, f, serveHTTPConnect, "proxytoken9", "")
	c, logs := proxiedClient(t, f, "example.com", Options{Proxy: "http://proxytoken9@" + p.Addr().String()})
	if _, err := c.Call("system.info"); err != nil {
		t.Fatalf("Call with a user-only proxy credential: %v", err)
	}
	assertNoLeak(t, &testProxy{user: "proxytoken9"}, nil, logs)
}

// TLS is end to end: the proxies tunnel to the fake whatever host is asked
// for, so only certificate verification can stop these connections.
func TestTLSVerifiedThroughProxy(t *testing.T) {
	clearProxyEnv(t)
	f := newFakeTrueNAS(t, nil)
	pool := x509.NewCertPool()
	pool.AddCert(f.Certificate())
	for _, px := range []struct {
		scheme string
		serve  func(*testProxy, net.Conn)
	}{
		{"http", serveHTTPConnect},
		{"socks5h", serveSOCKS5},
	} {
		p := startProxy(t, f, px.serve)
		for _, tc := range []struct {
			name, host string
			cfg        *tls.Config
			hint       string
		}{
			{"untrusted certificate", "example.com", &tls.Config{RootCAs: x509.NewCertPool()}, "--tls-ca <cert.pem>"},
			{"name mismatch", "truenas.example.net", &tls.Config{RootCAs: pool}, "not valid for truenas.example.net"},
		} {
			c, logs := proxiedClientTLS(t, f, tc.host, Options{Proxy: p.url(px.scheme, true)}, tc.cfg)
			before := len(p.seen())
			err := c.Authenticate()
			if err == nil {
				t.Errorf("%s through %s proxy: connected", tc.name, px.scheme)
				continue
			}
			if len(p.seen()) != before+1 {
				t.Errorf("%s through %s proxy: proxy not used", tc.name, px.scheme)
			}
			if !strings.Contains(err.Error(), tc.hint) {
				t.Errorf("%s through %s proxy: got %v, want hint %q", tc.name, px.scheme, err, tc.hint)
			}
			assertNoLeak(t, p, err, logs)
		}
	}
}

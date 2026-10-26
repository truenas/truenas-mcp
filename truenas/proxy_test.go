package truenas

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
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

func startProxy(t *testing.T, f *fakeTrueNAS, serve func(*testProxy, net.Conn)) *testProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &testProxy{Listener: l, backend: f.Listener.Addr().String(), user: "proxyuser", password: "proxysecret7"}
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
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
}

// proxiedClient talks to the fake as https://example.com:<port>, a name the
// fake's certificate covers but that only the test proxies route to it. The
// certificate is verified, so TLS must run end to end through the proxy.
func proxiedClient(t *testing.T, f *fakeTrueNAS, host string, opts Options) (*Client, *bytes.Buffer) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(f.Certificate())
	opts.APIUser = testAPIUser
	c, err := NewClientWithOptions(f.wsURL(host), testAPIKey, &tls.Config{RootCAs: pool}, opts)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() {
		c.Close()
		log.SetOutput(os.Stderr)
	})
	return c, &buf
}

func assertNoLeak(t *testing.T, p *testProxy, err error, logs *bytes.Buffer) {
	t.Helper()
	for _, secret := range []string{p.user, p.password} {
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
	if p.addrType != 3 {
		t.Errorf("SOCKS5 address type %d, want 3 (domain name, resolved by the proxy)", p.addrType)
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
	t.Setenv("NO_PROXY", "other.example,.invalid")
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
	var logs bytes.Buffer
	log.SetOutput(&logs)
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
	for _, tc := range []struct {
		env    map[string]string
		option string
		want   string // "" = direct
	}{
		{env: nil, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1"}, want: "http://a:1"},
		{env: map[string]string{"https_proxy": "a:1"}, want: "http://a:1"}, // scheme defaults to http
		{env: map[string]string{"HTTP_PROXY": "http://a:1"}, want: ""},     // wss is HTTPS traffic
		{env: map[string]string{"ALL_PROXY": "socks5h://b:2"}, want: "socks5h://b:2"},
		{env: map[string]string{"all_proxy": "SOCKS5://b:2"}, want: "socks5://b:2"},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "ALL_PROXY": "socks5h://b:2"}, want: "http://a:1"},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1"}, option: "socks5h://c:3", want: "socks5h://c:3"},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "NO_PROXY": "truenas.example"}, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "no_proxy": ".example"}, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "NO_PROXY": "*"}, want: ""},
		{env: map[string]string{"HTTPS_PROXY": "http://a:1", "NO_PROXY": "other.example"}, want: "http://a:1"},
		{env: map[string]string{"NO_PROXY": "truenas.example"}, option: "socks5h://c:3", want: ""},
	} {
		clearProxyEnv(t)
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		c := &Client{proxy: tc.option}
		p, err := c.proxyFor(target)
		got := ""
		if p != nil {
			got = p.String()
		}
		if err != nil || got != tc.want {
			t.Errorf("env %v option %q: got %q, %v; want %q", tc.env, tc.option, got, err, tc.want)
		}
	}

	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "https://u:pw@a:1")
	if _, err := (&Client{}).proxyFor(target); err == nil || !strings.Contains(err.Error(), "HTTPS_PROXY") {
		t.Errorf("https proxy: got %v, want a refusal naming HTTPS_PROXY", err)
	} else if strings.Contains(err.Error(), "pw") {
		t.Errorf("error leaks the proxy password: %v", err)
	}
}

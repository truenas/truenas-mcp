package truenas

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// noProxyValue in --proxy or TRUENAS_PROXY disables any proxy from the
// environment.
const noProxyValue = "none"

// proxyChoice is the proxy a connection goes through and where it came from.
type proxyChoice struct {
	url    *url.URL
	source string // e.g. "HTTPS_PROXY"; never the URL itself
}

// String returns the proxy without user info, path or query, for logs and
// errors.
func (p *proxyChoice) String() string {
	s := p.url.Scheme + "://"
	if p.url.User != nil {
		s += "redacted@"
	}
	return s + p.url.Host
}

// proxyFor returns the proxy to reach target through, or nil to connect
// directly. The proxy is Options.Proxy if set, else TRUENAS_PROXY, else
// HTTPS_PROXY, else ALL_PROXY (upper or lower case); "none" in the first two
// disables proxying. NO_PROXY is honoured in every case, and localhost and
// loopback addresses are always reached directly. A proxy setting is only
// parsed and validated when it applies to target, so an unusable inherited
// value does not break hosts that bypass it.
//
// Supported schemes are http (CONNECT), socks5 and socks5h. With either
// SOCKS scheme, host names are resolved by the proxy. TLS runs end to end
// through the tunnel, so the TrueNAS certificate is verified as usual.
//
// Errors never include the proxy URL, which may carry credentials.
func (c *Client) proxyFor(target *url.URL) (*proxyChoice, error) {
	raw, source := c.proxy, "--proxy"
	if raw == "" {
		raw, source = getenvAny("TRUENAS_PROXY")
	}
	if strings.EqualFold(raw, noProxyValue) {
		return nil, nil
	}
	if raw == "" {
		raw, source = getenvAny("HTTPS_PROXY", "https_proxy")
	}
	if raw == "" {
		raw, source = getenvAny("ALL_PROXY", "all_proxy")
	}
	if raw == "" || bypassProxy(target) {
		return nil, nil
	}

	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	p, err := url.Parse(raw)
	if err != nil || p.Hostname() == "" {
		return nil, fmt.Errorf("invalid proxy URL in %s", source)
	}
	p.Scheme = strings.ToLower(p.Scheme)
	port := "1080"
	switch p.Scheme {
	case "http":
		port = "80"
	case "socks5", "socks5h":
	case "https":
		return nil, fmt.Errorf("unsupported proxy scheme https in %s: use http://, socks5:// or socks5h://", source)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q in %s: use http://, socks5:// or socks5h://", p.Scheme, source)
	}
	if p.Port() == "" {
		p.Host = net.JoinHostPort(p.Hostname(), port)
	}
	return &proxyChoice{url: p, source: source}, nil
}

// bypassProxy reports whether NO_PROXY, or httpproxy's localhost and
// loopback rule, exempts target. httpproxy is given a placeholder proxy
// rather than ours: it does not know socks5h and quotes bad URLs in errors.
func bypassProxy(target *url.URL) bool {
	noProxy, _ := getenvAny("NO_PROXY", "no_proxy")
	check := httpproxy.Config{HTTPSProxy: "http://proxy.invalid", NoProxy: noProxy}
	t := *target
	t.Scheme = "https"
	via, _ := check.ProxyFunc()(&t)
	return via == nil
}

// ValidateProxy checks the proxy configuration for the TrueNAS host, so a
// bad --proxy, TRUENAS_PROXY, HTTPS_PROXY or ALL_PROXY value fails at
// startup rather than on first connect. Settings the host bypasses (via
// NO_PROXY or because it is loopback) are not checked. The environment is
// read again on every connect, which validates it again.
func (c *Client) ValidateProxy() error {
	u, err := c.endpointURL()
	if err != nil {
		return err
	}
	_, err = c.proxyFor(u)
	return err
}

// dialerProxy converts p for gorilla/websocket. Its built-in SOCKS5 dialer
// already sends host names to the proxy (socks5h behaviour) but only knows
// the socks5 scheme name, and its CONNECT dialer only sends credentials
// when a password is set, so a bare user name gets an empty password, as
// net/http does.
func dialerProxy(p *url.URL) *url.URL {
	d := *p
	if d.Scheme == "socks5h" {
		d.Scheme = "socks5"
	}
	if d.User != nil {
		if _, ok := d.User.Password(); !ok {
			d.User = url.UserPassword(d.User.Username(), "")
		}
	}
	return &d
}

func getenvAny(names ...string) (string, string) {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, name
		}
	}
	return "", ""
}

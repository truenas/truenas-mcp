package truenas

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// proxyFor returns the proxy to reach target through, or nil to connect
// directly. The proxy is Options.Proxy if set, else HTTPS_PROXY, else
// ALL_PROXY (upper or lower case). NO_PROXY is honoured in every case, and
// localhost and loopback addresses are always reached directly.
//
// Supported schemes are http (CONNECT), socks5 and socks5h. With either
// SOCKS scheme, host names are resolved by the proxy. TLS runs end to end
// through the tunnel, so the TrueNAS certificate is verified as usual.
//
// Errors never include the proxy URL, which may carry credentials.
func (c *Client) proxyFor(target *url.URL) (*url.URL, error) {
	raw, source := c.proxy, "--proxy"
	if raw == "" {
		raw, source = getenvAny("HTTPS_PROXY", "https_proxy")
	}
	if raw == "" {
		raw, source = getenvAny("ALL_PROXY", "all_proxy")
	}
	if raw == "" {
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
	switch p.Scheme {
	case "http", "socks5", "socks5h":
	case "https":
		return nil, fmt.Errorf("unsupported proxy scheme https in %s: use http://, socks5:// or socks5h://", source)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q in %s: use http://, socks5:// or socks5h://", p.Scheme, source)
	}

	// Let httpproxy apply NO_PROXY (and its localhost rule) without handing
	// it our URL: it does not know socks5h and quotes bad URLs in errors.
	noProxy, _ := getenvAny("NO_PROXY", "no_proxy")
	check := httpproxy.Config{HTTPSProxy: "http://proxy.invalid", NoProxy: noProxy}
	t := *target
	t.Scheme = "https"
	if via, _ := check.ProxyFunc()(&t); via == nil {
		return nil, nil
	}
	return p, nil
}

// dialerProxy converts p for gorilla/websocket, whose built-in SOCKS5
// dialer already sends host names to the proxy (socks5h behaviour) but only
// knows the socks5 scheme name.
func dialerProxy(p *url.URL) *url.URL {
	d := *p
	if d.Scheme == "socks5h" {
		d.Scheme = "socks5"
	}
	return &d
}

// redactProxy returns p as text with any user name and password removed.
func redactProxy(p *url.URL) string {
	r := *p
	if r.User != nil {
		r.User = url.User("redacted")
	}
	return r.String()
}

func getenvAny(names ...string) (string, string) {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, name
		}
	}
	return "", ""
}

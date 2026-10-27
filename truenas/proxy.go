package truenas

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
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
	// "none" is only an opt-out in --proxy and TRUENAS_PROXY. In the standard
	// variables it would be taken as a proxy host named "none", and quietly
	// connecting directly instead could bypass a proxy that policy requires,
	// so refuse and say how to opt out.
	if strings.EqualFold(raw, noProxyValue) {
		return nil, fmt.Errorf("%s=%s is not an opt-out: it would name a proxy host called %q. To connect without a proxy, set TRUENAS_PROXY=none (or pass --proxy none); otherwise unset %s", source, raw, raw, source)
	}

	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	p, err := url.Parse(raw)
	// url.Parse accepts "corp:3128:3128" for some schemes (splitting at the
	// last colon) and rejects it as a bad port for others. Either way the
	// cause is the same, so the reason is too.
	if (err == nil && strings.Contains(p.Hostname(), ":") && !strings.HasPrefix(p.Host, "[")) ||
		(err != nil && strings.Contains(err.Error(), "invalid port") && hasExtraColon(raw)) {
		return nil, fmt.Errorf("invalid proxy URL in %s: the host is not a host name or bracketed IPv6 address", source)
	}
	if err != nil || p.Hostname() == "" {
		return nil, fmt.Errorf("invalid proxy URL in %s: %s", source, proxyParseReason(err))
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
		if !validScheme(p.Scheme) {
			// Not echoed: only a short, plain scheme is quoted back.
			return nil, fmt.Errorf("unsupported proxy scheme in %s: use http://, socks5:// or socks5h://", source)
		}
		return nil, fmt.Errorf("unsupported proxy scheme %q in %s: use http://, socks5:// or socks5h://", p.Scheme, source)
	}
	if p.Port() == "" {
		p.Host = net.JoinHostPort(p.Hostname(), port)
	} else if n, err := parsePort(p.Port()); err != nil {
		return nil, fmt.Errorf("invalid proxy port in %s: the port must be 1-65535", source)
	} else {
		p.Host = net.JoinHostPort(p.Hostname(), strconv.Itoa(n)) // ":08080" -> ":8080"
	}
	return &proxyChoice{url: p, source: source}, nil
}

// hasExtraColon reports whether the host part of the URL raw (which has a
// scheme) holds more than one colon outside brackets, as in corp:3128:3128.
func hasExtraColon(raw string) bool {
	_, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return false
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	return !strings.HasPrefix(rest, "[") && strings.Count(rest, ":") > 1
}

// proxyParseReason says why a proxy URL did not parse (err is nil if it
// parsed but has no host name). It never repeats any part of the URL: url's
// own errors quote the URL, and the pieces they quote (a bad port or
// escape, say) can come from the user info or the host name.
func proxyParseReason(err error) string {
	if err == nil {
		return "no host name"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "invalid userinfo"):
		return "invalid user info: percent-encode special characters in the user name and password (for example @ as %40)"
	case strings.Contains(msg, "missing ']'"):
		return "unterminated [ in the host (an IPv6 address needs a closing ])"
	case strings.Contains(msg, "invalid port"):
		return "the port is not a number"
	case strings.Contains(msg, "invalid URL escape"):
		return "invalid percent-escape (a % must be followed by two hex digits; write a literal % as %25)"
	case strings.Contains(msg, "invalid character") && strings.Contains(msg, "host"):
		return "invalid character in the host name"
	case strings.Contains(msg, "invalid control character"):
		return "invalid control character"
	case strings.Contains(msg, "cannot contain colon"):
		return "the URL must look like scheme://host:port"
	}
	return "not a valid URL; expected scheme://[user:password@]host[:port]"
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

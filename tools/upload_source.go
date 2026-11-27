package tools

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Source fetching for upload_file. A URL source is downloaded by this
// server, over https only, to a private temporary file whose SHA-256 is
// checked against the one given before anything is sent to TrueNAS.
// Connections are made directly (no proxy), so the address checks below
// see the real destination of every connection, redirects included.

const (
	maxSourceRedirects   = 5
	sourceDialTimeout    = 30 * time.Second
	sourceTLSTimeout     = 30 * time.Second
	sourceHeaderTimeout  = 60 * time.Second
	sourceProbeTimeout   = 20 * time.Second
	maxSourceURLLength   = 2048
	progressReportPeriod = 5 * time.Second
)

// errSourceStalled is the cause of a transfer cancelled because no data
// moved for the stall timeout.
var errSourceStalled = errors.New("no data was received for too long")

// errSourcePolicy marks a source refused by the scheme, redirect or
// address rules, as opposed to a network failure.
var errSourcePolicy = errors.New("refused by the source rules")

// uploadConfig holds the limits and hooks of upload_file. Tests replace
// fields after NewRegistry.
type uploadConfig struct {
	maxBytes     int64
	stallTimeout time.Duration
	maxDuration  time.Duration
	tempDir      string // "" means os.TempDir()
	rootCAs      *x509.CertPool
	// lookup resolves a source host; nil means net.DefaultResolver.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// dial connects to an address the policy allowed; nil means a
	// net.Dialer. Tests use it to reach a local server under any address.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func defaultUploadConfig() uploadConfig {
	return uploadConfig{
		maxBytes:     maxUploadBytes,
		stallTimeout: uploadStallTimeout,
		maxDuration:  uploadMaxDuration,
	}
}

// parseSourceURL accepts an absolute https URL with a host and no user
// info. Errors never quote the URL, which may carry tokens.
func parseSourceURL(raw string) (*url.URL, error) {
	if len(raw) > maxSourceURLLength {
		return nil, fmt.Errorf("source_url is longer than %d characters", maxSourceURLLength)
	}
	if strings.ContainsFunc(raw, func(c rune) bool { return c < 0x21 || c == 0x7f }) {
		return nil, fmt.Errorf("source_url contains spaces or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("source_url is not a valid URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, fmt.Errorf("source_url must be an https:// URL (http, file and other schemes are refused)")
	}
	if u.User != nil {
		return nil, fmt.Errorf("source_url must not contain user info (user:password@)")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("source_url has no host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("source_url has an invalid port")
		}
	}
	u.Scheme = "https"
	u.Fragment = ""
	return u, nil
}

// displayURL is u without its query and fragment, for previews, task
// messages and logs: a query can carry a signed token.
func displayURL(u *url.URL) string {
	out := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
	if u.RawQuery != "" {
		out += "?..."
	}
	return out
}

// restrictedAddr reports whether a is not a public unicast address:
// loopback, private (RFC 1918, fc00::/7), shared (100.64.0.0/10),
// link-local, multicast or unspecified.
func restrictedAddr(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() ||
		a.IsUnspecified() || cgnatPrefix.Contains(a)
}

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// sourceClient fetches one source URL. allowRestricted is set when the
// URL's own host resolves only to restricted addresses (an internal
// mirror): then redirects may go to such addresses too. Otherwise every
// connection, including after a redirect, must go to a public address.
type sourceClient struct {
	cfg             uploadConfig
	allowRestricted bool
	http            *http.Client
}

func (r *Registry) newSourceClient(ctx context.Context, u *url.URL) (*sourceClient, error) {
	sc := &sourceClient{cfg: r.uploadCfg}
	addrs, err := sc.resolve(ctx, u.Hostname())
	if err != nil {
		return nil, err
	}
	sc.allowRestricted = true
	for _, a := range addrs {
		if !restrictedAddr(a) {
			sc.allowRestricted = false
		}
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           sc.dialContext,
		TLSClientConfig:       &tls.Config{RootCAs: sc.cfg.rootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   sourceTLSTimeout,
		ResponseHeaderTimeout: sourceHeaderTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1,
	}
	sc.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxSourceRedirects {
				return fmt.Errorf("%w: more than %d redirects", errSourcePolicy, maxSourceRedirects)
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("%w: redirected to a %s:// URL; only https is followed", errSourcePolicy, req.URL.Scheme)
			}
			if req.URL.User != nil {
				return fmt.Errorf("%w: redirected to a URL with user info", errSourcePolicy)
			}
			return nil
		},
	}
	return sc, nil
}

func (sc *sourceClient) close() {
	sc.http.CloseIdleConnections()
}

func (sc *sourceClient) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{a}, nil
	}
	lookup := sc.cfg.lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("could not resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("could not resolve %s: no addresses", host)
	}
	return addrs, nil
}

// dialContext resolves the host itself and connects only to addresses the
// policy allows, so a redirect (or a DNS answer) cannot steer the fetch to
// an internal address.
func (sc *sourceClient) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	addrs, err := sc.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	dial := sc.cfg.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: sourceDialTimeout, KeepAlive: 30 * time.Second}).DialContext
	}
	var lastErr error
	for _, a := range addrs {
		if restrictedAddr(a) && !sc.allowRestricted {
			lastErr = fmt.Errorf("%w: %s resolves to %s, a private, loopback or link-local address; the source URL's host is public, so connections to such addresses are refused", errSourcePolicy, host, a.Unmap())
			continue
		}
		conn, err := dial(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// sourceError words a failed request without quoting the URL (url.Error
// does), which may carry a token.
func sourceError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err
}

// probeSize asks for the source's size with HEAD (falling back to a GET
// whose body is not read). It returns -1 when the size is not reported.
func (sc *sourceClient) probeSize(ctx context.Context, u *url.URL) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, sourceProbeTimeout)
	defer cancel()
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
		if err != nil {
			return -1, err
		}
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := sc.http.Do(req)
		if err != nil {
			return -1, sourceError(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return resp.ContentLength, nil
		}
		if method == http.MethodGet || (resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotImplemented && resp.StatusCode != http.StatusForbidden) {
			return -1, fmt.Errorf("the server answered HTTP %d", resp.StatusCode)
		}
	}
	return -1, nil
}

// downloaded is a verified local copy of a URL source.
type downloaded struct {
	path   string
	size   int64
	sha256 string
}

// download streams the source to a new private temporary file, hashing it
// on the way, and keeps it only if it is complete, within maxBytes, and
// its SHA-256 is want. progress is called with the bytes received and the
// expected total (-1 if unknown).
func (sc *sourceClient) download(ctx context.Context, u *url.URL, want string, progress func(got, total int64)) (*downloaded, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := sc.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", stallCause(ctx, sourceError(err), sc.cfg.stallTimeout))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: the server answered HTTP %d", resp.StatusCode)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, fmt.Errorf("download failed: the server sent the file with Content-Encoding %q; only unencoded files are accepted", enc)
	}
	total := resp.ContentLength
	if total > sc.cfg.maxBytes {
		return nil, fmt.Errorf("the source is %s, over the %s limit", formatBytes(total), formatBytes(sc.cfg.maxBytes))
	}
	if total == 0 {
		return nil, fmt.Errorf("the source is empty")
	}

	f, err := os.CreateTemp(sc.cfg.tempDir, "truenas-mcp-upload-*")
	if err != nil {
		return nil, fmt.Errorf("could not create a temporary file on this host: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			f.Close()
			removeTemp(f.Name())
		}
	}()

	h := sha256.New()
	body := &stallReader{r: resp.Body}
	body.touch()
	go body.watch(ctx, sc.cfg.stallTimeout, cancel)
	var reported time.Time
	counted := &countingWriter{w: io.MultiWriter(f, h), report: func(n int64) {
		if progress != nil && time.Since(reported) >= progressReportPeriod {
			reported = time.Now()
			progress(n, total)
		}
	}}
	n, err := io.Copy(counted, io.LimitReader(body, sc.cfg.maxBytes+1))
	if err != nil {
		if errors.Is(err, os.ErrClosed) || isWriteErr(err, f) {
			return nil, fmt.Errorf("could not write the temporary file on this host (is %s full?): %w", tempDirName(sc.cfg.tempDir), err)
		}
		return nil, fmt.Errorf("download failed after %s: %w", formatBytes(n), stallCause(ctx, sourceError(err), sc.cfg.stallTimeout))
	}
	switch {
	case n > sc.cfg.maxBytes:
		return nil, fmt.Errorf("the source is larger than the %s limit", formatBytes(sc.cfg.maxBytes))
	case n == 0:
		return nil, fmt.Errorf("the source is empty")
	case total >= 0 && n != total:
		return nil, fmt.Errorf("download incomplete: received %d of %d bytes", n, total)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("could not write the temporary file on this host: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("could not write the temporary file on this host: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return nil, fmt.Errorf("SHA-256 mismatch: the downloaded file (%s) has SHA-256 %s, not the expected %s; nothing was uploaded", formatBytes(n), got, want)
	}
	keep = true
	return &downloaded{path: f.Name(), size: n, sha256: got}, nil
}

func tempDirName(dir string) string {
	if dir == "" {
		return os.TempDir()
	}
	return dir
}

// isWriteErr reports whether err came from writing f.
func isWriteErr(err error, f *os.File) bool {
	var pe *os.PathError
	return errors.As(err, &pe) && pe.Path == f.Name()
}

// stallCause replaces err with the stall explanation when the transfer
// was cancelled for stalling.
func stallCause(ctx context.Context, err error, stall time.Duration) error {
	if errors.Is(context.Cause(ctx), errSourceStalled) {
		return fmt.Errorf("%w (%s)", errSourceStalled, stall)
	}
	return err
}

// stallReader records when data last arrived; watch cancels the transfer
// when nothing arrives for stall.
type stallReader struct {
	r    io.Reader
	last atomic.Int64
}

func (s *stallReader) touch() { s.last.Store(time.Now().UnixNano()) }

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.touch()
	}
	return n, err
}

func (s *stallReader) watch(ctx context.Context, stall time.Duration, cancel context.CancelCauseFunc) {
	if stall <= 0 {
		return
	}
	tick := stall / 4
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Since(time.Unix(0, s.last.Load())) > stall {
				cancel(errSourceStalled)
				return
			}
		}
	}
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w      io.Writer
	n      int64
	report func(int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.report != nil {
		c.report(c.n)
	}
	return n, err
}

// hashingReader hashes what is read through it.
type hashingReader struct {
	r io.Reader
	h hash.Hash
}

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	h.h.Write(p[:n])
	return n, err
}

// hashFile returns the SHA-256 and size of f read from its start, and
// rewinds it.
func hashFile(ctx context.Context, f *os.File) (string, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, &ctxReader{ctx: ctx, r: f})
	if err != nil {
		return "", n, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ctxReader stops reading once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// removeTemp deletes a temporary download, logging when that fails so a
// left-over file on this host is not silent.
func removeTemp(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("upload_file: could not remove temporary file %s: %v", path, err)
	}
}

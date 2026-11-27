package truenas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// uploadPath is the middleware's HTTP file upload endpoint. It takes a
// multipart body whose first part, "data", is the JSON call
// {"method", "params"} and whose second part, "file", is piped into the
// job's input. It answers {"job_id": N} once the whole file part has been
// read (src/middlewared/middlewared/apps/file_app.py, FileApplication.upload).
const uploadPath = "/_upload"

const (
	uploadDialTimeout   = 30 * time.Second
	uploadTLSTimeout    = 30 * time.Second
	uploadHeaderTimeout = 5 * time.Minute
	uploadBodyCloseWait = 30 * time.Second
	// maxUploadReplyBytes bounds the reply body read from /_upload.
	maxUploadReplyBytes = 64 * 1024
	// maxUploadErrorText bounds the server text quoted in an error.
	maxUploadErrorText = 512
)

// ErrUploadStalled is the cause of an upload cancelled because no file
// data was read for UploadOptions.StallTimeout.
var ErrUploadStalled = errors.New("no data was sent for too long")

// UploadOptions configures UploadFile.
type UploadOptions struct {
	// StallTimeout cancels the upload when no file data has been read for
	// this long. 0 disables the check.
	StallTimeout time.Duration
	// Progress, when set, is called with the file bytes read so far.
	Progress func(sent int64)
}

// UploadFile starts method (a job that takes an input pipe, such as
// filesystem.put) with params through the /_upload endpoint and streams
// exactly size bytes of body into it as the file part. It returns the job
// id. It uses the host, port, TLS settings, proxy and API key of the
// JSON-RPC connection; the key is sent as a bearer token and never logged
// or quoted. The request is not retried: once bytes are sent, the job may
// already have written them.
func (c *Client) UploadFile(ctx context.Context, method string, params []interface{}, filename string, body io.Reader, size int64, opts UploadOptions) (int, error) {
	if size < 0 {
		return 0, fmt.Errorf("upload size must be known")
	}
	ws, err := c.endpointURL()
	if err != nil {
		return 0, err
	}
	target := &url.URL{Scheme: "https", Host: ws.Host, Path: uploadPath}

	data, err := json.Marshal(map[string]interface{}{"method": method, "params": params})
	if err != nil {
		return 0, fmt.Errorf("failed to encode the %s call: %w", method, err)
	}
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	dataPart, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="data"`},
		"Content-Type":        {"application/json"},
	})
	if err != nil {
		return 0, err
	}
	if _, err := dataPart.Write(data); err != nil {
		return 0, err
	}
	if _, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename=%q`, uploadFilename(filename))},
		"Content-Type":        {"application/octet-stream"},
	}); err != nil {
		return 0, err
	}
	prefix := append([]byte(nil), head.Bytes()...)
	head.Reset()
	if err := mw.Close(); err != nil {
		return 0, err
	}
	suffix := head.Bytes()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	file := &uploadBody{r: io.LimitReader(body, size), progress: opts.Progress}
	file.touch()
	if opts.StallTimeout > 0 {
		go file.watch(ctx, opts.StallTimeout, cancel)
	}
	reqBody := &closeSignal{
		r:    io.MultiReader(bytes.NewReader(prefix), &exactReader{r: file, left: size}, bytes.NewReader(suffix)),
		done: make(chan struct{}),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), reqBody)
	if err != nil {
		return 0, err
	}
	req.ContentLength = int64(len(prefix)) + size + int64(len(suffix))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	transport, via, err := c.uploadTransport(ws)
	if err != nil {
		return 0, err
	}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{
		Transport: transport,
		// A redirect is never followed: it would resend the key.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	log.Printf("Uploading %d bytes to %s%s (method %s)", size, logURL(target), via, method)
	// The transport may still read the body after Do returns; body must
	// not be read once UploadFile has returned.
	defer reqBody.wait(cancel)
	resp, err := httpClient.Do(req)
	if err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, ErrUploadStalled) {
			return 0, fmt.Errorf("upload to %s stopped: %w (%s)", logURL(target), ErrUploadStalled, opts.StallTimeout)
		}
		if hint := tlsHint(err, ws.Hostname()); hint != "" {
			return 0, fmt.Errorf("upload to %s failed: %s (%s)", logURL(target), c.scrubSecrets(err.Error()), hint)
		}
		return 0, fmt.Errorf("upload to %s failed: %s", logURL(target), c.scrubSecrets(err.Error()))
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxUploadReplyBytes))
	if resp.StatusCode != http.StatusOK {
		return 0, c.uploadStatusError(method, resp.StatusCode, reply)
	}
	var out struct {
		JobID *int `json:"job_id"`
	}
	if err := json.Unmarshal(reply, &out); err != nil || out.JobID == nil {
		return 0, fmt.Errorf("unexpected reply from %s (HTTP 200 without a job id)", uploadPath)
	}
	log.Printf("Upload to %s accepted: job %d", logURL(target), *out.JobID)
	return *out.JobID, nil
}

// uploadTransport is an HTTP transport to the TrueNAS host with the
// connection's TLS settings and proxy.
func (c *Client) uploadTransport(ws *url.URL) (*http.Transport, string, error) {
	proxy, err := c.proxyFor(ws)
	if err != nil {
		return nil, "", err
	}
	t := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: uploadDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       c.tlsConfig.Clone(),
		TLSHandshakeTimeout:   uploadTLSTimeout,
		ResponseHeaderTimeout: uploadHeaderTimeout,
		// The body is a single large stream; HTTP/1.1 keeps it simple.
		ForceAttemptHTTP2: false,
		MaxIdleConns:      1,
	}
	via := ""
	if proxy != nil {
		t.Proxy = http.ProxyURL(dialerProxy(proxy.url))
		via = " via proxy " + proxy.String()
	}
	return t, via, nil
}

// uploadStatusError words a non-200 reply from /_upload. The server text is
// scrubbed of the API key and other secrets, and bounded.
func (c *Client) uploadStatusError(method string, status int, body []byte) error {
	text := strings.TrimSpace(c.scrubSecrets(string(body)))
	if len(text) > maxUploadErrorText {
		text = text[:maxUploadErrorText] + "..."
	}
	var why string
	switch status {
	case http.StatusUnauthorized:
		why = "TrueNAS did not accept the API key for HTTP uploads"
	case http.StatusForbidden:
		why = fmt.Sprintf("TrueNAS refused the call: the API key's user is not allowed to run %s (it needs the FULL_ADMIN role), or API keys are not permitted by the system's security level", method)
	case http.StatusRequestEntityTooLarge:
		why = "TrueNAS (or a proxy in front of it) refused the request as too large"
	case http.StatusNotFound:
		why = fmt.Sprintf("the %s endpoint was not found on TrueNAS", uploadPath)
	default:
		why = fmt.Sprintf("TrueNAS refused the upload of %s", method)
	}
	if text != "" {
		return &UploadStatusError{Status: status, msg: fmt.Sprintf("%s (HTTP %d: %s)", why, status, text)}
	}
	return &UploadStatusError{Status: status, msg: fmt.Sprintf("%s (HTTP %d)", why, status)}
}

// UploadStatusError is a non-200 reply from /_upload.
type UploadStatusError struct {
	Status int
	msg    string
}

func (e *UploadStatusError) Error() string { return e.msg }

// NothingWritten reports whether the middleware refuses with this status
// before it starts the job, so no file data reached the target: failed
// authentication or authorization, an unknown endpoint or method, a call
// the method refused (412), or a request refused as too large.
func (e *UploadStatusError) NothingWritten() bool {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusPreconditionFailed, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// uploadFilename is name reduced to safe characters for the multipart
// header. The middleware ignores it; the target path is in params.
func uploadFilename(name string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' || r == '/' {
			return '_'
		}
		return r
	}, name)
	if clean == "" {
		return "upload"
	}
	return clean
}

// uploadBody counts and reports the file bytes read and records when the
// last ones were read, for the stall check.
type uploadBody struct {
	r        io.Reader
	n        atomic.Int64
	last     atomic.Int64 // unix nanoseconds of the last read with data
	progress func(int64)
}

func (b *uploadBody) touch() { b.last.Store(time.Now().UnixNano()) }

func (b *uploadBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.touch()
		total := b.n.Add(int64(n))
		if b.progress != nil {
			b.progress(total)
		}
	}
	return n, err
}

// watch cancels the request with ErrUploadStalled when nothing has been
// read for stall.
func (b *uploadBody) watch(ctx context.Context, stall time.Duration, cancel context.CancelCauseFunc) {
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
			if time.Since(time.Unix(0, b.last.Load())) > stall {
				cancel(ErrUploadStalled)
				return
			}
		}
	}
}

// closeSignal closes done when the transport closes the request body,
// which it does after its last read.
type closeSignal struct {
	r    io.Reader
	once sync.Once
	done chan struct{}
}

func (c *closeSignal) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *closeSignal) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

// wait cancels the request and waits until the transport has closed the
// body, bounded in case it never does.
func (c *closeSignal) wait(cancel context.CancelCauseFunc) {
	cancel(nil)
	select {
	case <-c.done:
	case <-time.After(uploadBodyCloseWait):
		log.Printf("upload: the request body was not released within %s", uploadBodyCloseWait)
	}
}

// exactReader fails when r ends before left bytes, so a source that shrinks
// during the upload is an error rather than a short file.
type exactReader struct {
	r    io.Reader
	left int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.r.Read(p)
	e.left -= int64(n)
	if err == io.EOF && e.left > 0 {
		return n, fmt.Errorf("the source ended %d bytes early: %w", e.left, io.ErrUnexpectedEOF)
	}
	if err == io.EOF {
		err = nil
	}
	return n, err
}

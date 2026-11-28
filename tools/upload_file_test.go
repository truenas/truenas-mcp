package tools

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// uploadTestTarget is the upload_file target the shared tool tables use:
// a new file in /mnt/tank/shares/docs.
const uploadTestTarget = "/mnt/tank/shares/docs/installer.iso"

// uploadTestSourceFile is a regular, non-empty file on this host for the
// shared tool tables: the test binary itself.
func uploadTestSourceFile() string {
	p, err := filepath.Abs(os.Args[0])
	if err != nil {
		return os.Args[0]
	}
	return p
}

// allowUploadTestSource enables source_path on r for the directory of
// uploadTestSourceFile, as an operator would with --upload-source-dirs.
func allowUploadTestSource(t *testing.T, r *Registry) {
	t.Helper()
	if err := r.SetUploadSourceDirs([]string{filepath.Dir(uploadTestSourceFile())}, nil); err != nil {
		t.Fatal(err)
	}
}

// allowUploadSources enables source_path on r for dirs.
func allowUploadSources(t *testing.T, r *Registry, dirs ...string) {
	t.Helper()
	if err := r.SetUploadSourceDirs(dirs, nil); err != nil {
		t.Fatal(err)
	}
}

// uploadTargetAbsent makes filesystem.stat of uploadTestTarget fail with
// ENOENT, for fakes that answer every stat with the same directory.
func uploadTargetAbsent(next func(string, json.RawMessage) (interface{}, map[string]interface{}, bool)) func(string, json.RawMessage) (interface{}, map[string]interface{}, bool) {
	return func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "filesystem.stat" {
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			if len(p) > 0 && p[0] == uploadTestTarget {
				return nil, rpcErr("[ENOENT] Path " + uploadTestTarget + " not found"), true
			}
		}
		if next != nil {
			return next(method, params)
		}
		return nil, nil, false
	}
}

// uploadFake serves what upload_file reads (the set_filesystem_acl fake,
// with .iso files absent unless added to stats, and free space in
// filesystem.statfs) and the /_upload endpoint, which records each
// request and starts a filesystem.put job.
type uploadFake struct {
	*fsACLFake
	umu      sync.Mutex
	avail    float64
	status   int    // /_upload reply status; 0 means 200
	jobState string // state of the started job; "" means SUCCESS
	requests []uploadRequest
}

type uploadRequest struct {
	Auth      string
	DataFirst bool
	Data      map[string]interface{}
	File      []byte
	Filename  string
}

func newUploadFake() *uploadFake {
	return &uploadFake{fsACLFake: newFSACLFake(), avail: 100 << 30}
}

func (u *uploadFake) handle(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
	var p []interface{}
	_ = json.Unmarshal(params, &p)
	switch method {
	case "filesystem.stat":
		path, _ := p[0].(string)
		u.mu.Lock()
		_, known := u.stats[path]
		u.mu.Unlock()
		if strings.HasSuffix(path, ".iso") && !known {
			return nil, rpcErr("[ENOENT] Path " + path + " not found"), true
		}
	case "filesystem.statfs":
		res, e, ok := u.fsACLFake.handle(method, params)
		if m, isMap := res.(map[string]interface{}); isMap {
			u.umu.Lock()
			m["avail_bytes"] = u.avail
			u.umu.Unlock()
		}
		return res, e, ok
	}
	return u.fsACLFake.handle(method, params)
}

func (u *uploadFake) serveUpload(w http.ResponseWriter, r *http.Request) {
	req := uploadRequest{Auth: r.Header.Get("Authorization")}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "not multipart", http.StatusBadRequest)
		return
	}
	for i := 0; ; i++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(part)
		switch part.FormName() {
		case "data":
			req.DataFirst = i == 0
			_ = json.Unmarshal(b, &req.Data)
		case "file":
			req.File = b
			req.Filename = part.FileName()
		}
	}
	u.umu.Lock()
	u.requests = append(u.requests, req)
	status, state := u.status, u.jobState
	u.umu.Unlock()
	if status != 0 && status != http.StatusOK {
		http.Error(w, "refused by test; key 1-testkey", status)
		return
	}
	if state == "" {
		state = "SUCCESS"
	}
	job := map[string]interface{}{
		"id": 88.0, "method": "filesystem.put", "state": state, "arguments": req.Data["params"],
		"time_started": map[string]interface{}{"$date": 1767225600000.0},
		"progress":     map[string]interface{}{"percent": 100.0, "description": "", "extra": nil},
	}
	if state == "SUCCESS" {
		job["result"] = true
	} else {
		job["error"] = "[EFAULT] disk full"
	}
	u.mutate(func() { u.jobs = append(u.jobs, job) })
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"job_id": 88}`))
}

func (u *uploadFake) uploads() []uploadRequest {
	u.umu.Lock()
	defer u.umu.Unlock()
	return append([]uploadRequest(nil), u.requests...)
}

// registry serves the fake and /_upload on one TLS server, like TrueNAS.
func (u *uploadFake) registry(t *testing.T, withTasks bool) (*Registry, *fakeMiddleware, *tasks.Manager) {
	t.Helper()
	fake := &fakeMiddleware{handle: u.handle}
	mux := http.NewServeMux()
	mux.HandleFunc("/_upload", u.serveUpload)
	mux.HandleFunc("/", fake.serve)
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	endpoint := "wss" + strings.TrimPrefix(srv.URL, "https") + "/api/current"
	client, err := truenas.NewClientWithOptions(endpoint, "1-testkey", &tls.Config{RootCAs: roots}, truenas.Options{APIUser: "svc", Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	var mgr *tasks.Manager
	if withTasks {
		mgr = tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Hour, CleanupInterval: time.Hour})
		t.Cleanup(mgr.Shutdown)
	}
	r := NewRegistry(client, mgr)
	r.uploadCfg.tempDir = t.TempDir()
	// Local sources are off by default; these tests read files from
	// t.TempDir(), so they allow the temporary directory like an operator
	// would. The tests of the setting itself turn it off again.
	allowUploadSources(t, r, os.TempDir())
	return r, fake, mgr
}

// sourceServer is an https file server reachable under any host name the
// registry's lookup maps: every connection goes to it.
type sourceServer struct {
	srv   *httptest.Server
	body  []byte
	mu    sync.Mutex
	paths []string
}

const testISOURL = "https://example.com/isos/installer.iso"

func newSourceServer(t *testing.T, body []byte, extra func(mux *http.ServeMux)) *sourceServer {
	t.Helper()
	s := &sourceServer{body: body}
	mux := http.NewServeMux()
	mux.HandleFunc("/isos/installer.iso", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		w.Header().Set("Content-Length", fmt.Sprint(len(s.body)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(s.body)
	})
	if extra != nil {
		extra(mux)
	}
	s.srv = httptest.NewTLSServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sourceServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// attach points r's source fetches at s: example.com is a public name,
// internal.example.com a private one, and every allowed connection is
// made to the test server.
func (s *sourceServer) attach(r *Registry) {
	pool := x509.NewCertPool()
	pool.AddCert(s.srv.Certificate())
	r.uploadCfg.rootCAs = pool
	r.uploadCfg.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "example.com":
			return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
		case "internal.example.com":
			return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
		case "metadata.example.com":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}
		return nil, fmt.Errorf("no such host")
	}
	addr := s.srv.Listener.Addr().String()
	r.uploadCfg.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func testISO() []byte {
	b := make([]byte, 256*1024+17)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func urlUploadArgs(body []byte) map[string]interface{} {
	return map[string]interface{}{"path": uploadTestTarget, "source_url": testISOURL, "sha256": sha256Hex(body)}
}

// waitTask polls the task until done reports true, failing after a while.
func waitTask(t *testing.T, mgr *tasks.Manager, id string, done func(*tasks.Task) bool) *tasks.Task {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		mgr.PollNow()
		task, err := mgr.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if done(task) {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not finish: status %s, message %q", id, task.Status, task.StatusMessage)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func finished(task *tasks.Task) bool { return task.Status.Terminal() }

func startedTaskID(t *testing.T, out string) string {
	t.Helper()
	m := decodeObj(t, out)
	id, _ := m["task_id"].(string)
	if id == "" || m["status"] != "started" {
		t.Fatalf("no started task in %s", out)
	}
	return id
}

func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func previewRoot(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	m := decodeObj(t, out)
	if p, ok := m["preview"].(map[string]interface{}); ok {
		return p
	}
	return m
}

func TestUploadFileClassifiedAndReadOnly(t *testing.T) {
	u := newUploadFake()
	r, fake, _ := u.registry(t, false)
	src := newSourceServer(t, testISO(), nil)
	src.attach(r)
	if safetyOf("upload_file").Access != Destructive || !hasParam(r.tools["upload_file"], "confirmation_token") {
		t.Fatal("upload_file is not destructive")
	}
	if readOnlyDryRunTools["upload_file"] {
		t.Fatal("upload_file must not be listed for dry runs in read-only mode")
	}
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if listed(r)["upload_file"] {
		t.Error("upload_file listed in read-only mode")
	}
	for _, extra := range []map[string]interface{}{{}, {"dry_run": true}, {"confirmation_token": "x"}, {"overwrite": true}} {
		_, err := r.CallTool(t.Context(), "upload_file", mergeArgs(urlUploadArgs(testISO()), extra))
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%v in read-only mode: err = %v", extra, err)
		}
	}
	if n := len(fake.called("filesystem.stat")); n != 0 {
		t.Errorf("read-only mode sent filesystem.stat %d time(s)", n)
	}
	if reqs := src.requests(); len(reqs) != 0 {
		t.Errorf("read-only mode fetched the source: %v", reqs)
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("read-only mode uploaded %d time(s)", n)
	}
}

func TestUploadFileURLConfirmFlow(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	r, _, mgr := u.registry(t, true)
	src := newSourceServer(t, body, nil)
	src.attach(r)
	args := urlUploadArgs(body)

	preview, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	if n := len(u.uploads()); n > 1 {
		t.Fatalf("uploaded %d times", n)
	}
	text := previewText(t, preview)
	for _, want := range []string{testISOURL, sha256Hex(body), uploadTestTarget, `"dataset":"tank/shares"`,
		fmt.Sprintf(`"size":%d`, len(body)), `"available_bytes":107374182400`, `"endpoint":"/_upload"`, `"append":false`} {
		if !strings.Contains(text, want) {
			t.Errorf("preview lacks %s:\n%s", want, text)
		}
	}
	summary := strings.Join(confirmSummaryFacts(t, preview), "\n")
	for _, want := range []string{`source: "` + testISOURL + `"`, `sha256: "` + sha256Hex(body) + `"`, `target: "` + uploadTestTarget + `"`,
		`dataset: "tank/shares"`, `free space: "100.00 GiB"`, `overwrite: "no (new file)"`, `size: "256.02 KiB (262161 bytes)"`} {
		if !strings.Contains(summary, want) {
			t.Errorf("confirm summary lacks %q:\n%s", want, summary)
		}
	}

	id := startedTaskID(t, out)
	task := waitTask(t, mgr, id, finished)
	if task.Status != tasks.TaskStatusCompleted {
		t.Fatalf("task %s: %s", task.Status, task.StatusMessage)
	}
	if task.JobID == nil || *task.JobID != 88 {
		t.Errorf("task job = %v, want 88", task.JobID)
	}
	want := fmt.Sprintf("Uploaded 256.02 KiB to %s (SHA-256 %s)", uploadTestTarget, sha256Hex(body))
	if task.StatusMessage != want {
		t.Errorf("task message = %q, want %q", task.StatusMessage, want)
	}
	reqs := u.uploads()
	if len(reqs) != 1 {
		t.Fatalf("%d uploads, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Auth != "Bearer 1-testkey" {
		t.Errorf("Authorization = %q", req.Auth)
	}
	if !req.DataFirst {
		t.Error("the data part was not first")
	}
	data, _ := json.Marshal(req.Data)
	if got := string(data); got != `{"method":"filesystem.put","params":["/mnt/tank/shares/docs/installer.iso",{"append":false}]}` {
		t.Errorf("data part = %s", got)
	}
	if string(req.File) != string(body) {
		t.Errorf("file part is %d bytes, want the %d source bytes", len(req.File), len(body))
	}
	if req.Filename != "installer.iso" {
		t.Errorf("file name = %q", req.Filename)
	}
	if left := tempFiles(t, r.uploadCfg.tempDir); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
	// The previews only ask for the size; the one GET is the download.
	got := src.requests()
	for i, req := range got {
		if want := "HEAD /isos/installer.iso"; i == len(got)-1 {
			if req != "GET /isos/installer.iso" {
				t.Errorf("last source request = %q, want the download", req)
			}
		} else if req != want {
			t.Errorf("source request %d = %q, want %q", i, req, want)
		}
	}
}

func confirmSummaryFacts(t *testing.T, preview map[string]interface{}) []string {
	t.Helper()
	p, _ := preview["preview"].(map[string]interface{})
	cs, _ := p["confirm_summary"].(map[string]interface{})
	var facts []string
	if h, ok := cs["headline"].(string); ok {
		facts = append(facts, h)
	}
	list, _ := cs["facts"].([]interface{})
	for _, f := range list {
		s, _ := f.(string)
		facts = append(facts, s)
	}
	if len(facts) == 0 {
		t.Fatalf("no confirm summary in %v", preview)
	}
	return facts
}

func TestUploadFileSHA256MismatchUploadsNothing(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	r, _, mgr := u.registry(t, true)
	src := newSourceServer(t, body, nil)
	src.attach(r)
	args := urlUploadArgs(body)
	args["sha256"] = strings.Repeat("ab", 32)
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	task := waitTask(t, mgr, startedTaskID(t, out), finished)
	if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, "SHA-256 mismatch") ||
		!strings.Contains(task.StatusMessage, sha256Hex(body)) || !strings.Contains(task.StatusMessage, "nothing was uploaded") {
		t.Errorf("task %s: %q", task.Status, task.StatusMessage)
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s) after a SHA-256 mismatch", n)
	}
	if left := tempFiles(t, r.uploadCfg.tempDir); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestUploadFileArgumentRefusals(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	r, _, _ := u.registry(t, false)
	src := newSourceServer(t, body, nil)
	src.attach(r)
	sum := sha256Hex(body)
	local := filepath.Join(t.TempDir(), "local.iso")
	if err := os.WriteFile(local, body, 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty.iso")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"no source", map[string]interface{}{"path": uploadTestTarget}, "exactly one of source_url"},
		{"both sources", map[string]interface{}{"path": uploadTestTarget, "source_url": testISOURL, "source_path": local, "sha256": sum}, "exactly one of source_url"},
		{"url without sha256", map[string]interface{}{"path": uploadTestTarget, "source_url": testISOURL}, "sha256 is required with source_url"},
		{"bad sha256", map[string]interface{}{"path": uploadTestTarget, "source_url": testISOURL, "sha256": "xyz"}, "64 hex characters"},
		{"http url", map[string]interface{}{"path": uploadTestTarget, "source_url": "http://example.com/a.iso", "sha256": sum}, "must be an https:// URL"},
		{"file url", map[string]interface{}{"path": uploadTestTarget, "source_url": "file:///etc/passwd", "sha256": sum}, "must be an https:// URL"},
		{"user info", map[string]interface{}{"path": uploadTestTarget, "source_url": "https://user:pw@example.com/a.iso", "sha256": sum}, "must not contain user info"},
		{"relative local path", map[string]interface{}{"path": uploadTestTarget, "source_path": "local.iso"}, "absolute path"},
		{"missing local file", map[string]interface{}{"path": uploadTestTarget, "source_path": filepath.Join(t.TempDir(), "nope.iso")}, "cannot be read"},
		{"local directory", map[string]interface{}{"path": uploadTestTarget, "source_path": t.TempDir()}, "not a regular file"},
		{"empty local file", map[string]interface{}{"path": uploadTestTarget, "source_path": empty}, "is empty"},
	} {
		_, err := r.CallTool(t.Context(), "upload_file", mergeArgs(tc.args, map[string]interface{}{"dry_run": true}))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "pw@") {
			t.Errorf("%s: error quotes the URL's user info: %v", tc.name, err)
		}
	}
	if reqs := src.requests(); len(reqs) != 0 {
		t.Errorf("refused calls fetched the source: %v", reqs)
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

func TestUploadFileLocalSizeLimit(t *testing.T) {
	u := newUploadFake()
	r, _, _ := u.registry(t, false)
	r.uploadCfg.maxBytes = 1024
	local := filepath.Join(t.TempDir(), "big.iso")
	if err := os.WriteFile(local, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.CallTool(t.Context(), "upload_file", map[string]interface{}{"path": uploadTestTarget, "source_path": local, "dry_run": true})
	if err == nil || !strings.Contains(err.Error(), "over the 1.00 KiB limit") {
		t.Errorf("err = %v", err)
	}
}

func TestUploadFileRedirectRules(t *testing.T) {
	body := testISO()
	sum := sha256Hex(body)
	src := newSourceServer(t, body, func(mux *http.ServeMux) {
		mux.HandleFunc("/to-http", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.com/isos/installer.iso", http.StatusFound)
		})
		mux.HandleFunc("/to-internal", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://internal.example.com/isos/installer.iso", http.StatusFound)
		})
		mux.HandleFunc("/to-metadata", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://metadata.example.com/latest", http.StatusFound)
		})
		mux.HandleFunc("/to-public", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com/isos/installer.iso", http.StatusFound)
		})
		mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/loop", http.StatusFound)
		})
	})
	u := newUploadFake()
	r, _, _ := u.registry(t, false)
	src.attach(r)
	call := func(url string) (map[string]interface{}, error) {
		out, err := r.CallTool(t.Context(), "upload_file", map[string]interface{}{"path": uploadTestTarget, "source_url": url, "sha256": sum, "dry_run": true})
		if err != nil {
			return nil, err
		}
		return previewRoot(t, out), nil
	}
	for _, tc := range []struct{ url, want string }{
		{"https://example.com/to-http", "only https is followed"},
		{"https://example.com/to-internal", "10.0.0.5; a private address"},
		{"https://example.com/to-metadata", "169.254.169.254"},
		{"https://internal.example.com/isos/installer.iso", ""},
		{"https://example.com/loop", "more than 5 redirects"},
	} {
		_, err := call(tc.url)
		if tc.want == "" {
			// The URL's own host is private, so it may be reached.
			if err != nil {
				t.Errorf("%s: %v", tc.url, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.url, err, tc.want)
		}
	}

	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

// TestUploadFileInternalMirrorPolicy checks the internal-mirror exception:
// a source host that resolves only to private LAN addresses (RFC 1918,
// fc00::/7) may reach private LAN addresses, for that request and its
// redirects, and nothing else that is restricted: never loopback,
// link-local (the metadata address), shared, unspecified or reserved.
func TestUploadFileInternalMirrorPolicy(t *testing.T) {
	body := testISO()
	sum := sha256Hex(body)
	src := newSourceServer(t, body, func(mux *http.ServeMux) {
		for path, to := range map[string]string{
			"/to-lan":      "https://lan.example.com/isos/installer.iso",
			"/to-ula":      "https://ula.example.com/isos/installer.iso",
			"/to-public":   "https://example.com/isos/installer.iso",
			"/to-loop":     "https://loop.example.com/isos/installer.iso",
			"/to-metadata": "https://metadata.example.com/isos/installer.iso",
			"/to-cgnat":    "https://cgnat.example.com/isos/installer.iso",
			"/to-zero":     "https://zero.example.com/isos/installer.iso",
			"/to-nat64":    "https://nat64.example.com/isos/installer.iso",
		} {
			mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, to, http.StatusFound)
			})
		}
	})
	u := newUploadFake()
	r, _, _ := u.registry(t, false)
	src.attach(r)
	hosts := map[string][]string{
		"example.com":          {"192.0.2.10"},
		"internal.example.com": {"10.0.0.5"},
		"lan.example.com":      {"192.168.1.20"},
		"ula.example.com":      {"fd00::5"},
		"loop.example.com":     {"127.0.0.1"},
		"metadata.example.com": {"169.254.169.254"},
		"cgnat.example.com":    {"100.64.0.1"},
		"zero.example.com":     {"0.0.0.1"},
		"nat64.example.com":    {"64:ff9b::a00:5"},
		"mixed.example.com":    {"10.0.0.6", "127.0.0.1"},
		"split.example.com":    {"10.0.0.7", "192.0.2.11"},
	}
	r.uploadCfg.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range hosts[host] {
			out = append(out, netip.MustParseAddr(a))
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no such host")
		}
		return out, nil
	}
	call := func(url string) (string, error) {
		return r.CallTool(t.Context(), "upload_file", map[string]interface{}{"path": uploadTestTarget, "source_url": url, "sha256": sum, "dry_run": true})
	}
	for _, url := range []string{
		"https://internal.example.com/isos/installer.iso",
		"https://internal.example.com/to-lan",
		"https://internal.example.com/to-ula",
		"https://internal.example.com/to-public",
		"https://ula.example.com/to-lan",
	} {
		out, err := call(url)
		if err != nil {
			t.Errorf("%s: %v", url, err)
			continue
		}
		if strings.Contains(out, "could not be read") {
			t.Errorf("%s: the size probe failed:\n%s", url, out)
		}
	}
	for _, tc := range []struct{ url, want string }{
		{"https://internal.example.com/to-loop", "127.0.0.1"},
		{"https://internal.example.com/to-metadata", "169.254.169.254"},
		{"https://internal.example.com/to-cgnat", "100.64.0.1"},
		{"https://internal.example.com/to-zero", "0.0.0.1"},
		{"https://internal.example.com/to-nat64", "64:ff9b::a00:5"},
		{"https://loop.example.com/isos/installer.iso", "127.0.0.1"},
		{"https://127.0.0.1/isos/installer.iso", "127.0.0.1"},
		{"https://[::1]/isos/installer.iso", "::1"},
		{"https://metadata.example.com/isos/installer.iso", "169.254.169.254"},
		{"https://cgnat.example.com/isos/installer.iso", "100.64.0.1"},
		// A host with a loopback address besides its private one is not
		// an internal mirror: neither address is used.
		{"https://mixed.example.com/isos/installer.iso", "refused by the source rules"},
		// A host with a public address besides its private one is not
		// an internal mirror either: its redirects stay public.
		{"https://split.example.com/to-lan", "192.168.1.20"},
		{"https://example.com/to-lan", "192.168.1.20"},
	} {
		_, err := call(tc.url)
		if err == nil || !strings.Contains(err.Error(), "refused by the source rules") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal naming %q", tc.url, err, tc.want)
		}
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

func TestUploadFileRedirectRefusedDuringDownload(t *testing.T) {
	// The size probe is not refused (HEAD answers directly), but the GET
	// redirects to a private address: the download must not follow it.
	body := testISO()
	src := newSourceServer(t, body, func(mux *http.ServeMux) {
		mux.HandleFunc("/sneaky.iso", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				return
			}
			http.Redirect(w, r, "https://internal.example.com/isos/installer.iso", http.StatusFound)
		})
	})
	u := newUploadFake()
	r, _, mgr := u.registry(t, true)
	src.attach(r)
	args := map[string]interface{}{"path": uploadTestTarget, "source_url": "https://example.com/sneaky.iso", "sha256": sha256Hex(body)}
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	task := waitTask(t, mgr, startedTaskID(t, out), finished)
	if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, "10.0.0.5") || !strings.Contains(task.StatusMessage, "nothing was uploaded") {
		t.Errorf("task %s: %q", task.Status, task.StatusMessage)
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

func TestUploadFileTargetRefusals(t *testing.T) {
	body := testISO()
	local := filepath.Join(t.TempDir(), "local.iso")
	if err := os.WriteFile(local, body, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		path    string
		setup   func(u *uploadFake)
		want    string
		blocked bool
	}{
		{name: "outside /mnt", path: "/etc/installer.iso", want: "only paths under /mnt/<pool>"},
		{name: "relative", path: "tank/installer.iso", want: "must be absolute"},
		{name: "dot dot", path: "/mnt/tank/shares/../installer.iso", want: "'..'"},
		{name: "not normalised", path: "/mnt/tank/shares//installer.iso", want: "empty or '.' segments"},
		{name: "pool root", path: "/mnt/tank", want: "root of pool 'tank'"},
		{name: "file in pool root", path: "/mnt/tank/installer.iso", want: "directly in the root dataset of pool 'tank'"},
		{name: "system path", path: "/mnt/.ix-apps/x.iso", want: "system path"},
		{name: "ix-apps", path: "/mnt/tank/ix-apps/x.iso", want: "'ix-apps', which TrueNAS manages itself"},
		{name: "system dataset", path: "/mnt/tank/.system/x.iso", want: "'.system', which TrueNAS manages itself"},
		{name: "dot file", path: "/mnt/tank/shares/.hidden.iso", want: "'.hidden.iso', which TrueNAS manages itself"},
		{name: "upper-case managed", path: "/mnt/tank/IX-APPLICATIONS/x.iso", want: "which TrueNAS manages itself"},
		{name: "boot pool", path: "/mnt/boot-pool/data/x.iso", want: "'boot-pool', which TrueNAS manages itself"},
		{name: "root dataset by mount", path: "/mnt/tank/isos/x.iso", want: "is on the root dataset of pool 'tank'", blocked: true},
		{name: "managed dataset by mount", path: "/mnt/tank/appdata/x.iso", want: "inside 'ix-applications'", blocked: true},
		{name: "read-only dataset", path: "/mnt/tank/ro/x.iso", want: "mounted read-only", blocked: true},
		{name: "non-zfs", path: "/mnt/tank/legacy/x.iso", want: "not on a ZFS dataset", blocked: true},
		{name: "locked dataset", path: "/mnt/tank/locked/x.iso", setup: func(u *uploadFake) {
			u.mutate(func() { u.datasets[7]["mounted"] = true })
		}, want: "tank/locked is locked", blocked: true},
		{name: "inside locked child", path: "/mnt/tank/shares/vault/x.iso", setup: func(u *uploadFake) {
			u.mutate(func() {
				d := fsDataset("tank/shares/vault", "/mnt/tank/shares/vault")
				d["locked"] = true
				u.datasets = append(u.datasets, d)
			})
		}, want: "inside dataset tank/shares/vault, which is locked", blocked: true},
		{name: "symlink in directory", path: "/mnt/tank/shares/link/x.iso", want: "symlink"},
		{name: "missing directory", path: "/mnt/tank/shares/nodir/x.iso", setup: func(u *uploadFake) {
			u.mutate(func() { u.fail["filesystem.stat"] = rpcErr("[ENOENT] Path /mnt/tank/shares/nodir not found") })
		}, want: "no directories are created"},
		{name: "target is a directory", path: "/mnt/tank/shares/docs/sub.iso", setup: func(u *uploadFake) {
			u.mutate(func() { u.stats["/mnt/tank/shares/docs/sub.iso"] = dirStat("/mnt/tank/shares/docs/sub.iso") })
		}, want: "not a regular file"},
		{name: "target is a symlink", path: "/mnt/tank/shares/docs/link.iso", setup: func(u *uploadFake) {
			u.mutate(func() {
				u.stats["/mnt/tank/shares/docs/link.iso"] = linkStat("/mnt/tank/shares/docs/link.iso", "/mnt/tank/shares/docs/report.txt")
			})
		}, want: "is a symlink"},
		{name: "parent is a file", path: "/mnt/tank/shares/docs/report.txt/x.iso", want: "not a directory"},
		{name: "dataset of another pool", path: "/mnt/tank/foreign/x.iso", setup: func(u *uploadFake) {
			u.mutate(func() { u.datasets = append(u.datasets, fsDataset("other/data", "/mnt/tank/foreign")) })
		}, want: `is on "other/data", not on a dataset of pool 'tank'`, blocked: true},
		{name: "dataset row missing", path: "/mnt/tank/hidden/x.iso", setup: func(u *uploadFake) {
			u.mutate(func() {
				d := fsDataset("tank/hidden", "/mnt/tank/hidden")
				d["query_hidden"] = true
				u.datasets = append(u.datasets, d)
			})
		}, want: "dataset tank/hidden of /mnt/tank/hidden was not found", blocked: true},
		{name: "not enough space", path: uploadTestTarget, setup: func(u *uploadFake) { u.avail = 1000 },
			want: "has 1000 B available, which is not enough for 256.02 KiB", blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUploadFake()
			if tc.setup != nil {
				tc.setup(u)
			}
			r, _, _ := u.registry(t, false)
			args := map[string]interface{}{"path": tc.path, "source_path": local}
			_, err := r.CallTool(t.Context(), "upload_file", mergeArgs(args, map[string]interface{}{"dry_run": true}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("dry run: err = %v, want %q", err, tc.want)
			}
			out, err := r.CallTool(t.Context(), "upload_file", args)
			if err == nil {
				t.Fatalf("confirmation flow issued a token:\n%s", out)
			}
			if strings.Contains(err.Error(), "confirmation_token\":\"") {
				t.Errorf("a refused upload issued a token: %v", err)
			}
			if tc.blocked && !strings.Contains(err.Error(), "no confirmation token was issued") {
				t.Errorf("blocked plan not reported as blocked: %v", err)
			}
			if n := len(u.uploads()); n != 0 {
				t.Errorf("uploaded %d time(s)", n)
			}
		})
	}
}

// existingISO adds an existing installer.iso to the fake.
func existingISO(u *uploadFake, size, mtime, inode float64) {
	u.mutate(func() {
		u.stats[uploadTestTarget] = map[string]interface{}{
			"realpath": uploadTestTarget, "type": "FILE", "size": size, "mtime": mtime, "inode": inode,
			"dev": 7.0, "mount_id": 50.0, "uid": 0.0, "gid": 0.0, "mode": float64(0o100644), "is_mountpoint": false,
		}
	})
}

func TestUploadFileOverwrite(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	existingISO(u, 1<<20, 1767225600.5, 900)
	r, _, mgr := u.registry(t, true)
	src := newSourceServer(t, body, nil)
	src.attach(r)
	args := urlUploadArgs(body)

	_, err := r.CallTool(t.Context(), "upload_file", args)
	if err == nil || !strings.Contains(err.Error(), "already exists (1.00 MiB); set overwrite to true") || !strings.Contains(err.Error(), "no confirmation token") {
		t.Fatalf("existing file without overwrite: %v", err)
	}

	args["overwrite"] = true
	first, err := r.CallTool(t.Context(), "upload_file", args)
	if err != nil {
		t.Fatal(err)
	}
	if replyToken(t, first) == "" {
		t.Fatal("overwrite preview issued no token")
	}
	summary := strings.Join(confirmSummaryFacts(t, decodeObj(t, first)), "\n")
	if !strings.Contains(summary, `overwrite: "yes: replaces the existing 1.00 MiB file"`) || !strings.Contains(summary, "Upload and replace") {
		t.Errorf("summary does not say it replaces the file:\n%s", summary)
	}
	if !strings.Contains(first, "truncated and rewritten in place") {
		t.Errorf("preview has no overwrite warning:\n%s", first)
	}

	// The file changes after the preview: the token is bound to its stat.
	for _, change := range []func(){
		func() { existingISO(u, 1<<20, 1767225601.5, 900) },
		func() { existingISO(u, 2<<20, 1767225600.5, 900) },
		func() { existingISO(u, 1<<20, 1767225600.5, 901) },
	} {
		existingISO(u, 1<<20, 1767225600.5, 900)
		first, err := r.CallTool(t.Context(), "upload_file", args)
		if err != nil {
			t.Fatal(err)
		}
		tok := replyToken(t, first)
		change()
		_, err = r.CallTool(t.Context(), "upload_file", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err == nil || !strings.Contains(err.Error(), "different planned operation") {
			t.Errorf("token accepted after the file changed: %v", err)
		}
	}
	if n := len(u.uploads()); n != 0 {
		t.Fatalf("uploaded %d time(s)", n)
	}

	existingISO(u, 1<<20, 1767225600.5, 900)
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	task := waitTask(t, mgr, startedTaskID(t, out), finished)
	if task.Status != tasks.TaskStatusCompleted {
		t.Fatalf("task %s: %s", task.Status, task.StatusMessage)
	}
	if reqs := u.uploads(); len(reqs) != 1 || string(reqs[0].File) != string(body) {
		t.Errorf("uploads = %d", len(reqs))
	}
}

func TestUploadFileTargetChangedDuringDownload(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	release := make(chan struct{})
	src := newSourceServer(t, body, func(mux *http.ServeMux) {
		mux.HandleFunc("/slow.iso", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method == http.MethodHead {
				return
			}
			<-release
			_, _ = w.Write(body)
		})
	})
	r, _, mgr := u.registry(t, true)
	src.attach(r)
	args := map[string]interface{}{"path": uploadTestTarget, "source_url": "https://example.com/slow.iso", "sha256": sha256Hex(body)}
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	id := startedTaskID(t, out)
	waitTask(t, mgr, id, func(task *tasks.Task) bool { return strings.HasPrefix(task.StatusMessage, "Downloading") })
	// Someone creates the target while the download runs.
	existingISO(u, 10, 1767225600, 902)
	close(release)
	task := waitTask(t, mgr, id, finished)
	if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, "already exists") || !strings.Contains(task.StatusMessage, "nothing was uploaded") {
		t.Errorf("task %s: %q", task.Status, task.StatusMessage)
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

func TestUploadFileLocalSource(t *testing.T) {
	body := testISO()
	dir := t.TempDir()
	local := filepath.Join(dir, "local.iso")
	if err := os.WriteFile(local, body, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.iso")
	if err := os.Symlink(local, link); err != nil {
		t.Fatal(err)
	}

	t.Run("with sha256", func(t *testing.T) {
		u := newUploadFake()
		r, _, mgr := u.registry(t, true)
		args := map[string]interface{}{"path": uploadTestTarget, "source_path": link, "sha256": strings.ToUpper(sha256Hex(body))}
		preview, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
		if text := previewText(t, preview); !strings.Contains(text, local) || !strings.Contains(text, `"type":"local_file"`) {
			t.Errorf("preview does not show the resolved local file:\n%s", text)
		}
		task := waitTask(t, mgr, startedTaskID(t, out), finished)
		if task.Status != tasks.TaskStatusCompleted || !strings.Contains(task.StatusMessage, sha256Hex(body)) {
			t.Fatalf("task %s: %s", task.Status, task.StatusMessage)
		}
		if reqs := u.uploads(); len(reqs) != 1 || string(reqs[0].File) != string(body) {
			t.Errorf("uploads = %d", len(reqs))
		}
	})

	t.Run("without sha256", func(t *testing.T) {
		u := newUploadFake()
		r, _, mgr := u.registry(t, true)
		args := map[string]interface{}{"path": uploadTestTarget, "source_path": local}
		preview, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
		if text := previewText(t, preview); !strings.Contains(text, "computed while uploading") {
			t.Errorf("preview does not say the SHA-256 is computed:\n%s", text)
		}
		task := waitTask(t, mgr, startedTaskID(t, out), finished)
		if task.Status != tasks.TaskStatusCompleted || !strings.Contains(task.StatusMessage, sha256Hex(body)) {
			t.Fatalf("task %s: %s", task.Status, task.StatusMessage)
		}
		res, _ := task.Result.(map[string]interface{})
		if res["sha256"] != sha256Hex(body) {
			t.Errorf("task result = %v", task.Result)
		}
	})

	t.Run("sha256 mismatch", func(t *testing.T) {
		u := newUploadFake()
		r, _, mgr := u.registry(t, true)
		args := map[string]interface{}{"path": uploadTestTarget, "source_path": local, "sha256": strings.Repeat("0", 64)}
		_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
		task := waitTask(t, mgr, startedTaskID(t, out), finished)
		if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, "SHA-256 mismatch") {
			t.Errorf("task %s: %q", task.Status, task.StatusMessage)
		}
		if n := len(u.uploads()); n != 0 {
			t.Errorf("uploaded %d time(s)", n)
		}
	})

	t.Run("changed after the preview", func(t *testing.T) {
		u := newUploadFake()
		r, _, _ := u.registry(t, true)
		changing := filepath.Join(t.TempDir(), "changing.iso")
		if err := os.WriteFile(changing, body, 0o600); err != nil {
			t.Fatal(err)
		}
		args := map[string]interface{}{"path": uploadTestTarget, "source_path": changing}
		first, err := r.CallTool(t.Context(), "upload_file", args)
		if err != nil {
			t.Fatal(err)
		}
		tok := replyToken(t, first)
		if err := os.WriteFile(changing, append(body, 1), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = r.CallTool(t.Context(), "upload_file", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err == nil || !strings.Contains(err.Error(), "different planned operation") {
			t.Errorf("token accepted after the source changed: %v", err)
		}
		if n := len(u.uploads()); n != 0 {
			t.Errorf("uploaded %d time(s)", n)
		}
	})
}

func TestUploadFileUploadErrors(t *testing.T) {
	body := testISO()
	local := filepath.Join(t.TempDir(), "local.iso")
	if err := os.WriteFile(local, body, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "nothing was written to " + uploadTestTarget},
		{http.StatusForbidden, "FULL_ADMIN"},
		{http.StatusInternalServerError, "may be partly written"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			u := newUploadFake()
			u.status = tc.status
			r, _, mgr := u.registry(t, true)
			args := map[string]interface{}{"path": uploadTestTarget, "source_path": local}
			_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
			task := waitTask(t, mgr, startedTaskID(t, out), finished)
			if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, tc.want) {
				t.Errorf("task %s: %q, want %q", task.Status, task.StatusMessage, tc.want)
			}
			if strings.Contains(task.StatusMessage, "1-testkey") {
				t.Errorf("task message quotes the API key: %q", task.StatusMessage)
			}
			if task.JobID != nil {
				t.Errorf("task tracks job %d after a refused upload", *task.JobID)
			}
		})
	}
}

func TestUploadFileJobFailureEndsTask(t *testing.T) {
	body := testISO()
	local := filepath.Join(t.TempDir(), "local.iso")
	if err := os.WriteFile(local, body, 0o600); err != nil {
		t.Fatal(err)
	}
	u := newUploadFake()
	u.jobState = "FAILED"
	r, _, mgr := u.registry(t, true)
	args := map[string]interface{}{"path": uploadTestTarget, "source_path": local}
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	task := waitTask(t, mgr, startedTaskID(t, out), finished)
	if task.Status != tasks.TaskStatusFailed || task.StatusMessage != "[EFAULT] disk full" {
		t.Errorf("task %s: %q", task.Status, task.StatusMessage)
	}
	if task.JobID == nil || *task.JobID != 88 {
		t.Errorf("task job = %v", task.JobID)
	}
}

func TestUploadFileJobOfAnotherMethodFailsTask(t *testing.T) {
	body := testISO()
	local := filepath.Join(t.TempDir(), "local.iso")
	if err := os.WriteFile(local, body, 0o600); err != nil {
		t.Fatal(err)
	}
	u := newUploadFake()
	u.jobState = "RUNNING"
	r, _, mgr := u.registry(t, true)
	args := map[string]interface{}{"path": uploadTestTarget, "source_path": local}
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	id := startedTaskID(t, out)
	task := waitTask(t, mgr, id, func(task *tasks.Task) bool { return task.JobID != nil })
	if task.Status != tasks.TaskStatusWorking {
		t.Fatalf("task %s: %q", task.Status, task.StatusMessage)
	}
	// TrueNAS restarted and job 88 is now something else.
	u.mutate(func() {
		u.jobs[len(u.jobs)-1]["method"] = "pool.scrub.scrub"
	})
	task = waitTask(t, mgr, id, finished)
	if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, "different job") {
		t.Errorf("task %s: %q", task.Status, task.StatusMessage)
	}
}

func TestUploadFileStalledDownload(t *testing.T) {
	body := testISO()
	src := newSourceServer(t, body, func(mux *http.ServeMux) {
		mux.HandleFunc("/stall.iso", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method == http.MethodHead {
				return
			}
			_, _ = w.Write(body[:1000])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
	})
	u := newUploadFake()
	r, _, mgr := u.registry(t, true)
	src.attach(r)
	r.uploadCfg.stallTimeout = 200 * time.Millisecond
	args := map[string]interface{}{"path": uploadTestTarget, "source_url": "https://example.com/stall.iso", "sha256": sha256Hex(body)}
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	task := waitTask(t, mgr, startedTaskID(t, out), finished)
	if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, "no data was received for too long") {
		t.Errorf("task %s: %q", task.Status, task.StatusMessage)
	}
	if left := tempFiles(t, r.uploadCfg.tempDir); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

func TestUploadFileDownloadChecks(t *testing.T) {
	body := testISO()
	src := newSourceServer(t, body, func(mux *http.ServeMux) {
		mux.HandleFunc("/short.iso", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method == http.MethodHead {
				return
			}
			_, _ = w.Write(body[:len(body)/2])
		})
		mux.HandleFunc("/gzip.iso", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(body)
		})
		mux.HandleFunc("/missing.iso", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	})
	for _, tc := range []struct{ path, want string }{
		{"/short.iso", "download failed"},
		{"/gzip.iso", "Content-Encoding"},
		{"/missing.iso", "HTTP 404"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			u := newUploadFake()
			r, _, mgr := u.registry(t, true)
			src.attach(r)
			args := map[string]interface{}{"path": uploadTestTarget, "source_url": "https://example.com" + tc.path, "sha256": sha256Hex(body)}
			_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
			task := waitTask(t, mgr, startedTaskID(t, out), finished)
			if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, tc.want) {
				t.Errorf("task %s: %q, want %q", task.Status, task.StatusMessage, tc.want)
			}
			if n := len(u.uploads()); n != 0 {
				t.Errorf("uploaded %d time(s)", n)
			}
		})
	}

	t.Run("over the limit", func(t *testing.T) {
		u := newUploadFake()
		r, _, _ := u.registry(t, true)
		src.attach(r)
		r.uploadCfg.maxBytes = 1000
		_, err := r.CallTool(t.Context(), "upload_file", mergeArgs(urlUploadArgs(body), map[string]interface{}{"dry_run": true}))
		if err == nil || !strings.Contains(err.Error(), "over the 1000 B limit") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestUploadFileUnknownSizeWarns(t *testing.T) {
	body := testISO()
	src := newSourceServer(t, body, func(mux *http.ServeMux) {
		mux.HandleFunc("/chunked.iso", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.(http.Flusher).Flush()
			_, _ = w.Write(body)
		})
	})
	u := newUploadFake()
	r, _, mgr := u.registry(t, true)
	src.attach(r)
	args := map[string]interface{}{"path": uploadTestTarget, "source_url": "https://example.com/chunked.iso", "sha256": sha256Hex(body)}
	preview, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	text := previewText(t, preview)
	if !strings.Contains(text, "does not report its size") || !strings.Contains(text, "unknown until downloaded") {
		t.Errorf("preview does not say the size is unknown:\n%s", text)
	}
	task := waitTask(t, mgr, startedTaskID(t, out), finished)
	if task.Status != tasks.TaskStatusCompleted {
		t.Fatalf("task %s: %s", task.Status, task.StatusMessage)
	}

	// The free space is checked again once the size is known.
	u2 := newUploadFake()
	r2, _, mgr2 := u2.registry(t, true)
	src.attach(r2)
	u2.avail = 1000
	_, out = confirmAndRun(t, r2, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	task = waitTask(t, mgr2, startedTaskID(t, out), finished)
	if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, "not enough for 256.02 KiB") {
		t.Errorf("task %s: %q", task.Status, task.StatusMessage)
	}
	if n := len(u2.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

func TestUploadFileOneUploadPerTarget(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	release := make(chan struct{})
	src := newSourceServer(t, body, func(mux *http.ServeMux) {
		mux.HandleFunc("/slow.iso", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method == http.MethodHead {
				return
			}
			<-release
			_, _ = w.Write(body)
		})
	})
	r, _, mgr := u.registry(t, true)
	src.attach(r)
	args := map[string]interface{}{"path": uploadTestTarget, "source_url": "https://example.com/slow.iso", "sha256": sha256Hex(body)}
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	id := startedTaskID(t, out)

	first, err := r.CallTool(t.Context(), "upload_file", args)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CallTool(t.Context(), "upload_file", mergeArgs(args, map[string]interface{}{"confirmation_token": replyToken(t, first)}))
	if err == nil || !strings.Contains(err.Error(), "already running") || !strings.Contains(err.Error(), "nothing was uploaded") {
		t.Errorf("second upload to the same target: %v", err)
	}
	close(release)
	if task := waitTask(t, mgr, id, finished); task.Status != tasks.TaskStatusCompleted {
		t.Errorf("task %s: %s", task.Status, task.StatusMessage)
	}
	if n := len(u.uploads()); n != 1 {
		t.Errorf("uploaded %d time(s), want 1", n)
	}
}

func TestUploadFileWithoutTaskManagerRunsInline(t *testing.T) {
	body := testISO()
	local := filepath.Join(t.TempDir(), "local.iso")
	if err := os.WriteFile(local, body, 0o600); err != nil {
		t.Fatal(err)
	}
	u := newUploadFake()
	r, _, _ := u.registry(t, false)
	args := map[string]interface{}{"path": uploadTestTarget, "source_path": local}
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	m := decodeObj(t, out)
	if m["job_id"] != 88.0 || m["sha256"] != sha256Hex(body) || m["size"] != float64(len(body)) {
		t.Errorf("reply = %s", out)
	}
	if n := len(u.uploads()); n != 1 {
		t.Errorf("uploaded %d time(s), want 1", n)
	}
}

func TestUploadFileTokenBoundToArguments(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	r, _, _ := u.registry(t, true)
	src := newSourceServer(t, body, nil)
	src.attach(r)
	args := urlUploadArgs(body)
	first, err := r.CallTool(t.Context(), "upload_file", args)
	if err != nil {
		t.Fatal(err)
	}
	tok := replyToken(t, first)
	other := mergeArgs(args, map[string]interface{}{"path": "/mnt/tank/shares/docs/other.iso", "confirmation_token": tok})
	if _, err := r.CallTool(t.Context(), "upload_file", other); err == nil || !strings.Contains(err.Error(), "different arguments") {
		t.Errorf("token accepted for another target: %v", err)
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

func TestRestrictedAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true, "127.0.0.1": true, "169.254.169.254": true,
		"100.64.0.1": true, "0.0.0.0": true, "::1": true, "fe80::1": true, "fd00::1": true, "::ffff:10.0.0.1": true,
		"224.0.0.1": true, "192.0.2.10": false, "198.51.100.7": false, "2001:db8::1": false,
		"0.1.2.3": true, "198.18.0.1": true, "198.19.255.254": true, "240.0.0.1": true, "255.255.255.255": true,
		"192.0.0.8": true, "64:ff9b::a00:1": true, "64:ff9b:1::a00:1": true, "2002:a00:1::1": true,
		"2001:0:a00:1::1": true, "::a00:1": true, "100::1": true, "ff02::1": true, "::": true,
		"203.0.113.9": false, "2606:4700::1": false,
		// IPv4-mapped forms of the reserved IPv4 ranges.
		"::ffff:100.64.0.1": true, "::ffff:198.18.0.1": true, "::ffff:0.1.2.3": true,
		"::ffff:240.0.0.1": true, "::ffff:192.0.0.8": true, "::ffff:192.0.2.10": false,
	} {
		if got := restrictedAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("restrictedAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

// TestSourceAddrAllowed checks the internal-mirror exception: only RFC 1918
// and fc00::/7 addresses are unlocked, never loopback, link-local,
// shared, unspecified, multicast or reserved ones.
func TestSourceAddrAllowed(t *testing.T) {
	for addr, want := range map[string][2]bool{ // {public host, internal mirror}
		"192.0.2.10":      {true, true},
		"10.1.2.3":        {false, true},
		"172.16.0.1":      {false, true},
		"192.168.1.1":     {false, true},
		"fd00::1":         {false, true},
		"::ffff:10.0.0.1": {false, true},
		"127.0.0.1":       {false, false},
		"::1":             {false, false},
		"169.254.169.254": {false, false},
		"fe80::1":         {false, false},
		"100.64.0.1":      {false, false},
		"0.0.0.0":         {false, false},
		"::":              {false, false},
		"224.0.0.1":       {false, false},
		"ff02::1":         {false, false},
		"198.18.0.1":      {false, false},
		"240.0.0.1":       {false, false},
		"64:ff9b::a00:1":  {false, false},
	} {
		a := netip.MustParseAddr(addr)
		if got := sourceAddrAllowed(a, false); got != want[0] {
			t.Errorf("sourceAddrAllowed(%s, public host) = %v, want %v", addr, got, want[0])
		}
		if got := sourceAddrAllowed(a, true); got != want[1] {
			t.Errorf("sourceAddrAllowed(%s, internal mirror) = %v, want %v", addr, got, want[1])
		}
	}
}

func TestDisplayURLHidesQuery(t *testing.T) {
	u, err := parseSourceURL("https://example.com/a.iso?X-Signature=secret#frag")
	if err != nil {
		t.Fatal(err)
	}
	if got := displayURL(u); got != "https://example.com/a.iso?..." {
		t.Errorf("displayURL = %q", got)
	}
}

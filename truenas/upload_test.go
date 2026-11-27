package truenas

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type uploadSeen struct {
	auth          string
	contentLength int64
	parts         []string
	data          map[string]interface{}
	file          []byte
	filename      string
}

// uploadServer serves /_upload with handler and records what it read.
func uploadServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, seen *uploadSeen)) (*Client, *httptest.Server, func() []uploadSeen) {
	t.Helper()
	var mu sync.Mutex
	var all []uploadSeen
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen := uploadSeen{auth: r.Header.Get("Authorization"), contentLength: r.ContentLength}
		defer func() {
			mu.Lock()
			all = append(all, seen)
			mu.Unlock()
		}()
		if r.URL.Path != uploadPath {
			http.NotFound(w, r)
			return
		}
		handler(w, r, &seen)
	}))
	t.Cleanup(srv.Close)
	endpoint := "wss://" + strings.TrimPrefix(srv.URL, "https://") + apiPath
	c, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, srv, func() []uploadSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]uploadSeen(nil), all...)
	}
}

func readUpload(t *testing.T, r *http.Request, seen *uploadSeen) bool {
	mr, err := r.MultipartReader()
	if err != nil {
		t.Errorf("not multipart: %v", err)
		return false
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return true
		}
		if err != nil {
			return false
		}
		b, err := io.ReadAll(part)
		if err != nil {
			return false
		}
		seen.parts = append(seen.parts, part.FormName())
		switch part.FormName() {
		case "data":
			_ = json.Unmarshal(b, &seen.data)
		case "file":
			seen.file = b
			seen.filename = part.FileName()
		}
	}
}

func TestUploadFileSendsDataThenFile(t *testing.T) {
	logs := logsOf(t)
	c, _, seen := uploadServer(t, func(w http.ResponseWriter, r *http.Request, s *uploadSeen) {
		if readUpload(t, r, s) {
			_, _ = w.Write([]byte(`{"job_id": 42}`))
		}
	})
	body := bytes.Repeat([]byte("0123456789abcdef"), 70000)
	var progress atomic.Int64
	id, err := c.UploadFile(context.Background(), "filesystem.put",
		[]interface{}{"/mnt/tank/isos/installer.iso", map[string]interface{}{"append": false}},
		"inst\"all/er.iso", bytes.NewReader(body), int64(len(body)),
		UploadOptions{StallTimeout: time.Minute, Progress: func(n int64) { progress.Store(n) }})
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Errorf("job id = %d", id)
	}
	if progress.Load() != int64(len(body)) {
		t.Errorf("progress = %d, want %d", progress.Load(), len(body))
	}
	got := seen()
	if len(got) != 1 {
		t.Fatalf("%d requests", len(got))
	}
	s := got[0]
	if s.auth != "Bearer "+testAPIKey {
		t.Errorf("Authorization = %q", s.auth)
	}
	if strings.Join(s.parts, ",") != "data,file" {
		t.Errorf("parts = %v, want data then file", s.parts)
	}
	data, _ := json.Marshal(s.data)
	if string(data) != `{"method":"filesystem.put","params":["/mnt/tank/isos/installer.iso",{"append":false}]}` {
		t.Errorf("data = %s", data)
	}
	if !bytes.Equal(s.file, body) {
		t.Errorf("file part is %d bytes, want %d", len(s.file), len(body))
	}
	if s.filename != "inst_all_er.iso" {
		t.Errorf("file name = %q", s.filename)
	}
	if s.contentLength <= int64(len(body)) {
		t.Errorf("Content-Length = %d, want the exact multipart length", s.contentLength)
	}
	if strings.Contains(logs.String(), testAPIKey) {
		t.Errorf("the API key was logged:\n%s", logs.String())
	}
}

func TestUploadFileStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		status  int
		nothing bool
		want    string
	}{
		{http.StatusUnauthorized, true, "did not accept the API key"},
		{http.StatusForbidden, true, "FULL_ADMIN"},
		{http.StatusMethodNotAllowed, true, "HTTP 405"},
		{http.StatusPreconditionFailed, true, "HTTP 412"},
		{http.StatusUnprocessableEntity, true, "HTTP 422"},
		{http.StatusInternalServerError, false, "HTTP 500"},
		{http.StatusBadGateway, false, "HTTP 502"},
	} {
		logs := logsOf(t)
		c, _, _ := uploadServer(t, func(w http.ResponseWriter, r *http.Request, s *uploadSeen) {
			_, _ = io.Copy(io.Discard, r.Body)
			http.Error(w, "denied for "+testAPIKey+" "+strings.Repeat("x", 2000), tc.status)
		})
		_, err := c.UploadFile(context.Background(), "filesystem.put", []interface{}{"/mnt/tank/a", map[string]interface{}{}},
			"a", strings.NewReader("abc"), 3, UploadOptions{})
		var se *UploadStatusError
		if !errors.As(err, &se) {
			t.Fatalf("%d: err = %v, want an UploadStatusError", tc.status, err)
		}
		if se.Status != tc.status || se.NothingWritten() != tc.nothing || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%d: status %d, nothing written %v, err %v", tc.status, se.Status, se.NothingWritten(), err)
		}
		if strings.Contains(err.Error(), testAPIKey) || strings.Contains(logs.String(), testAPIKey) {
			t.Errorf("%d: the API key was quoted: %v", tc.status, err)
		}
		if len(err.Error()) > maxUploadErrorText+400 {
			t.Errorf("%d: the error is not bounded (%d bytes)", tc.status, len(err.Error()))
		}
	}
}

func TestUploadFileDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
	}))
	t.Cleanup(other.Close)
	c, _, _ := uploadServer(t, func(w http.ResponseWriter, r *http.Request, s *uploadSeen) {
		http.Redirect(w, r, other.URL+uploadPath, http.StatusTemporaryRedirect)
	})
	_, err := c.UploadFile(context.Background(), "filesystem.put", []interface{}{"/mnt/tank/a", map[string]interface{}{}},
		"a", strings.NewReader("abc"), 3, UploadOptions{})
	if err == nil {
		t.Fatal("a redirect was accepted")
	}
	if followed.Load() {
		t.Error("the redirect was followed, resending the API key")
	}
}

func TestUploadFileShortSource(t *testing.T) {
	c, _, _ := uploadServer(t, func(w http.ResponseWriter, r *http.Request, s *uploadSeen) {
		if readUpload(t, r, s) {
			_, _ = w.Write([]byte(`{"job_id": 1}`))
		}
	})
	_, err := c.UploadFile(context.Background(), "filesystem.put", []interface{}{"/mnt/tank/a", map[string]interface{}{}},
		"a", strings.NewReader("abc"), 10, UploadOptions{})
	if err == nil || !strings.Contains(err.Error(), "ended 7 bytes early") {
		t.Errorf("err = %v", err)
	}
}

type blockingReader struct{ release chan struct{} }

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

func TestUploadFileStall(t *testing.T) {
	c, _, _ := uploadServer(t, func(w http.ResponseWriter, r *http.Request, s *uploadSeen) {
		readUpload(t, r, s)
	})
	src := &blockingReader{release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := c.UploadFile(context.Background(), "filesystem.put", []interface{}{"/mnt/tank/a", map[string]interface{}{}},
			"a", src, 10, UploadOptions{StallTimeout: 100 * time.Millisecond})
		done <- err
	}()
	// The transport is still blocked in Read; release it once the stall
	// has cancelled the request, as closing a real source would.
	time.Sleep(300 * time.Millisecond)
	close(src.release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrUploadStalled) {
			t.Errorf("err = %v, want ErrUploadStalled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("UploadFile did not return after a stall")
	}
}

func TestUploadFileReplyWithoutJobID(t *testing.T) {
	c, _, _ := uploadServer(t, func(w http.ResponseWriter, r *http.Request, s *uploadSeen) {
		readUpload(t, r, s)
		_, _ = w.Write([]byte(`{}`))
	})
	_, err := c.UploadFile(context.Background(), "filesystem.put", []interface{}{"/mnt/tank/a", map[string]interface{}{}},
		"a", strings.NewReader("abc"), 3, UploadOptions{})
	if err == nil || !strings.Contains(err.Error(), "without a job id") {
		t.Errorf("err = %v", err)
	}
}

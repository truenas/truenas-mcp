package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// slowSource serves /slow.iso, whose GET blocks until release is called
// (or the request is cancelled). started receives one value per GET.
func slowSource(t *testing.T, body []byte) (src *sourceServer, release func(), started chan struct{}) {
	t.Helper()
	gate := make(chan struct{})
	started = make(chan struct{}, 16)
	src = newSourceServer(t, body, func(mux *http.ServeMux) {
		mux.HandleFunc("/slow.iso", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method == http.MethodHead {
				return
			}
			started <- struct{}{}
			select {
			case <-gate:
				_, _ = w.Write(body)
			case <-r.Context().Done():
			}
		})
	})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	// Registered after the server, so it runs first: a blocked handler
	// would keep the server's Close waiting.
	t.Cleanup(release)
	return src, release, started
}

func slowArgs(body []byte, target string) map[string]interface{} {
	return map[string]interface{}{"path": target, "source_url": "https://example.com/slow.iso", "sha256": sha256Hex(body)}
}

// waitNoUploads waits until no upload slot is held.
func waitNoUploads(t *testing.T, r *Registry) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.uploadMu.Lock()
		n := len(r.uploads)
		r.uploadMu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d upload slot(s) still held", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitStarted(t *testing.T, started chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the download did not start")
	}
}

// TestUploadFileTargetChangedDuringDownloadRecheck is the test of the
// re-check after the download: with overwrite, the existing file is
// replaced by another (new inode) while the download runs. Nothing
// blocks the new state on its own, so only the comparison with the
// confirmed plan catches it.
func TestUploadFileTargetChangedDuringDownloadRecheck(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	existingISO(u, 1<<20, 1767225600.5, 900)
	src, release, started := slowSource(t, body)
	r, _, mgr := u.registry(t, true)
	src.attach(r)
	args := mergeArgs(slowArgs(body, uploadTestTarget), map[string]interface{}{"overwrite": true})
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
	id := startedTaskID(t, out)
	waitStarted(t, started)
	waitTask(t, mgr, id, func(task *tasks.Task) bool { return strings.HasPrefix(task.StatusMessage, "Downloading") })
	existingISO(u, 1<<20, 1767225600.5, 901)
	release()
	task := waitTask(t, mgr, id, finished)
	if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, "changed since the confirmation") || !strings.Contains(task.StatusMessage, "nothing was uploaded") {
		t.Errorf("task %s: %q", task.Status, task.StatusMessage)
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

// TestUploadFileConcurrencyCap checks that at most two uploads run at
// once, and that finished uploads free their slots.
func TestUploadFileConcurrencyCap(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	src, release, started := slowSource(t, body)
	r, _, mgr := u.registry(t, true)
	src.attach(r)
	a := slowArgs(body, "/mnt/tank/shares/docs/a.iso")
	b := slowArgs(body, "/mnt/tank/shares/docs/b.iso")
	c := urlUploadArgs(body)
	c["path"] = "/mnt/tank/shares/docs/c.iso"

	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, a)
	idA := startedTaskID(t, out)
	_, out = confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, b)
	idB := startedTaskID(t, out)
	waitStarted(t, started)
	waitStarted(t, started)

	first, err := r.CallTool(t.Context(), "upload_file", c)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CallTool(t.Context(), "upload_file", mergeArgs(c, map[string]interface{}{"confirmation_token": replyToken(t, first)}))
	if err == nil || !strings.Contains(err.Error(), "2 uploads are already running") || !strings.Contains(err.Error(), "nothing was uploaded") {
		t.Errorf("third upload: %v", err)
	}

	release()
	for _, id := range []string{idA, idB} {
		if task := waitTask(t, mgr, id, finished); task.Status != tasks.TaskStatusCompleted {
			t.Errorf("task %s: %s", task.Status, task.StatusMessage)
		}
	}
	waitNoUploads(t, r)
	_, out = confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, c)
	if task := waitTask(t, mgr, startedTaskID(t, out), finished); task.Status != tasks.TaskStatusCompleted {
		t.Errorf("third upload after the others finished: task %s: %s", task.Status, task.StatusMessage)
	}
	if n := len(u.uploads()); n != 3 {
		t.Errorf("uploaded %d time(s), want 3", n)
	}
}

// TestUploadFileSlotReleased checks that an upload's slot is freed however
// it ends (success, failure, a cancelled call, the time limit), so a later
// upload to the same target runs.
func TestUploadFileSlotReleased(t *testing.T) {
	body := testISO()

	t.Run("success and failure", func(t *testing.T) {
		u := newUploadFake()
		r, _, mgr := u.registry(t, true)
		src := newSourceServer(t, body, nil)
		src.attach(r)
		bad := mergeArgs(urlUploadArgs(body), map[string]interface{}{"sha256": strings.Repeat("0", 64)})
		for i := 0; i < maxConcurrentUploads+1; i++ {
			_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, bad)
			if task := waitTask(t, mgr, startedTaskID(t, out), finished); task.Status != tasks.TaskStatusFailed {
				t.Fatalf("task %s: %s", task.Status, task.StatusMessage)
			}
			waitNoUploads(t, r)
		}
		for i := 0; i < 2; i++ {
			_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, urlUploadArgs(body))
			if task := waitTask(t, mgr, startedTaskID(t, out), finished); task.Status != tasks.TaskStatusCompleted {
				t.Fatalf("upload %d: task %s: %s", i, task.Status, task.StatusMessage)
			}
			waitNoUploads(t, r)
		}
	})

	t.Run("cancelled inline call", func(t *testing.T) {
		u := newUploadFake()
		src, release, started := slowSource(t, body)
		r, _, _ := u.registry(t, false)
		src.attach(r)
		args := slowArgs(body, uploadTestTarget)
		first, err := r.CallTool(t.Context(), "upload_file", args)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := r.CallTool(ctx, "upload_file", mergeArgs(args, map[string]interface{}{"confirmation_token": replyToken(t, first)}))
			done <- err
		}()
		waitStarted(t, started)
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Error("a cancelled upload succeeded")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the cancelled upload did not return")
		}
		waitNoUploads(t, r)
		release()
		_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, args)
		if m := decodeObj(t, out); m["job_id"] != 88.0 {
			t.Errorf("upload after the cancelled one: %s", out)
		}
	})
}

// TestUploadFileMaxDuration checks that a transfer that runs over the time
// limit is stopped (the limit is lowered from 12 hours), and its slot freed.
func TestUploadFileMaxDuration(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	src, _, started := slowSource(t, body)
	r, _, mgr := u.registry(t, true)
	src.attach(r)
	if r.uploadCfg.maxDuration != 12*time.Hour {
		t.Errorf("default maxDuration = %s, want 12h", r.uploadCfg.maxDuration)
	}
	r.uploadCfg.maxDuration = 300 * time.Millisecond
	_, out := confirmAndRun(t, r, &fakeMiddleware{}, "upload_file", filesystemPutMethod, slowArgs(body, uploadTestTarget))
	id := startedTaskID(t, out)
	waitStarted(t, started)
	task := waitTask(t, mgr, id, finished)
	if task.Status != tasks.TaskStatusFailed || !strings.Contains(task.StatusMessage, "ran over the 300ms limit") || !strings.Contains(task.StatusMessage, "nothing was uploaded") {
		t.Errorf("task %s: %q", task.Status, task.StatusMessage)
	}
	waitNoUploads(t, r)
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
	if left := tempFiles(t, r.uploadCfg.tempDir); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

// TestUploadSourceNoProxy checks that downloads never use a proxy, so the
// address checks see the real destination of every connection.
func TestUploadSourceNoProxy(t *testing.T) {
	r := NewRegistry(nil, nil)
	u, _ := url.Parse("https://192.0.2.10/a.iso")
	sc, err := r.newSourceClient(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.close()
	tr, ok := sc.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", sc.http.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the download transport uses a proxy")
	}
}

// TestUploadFileRedirectWithUserInfoRefused checks that a redirect to a URL
// with user info is not followed.
func TestUploadFileRedirectWithUserInfoRefused(t *testing.T) {
	body := testISO()
	src := newSourceServer(t, body, func(mux *http.ServeMux) {
		mux.HandleFunc("/to-userinfo", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://user:pw@example.com/isos/installer.iso", http.StatusFound)
		})
	})
	u := newUploadFake()
	r, _, _ := u.registry(t, false)
	src.attach(r)
	_, err := r.CallTool(t.Context(), "upload_file", map[string]interface{}{"path": uploadTestTarget, "source_url": "https://example.com/to-userinfo", "sha256": sha256Hex(body), "dry_run": true})
	if err == nil || !strings.Contains(err.Error(), "redirected to a URL with user info") || strings.Contains(err.Error(), "pw@") {
		t.Errorf("err = %v", err)
	}
}

// writeSource writes a source file below dir, creating directories.
func writeSource(t *testing.T, dir, rel string, body []byte) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestUploadFileLocalSourceOffByDefault checks that source_path is refused
// unless the operator allowed directories, before the path is looked at,
// and that the preview issues no confirmation token.
func TestUploadFileLocalSourceOffByDefault(t *testing.T) {
	body := testISO()
	local := writeSource(t, t.TempDir(), "local.iso", body)
	if src, err := NewRegistry(nil, nil).parseUploadSource(map[string]interface{}{"source_path": local}); err == nil || !strings.Contains(err.Error(), "source_path is disabled") || src.LocalPath != "" {
		t.Errorf("a new registry read a local file: %v", err)
	}
	u := newUploadFake()
	r, _, _ := u.registry(t, false)
	if err := r.SetUploadSourceDirs(nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{local, filepath.Join(t.TempDir(), "missing.iso")} {
		out, err := r.CallTool(t.Context(), "upload_file", map[string]interface{}{"path": uploadTestTarget, "source_path": p})
		if err == nil || !strings.Contains(err.Error(), "source_path is disabled") || !strings.Contains(err.Error(), UploadSourceSetting) {
			t.Errorf("%s: err = %v", p, err)
		}
		if replyToken(t, out) != "" {
			t.Errorf("%s: a token was issued", p)
		}
	}
	// Blank entries do not enable it.
	if err := r.SetUploadSourceDirs([]string{"", "  "}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CallTool(t.Context(), "upload_file", map[string]interface{}{"path": uploadTestTarget, "source_path": local, "dry_run": true}); err == nil || !strings.Contains(err.Error(), "source_path is disabled") {
		t.Errorf("blank directories enabled source_path: %v", err)
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

// TestUploadFileLocalSourceRefusals checks the rules for an enabled
// source_path: inside an allowed directory, no hidden component, not this
// server's own files, not key or client configuration files.
func TestUploadFileLocalSourceRefusals(t *testing.T) {
	body := testISO()
	home := t.TempDir()
	outside := t.TempDir()
	isos := filepath.Join(home, "isos")
	good := writeSource(t, isos, "installer.iso", body)
	sshKey := writeSource(t, home, ".ssh/id_ed25519", []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n"))
	awsCreds := writeSource(t, isos, ".aws/credentials", []byte("[default]\nfake\n"))
	dotFile := writeSource(t, isos, ".hidden.iso", body)
	configISO := writeSource(t, isos, ".config/truenas-mcp/a.iso", body)
	keyFile := writeSource(t, isos, "own/apikey.txt", []byte("1-fake\n"))
	cfgDir := filepath.Join(isos, "cfg")
	cfgFile := writeSource(t, cfgDir, "truenas.pin", []byte("SHA256:00\n"))
	cfgOther := writeSource(t, cfgDir, "other.iso", body)
	claude := writeSource(t, isos, "claude_desktop_config.json", []byte("{}"))
	cursorCfg := writeSource(t, isos, "MCP.json", []byte("{}"))
	priv := writeSource(t, isos, "server.key", []byte("fake"))
	pem := writeSource(t, isos, "ca.PEM", []byte("fake"))
	outsideFile := writeSource(t, outside, "installer.iso", body)
	linkToKey := filepath.Join(isos, "innocent.iso")
	if err := os.Symlink(sshKey, linkToKey); err != nil {
		t.Fatal(err)
	}
	linkOut := filepath.Join(isos, "elsewhere.iso")
	if err := os.Symlink(outsideFile, linkOut); err != nil {
		t.Fatal(err)
	}
	// A directory whose name only starts with the allowed one is not
	// below it.
	sibling := writeSource(t, home, "isos2/installer.iso", body)
	// A hard link to this server's key file has its own innocent path.
	hardKey := filepath.Join(isos, "notes.iso")
	if err := os.Link(keyFile, hardKey); err != nil {
		t.Fatal(err)
	}
	// The allowed directory is home/isos; home itself (with .ssh) is not.
	// An operator who lists the whole home directory still gets the
	// hidden-path refusal, checked below with a second registry.
	u := newUploadFake()
	r, _, _ := u.registry(t, false)
	if err := r.SetUploadSourceDirs([]string{isos}, []string{keyFile, cfgDir}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, want string }{
		{sshKey, "not below an allowed upload source directory"},
		{outsideFile, "not below an allowed upload source directory"},
		{filepath.Join(outside, "missing.iso"), "not below an allowed upload source directory"},
		{linkOut, "not below an allowed upload source directory"},
		{linkToKey, "not below an allowed upload source directory"},
		{sibling, "not below an allowed upload source directory"},
		{hardKey, "this server's own key, certificate or configuration file (another link to it)"},
		{awsCreds, `hidden file or directory ".aws"`},
		{dotFile, `hidden file or directory ".hidden.iso"`},
		{configISO, `hidden file or directory ".config"`},
		{keyFile, "this server's own key, certificate or configuration file"},
		{cfgFile, "this server's own key, certificate or configuration file"},
		{cfgOther, "this server's own key, certificate or configuration file"},
		{claude, "MCP client configuration file"},
		{cursorCfg, "MCP client configuration file"},
		{priv, "key or certificate file"},
		{pem, "key or certificate file"},
	} {
		out, err := r.CallTool(t.Context(), "upload_file", map[string]interface{}{"path": uploadTestTarget, "source_path": tc.path})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.path, err, tc.want)
		}
		if replyToken(t, out) != "" {
			t.Errorf("%s: a token was issued", tc.path)
		}
	}
	if _, err := r.CallTool(t.Context(), "upload_file", map[string]interface{}{"path": uploadTestTarget, "source_path": good, "dry_run": true}); err != nil {
		t.Errorf("allowed file refused: %v", err)
	}

	// A fake ~/.ssh/id_ed25519 is refused even when the operator lists
	// the whole home directory, directly or through a symlink.
	r2, _, _ := newUploadFake().registry(t, false)
	if err := r2.SetUploadSourceDirs([]string{home}, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{sshKey, linkToKey} {
		out, err := r2.CallTool(t.Context(), "upload_file", map[string]interface{}{"path": uploadTestTarget, "source_path": p})
		if err == nil || !strings.Contains(err.Error(), `hidden file or directory ".ssh"`) {
			t.Errorf("%s: err = %v", p, err)
		}
		if replyToken(t, out) != "" {
			t.Errorf("%s: a token was issued for an SSH key", p)
		}
	}
	if n := len(u.uploads()); n != 0 {
		t.Errorf("uploaded %d time(s)", n)
	}
}

// TestSetUploadSourceDirsValidation checks the operator setting itself.
func TestSetUploadSourceDirsValidation(t *testing.T) {
	base := t.TempDir()
	file := writeSource(t, base, "file.iso", []byte("x"))
	hidden := filepath.Join(base, ".isos")
	if err := os.Mkdir(hidden, 0o700); err != nil {
		t.Fatal(err)
	}
	linkToHidden := filepath.Join(base, "isos-link")
	if err := os.Symlink(hidden, linkToHidden); err != nil {
		t.Fatal(err)
	}
	root := string(filepath.Separator)
	if v := filepath.VolumeName(base); v != "" {
		root = v + string(filepath.Separator)
	}
	r := NewRegistry(nil, nil)
	for _, tc := range []struct{ dir, want string }{
		{"isos", "not an absolute path"},
		{filepath.Join(base, "missing"), "upload source directory"},
		{file, "is not a directory"},
		{root, "root of a filesystem"},
		{hidden, `hidden directory ".isos"`},
		{linkToHidden, `hidden directory ".isos"`},
	} {
		if err := r.SetUploadSourceDirs([]string{tc.dir}, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.dir, err, tc.want)
		}
		if ls := r.localSourceConfig(); len(ls.dirs) != 0 {
			t.Errorf("%s: a refused setting enabled local sources", tc.dir)
		}
	}
	if err := r.SetUploadSourceDirs([]string{base}, nil); err != nil {
		t.Errorf("valid directory: %v", err)
	}
}

// replaceSameStat replaces the file at p with a new file (a new inode) of
// the same contents and modification time.
func replaceSameStat(t *testing.T, p string) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	tmp := p + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	if st2, err := os.Stat(p); err != nil || os.SameFile(st, st2) || st2.Size() != st.Size() || !st2.ModTime().Equal(st.ModTime()) {
		t.Fatalf("could not swap %s for a different file with the same size and time", p)
	}
}

// TestUploadFileLocalSourceSwapped checks that a different file swapped in
// at the same path, with the same size and modification time, is refused:
// at confirmation (the token is bound to the device and inode) and after
// confirmation, when the transfer opens the file.
func TestUploadFileLocalSourceSwapped(t *testing.T) {
	body := testISO()
	dir := t.TempDir()
	local := writeSource(t, dir, "local.iso", body)
	st, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	if fileID(st) == "" {
		t.Skip("no device and inode on this platform")
	}

	t.Run("before confirmation", func(t *testing.T) {
		u := newUploadFake()
		r, _, _ := u.registry(t, true)
		args := map[string]interface{}{"path": uploadTestTarget, "source_path": local}
		first, err := r.CallTool(t.Context(), "upload_file", args)
		if err != nil {
			t.Fatal(err)
		}
		replaceSameStat(t, local)
		_, err = r.CallTool(t.Context(), "upload_file", mergeArgs(args, map[string]interface{}{"confirmation_token": replyToken(t, first)}))
		if err == nil || !strings.Contains(err.Error(), "different planned operation") {
			t.Errorf("token accepted for a swapped file: %v", err)
		}
		if n := len(u.uploads()); n != 0 {
			t.Errorf("uploaded %d time(s)", n)
		}
	})

	t.Run("after confirmation", func(t *testing.T) {
		u := newUploadFake()
		r, _, _ := u.registry(t, false)
		args := map[string]interface{}{"path": uploadTestTarget, "source_path": local}
		plan, err := r.planUpload(t.Context(), r.client, args, false)
		if err != nil {
			t.Fatal(err)
		}
		replaceSameStat(t, local)
		_, err = r.runUpload(t.Context(), r.client, args, plan, func(string) {})
		// The opened file is checked before the re-plan (which would
		// also see the new inode), so the refusal names a different file.
		if err == nil || !strings.Contains(err.Error(), "changed since the confirmation (a different file") || !strings.Contains(err.Error(), "nothing was uploaded") {
			t.Errorf("swapped file: err = %v", err)
		}
		if n := len(u.uploads()); n != 0 {
			t.Errorf("uploaded %d time(s)", n)
		}
	})
}

// TestUploadSourceSameLocalFile checks each part of the opened-file check.
func TestUploadSourceSameLocalFile(t *testing.T) {
	dir := t.TempDir()
	a := writeSource(t, dir, "a.iso", []byte("abcdef"))
	b := writeSource(t, dir, "b.iso", []byte("abcdef"))
	stA, _ := os.Stat(a)
	if err := os.Chtimes(b, stA.ModTime(), stA.ModTime()); err != nil {
		t.Fatal(err)
	}
	stB, _ := os.Stat(b)
	src := uploadSource{LocalPath: a, Size: stA.Size(), ModTime: stA.ModTime(), FileID: fileID(stA), info: stA}
	if !src.sameLocalFile(stA) {
		t.Fatal("the same file does not match")
	}
	cases := map[string]uploadSource{
		"no stat":           {LocalPath: a, Size: stA.Size(), ModTime: stA.ModTime(), FileID: fileID(stA)},
		"other size":        {LocalPath: a, Size: stA.Size() + 1, ModTime: stA.ModTime(), FileID: fileID(stA), info: stA},
		"other time":        {LocalPath: a, Size: stA.Size(), ModTime: stA.ModTime().Add(time.Second), FileID: fileID(stA), info: stA},
		"other stat":        {LocalPath: a, Size: stA.Size(), ModTime: stA.ModTime(), FileID: fileID(stA), info: stB},
		"other file id":     {LocalPath: a, Size: stA.Size(), ModTime: stA.ModTime(), FileID: "1:2", info: stA},
		"file id not bound": {LocalPath: a, Size: stA.Size(), ModTime: stA.ModTime(), FileID: "", info: stA},
	}
	for name, s := range cases {
		if name == "file id not bound" && fileID(stA) == "" {
			continue
		}
		if s.sameLocalFile(stA) {
			t.Errorf("%s: matched", name)
		}
	}
}

// TestSweepStaleUploadTemps checks the startup sweep: only old regular
// files with upload_file's prefix are removed.
func TestSweepStaleUploadTemps(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-14 * time.Hour)
	mk := func(name string, mtime time.Time) string {
		p := writeSource(t, dir, name, []byte("x"))
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := mk(uploadTempPrefix+"123", old)
	fresh := mk(uploadTempPrefix+"456", time.Now())
	other := mk("other-123", old)
	sub := filepath.Join(dir, uploadTempPrefix+"dir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, old, old); err != nil {
		t.Fatal(err)
	}
	target := mk("kept-target", old)
	link := filepath.Join(dir, uploadTempPrefix+"link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(nil, nil)
	r.uploadCfg.tempDir = dir
	if n := r.SweepStaleUploadTemps(); n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale temporary file was kept")
	}
	for _, p := range []string{fresh, other, sub, target, link} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	// A file just inside the limit (13 hours) is kept.
	recent := mk(uploadTempPrefix+"789", time.Now().Add(-12*time.Hour))
	r.SweepStaleUploadTemps()
	if _, err := os.Stat(recent); err != nil {
		t.Error("a file younger than the transfer limit was removed")
	}
}

// TestUploadFileOverwriteCountsReplacedFile checks that the free-space
// check counts the space of the file an overwrite replaces.
func TestUploadFileOverwriteCountsReplacedFile(t *testing.T) {
	body := testISO()
	u := newUploadFake()
	existingISO(u, 1<<20, 1767225600.5, 900)
	u.avail = 1000
	r, _, _ := u.registry(t, false)
	src := newSourceServer(t, body, nil)
	src.attach(r)
	args := mergeArgs(urlUploadArgs(body), map[string]interface{}{"overwrite": true, "dry_run": true})
	if _, err := r.CallTool(t.Context(), "upload_file", args); err != nil {
		t.Errorf("overwrite of a larger file refused for space: %v", err)
	}
}

// TestUploadFileLocalSourceChangedDuringUpload checks the SHA-256 of the
// bytes actually sent: a local file rewritten in place after every check
// before the upload (same inode, so the open descriptor reads the new
// bytes) fails the transfer and says not to use the target.
func TestUploadFileLocalSourceChangedDuringUpload(t *testing.T) {
	body := testISO()
	local := writeSource(t, t.TempDir(), "local.iso", body)
	u := newUploadFake()
	r, _, _ := u.registry(t, false)
	args := map[string]interface{}{"path": uploadTestTarget, "source_path": local, "sha256": sha256Hex(body)}
	plan, err := r.planUpload(t.Context(), r.client, args, false)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), body...)
	changed[len(changed)-1] ^= 0xff
	rewritten := false
	_, err = r.runUpload(t.Context(), r.client, args, plan, func(msg string) {
		// The first upload progress message comes after the re-check and
		// before any byte is read for the upload.
		if !rewritten && strings.HasPrefix(msg, "Uploading to ") {
			rewritten = true
			f, err := os.OpenFile(local, os.O_WRONLY, 0)
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			if _, err := f.WriteAt(changed, 0); err != nil {
				t.Error(err)
			}
		}
	})
	if !rewritten {
		t.Fatal("the upload never started")
	}
	if err == nil || !strings.Contains(err.Error(), "the source changed during the upload. Do not use "+uploadTestTarget) {
		t.Errorf("err = %v, want the in-upload SHA-256 mismatch", err)
	}
	if n := len(u.uploads()); n != 1 {
		t.Errorf("uploads = %d, want 1 (the mismatch is found after sending)", n)
	}
}

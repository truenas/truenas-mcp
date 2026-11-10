package tools

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// jobFake is a TrueNAS fake whose core.get_jobs answer can be changed while a
// task is tracked, and which can drop the connection so that the client
// reconnects and logs in again (a new session generation), as it does after a
// middleware restart.
type jobFake struct {
	mu     sync.Mutex
	jobs   []interface{}          // core.get_jobs answer
	rpcErr map[string]interface{} // when set, core.get_jobs fails with it
	drop   bool                   // close the connection after the next reply
	logins int
}

func (f *jobFake) set(fn func(f *jobFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *jobFake) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		f.mu.Lock()
		resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "auth.login_ex":
			f.logins++
			resp["result"] = map[string]interface{}{"response_type": "SUCCESS"}
		case "core.get_jobs":
			if f.rpcErr != nil {
				resp["error"] = f.rpcErr
			} else {
				resp["result"] = f.jobs
			}
		default:
			resp["result"] = nil
		}
		drop := f.drop
		f.drop = false
		f.mu.Unlock()
		if err := conn.WriteJSON(resp); err != nil || drop {
			return
		}
	}
}

func newJobFakeRegistry(t *testing.T, cfg tasks.PollerConfig) (*Registry, *tasks.Manager, *jobFake) {
	t.Helper()
	f := &jobFake{}
	srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	endpoint := "wss" + strings.TrimPrefix(srv.URL, "https") + "/api/current"
	client, err := truenas.NewClientWithOptions(endpoint, "1-testkey", &tls.Config{RootCAs: roots}, truenas.Options{APIUser: "svc"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Hour
	}
	if cfg.CleanupInterval == 0 {
		cfg.CleanupInterval = time.Hour
	}
	mgr := tasks.NewManager(client, cfg)
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), mgr, f
}

func jobRow(id int, method, state string, startedMs int64) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "method": method, "state": state,
		"time_started": map[string]interface{}{"$date": startedMs},
		"progress":     map[string]interface{}{"percent": 10, "description": "working"},
	}
}

// pollUntil polls until the task leaves "working" or tries run out, so that a
// poll which lands on the dropped connection is simply retried.
func pollUntil(t *testing.T, r *Registry, mgr *tasks.Manager, id string, tries int) map[string]interface{} {
	t.Helper()
	var st map[string]interface{}
	for i := 0; i < tries; i++ {
		mgr.PollNow()
		st = taskState(t, r, id)
		if st["status"] != "working" {
			break
		}
	}
	return st
}

// After a middleware restart job ids start again at 1, so the tracked id can
// belong to a different job. The task must fail with a restart message, not
// report the other job's state as its own.
func TestTaskJobIDReusedAfterRestart(t *testing.T) {
	r, mgr, f := newJobFakeRegistry(t, tasks.PollerConfig{})
	f.set(func(f *jobFake) { f.jobs = []interface{}{jobRow(3, "app.start", "RUNNING", 1_700_000_000_000)} })
	task, err := mgr.CreateJobTask("start_app", nil, 3, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mgr.PollNow()
	if st := taskState(t, r, task.TaskID); st["status"] != "working" {
		t.Fatalf("before restart: %v", st)
	}

	// Restart: the connection drops after this poll's reply, and from then
	// on job 3 is someone else's job, which has already succeeded. Every
	// later read goes over a new connection and login.
	f.set(func(f *jobFake) { f.drop = true })
	mgr.PollNow()
	f.set(func(f *jobFake) {
		f.jobs = []interface{}{jobRow(3, "pool.dataset.query", "SUCCESS", 1_700_000_900_000)}
	})
	st := pollUntil(t, r, mgr, task.TaskID, 5)
	if st["status"] != "failed" {
		t.Fatalf("status = %v (%v), want failed: the job id now belongs to another job", st["status"], st["statusMessage"])
	}
	msg, _ := st["statusMessage"].(string)
	if !strings.Contains(msg, "restarted") || !strings.Contains(msg, "query_apps") {
		t.Errorf("statusMessage = %q, want a restart message pointing to query_apps", msg)
	}
	if f.logins < 2 {
		t.Errorf("client did not reconnect (logins = %d); the test does not cover a new session", f.logins)
	}
}

// For apply_update a reboot is expected; the message says so and points to
// system_info and update_status.
func TestApplyUpdateTaskAfterReboot(t *testing.T) {
	r, mgr, f := newJobFakeRegistry(t, tasks.PollerConfig{})
	f.set(func(f *jobFake) { f.jobs = []interface{}{jobRow(9, "update.run", "RUNNING", 1_700_000_000_000)} })
	task, err := mgr.CreateJobTask("apply_update", nil, 9, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.set(func(f *jobFake) { f.drop = true; f.jobs = []interface{}{} })
	st := pollUntil(t, r, mgr, task.TaskID, 8)
	msg, _ := st["statusMessage"].(string)
	if st["status"] != "failed" || !strings.Contains(msg, "reboot") || !strings.Contains(msg, "system_info") || !strings.Contains(msg, "update_status") {
		t.Errorf("task = %v / %q, want failed with the expected-reboot message", st["status"], msg)
	}
}

// A job row that is gone ends the task as failed after DefaultMissingJobPolls
// (3) polls, instead of leaving it working until it expires.
func TestTaskJobRowMissing(t *testing.T) {
	r, mgr, f := newJobFakeRegistry(t, tasks.PollerConfig{})
	f.set(func(f *jobFake) { f.jobs = []interface{}{jobRow(5, "pool.scrub.scrub", "RUNNING", 1_700_000_000_000)} })
	task, err := mgr.CreateJobTask("run_scrub", nil, 5, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.set(func(f *jobFake) { f.jobs = []interface{}{} })
	for i := 0; i < 2; i++ {
		mgr.PollNow()
	}
	if st := taskState(t, r, task.TaskID); st["status"] != "working" {
		t.Fatalf("failed before the configured number of polls: %v", st)
	}
	mgr.PollNow()
	st := taskState(t, r, task.TaskID)
	msg, _ := st["statusMessage"].(string)
	if st["status"] != "failed" || !strings.Contains(msg, "no longer exists") || !strings.Contains(msg, "get_scrub_status") {
		t.Errorf("task = %v / %q, want failed: job no longer exists", st["status"], msg)
	}
}

// Repeated poll errors are reported in statusMessage (the task keeps
// working) and logged to stderr; a successful poll clears the message.
func TestTaskPollErrorsReported(t *testing.T) {
	var logs strings.Builder
	var logMu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { logMu.Lock(); defer logMu.Unlock(); return logs.Write(p) }))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	r, mgr, f := newJobFakeRegistry(t, tasks.PollerConfig{})
	f.set(func(f *jobFake) { f.jobs = []interface{}{jobRow(4, "app.stop", "RUNNING", 1_700_000_000_000)} })
	task, err := mgr.CreateJobTask("stop_app", nil, 4, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.set(func(f *jobFake) { f.rpcErr = map[string]interface{}{"code": -32000, "message": "middleware busy"} })
	for i := 0; i < 3; i++ {
		mgr.PollNow()
	}
	st := taskState(t, r, task.TaskID)
	msg, _ := st["statusMessage"].(string)
	if st["status"] != "working" || !strings.Contains(msg, "3 consecutive polls") || !strings.Contains(msg, "middleware busy") {
		t.Errorf("task = %v / %q, want working with the poll failures reported", st["status"], msg)
	}
	logMu.Lock()
	logged := logs.String()
	logMu.Unlock()
	if !strings.Contains(logged, task.TaskID) || !strings.Contains(logged, "middleware busy") {
		t.Errorf("poll failures not logged: %q", logged)
	}

	f.set(func(f *jobFake) { f.rpcErr = nil })
	mgr.PollNow()
	if st := taskState(t, r, task.TaskID); strings.Contains(st["statusMessage"].(string), "consecutive") {
		t.Errorf("poll failure message kept after a successful poll: %v", st["statusMessage"])
	}
}

type writerFunc func(p []byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

// tasks_get and tasks_list say which tool a task belongs to, its job id, and
// the job's result, bounded and flagged when cut.
func TestTasksExposeToolJobIDAndResult(t *testing.T) {
	r, mgr, f := newJobFakeRegistry(t, tasks.PollerConfig{})
	small := jobRow(11, "app.create", "SUCCESS", 1_700_000_000_000)
	small["result"] = map[string]interface{}{"name": "web", "ok": true}
	f.set(func(f *jobFake) { f.jobs = []interface{}{small} })
	task, err := mgr.CreateJobTask("install_app", nil, 11, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mgr.PollNow()
	st := taskState(t, r, task.TaskID)
	if st["tool"] != "install_app" || st["job_id"] != float64(11) {
		t.Errorf("tool/job_id = %v / %v", st["tool"], st["job_id"])
	}
	res, _ := st["result"].(map[string]interface{})
	if st["status"] != "completed" || res["name"] != "web" || st["result_truncated"] == true {
		t.Errorf("result = %v (truncated %v), status %v", st["result"], st["result_truncated"], st["status"])
	}

	big := jobRow(12, "update.download", "SUCCESS", 1_700_000_000_000)
	big["result"] = strings.Repeat("x", 3*tasks.MaxJobResultBytes)
	f.set(func(f *jobFake) { f.jobs = []interface{}{big} })
	task2, err := mgr.CreateJobTask("download_update", nil, 12, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mgr.PollNow()
	st = taskState(t, r, task2.TaskID)
	s, _ := st["result"].(string)
	if s == "" || len(s) > tasks.MaxJobResultBytes || st["result_truncated"] != true || st["result_size_bytes"] == nil {
		t.Errorf("large result not bounded and flagged: %d bytes, truncated %v, size %v", len(s), st["result_truncated"], st["result_size_bytes"])
	}

	out, err := r.CallTool(t.Context(), "tasks_list", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	list := decode(t, out)["tasks"].([]interface{})
	tools := map[interface{}]bool{}
	for _, e := range list {
		tools[e.(map[string]interface{})["tool"]] = true
	}
	if !tools["install_app"] || !tools["download_update"] {
		t.Errorf("tasks_list does not name the tools: %v", tools)
	}
	// Arguments stay private: they can hold credentials.
	if strings.Contains(out, "arguments") {
		t.Errorf("tasks_list exposes arguments: %s", out)
	}
}

// If the job could not be read at creation and the session changed before
// the first poll, the message says the job's identity could not be
// confirmed, not that the id now belongs to a different job.
func TestTaskIdentityUnconfirmedMessage(t *testing.T) {
	r, mgr, f := newJobFakeRegistry(t, tasks.PollerConfig{})
	f.set(func(f *jobFake) { f.rpcErr = map[string]interface{}{"code": -32000, "message": "busy"} })
	task, err := mgr.CreateJobTask("start_app", nil, 6, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.set(func(f *jobFake) { f.drop = true })
	mgr.PollNow() // error reply, then the connection drops
	f.set(func(f *jobFake) {
		f.rpcErr = nil
		f.jobs = []interface{}{jobRow(6, "app.start", "RUNNING", 1_700_000_000_000)}
	})
	st := pollUntil(t, r, mgr, task.TaskID, 5)
	msg, _ := st["statusMessage"].(string)
	if st["status"] != "failed" || !strings.Contains(msg, "could not be confirmed") || strings.Contains(msg, "different job") {
		t.Errorf("task = %v / %q, want failed: identity could not be confirmed", st["status"], msg)
	}
}

// A restart between the start call and the identity read would record some
// other job under the returned id. The job must run the method the tool
// starts (app.start for start_app), so that job is never adopted.
func TestTaskRequiresExpectedJobMethod(t *testing.T) {
	r, mgr, f := newJobFakeRegistry(t, tasks.PollerConfig{})
	f.set(func(f *jobFake) {
		f.jobs = []interface{}{jobRow(1, "pool.dataset.query", "SUCCESS", 1_700_000_000_000)}
	})
	task, err := mgr.CreateJobTask("start_app", nil, 1, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mgr.PollNow()
	st := taskState(t, r, task.TaskID)
	msg, _ := st["statusMessage"].(string)
	if st["status"] != "failed" || !strings.Contains(msg, "restarted") || !strings.Contains(msg, "query_apps") {
		t.Errorf("task = %v / %q, want failed: job 1 is not an app.start job", st["status"], msg)
	}
}

// A job state the poller does not know still clears a stale poll-error
// message: the job was read, so "could not read" is no longer true.
func TestTaskUnknownStateClearsPollErrorMessage(t *testing.T) {
	r, mgr, f := newJobFakeRegistry(t, tasks.PollerConfig{})
	f.set(func(f *jobFake) { f.jobs = []interface{}{jobRow(8, "app.stop", "RUNNING", 1_700_000_000_000)} })
	task, err := mgr.CreateJobTask("stop_app", nil, 8, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.set(func(f *jobFake) { f.rpcErr = map[string]interface{}{"code": -32000, "message": "middleware busy"} })
	for i := 0; i < 3; i++ {
		mgr.PollNow()
	}
	if msg, _ := taskState(t, r, task.TaskID)["statusMessage"].(string); !strings.Contains(msg, "consecutive polls") {
		t.Fatalf("poll errors not reported: %q", msg)
	}
	f.set(func(f *jobFake) {
		f.rpcErr = nil
		f.jobs = []interface{}{jobRow(8, "app.stop", "SOMETHING_NEW", 1_700_000_000_000)}
	})
	mgr.PollNow()
	st := taskState(t, r, task.TaskID)
	if msg, _ := st["statusMessage"].(string); strings.Contains(msg, "consecutive") || st["status"] != "working" {
		t.Errorf("task = %v / %q, want working with the poll-error message cleared", st["status"], msg)
	}
}

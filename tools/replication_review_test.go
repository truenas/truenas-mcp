package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/redact"
)

// shortSnapshotTaskWait makes run_snapshot_task poll quickly in tests.
func shortSnapshotTaskWait(t *testing.T, wait time.Duration) {
	t.Helper()
	old, oldPoll := snapshotTaskRunWait, snapshotTaskRunPoll
	snapshotTaskRunWait, snapshotTaskRunPoll = wait, 5*time.Millisecond
	t.Cleanup(func() { snapshotTaskRunWait, snapshotTaskRunPoll = old, oldPoll })
}

// pool.snapshottask.run is a plain method on 25.10 and 27: it queues the
// task with zettarepl and returns null, with no job. The tool must follow
// the task's own state, not a job id.
func TestRunSnapshotTaskNullResultFollowsTaskState(t *testing.T) {
	shortSnapshotTaskWait(t, 2*time.Second)
	const before = "auto-2026-01-01_00-00"
	const s = "auto-2026-01-05_00-00"
	task := snapshotTaskFixture(3, "tank/data", true, []interface{}{}, "tank/data@"+before)
	datasets := []string{"tank", "tank/data", "tank/data/a"}
	snaps := []map[string]interface{}{snap("tank/data", before), snap("tank/data/a", before)}
	rf := &runFake{}
	polls := 0
	ran := false
	// Queued: zettarepl has not picked the task up yet, so the state is
	// still the previous run's.
	rf.onRun = func(string) { ran = true }
	r, fake, _ := newRunRegistry(t, nil, []interface{}{task}, datasets, &snaps, rf)
	inner := fake.handle
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		// Called with the fake's lock held, so calls are serialised.
		if method == "pool.snapshottask.query" && ran {
			rf.mu.Lock()
			polls++
			switch polls {
			case 1:
				task["state"] = map[string]interface{}{"state": "RUNNING", "datetime": map[string]interface{}{"$date": 1767571200000.0}, "last_snapshot": "tank/data@" + before}
			case 2:
				snaps = append(snaps, snap("tank/data", s), snap("tank/data/a", s))
				task["state"] = map[string]interface{}{"state": "FINISHED", "datetime": map[string]interface{}{"$date": 1767571201000.0}, "last_snapshot": "tank/data@" + s}
			}
			rf.mu.Unlock()
		}
		return inner(method, params)
	}

	_, out := confirmAndRun(t, r, fake, "run_snapshot_task", "pool.snapshottask.run", map[string]interface{}{"task_id": 3.0})
	resp := decodeObj(t, out)
	if resp["state"] != "FINISHED" || resp["snapshot_name"] != s || resp["snapshots_created_count"] != 2.0 {
		t.Fatalf("response = %v", resp)
	}
	if _, ok := resp["job_id"]; ok {
		t.Errorf("response has a job_id, but pool.snapshottask.run creates no job: %v", resp)
	}
	if strings.Contains(out, "tasks_get") {
		t.Errorf("response points at tasks_get, but there is no job to follow: %s", out)
	}
	if len(fake.called("core.get_jobs")) != 0 {
		t.Error("run_snapshot_task polled core.get_jobs for a job that does not exist")
	}
}

func TestRunSnapshotTaskErrorState(t *testing.T) {
	shortSnapshotTaskWait(t, 2*time.Second)
	task := snapshotTaskFixture(3, "tank/data", false, []interface{}{}, "tank/data@auto-2026-01-01_00-00")
	snaps := []map[string]interface{}{}
	rf := &runFake{}
	rf.onRun = func(string) {
		task["state"] = map[string]interface{}{"state": "ERROR", "datetime": map[string]interface{}{"$date": 1767571200000.0},
			"error": "Failed to create snapshot: out of space. password=SentinelPw4242"}
	}
	r, fake, _ := newRunRegistry(t, nil, []interface{}{task}, []string{"tank", "tank/data"}, &snaps, rf)
	first, err := r.CallTool(t.Context(), "run_snapshot_task", map[string]interface{}{"task_id": 3.0})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CallTool(t.Context(), "run_snapshot_task", map[string]interface{}{"task_id": 3.0, "confirmation_token": replyToken(t, first)})
	if err == nil || !strings.Contains(err.Error(), "periodic snapshot task 3 failed") || !strings.Contains(err.Error(), "out of space") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SentinelPw4242") {
		t.Errorf("error text not redacted: %v", err)
	}
	if len(fake.called("pool.snapshottask.run")) != 1 {
		t.Error("want exactly one run call")
	}
}

// zettarepl treats an already-existing snapshot name as success only
// during a DST step-back; last_snapshot is then unchanged. Otherwise it
// reports ERROR (covered by TestRunSnapshotTaskErrorState). This is the
// DST success path.
func TestRunSnapshotTaskSnapshotAlreadyExisted(t *testing.T) {
	shortSnapshotTaskWait(t, 2*time.Second)
	const last = "auto-2026-01-05_00-00"
	task := snapshotTaskFixture(3, "tank/data", false, []interface{}{}, "tank/data@"+last)
	snaps := []map[string]interface{}{snap("tank/data", last)}
	rf := &runFake{}
	rf.onRun = func(string) {
		task["state"] = map[string]interface{}{"state": "FINISHED", "datetime": map[string]interface{}{"$date": 1767571200000.0}, "last_snapshot": "tank/data@" + last}
	}
	r, fake, _ := newRunRegistry(t, nil, []interface{}{task}, []string{"tank", "tank/data"}, &snaps, rf)
	_, out := confirmAndRun(t, r, fake, "run_snapshot_task", "pool.snapshottask.run", map[string]interface{}{"task_id": 3.0})
	resp := decodeObj(t, out)
	msg, _ := resp["message"].(string)
	if resp["state"] != "FINISHED" || resp["new_snapshot"] != false || !strings.Contains(msg, "already existed") || !strings.Contains(msg, "DST step-back") {
		t.Errorf("response = %v", resp)
	}
}

func TestRunSnapshotTaskQueuedNotFinished(t *testing.T) {
	shortSnapshotTaskWait(t, 30*time.Millisecond)
	task := snapshotTaskFixture(3, "tank/data", false, []interface{}{}, nil)
	snaps := []map[string]interface{}{}
	r, fake, _ := newRunRegistry(t, nil, []interface{}{task}, []string{"tank", "tank/data"}, &snaps, &runFake{})
	_, out := confirmAndRun(t, r, fake, "run_snapshot_task", "pool.snapshottask.run", map[string]interface{}{"task_id": 3.0})
	resp := decodeObj(t, out)
	msg, _ := resp["message"].(string)
	if resp["state"] != "QUEUED" || !strings.Contains(msg, "query_snapshot_tasks task_id 3") || strings.Contains(msg, "tasks_get") {
		t.Errorf("response = %v", resp)
	}
}

func TestRunSnapshotTaskContextCancelled(t *testing.T) {
	shortSnapshotTaskWait(t, 10*time.Second)
	task := snapshotTaskFixture(3, "tank/data", false, []interface{}{}, nil)
	snaps := []map[string]interface{}{}
	rf := &runFake{}
	r, fake, _ := newRunRegistry(t, nil, []interface{}{task}, []string{"tank", "tank/data"}, &snaps, rf)
	first, err := r.CallTool(t.Context(), "run_snapshot_task", map[string]interface{}{"task_id": 3.0})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	rf.onRun = func(string) { time.AfterFunc(50*time.Millisecond, cancel) }
	out, err := r.CallTool(ctx, "run_snapshot_task", map[string]interface{}{"task_id": 3.0, "confirmation_token": replyToken(t, first)})
	if err != nil {
		t.Fatalf("cancelled wait after a queued run must not be reported as a failure: %v", err)
	}
	resp := decodeObj(t, out)
	if resp["state"] != "QUEUED" || !strings.Contains(resp["message"].(string), "query_snapshot_tasks") {
		t.Errorf("response = %v", resp)
	}
	if len(fake.called("pool.snapshottask.run")) != 1 {
		t.Error("want exactly one run call")
	}
}

// replicationOutcome: only an empty or missing progress text means nothing
// was sent; any other text it cannot parse is "could not determine".
func TestReplicationOutcomeUnrecognisedText(t *testing.T) {
	outcome := replicationOutcome([]string{"tank/a"})
	for _, desc := range []interface{}{"Transferring tank/a@s1 to target password=SentinelPw4242", 42.0} {
		msg, res := outcome(map[string]interface{}{"progress": map[string]interface{}{"description": desc}})
		m := res.(map[string]interface{})
		if v, ok := m["transferred"]; !ok || v != nil {
			t.Errorf("%v: transferred = %v, want null", desc, m["transferred"])
		}
		if strings.Contains(msg, "nothing transferred") || !strings.Contains(msg, "could not determine what was transferred") {
			t.Errorf("%v: message = %q", desc, msg)
		}
		raw, _ := json.Marshal(res)
		if strings.Contains(msg+string(raw), "SentinelPw4242") {
			t.Errorf("progress text not redacted: %s %s", msg, raw)
		}
	}
	msg, res := outcome(map[string]interface{}{"progress": map[string]interface{}{"description": "Transferring tank/a@s1"}})
	if d, _ := res.(map[string]interface{})["progress_description"].(string); d != "Transferring tank/a@s1" || !strings.Contains(msg, "Transferring tank/a@s1") {
		t.Errorf("raw text missing: %q %v", msg, res)
	}
	for _, job := range []map[string]interface{}{
		{"progress": map[string]interface{}{"description": ""}},
		{"progress": map[string]interface{}{"description": nil}},
		{"progress": map[string]interface{}{}},
		{"progress": nil},
		{},
	} {
		msg, res := outcome(job)
		if !strings.Contains(msg, "nothing transferred") || res.(map[string]interface{})["transferred"] != false {
			t.Errorf("%v: %q %v", job, msg, res)
		}
	}
}

// zettarepl keeps one send counter per replication task, across every
// source dataset and child (GlobalReplicationContext): the final "Sending
// N of M" text carries the task-wide N. On a retry the planned total M
// also counts the attempt that failed, so N, not M, is what was sent.
func TestReplicationOutcomeTaskWideCount(t *testing.T) {
	outcome := replicationOutcome([]string{"tank/a", "tank/b"})
	for _, tc := range []struct {
		desc          string
		sent, planned int
		last          string
	}{
		{"Sending 7 of 7: tank/b/child@auto-2026-01-04_00-00 (1 MiB / 1 MiB) [total 3 GiB of 3 GiB]", 7, 7, "tank/b/child@auto-2026-01-04_00-00"},
		{"Sending 5 of 8: tank/b@s9", 5, 8, "tank/b@s9"},
	} {
		msg, res := outcome(map[string]interface{}{"progress": map[string]interface{}{"description": tc.desc}})
		m := res.(map[string]interface{})
		if m["transferred"] != true || m["snapshots_sent"] != tc.sent || m["snapshots_planned"] != tc.planned || m["last_snapshot_sent"] != tc.last {
			t.Errorf("%q: result = %v", tc.desc, m)
		}
		if !strings.Contains(msg, "all source datasets") {
			t.Errorf("%q: message = %q", tc.desc, msg)
		}
	}
	// "[total {dst} of {max}]": max is max(source used, target used), not
	// the source used size alone. When the target is smaller, they differ.
	_, res := outcome(map[string]interface{}{"progress": map[string]interface{}{"description": "Sending 1 of 1: tank/a@s1 [total 2.5 GiB of 4.0 GiB]"}})
	size, _ := res.(map[string]interface{})["last_source_size"].(map[string]interface{})
	if size["target_used"] != "2.5 GiB" || size["max_used"] != "4.0 GiB" || size["source_used"] != nil {
		t.Errorf("last_source_size = %v, want target_used 2.5 GiB and max_used 4.0 GiB", size)
	}
}

func setNASVersion(t *testing.T, r *Registry, fake *fakeMiddleware, version string) {
	t.Helper()
	inner := fake.handle
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "system.info" {
			return map[string]interface{}{"hostname": "nas", "version": version, "uptime_seconds": 100, "timezone": "UTC"}, nil, true
		}
		return inner(method, params)
	}
	if err := r.client.RequireSupportedVersion(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// 25.10 refuses to run a disabled replication task; 26 and later run it.
func TestRunReplicationDisabledByVersion(t *testing.T) {
	disabled := func() map[string]interface{} {
		task := pushReplicationFixture()
		task["enabled"] = false
		return task
	}
	snaps := []map[string]interface{}{}
	r, fake, _ := newRunRegistry(t, []interface{}{disabled()}, nil, nil, &snaps, &runFake{})
	setNASVersion(t, r, fake, "25.10.1")
	if _, err := r.CallTool(t.Context(), "run_replication", map[string]interface{}{"task_id": 1.0, "dry_run": true}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("25.10: err = %v, want refused as disabled", err)
	}
	if len(fake.called("replication.run")) != 0 {
		t.Error("replication.run sent for a disabled task on 25.10")
	}

	r, fake, _ = newRunRegistry(t, []interface{}{disabled()}, nil, nil, &snaps, &runFake{})
	setNASVersion(t, r, fake, "27.0.0")
	resp := callTool(t, r, "run_replication", map[string]interface{}{"task_id": 1.0, "dry_run": true})
	if w := strings.Join(toStrings(resp["warnings"]), "|"); !strings.Contains(w, "disabled") || !strings.Contains(w, "still runs it") {
		t.Errorf("27: warnings = %v", w)
	}
}

// last_run.datetime is reported in the NAS time zone, like the scrub tools.
func TestTaskLastRunInNASTimezone(t *testing.T) {
	repl := pushReplicationFixture() // state.datetime 2026-01-01T00:00:00Z
	task := snapshotTaskFixture(3, "tank/data", false, []interface{}{}, "tank/data@auto-2026-01-01_00-00")
	client, fake := taskQueryFake(t, []interface{}{repl}, []interface{}{task}, []string{"tank", "tank/data"}, nil)
	inner := fake.handle
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "system.info" {
			return map[string]interface{}{"hostname": "nas", "version": "25.10.0", "timezone": "America/Chicago"}, nil, true
		}
		return inner(method, params)
	}
	r := NewRegistry(client, nil)
	const want = "2025-12-31T18:00:00-06:00"
	resp := callTool(t, r, "query_replications", map[string]interface{}{})
	got := resp["replications"].([]interface{})[0].(map[string]interface{})["last_run"].(map[string]interface{})["datetime"]
	if got != want {
		t.Errorf("query_replications last_run.datetime = %v, want %s", got, want)
	}
	resp = callTool(t, r, "query_snapshot_tasks", map[string]interface{}{})
	got = resp["snapshot_tasks"].([]interface{})[0].(map[string]interface{})["last_run"].(map[string]interface{})["datetime"]
	if got != want {
		t.Errorf("query_snapshot_tasks last_run.datetime = %v, want %s", got, want)
	}

	// Without a usable NAS time zone the times are UTC and a note says so.
	client, _ = taskQueryFake(t, []interface{}{repl}, nil, nil, nil)
	resp = callTool(t, NewRegistry(client, nil), "query_replications", map[string]interface{}{})
	got = resp["replications"].([]interface{})[0].(map[string]interface{})["last_run"].(map[string]interface{})["datetime"]
	if got != "2026-01-01T00:00:00Z" || resp["timezone_note"] == nil {
		t.Errorf("UTC fallback: datetime = %v, note = %v", got, resp["timezone_note"])
	}
}

// The projection fails closed: secrets in free-text state fields are
// redacted, and fields it does not know, at the top level or inside an
// embedded periodic snapshot task, never reach the output.
func TestQueryReplicationsLeakFailsClosed(t *testing.T) {
	const unknownSecret = "sentinel-unknown-field-value"
	const pw = "SentinelPw4242"
	repl := pushReplicationFixture()
	repl["state"] = map[string]interface{}{
		"state":    "ERROR",
		"datetime": map[string]interface{}{"$date": 1767225600000.0},
		"error":    "ssh: authentication failed password=" + pw + " Authorization: Bearer sentinelbearertoken0123456789",
		"warnings": []interface{}{"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAsentinelwarningkey\n-----END OPENSSH PRIVATE KEY-----"},
	}
	repl["future_credential_field"] = unknownSecret
	pst := repl["periodic_snapshot_tasks"].([]interface{})[0].(map[string]interface{})
	pst["future_nested_field"] = unknownSecret
	task := snapshotTaskFixture(3, "tank/data", false, []interface{}{}, nil)
	task["state"] = map[string]interface{}{"state": "ERROR", "error": "vmware login failed password=" + pw}
	task["future_credential_field"] = unknownSecret

	client, _ := taskQueryFake(t, []interface{}{repl}, []interface{}{task}, []string{"tank", "tank/data"}, nil)
	r := NewRegistry(client, nil)
	for _, name := range []string{"query_replications", "query_snapshot_tasks"} {
		out, err := r.CallTool(t.Context(), name, map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{out, redact.ToolOutput(out)} {
			for _, s := range append([]string{unknownSecret, pw, "sentinelbearertoken0123456789", "AAAAsentinelwarningkey", "future_credential_field", "future_nested_field"}, replicationSentinels...) {
				if strings.Contains(body, s) {
					t.Errorf("%s output contains %q:\n%s", name, s, body)
				}
			}
		}
	}
}

// create_snapshot warns when retention or a replication may act on the
// new snapshot, or when the dataset is a replication target.
func TestCreateSnapshotTaskInteractionWarnings(t *testing.T) {
	datasets := []string{"tank", "tank/data", "tank/data/a", "backup", "backup/copy", "tank/plain"}
	pst := snapshotTaskFixture(3, "tank/data", true, []interface{}{}, nil)
	push := pushReplicationFixture() // PUSH tank/data -> backup/data over SSH, bound to task 3
	local := pullReplicationFixture()
	local["id"], local["direction"], local["transport"] = 5.0, "PUSH", "LOCAL"
	local["source_datasets"], local["target_dataset"], local["recursive"] = []interface{}{"tank/plain"}, "backup/copy", true
	snaps := []map[string]interface{}{}
	r, fake, _ := newRunRegistry(t, []interface{}{push, local}, []interface{}{pst}, datasets, &snaps, &runFake{})

	warnings := func(args map[string]interface{}) string {
		t.Helper()
		resp := callTool(t, r, "create_snapshot", mergeArgs(args, map[string]interface{}{"dry_run": true}))
		return strings.Join(toStrings(resp["warnings"]), "|")
	}
	// Child of a recursive task's dataset, name in the task's schema.
	w := warnings(map[string]interface{}{"dataset": "tank/data/a", "name": "auto-2026-01-05_00-00"})
	for _, want := range []string{"periodic snapshot task(s) 3", "removed by that task's retention", "push replication task(s) 1"} {
		if !strings.Contains(w, want) {
			t.Errorf("schema match: warnings missing %q: %s", want, w)
		}
	}
	if w := warnings(map[string]interface{}{"dataset": "tank/data/a", "name": "before-upgrade"}); strings.Contains(w, "periodic snapshot task(s) 3") || !strings.Contains(w, "No retention applies") {
		t.Errorf("non-matching name warned about the task: %s", w)
	}
	// A child of a local replication's target.
	w = warnings(map[string]interface{}{"dataset": "backup/copy", "name": "manual-1"})
	if !strings.Contains(w, "inside the target) of replication task(s) 5") || !strings.Contains(w, "incremental") {
		t.Errorf("target: warnings = %s", w)
	}
	// The SSH push target is on the remote system, so backup/data here is
	// not its target.
	if w := warnings(map[string]interface{}{"dataset": "tank/plain", "name": "manual-1"}); strings.Contains(w, "of replication task(s)") || strings.Contains(w, "periodic snapshot task(s)") {
		t.Errorf("unrelated dataset warned: %s", w)
	}
	if len(fake.called("pool.snapshot.create")) != 0 {
		t.Error("dry run created a snapshot")
	}
}

// A failed or unreadable replication.query still warns that replication
// tasks could not be checked. An unreadable result must not drop the warning.
func TestCreateSnapshotReplicationQueryCheckFails(t *testing.T) {
	datasets := []string{"tank", "tank/data"}
	for _, tc := range []struct {
		name string
		ret  interface{}
		err  map[string]interface{}
	}{
		{"failed", nil, map[string]interface{}{"message": "Not authorized", "code": -32001}},
		{"unreadable", map[string]interface{}{"not": "a list"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snaps := []map[string]interface{}{}
			r, fake, _ := newRunRegistry(t, nil, nil, datasets, &snaps, &runFake{})
			inner := fake.handle
			fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
				if method == "replication.query" {
					return tc.ret, tc.err, true
				}
				return inner(method, params)
			}
			resp := callTool(t, r, "create_snapshot", map[string]interface{}{"dataset": "tank/data", "name": "manual-1", "dry_run": true})
			w := strings.Join(toStrings(resp["warnings"]), "|")
			if !strings.Contains(w, "Could not check replication") {
				t.Errorf("warnings = %s, want a could-not-check-replication warning", w)
			}
			if len(fake.called("pool.snapshot.create")) != 0 {
				t.Error("dry run created a snapshot")
			}
		})
	}
}

// When every covered dataset seems to miss the last-run snapshot, the
// result may be an empty lookup (on 27 a dataset removed mid-query empties
// it), and the note says to query again.
func TestQuerySnapshotTasksAllMissingNote(t *testing.T) {
	const s = "auto-2026-01-01_00-00"
	task := snapshotTaskFixture(3, "tank/data", true, []interface{}{}, "tank/data@"+s)
	client, _ := taskQueryFake(t, nil, []interface{}{task}, []string{"tank", "tank/data", "tank/data/a"}, nil)
	resp := callTool(t, NewRegistry(client, nil), "query_snapshot_tasks", map[string]interface{}{})
	cov := resp["snapshot_tasks"].([]interface{})[0].(map[string]interface{})["coverage"].(map[string]interface{})
	if note, _ := cov["note"].(string); !strings.Contains(note, "query again") {
		t.Errorf("note = %q", note)
	}
}

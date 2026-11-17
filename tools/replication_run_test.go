package tools

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// runFake extends taskQueryFake with the run and create methods and a
// core.get_jobs answer for the job they start.
type runFake struct {
	mu       sync.Mutex
	job      map[string]interface{} // the started job's row; nil until started
	jobState func(started map[string]interface{}) map[string]interface{}
	onRun    func(method string)
	failRun  map[string]interface{} // RPC error for the run/create call
}

func newRunRegistry(t *testing.T, replications, snapTasks []interface{}, datasets []string, snapshots *[]map[string]interface{}, rf *runFake) (*Registry, *fakeMiddleware, *tasks.Manager) {
	t.Helper()
	client, fake := taskQueryFake(t, replications, snapTasks, datasets, nil)
	inner := fake.handle
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		rf.mu.Lock()
		defer rf.mu.Unlock()
		switch method {
		case "replication.run", "pool.snapshottask.run":
			if rf.failRun != nil {
				return nil, rf.failRun, true
			}
			rf.job = map[string]interface{}{
				"id": 77.0, "method": method, "state": "RUNNING",
				"time_started": map[string]interface{}{"$date": 1767225600000.0},
				"progress":     map[string]interface{}{"percent": 0.0, "description": "", "extra": nil},
			}
			if rf.onRun != nil {
				rf.onRun(method)
			}
			return 77, nil, true
		case "pool.snapshot.create":
			if rf.failRun != nil {
				return nil, rf.failRun, true
			}
			var p []map[string]interface{}
			_ = json.Unmarshal(params, &p)
			ds, _ := p[0]["dataset"].(string)
			name, _ := p[0]["name"].(string)
			rec, _ := p[0]["recursive"].(bool)
			for _, d := range datasets {
				if d == ds || rec && strings.HasPrefix(d, ds+"/") {
					*snapshots = append(*snapshots, snap(d, name))
				}
			}
			entry := snap(ds, name)
			entry["createtxg"] = "1234"
			return entry, nil, true
		case "core.get_jobs":
			if rf.job == nil {
				return []interface{}{}, nil, true
			}
			row := rf.job
			if rf.jobState != nil {
				row = rf.jobState(rf.job)
			}
			return []interface{}{row}, nil, true
		case "pool.snapshot.query":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			filters, _ := p[0].([]interface{})
			out := []interface{}{}
			for _, s := range *snapshots {
				if jobMatchesFilters(s, filters) {
					out = append(out, s)
				}
			}
			return out, nil, true
		}
		return inner(method, params)
	}
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: 5 * time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake, mgr
}

func withJobState(state, description string) func(map[string]interface{}) map[string]interface{} {
	return func(started map[string]interface{}) map[string]interface{} {
		row := map[string]interface{}{}
		for k, v := range started {
			row[k] = v
		}
		row["state"] = state
		row["progress"] = map[string]interface{}{"percent": 100.0, "description": description, "extra": nil}
		if state == "FAILED" {
			row["error"] = description
		}
		return row
	}
}

// confirmAndRun previews name with args (which must issue a token and not
// call method), then calls it again with the token.
func confirmAndRun(t *testing.T, r *Registry, fake *fakeMiddleware, name, method string, args map[string]interface{}) (preview map[string]interface{}, out string) {
	t.Helper()
	first, err := r.CallTool(t.Context(), name, args)
	if err != nil {
		t.Fatalf("%s preview: %v", name, err)
	}
	if n := len(fake.called(method)); n != 0 {
		t.Fatalf("%s called %s %d time(s) without confirmation", name, method, n)
	}
	tok := replyToken(t, first)
	if tok == "" {
		t.Fatalf("%s without a token: want confirmation_required with a token, got:\n%s", name, first)
	}
	if err := json.Unmarshal([]byte(first), &preview); err != nil {
		t.Fatal(err)
	}
	out, err = r.CallTool(t.Context(), name, mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err != nil {
		t.Fatalf("%s with token: %v", name, err)
	}
	return preview, out
}

func decodeObj(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

func getTask(t *testing.T, r *Registry, mgr *tasks.Manager, taskID string) map[string]interface{} {
	t.Helper()
	mgr.PollNow()
	return callTool(t, r, "tasks_get", map[string]interface{}{"task_id": taskID})
}

func TestRunToolsReadOnlyHiddenAndRefused(t *testing.T) {
	rf := &runFake{}
	snaps := []map[string]interface{}{}
	r, fake, _ := newRunRegistry(t, []interface{}{pushReplicationFixture()}, []interface{}{snapshotTaskFixture(3, "tank/data", false, []interface{}{}, nil)}, []string{"tank", "tank/data"}, &snaps, rf)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	for _, tc := range []struct {
		name string
		args map[string]interface{}
	}{
		{"run_replication", map[string]interface{}{"task_id": 1.0}},
		{"run_snapshot_task", map[string]interface{}{"task_id": 3.0}},
		{"create_snapshot", map[string]interface{}{"dataset": "tank/data", "name": "s1"}},
	} {
		if names[tc.name] {
			t.Errorf("%s listed under --read-only", tc.name)
		}
		for _, extra := range []map[string]interface{}{{}, {"dry_run": true}, {"confirmation_token": "x"}} {
			if _, err := r.CallTool(t.Context(), tc.name, mergeArgs(tc.args, extra)); err == nil || !strings.Contains(err.Error(), "read-only mode") {
				t.Errorf("%s %v under --read-only: err = %v", tc.name, extra, err)
			}
		}
	}
	for _, m := range []string{"replication.run", "pool.snapshottask.run", "pool.snapshot.create"} {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("%s sent under --read-only", m)
		}
	}
	for _, name := range []string{"query_replications", "query_snapshot_tasks"} {
		if !names[name] {
			t.Errorf("%s hidden under --read-only", name)
		}
	}
}

func TestRunReplicationNothingTransferred(t *testing.T) {
	rf := &runFake{jobState: withJobState("SUCCESS", "")}
	const last = "auto-2026-01-01_00-00"
	snaps := []map[string]interface{}{
		withTxg(snap("tank/data", "auto-2025-12-31_00-00"), "90"),
		withTxg(snap("tank/data", last), "100"),
		withTxg(snap("tank/data", "manual-before-upgrade"), "120"),
	}
	r, fake, mgr := newRunRegistry(t, []interface{}{pushReplicationFixture()}, nil, nil, &snaps, rf)

	preview, out := confirmAndRun(t, r, fake, "run_replication", "replication.run", map[string]interface{}{"task_id": 1.0})
	body, _ := json.Marshal(preview)
	for _, want := range []string{"ships snapshots that already exist and creates none", "no new snapshots on tank/data", "Retention SOURCE", `"would_send":false`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("preview missing %q:\n%s", want, body)
		}
	}
	for _, s := range replicationSentinels {
		if strings.Contains(string(body), s) {
			t.Errorf("preview leaks %q", s)
		}
	}

	calls := fake.called("replication.run")
	if len(calls) != 1 || string(calls[0].Params) != "[1]" {
		t.Fatalf("replication.run calls = %v, want one call with [1]", calls)
	}
	resp := decodeObj(t, out)
	if resp["job_id"] != 77.0 || resp["started"] != true || resp["replication_task_id"] != 1.0 {
		t.Errorf("response = %v", resp)
	}
	if b := resp["bound_snapshot_tasks"].([]interface{}); len(b) != 1 || b[0] != "3" {
		t.Errorf("bound_snapshot_tasks = %v", b)
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "tasks_get") || !strings.Contains(msg, "creates none") {
		t.Errorf("message = %q", msg)
	}
	task := getTask(t, r, mgr, resp["task_id"].(string))
	if task["status"] != "completed" {
		t.Fatalf("task = %v", task)
	}
	if msg := task["statusMessage"]; msg != "Replication finished: no new snapshots on tank/data; nothing transferred" {
		t.Errorf("statusMessage = %v", msg)
	}
	if res := task["result"].(map[string]interface{}); res["transferred"] != false || res["snapshots_sent"] != 0.0 {
		t.Errorf("result = %v", res)
	}
}

func withTxg(s map[string]interface{}, txg string) map[string]interface{} {
	s["createtxg"] = txg
	return s
}

func TestRunReplicationSentSnapshots(t *testing.T) {
	rf := &runFake{jobState: withJobState("SUCCESS", "Sending 3 of 3: tank/data@auto-2026-01-04_00-00 (1.2 GiB / 1.2 GiB) [total 2.5 GiB of 2.5 GiB]")}
	snaps := []map[string]interface{}{
		withTxg(snap("tank/data", "auto-2026-01-01_00-00"), "100"),
		withTxg(snap("tank/data", "auto-2026-01-02_00-00"), "110"),
		withTxg(snap("tank/data", "auto-2026-01-03_00-00"), "120"),
		withTxg(snap("tank/data", "auto-2026-01-04_00-00"), "130"),
	}
	r, fake, mgr := newRunRegistry(t, []interface{}{pushReplicationFixture()}, nil, nil, &snaps, rf)
	preview, out := confirmAndRun(t, r, fake, "run_replication", "replication.run", map[string]interface{}{"task_id": 1.0})
	est := preview["preview"].(map[string]interface{})["current_state"].(map[string]interface{})["estimate"].(map[string]interface{})
	if est["would_send"] != true || est["new_snapshots"] != 3.0 || est["newest"] != "tank/data@auto-2026-01-04_00-00" {
		t.Errorf("estimate = %v", est)
	}
	task := getTask(t, r, mgr, decodeObj(t, out)["task_id"].(string))
	if msg, _ := task["statusMessage"].(string); msg != "Replication finished: sent 3 snapshot(s), the last tank/data@auto-2026-01-04_00-00 (2.5 GiB)" {
		t.Errorf("statusMessage = %q", msg)
	}
	res := task["result"].(map[string]interface{})
	if res["transferred"] != true || res["snapshots_sent"] != 3.0 || res["data_sent"] != "2.5 GiB" {
		t.Errorf("result = %v", res)
	}
}

func TestRunReplicationFailedJob(t *testing.T) {
	rf := &runFake{jobState: withJobState("FAILED", "No incremental base on dataset 'tank/data' and replication from scratch is not allowed")}
	snaps := []map[string]interface{}{}
	r, fake, mgr := newRunRegistry(t, []interface{}{pushReplicationFixture()}, nil, nil, &snaps, rf)
	_, out := confirmAndRun(t, r, fake, "run_replication", "replication.run", map[string]interface{}{"task_id": 1.0})
	task := getTask(t, r, mgr, decodeObj(t, out)["task_id"].(string))
	if task["status"] != "failed" || !strings.Contains(task["statusMessage"].(string), "No incremental base") {
		t.Errorf("task = %v", task)
	}
}

func TestRunReplicationBlocked(t *testing.T) {
	for _, tc := range []struct {
		state map[string]interface{}
		want  string
	}{
		{map[string]interface{}{"state": "RUNNING"}, "replication task 1 is already running"},
		{map[string]interface{}{"state": "HOLD", "reason": "Target dataset does not exist"}, "replication task 1 is on hold: Target dataset does not exist"},
	} {
		task := pushReplicationFixture()
		task["state"] = tc.state
		rf := &runFake{}
		snaps := []map[string]interface{}{}
		r, fake, _ := newRunRegistry(t, []interface{}{task}, nil, nil, &snaps, rf)
		for _, args := range []map[string]interface{}{{"task_id": 1.0}, {"task_id": 1.0, "dry_run": true}} {
			out, err := r.CallTool(t.Context(), "run_replication", args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%v: err = %v, want %q", args, err, tc.want)
			}
			if replyToken(t, out) != "" {
				t.Errorf("blocked run issued a token: %s", out)
			}
		}
		if len(fake.called("replication.run")) != 0 {
			t.Error("replication.run sent for a blocked task")
		}
	}
	rf := &runFake{}
	snaps := []map[string]interface{}{}
	r, _, _ := newRunRegistry(t, []interface{}{pushReplicationFixture()}, nil, nil, &snaps, rf)
	if _, err := r.CallTool(t.Context(), "run_replication", map[string]interface{}{"task_id": 9.0}); err == nil || !strings.Contains(err.Error(), "replication task 9 not found") {
		t.Errorf("unknown task: err = %v", err)
	}
}

func TestRunReplicationPullEstimateUnknown(t *testing.T) {
	rf := &runFake{}
	snaps := []map[string]interface{}{}
	r, fake, _ := newRunRegistry(t, []interface{}{pullReplicationFixture()}, nil, nil, &snaps, rf)
	resp := callTool(t, r, "run_replication", map[string]interface{}{"task_id": 2.0, "dry_run": true})
	est := resp["current_state"].(map[string]interface{})["estimate"].(map[string]interface{})
	if est["would_send"] != nil || !strings.Contains(est["reason"].(string), "remote system") {
		t.Errorf("estimate = %v", est)
	}
	if !strings.Contains(strings.Join(toStrings(resp["warnings"]), "|"), "disabled") {
		t.Errorf("warnings = %v, want the disabled note", resp["warnings"])
	}
	if len(fake.called("replication.run")) != 0 {
		t.Error("dry run sent replication.run")
	}
}

func toStrings(v interface{}) []string {
	list, _ := v.([]interface{})
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestReplicationOutcome(t *testing.T) {
	outcome := replicationOutcome([]string{"tank/a", "tank/b"})
	for _, tc := range []struct {
		desc, msg string
		sent      int
	}{
		{"", "Replication finished: no new snapshots on tank/a, tank/b; nothing transferred", 0},
		{"Sending 1 of 1: tank/a@s1", "Replication finished: sent 1 snapshot(s), the last tank/a@s1", 1},
		{"Sending 5 of 5: tank/b@s9 (10 MiB / 10 MiB)", "Replication finished: sent 5 snapshot(s), the last tank/b@s9", 5},
	} {
		msg, res := outcome(map[string]interface{}{"progress": map[string]interface{}{"description": tc.desc}})
		if msg != tc.msg || res.(map[string]interface{})["snapshots_sent"] != tc.sent {
			t.Errorf("%q: %q %v", tc.desc, msg, res)
		}
	}
	msg, _ := outcome(map[string]interface{}{"progress": nil})
	if !strings.Contains(msg, "nothing transferred") {
		t.Errorf("no progress: %q", msg)
	}
}

func TestNamingSchemaRegexp(t *testing.T) {
	re := namingSchemaRegexp("auto-%Y-%m-%d_%H-%M")
	for name, want := range map[string]bool{
		"auto-2026-01-01_00-00":   true,
		"auto-2026-01-01_00-00x":  false,
		"manual-2026-01-01_00-00": false,
		"auto-26-01-01_00-00":     false,
	} {
		if re.MatchString(name) != want {
			t.Errorf("%s: match = %v, want %v", name, !want, want)
		}
	}
	if !namingSchemaRegexp("x.%s").MatchString("x.1767225600") || namingSchemaRegexp("x.%s").MatchString("xy1767225600") {
		t.Error("literal dot must be escaped and the epoch token must match digits")
	}
}

func TestRunSnapshotTaskReturnsCreatedSnapshots(t *testing.T) {
	old, oldPoll := snapshotTaskRunWait, snapshotTaskRunPoll
	snapshotTaskRunWait, snapshotTaskRunPoll = time.Second, 5*time.Millisecond
	t.Cleanup(func() { snapshotTaskRunWait, snapshotTaskRunPoll = old, oldPoll })

	const s = "auto-2026-01-05_00-00"
	task := snapshotTaskFixture(3, "tank/data", true, []interface{}{"tank/data/scratch"}, "tank/data@auto-2026-01-01_00-00")
	task["allow_empty"] = false
	datasets := []string{"tank", "tank/data", "tank/data/a", "tank/data/b", "tank/data/scratch"}
	snaps := []map[string]interface{}{}
	rf := &runFake{jobState: withJobState("SUCCESS", "")}
	// The run creates the snapshot on data and data/a (data/b is unchanged
	// and allow_empty is off) and records it as the task's last snapshot.
	rf.onRun = func(string) {
		snaps = append(snaps, snap("tank/data", s), snap("tank/data/a", s))
		task["state"] = map[string]interface{}{"state": "FINISHED", "last_snapshot": "tank/data@" + s}
	}
	r, fake, _ := newRunRegistry(t, []interface{}{pushReplicationFixture()}, []interface{}{task}, datasets, &snaps, rf)

	preview, out := confirmAndRun(t, r, fake, "run_snapshot_task", "pool.snapshottask.run", map[string]interface{}{"task_id": 3.0})
	body, _ := json.Marshal(preview)
	for _, want := range []string{"Replication task(s) 1 start when this task finishes", "allow_empty is off", `"datasets_covered":3`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("preview missing %q:\n%s", want, body)
		}
	}
	if calls := fake.called("pool.snapshottask.run"); len(calls) != 1 || string(calls[0].Params) != "[3]" {
		t.Fatalf("pool.snapshottask.run calls = %v", calls)
	}
	resp := decodeObj(t, out)
	if resp["state"] != "SUCCESS" || resp["snapshot_name"] != s || resp["job_id"] != 77.0 || resp["task_id"] == nil {
		t.Errorf("response = %v", resp)
	}
	created := toStrings(resp["snapshots_created"])
	if strings.Join(created, ",") != "tank/data@"+s+",tank/data/a@"+s || resp["snapshots_created_count"] != 2.0 {
		t.Errorf("snapshots_created = %v", created)
	}
	if w := toStrings(resp["datasets_without_snapshot"]); len(w) != 1 || w[0] != "tank/data/b" {
		t.Errorf("datasets_without_snapshot = %v", w)
	}
	msg, _ := resp["message"].(string)
	for _, want := range []string{"on 2 of 3 covered dataset(s)", "allow_empty is off", "Replication task(s) 1 now ship it"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
}

func TestRunSnapshotTaskFailureAndBlocks(t *testing.T) {
	old, oldPoll := snapshotTaskRunWait, snapshotTaskRunPoll
	snapshotTaskRunWait, snapshotTaskRunPoll = time.Second, 5*time.Millisecond
	t.Cleanup(func() { snapshotTaskRunWait, snapshotTaskRunPoll = old, oldPoll })

	snaps := []map[string]interface{}{}
	rf := &runFake{jobState: withJobState("FAILED", "[EEXIST] Snapshot tank/data@auto-2026-01-05_00-00 already existed. This probably happened because the task already ran on schedule.")}
	task := snapshotTaskFixture(3, "tank/data", false, []interface{}{}, nil)
	r, _, _ := newRunRegistry(t, nil, []interface{}{task}, []string{"tank", "tank/data"}, &snaps, rf)
	first, err := r.CallTool(t.Context(), "run_snapshot_task", map[string]interface{}{"task_id": 3.0})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CallTool(t.Context(), "run_snapshot_task", map[string]interface{}{"task_id": 3.0, "confirmation_token": replyToken(t, first)})
	if err == nil || !strings.Contains(err.Error(), "periodic snapshot task 3 failed") || !strings.Contains(err.Error(), "already existed") {
		t.Errorf("failed job: err = %v", err)
	}

	disabled := snapshotTaskFixture(4, "tank/data", false, []interface{}{}, nil)
	disabled["enabled"] = false
	missing := snapshotTaskFixture(5, "tank/gone", false, []interface{}{}, nil)
	r, fake, _ := newRunRegistry(t, nil, []interface{}{disabled, missing}, []string{"tank", "tank/data"}, &snaps, &runFake{})
	for id, want := range map[float64]string{4: "periodic snapshot task 4 is disabled", 5: "dataset 'tank/gone' of periodic snapshot task 5 does not exist", 6: "periodic snapshot task 6 not found"} {
		out, err := r.CallTool(t.Context(), "run_snapshot_task", map[string]interface{}{"task_id": id})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("task %v: err = %v, want %q", id, err, want)
		}
		if replyToken(t, out) != "" {
			t.Errorf("task %v: blocked run issued a token", id)
		}
	}
	if len(fake.called("pool.snapshottask.run")) != 0 {
		t.Error("pool.snapshottask.run sent for a blocked task")
	}
}

func TestRunSnapshotTaskStillRunning(t *testing.T) {
	old, oldPoll := snapshotTaskRunWait, snapshotTaskRunPoll
	snapshotTaskRunWait, snapshotTaskRunPoll = 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { snapshotTaskRunWait, snapshotTaskRunPoll = old, oldPoll })

	snaps := []map[string]interface{}{}
	r, fake, _ := newRunRegistry(t, nil, []interface{}{snapshotTaskFixture(3, "tank/data", false, []interface{}{}, nil)}, []string{"tank", "tank/data"}, &snaps, &runFake{})
	_, out := confirmAndRun(t, r, fake, "run_snapshot_task", "pool.snapshottask.run", map[string]interface{}{"task_id": 3.0})
	resp := decodeObj(t, out)
	if resp["state"] != "RUNNING" || !strings.Contains(resp["message"].(string), "tasks_get") {
		t.Errorf("response = %v", resp)
	}
}

func TestCreateSnapshot(t *testing.T) {
	datasets := []string{"tank", "tank/data", "tank/data/a", "tank/other"}
	snaps := []map[string]interface{}{snap("tank/other", "before-upgrade")}
	r, fake, _ := newRunRegistry(t, nil, nil, datasets, &snaps, &runFake{})

	resp := callTool(t, r, "create_snapshot", map[string]interface{}{"dataset": "tank/data", "name": "before-upgrade", "recursive": true, "dry_run": true})
	if resp["dry_run"] != true || resp["current_state"].(map[string]interface{})["datasets_to_capture"] != 2.0 {
		t.Errorf("dry run = %v", resp)
	}
	if len(fake.called("pool.snapshot.create")) != 0 {
		t.Fatal("dry run created a snapshot")
	}

	// Write, not destructive: no confirmation token is needed.
	resp = callTool(t, r, "create_snapshot", map[string]interface{}{"dataset": "tank/data", "name": "before-upgrade", "recursive": true})
	calls := fake.called("pool.snapshot.create")
	if len(calls) != 1 {
		t.Fatalf("pool.snapshot.create calls = %d", len(calls))
	}
	var p []map[string]interface{}
	_ = json.Unmarshal(calls[0].Params, &p)
	if p[0]["dataset"] != "tank/data" || p[0]["name"] != "before-upgrade" || p[0]["recursive"] != true || len(p[0]) != 3 {
		t.Errorf("create params = %v", p[0])
	}
	if resp["snapshot"] != "tank/data@before-upgrade" || resp["created"] != true || resp["snapshots_created_count"] != 2.0 {
		t.Errorf("response = %v", resp)
	}

	for _, tc := range []struct {
		args map[string]interface{}
		want string
	}{
		{map[string]interface{}{"dataset": "tank/data", "name": "before-upgrade"}, "already exists"},
		{map[string]interface{}{"dataset": "tank", "name": "before-upgrade", "recursive": true}, "already exists on 3 of the dataset(s)"},
		{map[string]interface{}{"dataset": "tank/nope", "name": "x"}, "dataset 'tank/nope' not found"},
		{map[string]interface{}{"dataset": "tank/data", "name": "bad name"}, "invalid snapshot name"},
		{map[string]interface{}{"dataset": "tank/data", "name": "a/b"}, "invalid snapshot name"},
		{map[string]interface{}{"dataset": "tank/data@x", "name": "y"}, "without '@'"},
		{map[string]interface{}{"dataset": "tank/data", "name": strings.Repeat("n", 250)}, "too long"},
	} {
		if _, err := r.CallTool(t.Context(), "create_snapshot", tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	if n := len(fake.called("pool.snapshot.create")); n != 1 {
		t.Errorf("refused creates still called pool.snapshot.create: %d calls", n)
	}
}

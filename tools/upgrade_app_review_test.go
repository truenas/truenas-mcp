package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/truenas/truenas-mcp/tasks"
)

// health_timeout_seconds: absent defaults to 60; an integer 0..600 is
// accepted; everything else is an error, not a silent coercion.
func TestUpgradeHealthTimeoutValidation(t *testing.T) {
	if got, err := upgradeHealthTimeout(map[string]interface{}{}); err != nil || got != 60*time.Second {
		t.Errorf("absent: %v, %v", got, err)
	}
	accept := map[string]time.Duration{"0": 0, "1": time.Second, "60": 60 * time.Second, "599": 599 * time.Second, "600": 600 * time.Second}
	for in, want := range accept {
		var n float64
		_, _ = fmt.Sscan(in, &n)
		for _, v := range []interface{}{n, int(n), int64(n)} {
			got, err := upgradeHealthTimeout(map[string]interface{}{"health_timeout_seconds": v})
			if err != nil || got != want {
				t.Errorf("%T(%s): %v, %v; want %v", v, in, got, err, want)
			}
		}
	}
	reject := map[string]interface{}{
		"601": float64(601), "99999": float64(99999), "negative": float64(-1), "-3": float64(-3),
		"fraction 0.5": 0.5, "fraction 1.9": 1.9, "fraction 600.5": 600.5,
		"string": "30", "empty string": "", "true": true, "false": false, "null": nil,
		"list": []interface{}{1.0}, "object": map[string]interface{}{},
	}
	for name, v := range reject {
		_, err := upgradeHealthTimeout(map[string]interface{}{"health_timeout_seconds": v})
		if err == nil {
			t.Errorf("%s (%v) was accepted", name, v)
		} else if !strings.Contains(err.Error(), "health_timeout_seconds must be an integer from 0 to 600") {
			t.Errorf("%s: unclear error %q", name, err)
		}
	}
}

// An invalid value is refused before anything reaches the middleware, for the
// real call and the dry run alike; the boundaries 600 / 601 behave as stated.
func TestUpgradeAppRejectsBadHealthTimeoutBeforeUpgrade(t *testing.T) {
	for name, v := range map[string]interface{}{"601": 601.0, "string": "30", "bool": true, "null": nil, "fraction": 1.5, "negative": -1.0} {
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s dry_run=%v", name, dry), func(t *testing.T) {
				r, _, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
				_, err := r.CallTool("upgrade_app", map[string]interface{}{
					"app_name": "plex", "confirm": true, "dry_run": dry, "health_timeout_seconds": v,
				})
				if err == nil || !strings.Contains(err.Error(), "health_timeout_seconds") {
					t.Fatalf("want a health_timeout_seconds error, got %v", err)
				}
				if n := len(f.called("app.upgrade")); n != 0 {
					t.Errorf("app.upgrade was called %d times", n)
				}
			})
		}
	}

	r, _, _ := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	resp := startUpgrade(t, r, map[string]interface{}{"health_timeout_seconds": 600})
	if !strings.Contains(resp["message"].(string), "up to 600s") {
		t.Errorf("600 not honoured: %v", resp["message"])
	}
	r, _, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	if _, err := r.CallTool("upgrade_app", map[string]interface{}{"app_name": "plex", "confirm": true, "health_timeout_seconds": 601}); err == nil {
		t.Error("601 accepted")
	}
	if len(f.called("app.upgrade")) != 0 {
		t.Error("601 reached app.upgrade")
	}
}

// The schema states what the handler enforces.
func TestUpgradeAppSchemaDeclaresTimeoutBounds(t *testing.T) {
	r, _, _ := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	tool, ok := r.tools["upgrade_app"]
	if !ok {
		t.Fatal("no upgrade_app tool")
	}
	props := tool.Definition.InputSchema["properties"].(map[string]interface{})
	b, _ := json.Marshal(props["health_timeout_seconds"])
	var p map[string]interface{}
	_ = json.Unmarshal(b, &p)
	if p["type"] != "integer" || p["minimum"] != 0.0 || p["maximum"] != 600.0 || p["default"] != 60.0 {
		t.Errorf("health_timeout_seconds schema = %s", b)
	}
}

// The version list cap at its boundary: 20 are untouched, 21 are cut to 20.
func TestNormalizeUpgradeSummaryVersionCapBoundary(t *testing.T) {
	for _, tc := range []struct {
		n         int
		truncated bool
	}{{19, false}, {20, false}, {21, true}} {
		s := upgradeSummaryFixture("x")
		var versions []interface{}
		for i := 0; i < tc.n; i++ {
			versions = append(versions, map[string]interface{}{"version": fmt.Sprintf("1.0.%d", i)})
		}
		s["available_versions_for_upgrade"] = versions
		n := normalizeUpgradeSummary(s).(map[string]interface{})
		got := len(n["available_versions_for_upgrade"].([]interface{}))
		want := tc.n
		if want > 20 {
			want = 20
		}
		_, flagged := n["available_versions_truncated"]
		if got != want || flagged != tc.truncated {
			t.Errorf("%d versions: kept %d (want %d), truncated flag %v (want %v)", tc.n, got, want, flagged, tc.truncated)
		}
		if tc.truncated && n["available_versions_total"] != tc.n {
			t.Errorf("total = %v", n["available_versions_total"])
		}
	}
}

// The changelog cap at its boundary, cut mid-rune.
func TestNormalizeUpgradeSummaryChangelogBoundary(t *testing.T) {
	exact := strings.Repeat("a", 4000) // the documented cap
	n := normalizeUpgradeSummary(upgradeSummaryFixture(exact)).(map[string]interface{})
	if n["changelog"] != exact || n["changelog_truncated"] == true {
		t.Error("a changelog of exactly the cap must be kept whole")
	}
	// One byte over is cut (a changelog cap one byte too generous is caught).
	n = normalizeUpgradeSummary(upgradeSummaryFixture(exact + "a")).(map[string]interface{})
	if len(n["changelog"].(string)) != 4000 || n["changelog_truncated"] != true {
		t.Errorf("4001-byte changelog: len=%d truncated=%v", len(n["changelog"].(string)), n["changelog_truncated"])
	}
	// Made of 3-byte runes so the cap lands inside a rune.
	over := strings.Repeat("€", 4000/3+1) // 4001 bytes
	n = normalizeUpgradeSummary(upgradeSummaryFixture(over)).(map[string]interface{})
	cl := n["changelog"].(string)
	if !utf8.ValidString(cl) || len(cl) > 4000 || n["changelog_truncated"] != true {
		t.Errorf("changelog: valid=%v len=%d truncated=%v", utf8.ValidString(cl), len(cl), n["changelog_truncated"])
	}
}

// A RUNNING app with a container that is still starting is not done: the task
// keeps waiting, and at the timeout it fails with a message that says the
// upgrade succeeded and the app was slow, naming the knob to raise.
func TestUpgradeAppStartingContainerKeepsWaiting(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	f.appState.Store(appEntry("plex", "RUNNING", [2]string{"web", "running"}, [2]string{"db", "starting"}))
	id := startUpgrade(t, r, map[string]interface{}{"health_timeout_seconds": 1})["task_id"].(string)

	mgr.PollNow()
	st := taskState(t, r, id)
	if st["status"] != "working" || !strings.Contains(st["statusMessage"].(string), "db (starting)") {
		t.Fatalf("starting container: %v / %v", st["status"], st["statusMessage"])
	}
	time.Sleep(1100 * time.Millisecond)
	mgr.PollNow()
	st = taskState(t, r, id)
	msg, _ := st["statusMessage"].(string)
	if st["status"] != "failed" || !strings.Contains(msg, "upgrade job succeeded") || !strings.Contains(msg, "db (starting)") ||
		!strings.Contains(msg, "not an upgrade failure") || !strings.Contains(msg, "health_timeout_seconds") {
		t.Errorf("after timeout: %v / %q", st["status"], msg)
	}

	// Once it is up the task completes.
	f.appState.Store(appEntry("plex", "RUNNING", [2]string{"web", "running"}, [2]string{"db", "running"}))
	id2 := startUpgrade(t, r, nil)["task_id"].(string)
	mgr.PollNow()
	if st := taskState(t, r, id2); st["status"] != "completed" {
		t.Errorf("healthy app: %v / %v", st["status"], st["statusMessage"])
	}
}

// A crashed container is a real failure and is not described as slow start,
// even if another container is still starting.
func TestUpgradeAppCrashedIsNotReportedAsSlowStart(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	f.appState.Store(appEntry("plex", "RUNNING", [2]string{"web", "crashed"}, [2]string{"db", "starting"}))
	id := startUpgrade(t, r, map[string]interface{}{"health_timeout_seconds": 0})["task_id"].(string)
	mgr.PollNow()
	st := taskState(t, r, id)
	msg, _ := st["statusMessage"].(string)
	if st["status"] != "failed" || strings.Contains(msg, "not an upgrade failure") || !strings.Contains(msg, "web (crashed)") {
		t.Errorf("%v / %q", st["status"], msg)
	}
}

// tasks_get reads the task while the poller updates it. Under -race this
// fails if the two share memory.
func TestTasksGetWhilePolling(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	f.appState.Store(appEntry("plex", "DEPLOYING", [2]string{"web", "starting"}))
	id := startUpgrade(t, r, map[string]interface{}{"health_timeout_seconds": 600})["task_id"].(string)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				out, err := r.CallTool("tasks_get", map[string]interface{}{"task_id": id})
				if err != nil {
					t.Error(err)
					return
				}
				_ = out
				if _, err := r.CallTool("tasks_list", map[string]interface{}{}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for i := 0; i < 15; i++ {
		mgr.PollNow()
	}
	close(stop)
	wg.Wait()
	if st := taskState(t, r, id); st["job"] == nil {
		t.Error("the job summary written by the poller is not visible")
	}
}

// The "4 KiB" cap holds on every path that returns a job: the job in
// tasks_get and each entry of query_jobs, with long text in every field.
func TestJobSummaryCapOnEveryPath(t *testing.T) {
	long := strings.Repeat("€", 6000)
	job := map[string]interface{}{
		"id": float64(upgradeJobID), "method": "app.upgrade", "state": "FAILED", "description": long,
		"progress": map[string]interface{}{"percent": 50.0, "description": long},
		"error":    long, "exception": long, "abortable": false,
		"time_started": map[string]interface{}{"$date": float64(1767225600000)},
		"result":       strings.Repeat("r", 30000),
	}

	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	f.job.Store(job)
	id := startUpgrade(t, r, nil)["task_id"].(string)
	mgr.PollNow()
	out, err := r.CallTool("tasks_get", map[string]interface{}{"task_id": id})
	if err != nil {
		t.Fatal(err)
	}
	j := decode(t, out)["job"]
	b, _ := json.MarshalIndent(j, "  ", "  ")
	if len(b) > tasks.MaxJobResultBytes {
		t.Errorf("tasks_get job is %d bytes, want <= %d", len(b), tasks.MaxJobResultBytes)
	}
	if !utf8.ValidString(out) {
		t.Error("tasks_get output is not valid UTF-8")
	}

	fake := &fakeMiddleware{handle: jobsHandle([]interface{}{job, job}, 2)}
	client, _ := newFakeClientFrom(t, fake)
	qj, err := NewRegistry(client, mgr).CallTool("query_jobs", map[string]interface{}{"state": "all"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range decode(t, qj)["jobs"].([]interface{}) {
		eb, _ := json.MarshalIndent(e, "    ", "  ")
		if len(eb) > tasks.MaxJobResultBytes {
			t.Errorf("query_jobs entry is %d bytes, want <= %d", len(eb), tasks.MaxJobResultBytes)
		}
	}
}

// jobsHandle answers core.get_jobs from jobs (honouring options.limit) and a
// {"count": true} query with total.
func jobsHandle(jobs []interface{}, total int) func(string, json.RawMessage) (interface{}, map[string]interface{}, bool) {
	return func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "core.get_jobs" {
			return nil, nil, false
		}
		var raw []json.RawMessage
		_ = json.Unmarshal(params, &raw)
		var opts map[string]interface{}
		if len(raw) > 1 {
			_ = json.Unmarshal(raw[1], &opts)
		}
		if opts["count"] == true {
			return total, nil, true
		}
		out := jobs
		if l, ok := opts["limit"].(float64); ok && int(l) < len(out) {
			out = out[:int(l)]
		}
		return out, nil, true
	}
}

func numberedJobs(n int) []interface{} {
	var jobs []interface{}
	for i := 0; i < n; i++ {
		jobs = append(jobs, map[string]interface{}{"id": float64(i + 1), "method": "m", "state": "SUCCESS", "description": "d"})
	}
	return jobs
}

func queryJobs(t *testing.T, jobs []interface{}, total int, args map[string]interface{}) (map[string]interface{}, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{handle: jobsHandle(jobs, total)}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	out, err := NewRegistry(client, mgr).CallTool("query_jobs", args)
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, out), fake
}

// query_jobs says when the list was cut and how many there are in all.
func TestQueryJobsHasMoreAndTotal(t *testing.T) {
	// 120 jobs match; the default limit is 50.
	resp, fake := queryJobs(t, numberedJobs(120), 120, map[string]interface{}{"state": "all"})
	if resp["has_more"] != true || resp["total"] != 120.0 || resp["job_count"] != 50.0 || resp["limit"] != 50.0 {
		t.Errorf("cut list: has_more=%v total=%v job_count=%v limit=%v", resp["has_more"], resp["total"], resp["job_count"], resp["limit"])
	}
	if n := len(fake.called("core.get_jobs")); n != 2 {
		t.Errorf("core.get_jobs calls = %d, want the list and the count", n)
	}

	// Exactly as many as the limit: nothing more, no extra count query.
	resp, fake = queryJobs(t, numberedJobs(50), 50, map[string]interface{}{"state": "all"})
	if resp["has_more"] != false || resp["total"] != 50.0 || resp["job_count"] != 50.0 {
		t.Errorf("exact fit: %v", resp)
	}
	if n := len(fake.called("core.get_jobs")); n != 1 {
		t.Errorf("core.get_jobs calls = %d, want 1", n)
	}

	// One more than the limit.
	resp, _ = queryJobs(t, numberedJobs(11), 11, map[string]interface{}{"state": "all", "limit": 10.0})
	if resp["has_more"] != true || resp["total"] != 11.0 || resp["job_count"] != 10.0 {
		t.Errorf("limit 10 of 11: %v", resp)
	}

	// Fewer than the limit.
	resp, _ = queryJobs(t, numberedJobs(3), 3, map[string]interface{}{"state": "all"})
	if resp["has_more"] != false || resp["total"] != 3.0 {
		t.Errorf("3 jobs: %v", resp)
	}
}

// If the count query fails the list still comes back, flagged has_more.
func TestQueryJobsCountFailureStillFlagsHasMore(t *testing.T) {
	inner := jobsHandle(numberedJobs(20), 20)
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if strings.Contains(string(params), `"count"`) {
			return nil, map[string]interface{}{"code": -32000, "message": "nope"}, true
		}
		return inner(method, params)
	}}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	out, err := NewRegistry(client, mgr).CallTool("query_jobs", map[string]interface{}{"state": "all", "limit": 5.0})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	if resp["has_more"] != true || resp["job_count"] != 5.0 {
		t.Errorf("%v", resp)
	}
	if _, has := resp["total"]; has {
		t.Errorf("total must be omitted when it is unknown: %v", resp["total"])
	}
}

// query_jobs bounds exception and error text, flags the cut, and keeps text
// that is short enough.
func TestQueryJobsBoundsErrorAndException(t *testing.T) {
	long := strings.Repeat("€", tasks.MaxJobTextBytes) // 3 bytes per rune
	// 3000 bytes: over the 2000-byte text cap but, alone, within the 4 KiB
	// total, so only the text cap itself can cut it.
	mid := strings.Repeat("€", 1000)
	jobs := []interface{}{
		map[string]interface{}{"id": 5.0, "state": "FAILED", "error": mid},
		map[string]interface{}{"id": 6.0, "state": "FAILED", "exception": mid},
		map[string]interface{}{"id": 1.0, "state": "FAILED", "error": long, "exception": long},
		map[string]interface{}{"id": 2.0, "state": "FAILED", "error": "short error", "exception": "short exception"},
		map[string]interface{}{"id": 3.0, "state": "FAILED", "error": strings.Repeat("e", tasks.MaxJobTextBytes), "exception": "x"},
		map[string]interface{}{"id": 4.0, "state": "FAILED", "exception": strings.Repeat("x", tasks.MaxJobTextBytes)},
	}
	resp, _ := queryJobs(t, jobs, 6, map[string]interface{}{"state": "all"})
	got := resp["jobs"].([]interface{})
	for i, k := range []string{"error", "exception"} {
		e := got[i].(map[string]interface{})
		s, _ := e[k].(string)
		if len(s) > tasks.MaxJobTextBytes || !utf8.ValidString(s) || !strings.HasPrefix(mid, s) || e[k+"_truncated"] != true {
			t.Errorf("a single %s of 3000 bytes: len=%d truncated=%v", k, len(s), e[k+"_truncated"])
		}
	}
	got = got[2:]
	e := got[0].(map[string]interface{})
	for _, k := range []string{"error", "exception"} {
		s := e[k].(string)
		if len(s) > tasks.MaxJobTextBytes || !utf8.ValidString(s) || !strings.HasPrefix(long, s) {
			t.Errorf("%s: len=%d valid=%v", k, len(s), utf8.ValidString(s))
		}
		if e[k+"_truncated"] != true {
			t.Errorf("%s_truncated not set: %v", k, e)
		}
	}
	e = got[1].(map[string]interface{})
	if e["error"] != "short error" || e["exception"] != "short exception" || e["error_truncated"] != nil || e["exception_truncated"] != nil {
		t.Errorf("short text changed or flagged: %v", e)
	}
	// Exactly at the cap is not cut (one long field fits in the 4 KiB total).
	e = got[2].(map[string]interface{})
	if len(e["error"].(string)) != tasks.MaxJobTextBytes || e["error_truncated"] != nil {
		t.Errorf("error at the cap was cut: %v", e["error_truncated"])
	}
	e = got[3].(map[string]interface{})
	if len(e["exception"].(string)) != tasks.MaxJobTextBytes || e["exception_truncated"] != nil {
		t.Errorf("exception at the cap was cut: %v", e["exception_truncated"])
	}
}

// query_apps shows the current and the latest version so a caller can see
// what an upgrade would go to.
func TestQueryAppsShowsVersions(t *testing.T) {
	upgradable := appEntry("plex", "RUNNING")
	upgradable["human_version"] = "1.0.0_1.0.0"
	upgradable["latest_version"] = "1.2.0"
	upgradable["upgrade_available"] = true
	current := appEntry("nginx", "RUNNING")
	current["latest_version"] = "1.2.0" // always set, even with no upgrade
	fake := &fakeMiddleware{results: map[string]interface{}{"app.query": []interface{}{upgradable, current}}}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	out, err := NewRegistry(client, mgr).CallTool("query_apps", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	var apps []interface{}
	if err := json.Unmarshal([]byte(out), &apps); err != nil {
		t.Fatalf("bad JSON: %v: %s", err, out)
	}
	byName := map[string]map[string]interface{}{}
	for _, a := range apps {
		m := a.(map[string]interface{})
		byName[m["name"].(string)] = m
	}
	p := byName["plex"]
	if p["human_version"] != "1.0.0_1.0.0" || p["latest_version"] != "1.2.0" || p["upgrade_available"] != true {
		t.Errorf("plex: %v", p)
	}
	n := byName["nginx"]
	if n["upgrade_available"] != false || n["latest_version"] != "1.2.0" || n["human_version"] != "2.0.0_1.2.0" {
		t.Errorf("nginx: %v", n)
	}
}

// The job summary on the task follows the job while it runs, also when the
// status message does not change between polls (the poller keeps the
// summary even though nothing "updated").
func TestTasksGetJobSummaryFollowsRunningJob(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	running := func(percent float64) map[string]interface{} {
		return map[string]interface{}{
			"id": float64(upgradeJobID), "method": "app.upgrade", "state": "RUNNING",
			"progress": map[string]interface{}{"percent": percent, "description": "Pulling images"},
		}
	}
	f.job.Store(running(10))
	id := startUpgrade(t, r, nil)["task_id"].(string)
	percent := func() interface{} {
		job, _ := taskState(t, r, id)["job"].(map[string]interface{})
		prog, _ := job["progress"].(map[string]interface{})
		return prog["percent"]
	}
	mgr.PollNow()
	if percent() != 10.0 {
		t.Fatalf("first poll: percent = %v", percent())
	}
	f.job.Store(running(60)) // same status message, new percent
	mgr.PollNow()
	if percent() != 60.0 {
		t.Errorf("job summary is stale: percent = %v, want 60", percent())
	}
}

// At the largest limit query_jobs must not ask the middleware for more than
// its 10000-row maximum.
func TestQueryJobsLimitCappedAtMiddlewareMax(t *testing.T) {
	for _, l := range []float64{9999, 10000, 50000} {
		resp, fake := queryJobs(t, numberedJobs(3), 3, map[string]interface{}{"state": "all", "limit": l})
		if resp["limit"] != 9999.0 {
			t.Errorf("limit %v: reported limit %v, want 9999", l, resp["limit"])
		}
		calls := fake.called("core.get_jobs")
		if len(calls) == 0 {
			t.Fatalf("limit %v: no core.get_jobs call", l)
		}
		var raw []json.RawMessage
		_ = json.Unmarshal(calls[0].Params, &raw)
		var opts map[string]interface{}
		if len(raw) < 2 || json.Unmarshal(raw[1], &opts) != nil {
			t.Fatalf("limit %v: bad params %s", l, calls[0].Params)
		}
		if opts["limit"] != 10000.0 {
			t.Errorf("limit %v: sent options.limit %v, want 10000", l, opts["limit"])
		}
	}
}

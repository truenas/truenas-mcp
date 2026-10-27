package tools

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// Fixtures mimic the 25.10 / 27.0 middleware shapes (app.upgrade_summary,
// app.upgrade, app.query, core.get_jobs) with fake values.

const upgradeJobID = 42

func upgradeSummaryFixture(changelog interface{}) map[string]interface{} {
	return map[string]interface{}{
		"latest_version":        "1.2.0",
		"latest_human_version":  "2.0.0_1.2.0",
		"upgrade_version":       "1.2.0",
		"upgrade_human_version": "2.0.0_1.2.0",
		"available_versions_for_upgrade": []interface{}{
			map[string]interface{}{"version": "1.2.0", "human_version": "2.0.0_1.2.0"},
		},
		"changelog": changelog,
	}
}

// appEntry is an app.query entry; containers are service->state pairs.
func appEntry(name, state string, containers ...[2]string) map[string]interface{} {
	details := []interface{}{}
	for _, c := range containers {
		details = append(details, map[string]interface{}{
			"id": "abc", "service_name": c[0], "image": "example/" + c[0] + ":1", "state": c[1],
			"port_config": []interface{}{}, "volume_mounts": []interface{}{},
		})
	}
	return map[string]interface{}{
		"name": name, "id": name, "state": state, "version": "1.2.0", "human_version": "2.0.0_1.2.0",
		"latest_version": "1.2.0", "upgrade_available": false, "image_updates_available": false,
		"custom_app": false, "metadata": map[string]interface{}{"title": "Example"},
		"active_workloads": map[string]interface{}{"containers": len(details), "container_details": details},
	}
}

// bigAppEntry is a job result far larger than the bound.
func bigAppEntry(name, state string) map[string]interface{} {
	app := appEntry(name, state, [2]string{"web", "running"})
	cfg := map[string]interface{}{}
	for i := 0; i < 400; i++ {
		cfg[strings.Repeat("k", 10)+string(rune('a'+i%26))+strings.Repeat("x", i%7)+string(rune('A'+i/26))] = strings.Repeat("v", 60)
	}
	app["config"] = cfg
	return app
}

type upgradeFake struct {
	*fakeMiddleware
	appState atomic.Value // map[string]interface{} returned by app.query
	job      atomic.Value // map[string]interface{} returned by core.get_jobs
}

func newUpgradeFake(t *testing.T, summary interface{}) (*truenas.Client, *upgradeFake) {
	t.Helper()
	f := &upgradeFake{}
	f.appState.Store(appEntry("plex", "RUNNING", [2]string{"web", "running"}))
	f.job.Store(map[string]interface{}{
		"id": float64(upgradeJobID), "method": "app.upgrade", "state": "SUCCESS",
		"time_started":  map[string]interface{}{"$date": float64(1767225600000)}, // 2026-01-01T00:00:00Z
		"time_finished": map[string]interface{}{"$date": float64(1767225660000)},
		"progress":      map[string]interface{}{"percent": 100.0, "description": "Upgraded app successfully"},
		"result":        f.appState.Load(),
	})
	f.fakeMiddleware = &fakeMiddleware{
		results: map[string]interface{}{
			"app.upgrade_summary": summary,
			"app.upgrade":         upgradeJobID,
		},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			switch method {
			case "app.query":
				return []interface{}{f.appState.Load()}, nil, true
			case "core.get_jobs":
				return []interface{}{f.job.Load()}, nil, true
			}
			return nil, nil, false
		},
	}
	client, _ := newFakeClientFrom(t, f.fakeMiddleware)
	return client, f
}

func newUpgradeRegistry(t *testing.T, summary interface{}) (*Registry, *tasks.Manager, *upgradeFake) {
	client, f := newUpgradeFake(t, summary)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), mgr, f
}

func startUpgrade(t *testing.T, r *Registry, extra map[string]interface{}) map[string]interface{} {
	t.Helper()
	args := map[string]interface{}{"app_name": "plex", "confirm": true}
	for k, v := range extra {
		args[k] = v
	}
	out, err := r.CallTool("upgrade_app", args)
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, out)
}

func taskState(t *testing.T, r *Registry, id string) map[string]interface{} {
	t.Helper()
	out, err := r.CallTool("tasks_get", map[string]interface{}{"task_id": id})
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, out)
}

// Item 1: a null, empty or blank changelog is reported as "no changelog
// provided", never as null.
func TestUpgradeAppNullChangelog(t *testing.T) {
	for name, changelog := range map[string]interface{}{"null": nil, "empty": "", "blank": " \n\t"} {
		t.Run(name, func(t *testing.T) {
			r, _, _ := newUpgradeRegistry(t, upgradeSummaryFixture(changelog))
			out, err := r.CallTool("upgrade_app", map[string]interface{}{"app_name": "plex", "confirm": true})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "null") {
				t.Errorf("output contains null: %s", out)
			}
			summary := decode(t, out)["upgrade_summary"].(map[string]interface{})
			if summary["changelog"] != "no changelog provided" {
				t.Errorf("changelog = %v", summary["changelog"])
			}
		})
	}
}

func TestUpgradeAppDryRunNullChangelogAndChangelogKept(t *testing.T) {
	r, _, f := newUpgradeRegistry(t, upgradeSummaryFixture(nil))
	f.results["app.query"] = []interface{}{appEntry("plex", "RUNNING")}
	// Without confirm the call is a preview built by the dry-run path.
	out, err := r.CallTool("upgrade_app", map[string]interface{}{"app_name": "plex"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.called("app.upgrade"); len(got) != 0 {
		t.Fatalf("preview must not upgrade: %d app.upgrade calls", len(got))
	}
	if !strings.Contains(out, `"changelog": "no changelog provided"`) || strings.Contains(out, "null") {
		t.Errorf("preview should say no changelog provided and not print null: %s", out)
	}
	// The dry-run plan states the health check.
	if !strings.Contains(out, "Verify the app reports RUNNING") {
		t.Errorf("preview missing the verify step: %s", out)
	}

	// A real changelog is kept as is.
	n := normalizeUpgradeSummary(upgradeSummaryFixture("Fixed things")).(map[string]interface{})
	if n["changelog"] != "Fixed things" {
		t.Errorf("changelog = %v", n["changelog"])
	}
}

func TestNormalizeUpgradeSummaryBounds(t *testing.T) {
	long := strings.Repeat("é", maxChangelogBytes) // 2 bytes per rune
	s := upgradeSummaryFixture(long)
	var versions []interface{}
	for i := 0; i < 30; i++ {
		versions = append(versions, map[string]interface{}{"version": "1.0." + string(rune('a'+i%26))})
	}
	s["available_versions_for_upgrade"] = versions
	n := normalizeUpgradeSummary(s).(map[string]interface{})
	cl := n["changelog"].(string)
	if len(cl) > maxChangelogBytes || n["changelog_truncated"] != true || n["changelog_length"] != len(long) {
		t.Errorf("changelog not bounded: len=%d truncated=%v length=%v", len(cl), n["changelog_truncated"], n["changelog_length"])
	}
	if !strings.HasPrefix(long, cl) {
		t.Error("truncation split a rune")
	}
	if got := len(n["available_versions_for_upgrade"].([]interface{})); got != maxUpgradeVersions || n["available_versions_truncated"] != true || n["available_versions_total"] != 30 {
		t.Errorf("versions not capped: %d %v %v", got, n["available_versions_truncated"], n["available_versions_total"])
	}
	// A list of summaries is normalized per element.
	l := normalizeUpgradeSummary([]interface{}{upgradeSummaryFixture(nil)}).([]interface{})
	if l[0].(map[string]interface{})["changelog"] != "no changelog provided" {
		t.Errorf("list element not normalized: %v", l)
	}
}

// Item 2: the response states what was requested, not an observed snapshot,
// and the request reaches app.upgrade unchanged.
func TestUpgradeAppSnapshotRequestedNotClaimed(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want bool
	}{
		{"default", nil, true},
		{"explicit true", map[string]interface{}{"snapshot_hostpaths": true}, true},
		{"explicit false", map[string]interface{}{"snapshot_hostpaths": false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
			resp := startUpgrade(t, r, tc.args)
			if _, claimed := resp["snapshot_created"]; claimed {
				t.Errorf("snapshot_created must not be reported: %v", resp)
			}
			if resp["snapshot_requested"] != tc.want {
				t.Errorf("snapshot_requested = %v, want %v", resp["snapshot_requested"], tc.want)
			}
			note, _ := resp["snapshot_note"].(string)
			if tc.want && !strings.Contains(note, "does not report") {
				t.Errorf("note should say the job does not report snapshots: %q", note)
			}
			// Middleware stops the app and snapshots ix-volumes whatever the
			// option says; the note must not imply otherwise.
			if !strings.Contains(note, "stops the app") || !strings.Contains(note, "ix-volumes") {
				t.Errorf("note should say the app is always stopped and ix-volumes always snapshotted: %q", note)
			}
			if !tc.want && !strings.Contains(note, "no host-path snapshot") {
				t.Errorf("note should say no host-path snapshot is requested: %q", note)
			}
			calls := f.called("app.upgrade")
			if len(calls) != 1 {
				t.Fatalf("app.upgrade calls = %d", len(calls))
			}
			var raw []json.RawMessage
			_ = json.Unmarshal(calls[0].Params, &raw)
			var opts map[string]interface{}
			_ = json.Unmarshal(raw[1], &opts)
			if opts["snapshot_hostpaths"] != tc.want {
				t.Errorf("sent snapshot_hostpaths = %v, want %v", opts["snapshot_hostpaths"], tc.want)
			}
		})
	}
}

// Item 3: job times are RFC 3339 UTC, not raw epoch milliseconds.
func TestUpgradeAppTaskTimesAreRFC3339(t *testing.T) {
	r, mgr, _ := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	id := startUpgrade(t, r, nil)["task_id"].(string)
	mgr.PollNow()
	job := taskState(t, r, id)["job"].(map[string]interface{})
	if job["time_started"] != "2026-01-01T00:00:00Z" || job["time_finished"] != "2026-01-01T00:01:00Z" {
		t.Errorf("times = %v / %v", job["time_started"], job["time_finished"])
	}
}

// Item 3 (query_jobs): same conversion, and a bare epoch value too.
func TestJobTimeForms(t *testing.T) {
	summary := func(in interface{}) interface{} {
		return tasks.SummarizeJob(map[string]interface{}{"id": 1.0, "time_started": in})["time_started"]
	}
	for name, in := range map[string]interface{}{
		"date object": map[string]interface{}{"$date": float64(1767225600000)},
		"bare ms":     float64(1767225600000),
		"bare s":      float64(1767225600),
		"string ms":   "1767225600000",
	} {
		if got := summary(in); got != "2026-01-01T00:00:00Z" {
			t.Errorf("%s: %v", name, got)
		}
	}
	for name, in := range map[string]interface{}{"text": "soon", "tiny": float64(5)} {
		if got := summary(in); got != in {
			t.Errorf("%s should pass through unchanged, got %v", name, got)
		}
	}
}

// Item 4: a huge job result is summarised and flagged, not returned verbatim,
// and the important fields survive.
func TestUpgradeAppHugeJobResultIsBounded(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	big := bigAppEntry("plex", "RUNNING")
	raw, _ := json.Marshal(big)
	if len(raw) < 20000 {
		t.Fatalf("fixture too small: %d", len(raw))
	}
	f.appState.Store(big)
	job := f.job.Load().(map[string]interface{})
	job["result"] = big
	f.job.Store(job)

	id := startUpgrade(t, r, nil)["task_id"].(string)
	mgr.PollNow()
	out, err := r.CallTool("tasks_get", map[string]interface{}{"task_id": id})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > tasks.MaxJobResultBytes {
		t.Errorf("tasks_get output is %d bytes, want <= %d", len(out), tasks.MaxJobResultBytes)
	}
	st := decode(t, out)
	j := st["job"].(map[string]interface{})
	if j["result_truncated"] != true || j["result_size_bytes"] != float64(len(raw)) {
		t.Errorf("truncation not flagged: %v %v", j["result_truncated"], j["result_size_bytes"])
	}
	app := j["app"].(map[string]interface{})
	if app["name"] != "plex" || app["state"] != "RUNNING" || app["version"] != "1.2.0" || app["human_version"] != "2.0.0_1.2.0" {
		t.Errorf("app summary lost fields: %v", app)
	}
	if j["state"] != "SUCCESS" || j["progress"].(map[string]interface{})["percent"] != 100.0 {
		t.Errorf("job state/progress lost: %v", j)
	}
	if strings.Contains(out, "vvvvvvvv") {
		t.Error("config leaked into the summary")
	}

	// query_jobs bounds the same result.
	f.results["core.get_jobs"] = []interface{}{job}
	f.handle = nil
	qj, err := r.CallTool("query_jobs", map[string]interface{}{"state": "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(qj) > 2*tasks.MaxJobResultBytes {
		t.Errorf("query_jobs output is %d bytes", len(qj))
	}
	jobs := decode(t, qj)["jobs"].([]interface{})
	entry := jobs[0].(map[string]interface{})
	if entry["result_truncated"] != true || entry["result_size_bytes"] != float64(len(raw)) {
		t.Errorf("query_jobs truncation not flagged: %v", entry)
	}
	if res := entry["result"].(map[string]interface{}); res["name"] != "plex" || res["state"] != "RUNNING" {
		t.Errorf("query_jobs result lost fields: %v", res)
	}
	if entry["time_started"] != "2026-01-01T00:00:00Z" {
		t.Errorf("query_jobs time_started = %v", entry["time_started"])
	}
}

func TestBoundResultSmallAndNonApp(t *testing.T) {
	small := map[string]interface{}{"ok": true}
	if out, trunc, _ := tasks.BoundResult(small); trunc || out == nil {
		t.Errorf("small result changed: %v %v", out, trunc)
	}
	huge := map[string]interface{}{"blob": strings.Repeat("z", 3*tasks.MaxJobResultBytes)}
	out, trunc, size := tasks.BoundResult(huge)
	s, isString := out.(string)
	if !trunc || !isString || len(s) > tasks.MaxJobResultBytes || size < 3*tasks.MaxJobResultBytes {
		t.Errorf("non-app result not bounded: trunc=%v len=%d size=%d", trunc, len(s), size)
	}
	sum := tasks.SummarizeJob(map[string]interface{}{
		"id": 1.0, "state": "FAILED", "error": strings.Repeat("e", 3*tasks.MaxJobTextBytes),
		"exception": strings.Repeat("t", 3*tasks.MaxJobTextBytes),
	})
	if len(sum["error"].(string)) > tasks.MaxJobTextBytes || sum["error_truncated"] != true || sum["exception_truncated"] != true {
		t.Errorf("error/exception not bounded: %v", sum)
	}
}

// Item 5: a finished job is not success until the app is RUNNING.
func TestUpgradeAppSuccessWaitsForRunningApp(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	f.appState.Store(appEntry("plex", "DEPLOYING", [2]string{"web", "starting"}))
	resp := startUpgrade(t, r, nil)
	id := resp["task_id"].(string)
	if !strings.Contains(resp["message"].(string), "up to 60s") {
		t.Errorf("default timeout not stated: %v", resp["message"])
	}

	mgr.PollNow() // job SUCCESS, app still DEPLOYING
	st := taskState(t, r, id)
	if st["status"] != "working" || !strings.Contains(st["statusMessage"].(string), "DEPLOYING") {
		t.Fatalf("job finished but app deploying must stay working, got %v / %v", st["status"], st["statusMessage"])
	}
	if strings.Contains(strings.ToLower(st["statusMessage"].(string)), "completed") {
		t.Errorf("claimed success early: %v", st["statusMessage"])
	}

	f.appState.Store(appEntry("plex", "RUNNING", [2]string{"web", "running"}, [2]string{"init", "exited"}))
	mgr.PollNow()
	st = taskState(t, r, id)
	if st["status"] != "completed" || !strings.Contains(st["statusMessage"].(string), "plex is RUNNING") {
		t.Errorf("RUNNING app (exited init container) should complete: %v / %v", st["status"], st["statusMessage"])
	}
}

func TestUpgradeAppFailsWhenAppNotHealthy(t *testing.T) {
	cases := []struct {
		name string
		app  map[string]interface{}
		want string
	}{
		{"crashed", appEntry("plex", "CRASHED", [2]string{"web", "crashed"}), "upgrade job finished but app is CRASHED"},
		{"stopped", appEntry("plex", "STOPPED"), "upgrade job finished but app is STOPPED"},
		{"running with crashed container", appEntry("plex", "RUNNING", [2]string{"web", "running"}, [2]string{"db", "crashed"}), "upgrade job finished but app is RUNNING with containers not healthy: db (crashed)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
			f.appState.Store(tc.app)
			// 0 = check once, no waiting.
			id := startUpgrade(t, r, map[string]interface{}{"health_timeout_seconds": 0})["task_id"].(string)
			mgr.PollNow()
			st := taskState(t, r, id)
			if st["status"] != "failed" || st["statusMessage"] != tc.want {
				t.Errorf("status=%v message=%q, want failed %q", st["status"], st["statusMessage"], tc.want)
			}
		})
	}
}

func TestUpgradeAppHealthTimeoutIsConfigurableAndBounded(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	f.appState.Store(appEntry("plex", "DEPLOYING", [2]string{"web", "starting"}))
	id := startUpgrade(t, r, map[string]interface{}{"health_timeout_seconds": 1})["task_id"].(string)

	mgr.PollNow()
	if st := taskState(t, r, id); st["status"] != "working" {
		t.Fatalf("within the timeout the task keeps waiting, got %v", st["status"])
	}
	time.Sleep(1100 * time.Millisecond)
	mgr.PollNow()
	st := taskState(t, r, id)
	msg, _ := st["statusMessage"].(string)
	if st["status"] != "failed" || !strings.Contains(msg, "upgrade job succeeded") || !strings.Contains(msg, "still starting (DEPLOYING)") ||
		!strings.Contains(msg, "not an upgrade failure") || !strings.Contains(msg, "health_timeout_seconds") {
		t.Errorf("after the timeout: %v / %q", st["status"], msg)
	}

}

func TestUpgradeAppJobFailureStillFails(t *testing.T) {
	r, mgr, f := newUpgradeFakeRegistryJob(t, map[string]interface{}{
		"id": float64(upgradeJobID), "state": "FAILED", "error": strings.Repeat("boom ", 2000),
	})
	_ = f
	id := startUpgrade(t, r, nil)["task_id"].(string)
	mgr.PollNow()
	st := taskState(t, r, id)
	if st["status"] != "failed" || len(st["statusMessage"].(string)) > tasks.MaxJobTextBytes {
		t.Errorf("failed job: %v, message len %d", st["status"], len(st["statusMessage"].(string)))
	}
	if st["job"].(map[string]interface{})["error_truncated"] != true {
		t.Errorf("error truncation not flagged: %v", st["job"])
	}
}

func newUpgradeFakeRegistryJob(t *testing.T, job map[string]interface{}) (*Registry, *tasks.Manager, *upgradeFake) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	f.job.Store(job)
	return r, mgr, f
}

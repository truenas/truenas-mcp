package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// upgradeJobWithResult makes the upgrade job SUCCESS with result as the app
// snapshot app.upgrade returns: the app as it was when the job ended.
func upgradeJobWithResult(f *upgradeFake, result map[string]interface{}) {
	job := f.job.Load().(map[string]interface{})
	copied := make(map[string]interface{}, len(job))
	for k, v := range job {
		copied[k] = v
	}
	copied["result"] = result
	f.job.Store(copied)
}

func jobApp(t *testing.T, st map[string]interface{}) (map[string]interface{}, map[string]interface{}) {
	t.Helper()
	job, _ := st["job"].(map[string]interface{})
	app, _ := job["app"].(map[string]interface{})
	if app == nil {
		t.Fatalf("no job.app in %v", st)
	}
	return job, app
}

// The job result says DEPLOYING / starting, the live app is RUNNING: once the
// task completes, job.app shows the live state, consistent with statusMessage,
// and the result stays bounded and flagged.
func TestUpgradeTaskJobAppIsRefreshedOnCompletion(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	stale := bigAppEntry("plex", "DEPLOYING")
	stale["active_workloads"].(map[string]interface{})["container_details"].([]interface{})[0].(map[string]interface{})["state"] = "starting"
	upgradeJobWithResult(f, stale)
	f.appState.Store(appEntry("plex", "RUNNING", [2]string{"web", "running"}))

	id := startUpgrade(t, r, nil)["task_id"].(string)
	mgr.PollNow()
	out, err := r.CallTool(t.Context(), "tasks_get", map[string]interface{}{"task_id": id})
	if err != nil {
		t.Fatal(err)
	}
	st := decode(t, out)
	if st["status"] != "completed" || !strings.Contains(st["statusMessage"].(string), "RUNNING") {
		t.Fatalf("task: %v / %v", st["status"], st["statusMessage"])
	}
	job, app := jobApp(t, st)
	if app["state"] != "RUNNING" {
		t.Errorf("job.app.state = %v, want the live RUNNING", app["state"])
	}
	containers := app["containers"].([]interface{})
	if c := containers[0].(map[string]interface{}); c["state"] != "running" {
		t.Errorf("container state = %v, want running", c["state"])
	}
	if strings.Contains(out, "DEPLOYING") || strings.Contains(out, `"starting"`) {
		t.Errorf("stale app state still shown: %s", out)
	}
	if job["app_source"] != "app.query" {
		t.Errorf("app_source = %v", job["app_source"])
	}
	if ts, _ := job["app_checked_at"].(string); ts == "" {
		t.Error("app_checked_at missing")
	} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("app_checked_at = %q: %v", ts, err)
	}
	// The job's own result is still summarised, and says so.
	if job["result_truncated"] != true || job["result_size_bytes"] == nil || job["state"] != "SUCCESS" {
		t.Errorf("job fields: %v", job)
	}
	if len(out) > tasks.MaxJobResultBytes || strings.Contains(out, "vvvvvvvv") {
		t.Errorf("tasks_get output not bounded: %d bytes", len(out))
	}
}

// While the task waits, job.app follows the live app, so it agrees with the
// "currently ..." status message as the app comes up.
func TestUpgradeTaskJobAppFollowsLiveStateWhileWaiting(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	upgradeJobWithResult(f, appEntry("plex", "RUNNING", [2]string{"web", "running"}))
	f.appState.Store(appEntry("plex", "DEPLOYING", [2]string{"web", "starting"}))

	id := startUpgrade(t, r, map[string]interface{}{"health_timeout_seconds": 600})["task_id"].(string)
	mgr.PollNow()
	st := taskState(t, r, id)
	if st["status"] != "working" || !strings.Contains(st["statusMessage"].(string), "currently DEPLOYING") {
		t.Fatalf("task: %v / %v", st["status"], st["statusMessage"])
	}
	if _, app := jobApp(t, st); app["state"] != "DEPLOYING" {
		t.Errorf("job.app.state = %v, want the live DEPLOYING", app["state"])
	}

	f.appState.Store(appEntry("plex", "RUNNING", [2]string{"web", "running"}))
	mgr.PollNow()
	st = taskState(t, r, id)
	if _, app := jobApp(t, st); st["status"] != "completed" || app["state"] != "RUNNING" {
		t.Errorf("after start: %v, job.app.state = %v", st["status"], app["state"])
	}
}

// If the live app cannot be read, the job's copy is kept but labelled.
func TestUpgradeTaskJobAppLabelledWhenNotRefreshed(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	upgradeJobWithResult(f, appEntry("plex", "DEPLOYING", [2]string{"web", "starting"}))
	handle := f.handle
	f.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "app.query" {
			return nil, map[string]interface{}{"code": -32000, "message": "unavailable"}, true
		}
		return handle(method, params)
	}

	id := startUpgrade(t, r, map[string]interface{}{"health_timeout_seconds": 600})["task_id"].(string)
	mgr.PollNow()
	st := taskState(t, r, id)
	job, app := jobApp(t, st)
	if st["status"] != "working" || app["state"] != "DEPLOYING" {
		t.Errorf("task: %v, job.app.state = %v", st["status"], app["state"])
	}
	if src, _ := job["app_source"].(string); !strings.HasPrefix(src, "job result") {
		t.Errorf("app_source = %v", job["app_source"])
	}
	if _, ok := job["app_checked_at"]; ok {
		t.Error("app_checked_at set although the app was not read")
	}
}

// If app.query succeeds but no longer lists the app, the label says so
// rather than claiming the state could not be read.
func TestUpgradeTaskJobAppLabelledWhenAppNotFound(t *testing.T) {
	r, mgr, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	upgradeJobWithResult(f, appEntry("plex", "DEPLOYING", [2]string{"web", "starting"}))
	handle := f.handle
	f.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "app.query" {
			return []interface{}{}, nil, true
		}
		return handle(method, params)
	}

	id := startUpgrade(t, r, map[string]interface{}{"health_timeout_seconds": 600})["task_id"].(string)
	mgr.PollNow()
	job, app := jobApp(t, taskState(t, r, id))
	if app["state"] != "DEPLOYING" {
		t.Errorf("job.app.state = %v", app["state"])
	}
	if job["app_source"] != "job result (app.query no longer lists the app)" {
		t.Errorf("app_source = %v", job["app_source"])
	}
	if _, ok := job["app_checked_at"]; ok {
		t.Error("app_checked_at set although the app was not found")
	}
}

// upgrade_app and query_jobs say that job SUCCESS is not app health.
func TestQueryJobsDescriptionAndUpgradeDescription(t *testing.T) {
	r, _, _ := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	want := map[string][]string{
		"upgrade_app": {"SUCCESS only means the upgrade job finished, not that the app is healthy", "tasks_get or query_apps", "list jobs with query_jobs", "find the entry whose id matches the returned job_id"},
		"query_jobs":  {"SUCCESS means the job finished, not that the app", "tasks_get or query_apps"},
	}
	seen := 0
	for _, tool := range r.ListTools() {
		for _, s := range want[tool.Name] {
			if !strings.Contains(tool.Description, s) {
				t.Errorf("%s description lacks %q", tool.Name, s)
			}
		}
		if want[tool.Name] != nil {
			seen++
		}
		// query_jobs takes no job_id argument, so the restart advice must
		// not suggest passing one.
		if tool.Name == "upgrade_app" && strings.Contains(tool.Description, "query_jobs using") {
			t.Errorf("upgrade_app description suggests a query_jobs job_id argument: %q", tool.Description)
		}
		if tool.Name == "query_jobs" {
			props, _ := tool.InputSchema["properties"].(map[string]interface{})
			if _, ok := props["job_id"]; ok {
				t.Error("query_jobs has a job_id argument; update the upgrade_app restart advice")
			}
		}
	}
	if seen != len(want) {
		t.Errorf("saw %d of %d tools", seen, len(want))
	}
}

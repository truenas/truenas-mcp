package tools

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// The remaining non-destructive write tools migrated onto the shared plan
// in this PR. start_app was already plan-backed and is included so the
// table stays complete.
var issue111hPlanBackedWriteTools = []string{
	"start_app",
	"download_update",
	"dismiss_alert",
	"restore_alert",
	"refresh_directory_cache",
	"create_scrub_schedule",
	"run_scrub",
	"install_app",
	"create_dataset",
	"create_smb_share",
	"create_nfs_share",
}

func TestPlanBackedWriteToolsRegisterAPlan(t *testing.T) {
	r := NewRegistry(nil, nil)
	for _, name := range issue111hPlanBackedWriteTools {
		if safetyOf(name).Access != Write {
			t.Errorf("%s is listed as a plan-backed write tool but is not classified Write", name)
			continue
		}
		tool, ok := r.tools[name]
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		if tool.Plan == nil {
			t.Errorf("%s must register a Plan so dry-run and the real call share one middleware payload", name)
		}
	}
}

func issue111hDownloadClient(t *testing.T, results map[string]interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	client, fake := newFakeClientWith(t, results, nil)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake
}

func lastCallParams(t *testing.T, fake *fakeMiddleware, method string) []interface{} {
	t.Helper()
	calls := fake.called(method)
	if len(calls) == 0 {
		t.Fatalf("%s was not called", method)
	}
	var params []interface{}
	if err := json.Unmarshal(calls[len(calls)-1].Params, &params); err != nil {
		t.Fatalf("parse %s params: %v (%s)", method, err, calls[len(calls)-1].Params)
	}
	return params
}

// 25.10 update.download takes optional train and version, both-or-neither
// (src/middlewared/middlewared/plugins/update_/download.py). When both are
// set they are passed through as positional parameters; when both are
// omitted the call stays params: [].
func TestDownloadUpdatePassesTrainAndVersion(t *testing.T) {
	r, fake := issue111hDownloadClient(t, map[string]interface{}{
		"update.status":   updateStatusAvailable("25.10.1"),
		"update.download": 7,
	})
	args := map[string]interface{}{
		"train":   "TrueNAS-SCALE-Fangtooth",
		"version": "25.10.1",
	}

	out, err := r.CallTool(t.Context(), "download_update", withArgs(args, obj{"dry_run": true}))
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "TrueNAS-SCALE-Fangtooth") || !strings.Contains(out, "25.10.1") {
		t.Errorf("dry-run preview should name the requested train and version:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "ignored") {
		t.Errorf("dry-run still treats train/version as ignored:\n%s", out)
	}
	if n := len(fake.called("update.download")); n != 0 {
		t.Errorf("dry-run called update.download %d times", n)
	}

	out, err = r.CallTool(t.Context(), "download_update", args)
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["job_id"] != float64(7) {
		t.Errorf("response = %v", resp)
	}
	params := lastCallParams(t, fake, "update.download")
	if len(params) != 2 || params[0] != "TrueNAS-SCALE-Fangtooth" || params[1] != "25.10.1" {
		t.Errorf("update.download params = %#v, want [train, version]", params)
	}
	task, err := r.taskManager.Get(resp["task_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if task.Arguments["train"] != "TrueNAS-SCALE-Fangtooth" || task.Arguments["version"] != "25.10.1" {
		t.Errorf("task arguments dropped train/version: %v", task.Arguments)
	}
}

func TestDownloadUpdateOmitsParamsWhenTrainAndVersionOmitted(t *testing.T) {
	r, fake := issue111hDownloadClient(t, map[string]interface{}{
		"update.status":   updateStatusAvailable("25.10.1"),
		"update.download": 7,
	})
	out, err := r.CallTool(t.Context(), "download_update", map[string]interface{}{})
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	params := lastCallParams(t, fake, "update.download")
	if len(params) != 0 {
		t.Errorf("omitted train/version must keep update.download params empty, got %#v", params)
	}
	if strings.Contains(out, "ignored") {
		t.Errorf("unexpected ignored warning:\n%s", out)
	}
}

func TestDownloadUpdateRejectsOnlyOneOfTrainOrVersion(t *testing.T) {
	r, fake := issue111hDownloadClient(t, map[string]interface{}{
		"update.status":   updateStatusAvailable("25.10.1"),
		"update.download": 7,
	})
	for _, args := range []map[string]interface{}{
		{"train": "TrueNAS-SCALE-Fangtooth"},
		{"version": "25.10.1"},
	} {
		for _, extra := range []obj{{"dry_run": true}, {}} {
			out, err := r.CallTool(t.Context(), "download_update", withArgs(args, extra))
			if err == nil {
				t.Errorf("%v: expected error, got %s", withArgs(args, extra), out)
				continue
			}
			msg := err.Error()
			if !strings.Contains(msg, "train") || !strings.Contains(msg, "version") || !strings.Contains(msg, "both") {
				t.Errorf("%v: err = %v, want a both-or-neither message", withArgs(args, extra), err)
			}
		}
	}
	if n := len(fake.called("update.download")); n != 0 {
		t.Errorf("rejected calls still invoked update.download %d times", n)
	}
}

func TestDownloadUpdateAlreadyDownloadedDoesNotSkipRequestedVersion(t *testing.T) {
	r, fake := issue111hDownloadClient(t, map[string]interface{}{
		"update.status": updateStatusResult("NORMAL", updateStatusDetails(updateStatusNewVersion("25.10.2")), nil, map[string]interface{}{
			"percent": float64(100), "version": "25.10.1", "description": "Update downloaded.",
		}),
		"update.download": 9,
	})
	out, err := r.CallTool(t.Context(), "download_update", map[string]interface{}{
		"train": "TrueNAS-SCALE-Fangtooth", "version": "25.10.2",
	})
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if strings.Contains(out, `"already_downloaded"`) {
		t.Errorf("short-circuited a request for a different version:\n%s", out)
	}
	params := lastCallParams(t, fake, "update.download")
	if len(params) != 2 || params[0] != "TrueNAS-SCALE-Fangtooth" || params[1] != "25.10.2" {
		t.Errorf("update.download params = %#v, want requested train and version", params)
	}
}

func TestREADMENotesAuxiliaryParametersInReadOnlyDirectoryConfig(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(text)
	if !strings.Contains(body, "auxiliary_parameters") {
		t.Fatal("README.md does not mention auxiliary_parameters")
	}
	// The exact-key mask also hides this field in read-only query output,
	// not only in configure/leave previews.
	if !strings.Contains(body, "query_directory_services") ||
		!(strings.Contains(strings.ToLower(body), "read-only") || strings.Contains(body, "read only") || strings.Contains(body, "query output") || strings.Contains(body, "directory config output")) {
		t.Error("README.md must say auxiliary_parameters is also masked in read-only directory config output (query_directory_services)")
	}
}

func TestREADMENotesDownloadUpdatePassesTrainAndVersion(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(text)
	if !strings.Contains(body, "download_update") {
		t.Fatal("README.md does not mention download_update")
	}
	// Must document that train and version are passed through to
	// update.download, not ignored.
	lower := strings.ToLower(body)
	if !strings.Contains(body, "update.download") {
		t.Error("README.md must mention the update.download middleware method for download_update")
	}
	if !strings.Contains(lower, "train") || !strings.Contains(lower, "version") {
		t.Error("README.md must mention download_update train and version")
	}
	// A leftover "ignored" claim next to those fields is the bug being fixed.
	if strings.Contains(body, "train and version were ignored") ||
		strings.Contains(body, "documents them as ignored") {
		t.Error("README.md still describes download_update train/version as ignored")
	}
	if !strings.Contains(lower, "both") {
		t.Error("README.md must say train and version are both passed through or both omitted")
	}
}

func TestREADMENotesBlankTrainAndVersionAreOmitted(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Only the download_update documentation; "whitespace" also appears in an
	// unrelated URL-redaction note.
	var section string
	for _, para := range strings.Split(string(text), "\n\n") {
		if strings.Contains(para, "download_update") && (strings.Contains(para, "train") || strings.Contains(para, "`train`")) {
			section += para + "\n"
		}
	}
	if section == "" {
		t.Fatal("README.md has no download_update paragraph that mentions train")
	}
	lower := strings.ToLower(section)
	hasBlank := strings.Contains(lower, "blank") || strings.Contains(lower, "whitespace")
	hasOmitted := strings.Contains(lower, "omitted") || strings.Contains(lower, "omit")
	hasEmptyParams := strings.Contains(section, "params: []") || strings.Contains(section, "params []") || strings.Contains(section, "`[]`")
	if !hasBlank || !hasOmitted || !hasEmptyParams {
		t.Errorf("README.md download_update docs must say that if train and version are both blank or whitespace, they are treated as omitted and params [] is sent; section=%q", section)
	}
}

func TestDownloadUpdateSchemaDocumentsTrainAndVersion(t *testing.T) {
	r := NewRegistry(nil, nil)
	props := r.tools["download_update"].Definition.InputSchema["properties"].(map[string]interface{})
	for _, field := range []string{"train", "version"} {
		desc, _ := props[field].(map[string]interface{})["description"].(string)
		if desc == "" {
			t.Errorf("%s schema description is empty", field)
			continue
		}
		if strings.Contains(strings.ToLower(desc), "ignored") {
			t.Errorf("%s schema still says it is ignored: %q", field, desc)
		}
	}
}

func TestDismissAndRestoreAlertSharePlanPayload(t *testing.T) {
	alerts := []interface{}{
		map[string]interface{}{"uuid": "alert-1", "level": "WARNING", "dismissed": false, "formatted": "disk"},
	}
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"alert.list":    alerts,
		"alert.dismiss": nil,
		"alert.restore": nil,
	}, nil)
	r := NewRegistry(client, nil)

	for _, tc := range []struct{ tool, method, verb string }{
		{"dismiss_alert", "alert.dismiss", "dismissed"},
		{"restore_alert", "alert.restore", "restored"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			if r.tools[tc.tool].Plan == nil {
				t.Fatalf("%s has no Plan", tc.tool)
			}
			args := map[string]interface{}{"uuid": "alert-1"}
			out, err := r.CallTool(t.Context(), tc.tool, withArgs(args, obj{"dry_run": true}))
			if err != nil {
				t.Fatalf("dry-run: %v\n%s", err, out)
			}
			if !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, `"executed": false`) {
				t.Errorf("plan-backed dry-run shape missing dry_run/executed:\n%s", out)
			}
			if n := len(fake.called(tc.method)); n != 0 {
				t.Errorf("dry-run called %s %d times", tc.method, n)
			}
			out, err = r.CallTool(t.Context(), tc.tool, args)
			if err != nil {
				t.Fatalf("execute: %v\n%s", err, out)
			}
			if !strings.Contains(out, "alert-1") || !strings.Contains(out, tc.verb) {
				t.Errorf("execute result = %s", out)
			}
			params := lastCallParams(t, fake, tc.method)
			if len(params) != 1 || params[0] != "alert-1" {
				t.Errorf("%s params = %#v, want [uuid]", tc.method, params)
			}
		})
	}
}

// alert.list omits alerts whose policy is NEVER, so a uuid can be valid
// even when it is not listed. Main's dismiss_alert / restore_alert still
// sent alert.dismiss / alert.restore with that uuid; refusing it would
// make those alerts impossible to dismiss or restore.
func TestDismissAndRestoreAlertUnlistedUUIDStillCallsMiddleware(t *testing.T) {
	const hidden = "never-policy-alert"
	wantPayload, err := json.Marshal([]interface{}{hidden})
	if err != nil {
		t.Fatal(err)
	}
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"alert.list": []interface{}{
			map[string]interface{}{"uuid": "alert-1", "level": "WARNING", "dismissed": false, "formatted": "disk"},
		},
		"alert.dismiss": nil,
		"alert.restore": nil,
	}, nil)
	r := NewRegistry(client, nil)

	for _, tc := range []struct{ tool, method, verb string }{
		{"dismiss_alert", "alert.dismiss", "dismissed"},
		{"restore_alert", "alert.restore", "restored"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			args := map[string]interface{}{"uuid": hidden}
			out, err := r.CallTool(t.Context(), tc.tool, withArgs(args, obj{"dry_run": true}))
			if err != nil {
				t.Fatalf("dry-run refused an unlisted uuid: %v\n%s", err, out)
			}
			if strings.Contains(strings.ToLower(out), "alert not found") && strings.Contains(out, `"status": "error"`) {
				t.Errorf("dry-run should not refuse an unlisted uuid:\n%s", out)
			}
			if n := len(fake.called(tc.method)); n != 0 {
				t.Errorf("dry-run called %s %d times", tc.method, n)
			}

			out, err = r.CallTool(t.Context(), tc.tool, args)
			if err != nil {
				t.Fatalf("execute refused an unlisted uuid: %v\n%s", err, out)
			}
			if !strings.Contains(out, hidden) || !strings.Contains(out, tc.verb) {
				t.Errorf("execute result = %s", out)
			}
			calls := fake.called(tc.method)
			if len(calls) != 1 {
				t.Fatalf("%s called %d times, want 1", tc.method, len(calls))
			}
			if !bytes.Equal(calls[0].Params, wantPayload) {
				t.Errorf("%s params = %s, want %s (byte-identical to main)", tc.method, calls[0].Params, wantPayload)
			}
		})
	}
}

func TestRefreshDirectoryCacheSharesPlanPayload(t *testing.T) {
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"directoryservices.status":        map[string]interface{}{"type": "ACTIVEDIRECTORY", "status": "HEALTHY", "status_msg": nil},
		"directoryservices.cache_refresh": 7,
		"core.get_jobs":                   []interface{}{},
	}, nil)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second})
	t.Cleanup(mgr.Shutdown)
	r := NewRegistry(client, mgr)
	if r.tools["refresh_directory_cache"].Plan == nil {
		t.Fatal("refresh_directory_cache has no Plan")
	}
	out, err := r.CallTool(t.Context(), "refresh_directory_cache", map[string]interface{}{"dry_run": true})
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, "directoryservices.cache_refresh") {
		t.Errorf("dry-run preview:\n%s", out)
	}
	if n := len(fake.called("directoryservices.cache_refresh")); n != 0 {
		t.Errorf("dry-run called cache_refresh %d times", n)
	}
	out, err = r.CallTool(t.Context(), "refresh_directory_cache", map[string]interface{}{})
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if n := len(fake.called("directoryservices.cache_refresh")); n != 1 {
		t.Errorf("execute called cache_refresh %d times, want 1", n)
	}
	params := lastCallParams(t, fake, "directoryservices.cache_refresh")
	if len(params) != 0 {
		t.Errorf("cache_refresh params = %#v, want empty", params)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["cache_refreshed"] == true {
		t.Errorf("cache_refreshed should not be true until the job finishes: %v", resp)
	}
	if resp["status"] != "submitted" || resp["directory_type"] != "ACTIVEDIRECTORY" || resp["job_id"] != float64(7) {
		t.Errorf("response = %v", resp)
	}
	if _, err := mgr.Get(resp["task_id"].(string)); err != nil {
		t.Fatal(err)
	}
}

func TestCreateScrubScheduleSharesPlanPayload(t *testing.T) {
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"pool.query": []interface{}{
			map[string]interface{}{"id": 2.0, "name": "spare", "status": "ONLINE", "size": 1.0e12},
		},
		"pool.scrub.query":  []interface{}{},
		"pool.scrub.create": map[string]interface{}{"id": 9.0, "pool": 2.0},
	}, nil)
	r := NewRegistry(client, nil)
	if r.tools["create_scrub_schedule"].Plan == nil {
		t.Fatal("create_scrub_schedule has no Plan")
	}
	args := map[string]interface{}{
		"pool":      "spare",
		"threshold": float64(35),
		"schedule":  map[string]interface{}{"minute": "00", "hour": "03", "dom": "*", "dow": "7", "month": "*"},
	}
	out, err := r.CallTool(t.Context(), "create_scrub_schedule", withArgs(args, obj{"dry_run": true}))
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, `"executed": false`) {
		t.Errorf("plan-backed dry-run shape:\n%s", out)
	}
	if n := len(fake.called("pool.scrub.create")); n != 0 {
		t.Errorf("dry-run called pool.scrub.create %d times", n)
	}
	out, err = r.CallTool(t.Context(), "create_scrub_schedule", args)
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	params := lastCallParams(t, fake, "pool.scrub.create")
	if len(params) != 1 {
		t.Fatalf("pool.scrub.create params = %#v, want one payload object", params)
	}
	payload, ok := params[0].(map[string]interface{})
	if !ok {
		t.Fatalf("payload type %T", params[0])
	}
	if payload["pool"] != float64(2) {
		t.Errorf("payload pool = %v, want pool id 2", payload["pool"])
	}
	if payload["threshold"] != float64(35) {
		t.Errorf("payload threshold = %v", payload["threshold"])
	}
	if payload["enabled"] != true {
		t.Errorf("payload enabled = %v", payload["enabled"])
	}
	sched, _ := payload["schedule"].(map[string]interface{})
	if sched["hour"] != "03" || sched["dow"] != "7" {
		t.Errorf("payload schedule = %v", sched)
	}
}

func TestRunScrubSharesPlanPayloadAndExpectedMethod(t *testing.T) {
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"pool.query": []interface{}{
			map[string]interface{}{"id": 1.0, "name": "tank", "status": "ONLINE", "size": 1.0e12},
		},
		"core.get_jobs":    []interface{}{},
		"pool.scrub.scrub": 99,
	}, nil)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second})
	t.Cleanup(mgr.Shutdown)
	r := NewRegistry(client, mgr)
	if r.tools["run_scrub"].Plan == nil {
		t.Fatal("run_scrub has no Plan")
	}
	args := map[string]interface{}{"pool": "tank"}
	out, err := r.CallTool(t.Context(), "run_scrub", withArgs(args, obj{"dry_run": true}))
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, `"executed": false`) {
		t.Errorf("plan-backed dry-run shape:\n%s", out)
	}
	if n := len(fake.called("pool.scrub.scrub")); n != 0 {
		t.Errorf("dry-run called pool.scrub.scrub %d times", n)
	}
	out, err = r.CallTool(t.Context(), "run_scrub", args)
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["scrub_started"] != true || resp["job_id"] != float64(99) {
		t.Errorf("response = %v", resp)
	}
	params := lastCallParams(t, fake, "pool.scrub.scrub")
	if len(params) != 2 || params[0] != "tank" || params[1] != "START" {
		t.Errorf("pool.scrub.scrub params = %#v, want [poolName, START]", params)
	}
	if _, err := mgr.Get(resp["task_id"].(string)); err != nil {
		t.Fatal(err)
	}
}

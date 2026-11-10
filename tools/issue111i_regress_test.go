package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/tasks"
)

// issue111iPlanBackedWriteTools is the last group of Write tools moved
// onto the shared plan in this PR.
var issue111iPlanBackedWriteTools = []string{
	"install_app",
	"create_dataset",
	"create_smb_share",
	"create_nfs_share",
}

func TestIssue111iWriteToolsRegisterAPlan(t *testing.T) {
	r := NewRegistry(nil, nil)
	for _, name := range issue111iPlanBackedWriteTools {
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

func issue111iPools() []interface{} {
	return []interface{}{
		map[string]interface{}{"id": 1.0, "name": "tank", "status": "ONLINE", "size": 1.0e12},
	}
}

func issue111iHostPathDatasets() []interface{} {
	return []interface{}{
		map[string]interface{}{"id": "tank/apps/jellyfin/config", "name": "tank/apps/jellyfin/config", "type": "FILESYSTEM"},
		map[string]interface{}{"id": "tank/apps/jellyfin/data", "name": "tank/apps/jellyfin/data", "type": "FILESYSTEM"},
	}
}

func issue111iInstallRegistry(t *testing.T, extra map[string]interface{}, existingApp bool, datasets []interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	if datasets == nil {
		datasets = issue111iHostPathDatasets()
	}
	results := map[string]interface{}{
		"catalog.get_app_details": hostPathCatalogDetails(),
		"app.available": []interface{}{
			map[string]interface{}{"name": "jellyfin", "train": "community"},
		},
		"app.query":     []interface{}{},
		"pool.query":    issue111iPools(),
		"core.get_jobs": []interface{}{},
		"app.create":    42,
	}
	if existingApp {
		results["app.query"] = []interface{}{
			map[string]interface{}{"name": "media", "id": "media", "state": "STOPPED"},
		}
	}
	for k, v := range extra {
		results[k] = v
	}
	fake := &fakeMiddleware{results: results}
	rows := datasets
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "pool.dataset.query" {
			return datasetQueryLikeMiddleware(t, params, rows), nil, true
		}
		return nil, nil, false
	}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake
}

func issue111iInstallArgs() map[string]interface{} {
	return map[string]interface{}{
		"app_name":    "media",
		"catalog_app": "jellyfin",
		"train":       "community",
		"values":      validHostPathValues(),
	}
}

func issue111iSecretValues() map[string]interface{} {
	values := cloneValues(validHostPathValues())
	values["password"] = "super-secret-pw"
	values["nested"] = map[string]interface{}{
		"api_token": "tok_abc123",
		"db": map[string]interface{}{
			"admin_password": "nested-secret",
		},
	}
	return values
}

func issue111iSecretStrings() []string {
	return []string{"super-secret-pw", "tok_abc123", "nested-secret"}
}

func assertNoInstallSecrets(t *testing.T, label, body string) {
	t.Helper()
	for _, secret := range issue111iSecretStrings() {
		if strings.Contains(body, secret) {
			t.Errorf("%s leaked %q:\n%s", label, secret, body)
		}
	}
}

func assertJSONValueEqual(t *testing.T, got json.RawMessage, want []interface{}) {
	t.Helper()
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	var gotV, wantV interface{}
	if err := json.Unmarshal(got, &gotV); err != nil {
		t.Fatalf("decode got %s: %v", got, err)
	}
	if err := json.Unmarshal(wantRaw, &wantV); err != nil {
		t.Fatalf("decode want %s: %v", wantRaw, err)
	}
	if !reflect.DeepEqual(gotV, wantV) {
		t.Errorf("middleware params = %s\nwant %s", got, wantRaw)
	}
}

func TestInstallAppSharesPlanPayload(t *testing.T) {
	r, fake := issue111iInstallRegistry(t, nil, false, nil)
	if r.tools["install_app"].Plan == nil {
		t.Fatal("install_app has no Plan")
	}
	args := issue111iInstallArgs()
	out, err := r.CallTool(t.Context(), "install_app", withArgs(args, obj{"dry_run": true}))
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, `"executed": false`) {
		t.Errorf("plan-backed dry-run shape:\n%s", out)
	}
	if !strings.Contains(out, "app.create") {
		t.Errorf("preview should name app.create:\n%s", out)
	}
	if n := len(fake.called("app.create")); n != 0 {
		t.Errorf("dry-run called app.create %d times", n)
	}
	out, err = r.CallTool(t.Context(), "install_app", issue111iInstallArgs())
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	calls := fake.called("app.create")
	if len(calls) != 1 {
		t.Fatalf("app.create called %d times, want 1", len(calls))
	}
	want := []interface{}{map[string]interface{}{
		"app_name":    "media",
		"catalog_app": "jellyfin",
		"train":       "community",
		"version":     "latest",
		"values":      validHostPathValues(),
	}}
	assertJSONValueEqual(t, calls[0].Params, want)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["job_id"] != float64(42) {
		t.Errorf("job_id = %v, want 42", resp["job_id"])
	}
	taskID, _ := resp["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task_id in %s", out)
	}
	if _, err := r.taskManager.Get(taskID); err != nil {
		t.Fatalf("Get task: %v", err)
	}
}

func TestInstallAppMasksSecretsInPreviewTaskArgsAndErrors(t *testing.T) {
	r, fake := issue111iInstallRegistry(t, nil, false, nil)
	args := issue111iInstallArgs()
	args["values"] = issue111iSecretValues()

	out, err := r.CallTool(t.Context(), "install_app", withArgs(args, obj{"dry_run": true}))
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	assertNoInstallSecrets(t, "dry-run preview", out)
	if !strings.Contains(out, redact.Marker) {
		t.Errorf("preview should mask secret values with %s:\n%s", redact.Marker, out)
	}

	out, err = r.CallTool(t.Context(), "install_app", cloneValues(args))
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	assertNoInstallSecrets(t, "execute result", out)
	calls := fake.called("app.create")
	if len(calls) != 1 {
		t.Fatalf("app.create called %d times, want 1", len(calls))
	}
	// The middleware payload must still carry the real secrets.
	if !strings.Contains(string(calls[0].Params), "super-secret-pw") ||
		!strings.Contains(string(calls[0].Params), "tok_abc123") ||
		!strings.Contains(string(calls[0].Params), "nested-secret") {
		t.Errorf("app.create payload dropped secrets: %s", calls[0].Params)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	taskID, _ := resp["task_id"].(string)
	task, err := r.taskManager.Get(taskID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	argsJSON, err := json.Marshal(task.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	assertNoInstallSecrets(t, "stored task arguments", string(argsJSON))
	if !strings.Contains(string(argsJSON), redact.Marker) {
		t.Errorf("stored task arguments should contain %s: %s", redact.Marker, argsJSON)
	}

	// A rejected install (existing instance name) must not echo secrets.
	r2, fake2 := issue111iInstallRegistry(t, nil, true, nil)
	rejArgs := issue111iInstallArgs()
	rejArgs["values"] = issue111iSecretValues()
	out, err = r2.CallTool(t.Context(), "install_app", withArgs(rejArgs, obj{"dry_run": true}))
	if err == nil {
		t.Fatalf("expected existing-name dry-run error, got %s", out)
	}
	assertNoInstallSecrets(t, "existing-name dry-run error", err.Error()+"\n"+out)
	out, err = r2.CallTool(t.Context(), "install_app", rejArgs)
	if err == nil {
		t.Fatalf("expected existing-name execute error, got %s", out)
	}
	assertNoInstallSecrets(t, "existing-name execute error", err.Error()+"\n"+out)
	if n := len(fake2.called("app.create")); n != 0 {
		t.Errorf("existing-name still called app.create %d times", n)
	}
}

func TestInstallAppExistingNameIsRefusedOnPreviewAndExecute(t *testing.T) {
	r, fake := issue111iInstallRegistry(t, nil, true, nil)
	args := issue111iInstallArgs()
	out, err := r.CallTool(t.Context(), "install_app", withArgs(args, obj{"dry_run": true}))
	if err == nil {
		t.Fatalf("dry-run should refuse an existing instance, got %s", out)
	}
	if !strings.Contains(err.Error(), "already exists") && !strings.Contains(out, "already exists") {
		t.Errorf("existing-name dry-run error = %v\n%s", err, out)
	}
	out, err = r.CallTool(t.Context(), "install_app", args)
	if err == nil {
		t.Fatalf("execute should refuse an existing instance, got %s", out)
	}
	if !strings.Contains(err.Error(), "already exists") && !strings.Contains(out, "already exists") {
		t.Errorf("existing-name execute error = %v\n%s", err, out)
	}
	if n := len(fake.called("app.create")); n != 0 {
		t.Errorf("existing name still called app.create %d times", n)
	}
}

func TestInstallAppMissingHostPathIsRefusedOnPreviewAndExecute(t *testing.T) {
	r, fake := issue111iInstallRegistry(t, nil, false, []interface{}{})
	args := issue111iInstallArgs()
	out, err := r.CallTool(t.Context(), "install_app", withArgs(args, obj{"dry_run": true}))
	if err == nil {
		t.Fatalf("dry-run should refuse missing host-path datasets, got %s", out)
	}
	msg := err.Error() + "\n" + out
	if !strings.Contains(msg, "tank/apps/jellyfin") && !strings.Contains(msg, "must exist") {
		t.Errorf("missing-dataset dry-run error = %v\n%s", err, out)
	}
	out, err = r.CallTool(t.Context(), "install_app", args)
	if err == nil {
		t.Fatalf("execute should refuse missing host-path datasets, got %s", out)
	}
	msg = err.Error() + "\n" + out
	if !strings.Contains(msg, "tank/apps/jellyfin") && !strings.Contains(msg, "must exist") {
		t.Errorf("missing-dataset execute error = %v\n%s", err, out)
	}
	if n := len(fake.called("app.create")); n != 0 {
		t.Errorf("missing datasets still called app.create %d times", n)
	}
}

func TestInstallAppUnknownVersionIsRefusedOnPreviewAndExecute(t *testing.T) {
	r, fake := issue111iInstallRegistry(t, nil, false, nil)
	args := issue111iInstallArgs()
	args["version"] = "9.9.9"
	out, err := r.CallTool(t.Context(), "install_app", withArgs(args, obj{"dry_run": true}))
	if err == nil {
		t.Fatalf("dry-run should refuse an unknown catalog version, got %s", out)
	}
	msg := err.Error() + "\n" + out
	if !strings.Contains(msg, "9.9.9") && !strings.Contains(msg, "schema") {
		t.Errorf("unknown-version dry-run error = %v\n%s", err, out)
	}
	out, err = r.CallTool(t.Context(), "install_app", args)
	if err == nil {
		t.Fatalf("execute should refuse an unknown catalog version, got %s", out)
	}
	msg = err.Error() + "\n" + out
	if !strings.Contains(msg, "9.9.9") && !strings.Contains(msg, "schema") {
		t.Errorf("unknown-version execute error = %v\n%s", err, out)
	}
	if n := len(fake.called("app.create")); n != 0 {
		t.Errorf("unknown version still called app.create %d times", n)
	}
}

func issue111iDatasetRegistry(t *testing.T, extra map[string]interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	results := map[string]interface{}{
		"pool.query": issue111iPools(),
		"pool.dataset.create": map[string]interface{}{
			"id": "tank/media", "name": "tank/media", "type": "FILESYSTEM",
			"pool": "tank", "mountpoint": "/mnt/tank/media", "encrypted": false,
		},
	}
	for k, v := range extra {
		results[k] = v
	}
	fake := &fakeMiddleware{results: results}
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "pool.dataset.query" {
			return datasetQueryLikeMiddleware(t, params, nil), nil, true
		}
		if method == "pool.query" {
			pools, _ := results["pool.query"].([]interface{})
			name := poolQueryNameFilter(params)
			if name == "" {
				return pools, nil, true
			}
			var matched []interface{}
			for _, p := range pools {
				m, _ := p.(map[string]interface{})
				if m["name"] == name {
					matched = append(matched, p)
				}
			}
			return matched, nil, true
		}
		return nil, nil, false
	}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

func TestCreateDatasetSharesPlanPayload(t *testing.T) {
	r, fake := issue111iDatasetRegistry(t, nil)
	if r.tools["create_dataset"].Plan == nil {
		t.Fatal("create_dataset has no Plan")
	}
	args := map[string]interface{}{"name": "tank/media"}
	out, err := r.CallTool(t.Context(), "create_dataset", withArgs(args, obj{"dry_run": true}))
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, `"executed": false`) {
		t.Errorf("plan-backed dry-run shape:\n%s", out)
	}
	if n := len(fake.called("pool.dataset.create")); n != 0 {
		t.Errorf("dry-run called pool.dataset.create %d times", n)
	}
	out, err = r.CallTool(t.Context(), "create_dataset", args)
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	calls := fake.called("pool.dataset.create")
	if len(calls) != 1 {
		t.Fatalf("pool.dataset.create called %d times, want 1", len(calls))
	}
	want := []interface{}{map[string]interface{}{
		"name":             "tank/media",
		"type":             "FILESYSTEM",
		"create_ancestors": true,
	}}
	assertJSONValueEqual(t, calls[0].Params, want)
}

func TestCreateDatasetPreflightOnExecute(t *testing.T) {
	r, fake := issue111iDatasetRegistry(t, map[string]interface{}{
		"pool.query": []interface{}{},
	})
	args := map[string]interface{}{"name": "missing/ds"}
	out, err := r.CallTool(t.Context(), "create_dataset", withArgs(args, obj{"dry_run": true}))
	if err == nil {
		t.Fatalf("dry-run should refuse a missing pool, got %s", out)
	}
	out, err = r.CallTool(t.Context(), "create_dataset", args)
	if err == nil {
		t.Fatalf("execute should refuse a missing pool, got %s", out)
	}
	if n := len(fake.called("pool.dataset.create")); n != 0 {
		t.Errorf("missing pool still called pool.dataset.create %d times", n)
	}

	existing := []interface{}{
		map[string]interface{}{"id": "tank/media", "name": "tank/media", "type": "FILESYSTEM"},
	}
	r2, fake2 := issue111iDatasetRegistry(t, nil)
	// Swap the query handle so tank/media already exists.
	fake2.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "pool.dataset.query" {
			return datasetQueryLikeMiddleware(t, params, existing), nil, true
		}
		if method == "pool.query" {
			return issue111iPools(), nil, true
		}
		return nil, nil, false
	}
	args = map[string]interface{}{"name": "tank/media"}
	out, err = r2.CallTool(t.Context(), "create_dataset", args)
	if err == nil {
		t.Fatalf("execute should refuse an existing dataset, got %s", out)
	}
	if n := len(fake2.called("pool.dataset.create")); n != 0 {
		t.Errorf("existing dataset still called pool.dataset.create %d times", n)
	}
}

func issue111iShareRegistry(t *testing.T, extra map[string]interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	results := map[string]interface{}{
		"system.info":              map[string]interface{}{"hostname": "truenas.local"},
		"directoryservices.status": map[string]interface{}{"type": "ACTIVEDIRECTORY", "status": "HEALTHY", "status_msg": nil},
		"sharing.smb.create": map[string]interface{}{
			"id": 1.0, "name": "share", "path": "/mnt/tank/share", "enabled": true,
		},
		"sharing.nfs.create": map[string]interface{}{
			"id": 2.0, "path": "/mnt/tank/share", "enabled": true,
		},
	}
	for k, v := range extra {
		results[k] = v
	}
	client, fake := newFakeClientWith(t, results, nil)
	return NewRegistry(client, nil), fake
}

func TestCreateSMBShareSharesPlanPayload(t *testing.T) {
	r, fake := issue111iShareRegistry(t, nil)
	if r.tools["create_smb_share"].Plan == nil {
		t.Fatal("create_smb_share has no Plan")
	}
	args := map[string]interface{}{"name": "share", "path": "/mnt/tank/share"}
	out, err := r.CallTool(t.Context(), "create_smb_share", withArgs(args, obj{"dry_run": true}))
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, `"executed": false`) {
		t.Errorf("plan-backed dry-run shape:\n%s", out)
	}
	if !strings.Contains(out, "visible and accessible") && !strings.Contains(out, "hostsallow") {
		t.Errorf("preview should keep the SMB security warnings:\n%s", out)
	}
	if n := len(fake.called("sharing.smb.create")); n != 0 {
		t.Errorf("dry-run called sharing.smb.create %d times", n)
	}
	out, err = r.CallTool(t.Context(), "create_smb_share", args)
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	calls := fake.called("sharing.smb.create")
	if len(calls) != 1 {
		t.Fatalf("sharing.smb.create called %d times, want 1", len(calls))
	}
	want := []interface{}{map[string]interface{}{
		"name":    "share",
		"path":    "/mnt/tank/share",
		"enabled": true,
	}}
	assertJSONValueEqual(t, calls[0].Params, want)
}

func TestCreateNFSShareSharesPlanPayload(t *testing.T) {
	r, fake := issue111iShareRegistry(t, nil)
	if r.tools["create_nfs_share"].Plan == nil {
		t.Fatal("create_nfs_share has no Plan")
	}
	args := map[string]interface{}{"path": "/mnt/tank/share"}
	out, err := r.CallTool(t.Context(), "create_nfs_share", withArgs(args, obj{"dry_run": true}))
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"dry_run": true`) || !strings.Contains(out, `"executed": false`) {
		t.Errorf("plan-backed dry-run shape:\n%s", out)
	}
	if !strings.Contains(out, "any network/host") && !strings.Contains(out, "read-write") {
		t.Errorf("preview should keep the NFS security warnings:\n%s", out)
	}
	if n := len(fake.called("sharing.nfs.create")); n != 0 {
		t.Errorf("dry-run called sharing.nfs.create %d times", n)
	}
	out, err = r.CallTool(t.Context(), "create_nfs_share", args)
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	calls := fake.called("sharing.nfs.create")
	if len(calls) != 1 {
		t.Fatalf("sharing.nfs.create called %d times, want 1", len(calls))
	}
	want := []interface{}{map[string]interface{}{
		"path":    "/mnt/tank/share",
		"enabled": true,
	}}
	assertJSONValueEqual(t, calls[0].Params, want)
}

func TestRefreshDirectoryCacheReportsSubmittedStatus(t *testing.T) {
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"directoryservices.status":        map[string]interface{}{"type": "ACTIVEDIRECTORY", "status": "HEALTHY", "status_msg": nil},
		"directoryservices.cache_refresh": 7,
		"core.get_jobs":                   []interface{}{},
	}, nil)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second})
	t.Cleanup(mgr.Shutdown)
	r := NewRegistry(client, mgr)
	out, err := r.CallTool(t.Context(), "refresh_directory_cache", map[string]interface{}{})
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	if n := len(fake.called("directoryservices.cache_refresh")); n != 1 {
		t.Errorf("cache_refresh called %d times, want 1", n)
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
	if resp["status"] != "submitted" {
		t.Errorf("status = %v, want submitted", resp["status"])
	}
	if resp["job_id"] != float64(7) {
		t.Errorf("job_id = %v, want 7", resp["job_id"])
	}
	taskID, _ := resp["task_id"].(string)
	if taskID == "" {
		t.Fatalf("missing task_id in %s", out)
	}
	if _, err := r.taskManager.Get(taskID); err != nil {
		t.Fatalf("Get task: %v", err)
	}
}

func TestIssue111iREADMEDocumentsPlanMigration(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	needles := []string{
		"install_app",
		"create_dataset",
		"create_smb_share",
		"create_nfs_share",
		"app.create",
	}
	// The behaviour note must sit with the other plan-backed write-tool
	// paragraphs (the ones that also mention start_app / shared plan).
	idx := strings.Index(body, "`start_app` builds one plan")
	if idx < 0 {
		t.Fatal("could not find the plan-backed behaviour section in README.md")
	}
	section := body[idx:]
	if end := strings.Index(section, "\n## "); end > 0 {
		section = section[:end]
	}
	for _, n := range needles {
		if !strings.Contains(section, n) {
			t.Errorf("README plan-backed section should mention %q", n)
		}
	}
	if !strings.Contains(section, "password") && !strings.Contains(section, "secret") && !strings.Contains(section, "values") {
		t.Error("README should say install_app values secrets are masked in previews")
	}
	if !strings.Contains(section, "host-path") && !strings.Contains(section, "host path") && !strings.Contains(section, "already exists") {
		t.Error("README should document that install_app dry-run now matches the real call on host-path and existing-name guards")
	}
	if !strings.Contains(section, "submitted") || !strings.Contains(section, "refresh_directory_cache") {
		t.Error("README should say refresh_directory_cache reports submitted status instead of cache_refreshed true")
	}
}

package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/tasks"
)

const issue156Secret = "issue156-super-secret-password"

// issue156Registry is a 25.10-shaped fake covering the ten destructive
// tools plus download_update, so elicitation and dry-run prompts can be
// checked against the same fixtures.
func issue156Registry(t *testing.T, appConfig map[string]interface{}, catalog map[string]interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	if catalog == nil {
		catalog = hostPathCatalogDetails()
	}
	if appConfig == nil {
		appConfig = map[string]interface{}{}
	}
	plex := appEntry("plex", "RUNNING")
	plex["version"] = "1.0.0"
	plex["human_version"] = "1.0.0"
	plex["latest_version"] = "1.2.0"
	plex["upgrade_available"] = true

	existing := map[string]bool{
		"tank/apps/jellyfin/config": true,
		"tank/apps/jellyfin/data":   true,
		"tank/apps/jellyfin/media":  true,
	}
	installed := []interface{}{plex, issue157InstalledApp(false)}
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"catalog.get_app_details": catalog,
			"pool.query": []interface{}{
				map[string]interface{}{"id": 1.0, "name": "tank", "size": 1.0e12, "status": "ONLINE"},
			},
			"app.query":           installed,
			"app.config":          appConfig,
			"app.upgrade_summary": upgradeSummaryFixture("notes"),
			"app.create":          101,
			"app.update":          102,
			"boot.environment.query": []interface{}{
				map[string]interface{}{"id": "25.04.1", "active": false, "activated": false, "keep": false},
			},
			"pool.scrub.query": []interface{}{
				map[string]interface{}{"id": 1.0, "pool_name": "tank", "enabled": true, "threshold": 35.0,
					"schedule": map[string]interface{}{"minute": "00", "hour": "00", "dom": "*", "dow": "7", "month": "*"}},
			},
			"directoryservices.status": map[string]interface{}{"type": "ACTIVEDIRECTORY", "status": "HEALTHY", "status_msg": nil},
			"update.status":            updateStatusAvailable("25.10.1"),
			"core.get_jobs":            []interface{}{},
		},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method == "app.query" {
				return filterAppQuery(params, installed), nil, true
			}
			if method != "pool.dataset.query" {
				return nil, nil, false
			}
			var args []interface{}
			if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
				return []interface{}{}, nil, true
			}
			filters, _ := args[0].([]interface{})
			name := ""
			if len(filters) > 0 {
				if f, ok := filters[0].([]interface{}); ok && len(f) >= 3 {
					if s, ok := f[2].(string); ok {
						name = s
					}
				}
			}
			if existing[name] {
				return []interface{}{map[string]interface{}{"name": name}}, nil, true
			}
			return []interface{}{}, nil, true
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake
}

func issue156Elicit(t *testing.T, r *Registry, name string, args map[string]interface{}) string {
	t.Helper()
	out, prompt, err := elicitReply(t, r, name, args, formElicit())
	if err != nil || out != "" || prompt == nil || prompt.Message == "" {
		t.Fatalf("%s elicit: out=%q prompt=%+v err=%v; want a prompt and no result", name, out, prompt, err)
	}
	return prompt.Message
}

func issue156MustContain(t *testing.T, msg, name string, want []string) {
	t.Helper()
	if !strings.Contains(msg, "Facts:") {
		t.Errorf("%s elicitation is missing a Facts section:\n%s", name, msg)
	}
	for _, s := range want {
		if !strings.Contains(msg, s) {
			t.Errorf("%s elicitation is missing %q:\n%s", name, s, msg)
		}
	}
}

func issue156MustNotContain(t *testing.T, msg, name string, refuse []string) {
	t.Helper()
	for _, s := range refuse {
		if strings.Contains(msg, s) {
			t.Errorf("%s elicitation leaked %q:\n%s", name, s, msg)
		}
	}
}

// TestIssue156ElicitationShowsWhatIsApproved is the #156 regression: the
// elicitation message must name the target and the facts a human needs to
// approve, and must not be derived from a generic or fictional first plan
// step.
func TestIssue156ElicitationShowsWhatIsApproved(t *testing.T) {
	r, _ := issue156Registry(t, issue157IxVolumeConfig(), nil)

	cases := []struct {
		name   string
		args   map[string]interface{}
		want   []string
		refuse []string
	}{
		{
			name:   "apply_update",
			args:   map[string]interface{}{"reboot": true},
			want:   []string{"apply_update", "25.10.0", "25.10.1", "reboot"},
			refuse: []string{"Stop critical system services", "Verify update installation"},
		},
		{
			name: "system_reboot",
			args: map[string]interface{}{"delay": 60.0},
			want: []string{"system_reboot", "60"},
		},
		{
			name: "delete_boot_environment",
			args: map[string]interface{}{"id": "25.04.1"},
			want: []string{"delete_boot_environment", "25.04.1"},
		},
		{
			name: "delete_scrub_schedule",
			args: map[string]interface{}{"id": 1.0},
			want: []string{"delete_scrub_schedule", "tank"},
		},
		{
			name: "stop_app",
			args: map[string]interface{}{"app_name": "plex"},
			want: []string{"stop_app", "plex", "RUNNING"},
		},
		{
			name: "delete_app",
			args: map[string]interface{}{"app_name": "plex", "remove_images": true},
			want: []string{"delete_app", "plex", "RUNNING"},
		},
		{
			name:   "upgrade_app",
			args:   map[string]interface{}{"app_name": "plex", "version": "1.2.0", "snapshot_hostpaths": true},
			want:   []string{"upgrade_app", "plex", "1.0.0", "1.2.0", "snapshot_hostpaths"},
			refuse: []string{"Stop application containers"},
		},
		{
			name: "configure_directory_service",
			args: map[string]interface{}{
				"service_type": "ACTIVEDIRECTORY", "enable": true, "force": true,
				"credential":    map[string]interface{}{"credential_type": "KERBEROS_USER", "username": "admin", "password": issue156Secret},
				"configuration": map[string]interface{}{"domain": "corp.example.com", "hostname": "nas"},
			},
			want:   []string{"configure_directory_service", "ACTIVEDIRECTORY", "corp.example.com", "enable", "force"},
			refuse: []string{issue156Secret},
		},
		{
			name: "leave_directory_service",
			args: map[string]interface{}{
				"credential": map[string]interface{}{"credential_type": "KERBEROS_USER", "username": "admin", "password": issue156Secret},
			},
			want:   []string{"leave_directory_service", "ACTIVEDIRECTORY"},
			refuse: []string{issue156Secret},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := issue156Elicit(t, r, tc.name, tc.args)
			issue156MustContain(t, msg, tc.name, tc.want)
			issue156MustNotContain(t, msg, tc.name, tc.refuse)
		})
	}

	t.Run("update_app", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		setStoragePath(values, "config", "/mnt/tank/apps/jellyfin/media")
		values["notes"] = "new-notes-value"
		// notes is not in the default schema; change storage path only here.
		// A dedicated schema case covers changed keys and secrets below.
		r, _ := issue156Registry(t, issue157IxVolumeConfig(), nil)
		msg := issue156Elicit(t, r, "update_app", map[string]interface{}{
			"app_name":             "jellyfin",
			"values":               values,
			"allow_storage_change": true,
		})
		issue156MustContain(t, msg, "update_app", []string{"update_app", "jellyfin", "storage", "host_path", "/mnt/tank/apps/jellyfin/media"})
	})
}

func issue156NotesSchema() map[string]interface{} {
	schema := hostPathTestSchema()
	groups, _ := schema["groups"].([]interface{})
	schema["groups"] = append(groups, map[string]interface{}{"name": "App Config"})
	questions, _ := schema["questions"].([]interface{})
	schema["questions"] = append([]interface{}{
		map[string]interface{}{
			"variable": "notes",
			"group":    "App Config",
			"schema":   map[string]interface{}{"type": "string"},
		},
		map[string]interface{}{
			"variable": "password",
			"group":    "App Config",
			"schema":   map[string]interface{}{"type": "string"},
		},
	}, questions...)
	return map[string]interface{}{
		"name":           "jellyfin",
		"latest_version": "1.2.0",
		"versions": map[string]interface{}{
			"1.2.0": map[string]interface{}{"schema": schema},
		},
	}
}

// TestIssue156RedactsSecretsAndTruncatesFacts checks that newly shown
// confirmation facts go through existing redaction and are cut at 2 KiB
// with a "more changes not shown" notice.
func TestIssue156RedactsSecretsAndTruncatesFacts(t *testing.T) {
	current := issue157IxVolumeConfig()
	current["notes"] = "old-notes"
	r, _ := issue156Registry(t, current, issue156NotesSchema())

	values := map[string]interface{}{
		"password": issue156Secret,
		"notes":    strings.Repeat("N", 4000),
	}
	msg := issue156Elicit(t, r, "update_app", map[string]interface{}{
		"app_name": "jellyfin",
		"values":   values,
	})
	issue156MustNotContain(t, msg, "update_app", []string{issue156Secret})
	if !strings.Contains(msg, redact.Marker) && !strings.Contains(msg, "password") {
		// Either the key is shown next to a redacted value, or the value
		// must at least not leak. Prefer seeing the marker.
		t.Errorf("update_app elicitation should redact the password value:\n%s", msg)
	}
	if strings.Contains(msg, issue156Secret) {
		t.Errorf("password leaked")
	}
	if !strings.Contains(msg, "notes") {
		t.Errorf("update_app elicitation should name the changed notes key:\n%s", msg)
	}
	if !strings.Contains(msg, "more changes not shown") {
		t.Errorf("oversized change set should be cut with \"more changes not shown\":\n%s", msg)
	}
	if utf8.RuneCountInString(msg) < 1 {
		t.Fatal("empty message")
	}
}

// TestIssue156ApplyAndDownloadPlansOmitFictionalSteps is the other half of
// #156: apply_update and download_update previews must list the middleware
// call and facts read from the system, not invented connect/stop/verify
// steps.
func TestIssue156ApplyAndDownloadPlansOmitFictionalSteps(t *testing.T) {
	r, _ := issue156Registry(t, nil, nil)

	apply, err := r.CallTool(t.Context(), "apply_update", map[string]interface{}{"dry_run": true, "reboot": true})
	if err != nil {
		t.Fatalf("apply_update dry-run: %v", err)
	}
	for _, s := range []string{"Stop critical system services", "Verify update installation"} {
		if strings.Contains(apply, s) {
			t.Errorf("apply_update dry-run still has fictional step %q:\n%s", s, apply)
		}
	}
	if !strings.Contains(apply, "update.run") && !strings.Contains(apply, "25.10.1") {
		t.Errorf("apply_update dry-run should name the middleware call or the new version:\n%s", apply)
	}

	dl, err := r.CallTool(t.Context(), "download_update", map[string]interface{}{"dry_run": true})
	if err != nil {
		t.Fatalf("download_update dry-run: %v", err)
	}
	for _, s := range []string{"Connect to TrueNAS update server", "update.truenas.com", "Verify update package integrity"} {
		if strings.Contains(dl, s) {
			t.Errorf("download_update dry-run still has fictional step %q:\n%s", s, dl)
		}
	}
	if !strings.Contains(dl, "update.download") && !strings.Contains(dl, "25.10.0") {
		t.Errorf("download_update dry-run should name the middleware call or the current version:\n%s", dl)
	}
}

func filterAppQuery(params json.RawMessage, all []interface{}) []interface{} {
	var args []interface{}
	if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
		return all
	}
	filters, _ := args[0].([]interface{})
	want := ""
	for _, f := range filters {
		t, ok := f.([]interface{})
		if !ok || len(t) < 3 {
			continue
		}
		field, _ := t[0].(string)
		op, _ := t[1].(string)
		if field == "name" && op == "=" {
			want, _ = t[2].(string)
		}
	}
	if want == "" {
		return all
	}
	var out []interface{}
	for _, item := range all {
		m, _ := item.(map[string]interface{})
		if name, _ := m["name"].(string); name == want {
			out = append(out, item)
		}
	}
	return out
}

func TestIssue156DestructivePlansRequireConfirmSummary(t *testing.T) {
	r, _ := issue156Registry(t, issue157IxVolumeConfig(), nil)
	var missing []string
	for name, tool := range r.tools {
		if safetyOf(name).Access != Destructive {
			continue
		}
		if tool.Plan == nil {
			t.Errorf("%s is destructive but has no Plan", name)
			continue
		}
		args := toolArgsForSafetyFlagTest(name)
		if name == "update_app" {
			args["app_name"] = "jellyfin"
			args["allow_storage_change"] = true
		}
		if name == "upgrade_app" {
			args["version"] = "1.2.0"
		}
		plan, err := tool.Plan(t.Context(), r.client, args)
		if err != nil {
			t.Errorf("%s Plan: %v", name, err)
			continue
		}
		if plan.ConfirmSummary.empty() {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("destructive plans missing ConfirmSummary: %s", strings.Join(missing, ", "))
	}
}

func TestIssue156ConfirmSummaryIsNotInPlanHash(t *testing.T) {
	a := &writePlan{
		Method:     "update.run",
		CallParams: []interface{}{map[string]interface{}{"reboot": true}},
		Bind:       map[string]interface{}{"new_version": "25.10.1"},
		ConfirmSummary: newConfirmSummary("Apply a system update",
			confirmFact("current version", "25.10.0"),
		),
	}
	b := &writePlan{
		Method:         a.Method,
		CallParams:     a.CallParams,
		Bind:           a.Bind,
		ConfirmSummary: newConfirmSummary("Different headline", "other fact"),
	}
	ha, err := a.planHash()
	if err != nil {
		t.Fatal(err)
	}
	hb, err := b.planHash()
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatal("plan hash changed when only ConfirmSummary changed")
	}
	hc, err := confirmationPlanHashBound(a.Method, a.CallParams, a.Bind)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hc {
		t.Fatal("plan hash is not Method, CallParams and Bind only")
	}
}

func TestIssue156MissingConfirmSummaryFailsClosed(t *testing.T) {
	err := requireConfirmSummary("apply_update", nil)
	if err == nil || !strings.Contains(err.Error(), "missing confirmation summary") || !strings.Contains(err.Error(), "nothing was executed") {
		t.Fatalf("nil plan: %v", err)
	}
	err = requireConfirmSummary("apply_update", &writePlan{Tool: "apply_update"})
	if err == nil || !strings.Contains(err.Error(), "missing confirmation summary") {
		t.Fatalf("empty summary: %v", err)
	}
	if err := requireConfirmSummary("apply_update", &writePlan{ConfirmSummary: newConfirmSummary("Apply a system update")}); err != nil {
		t.Fatalf("valid summary refused: %v", err)
	}
}

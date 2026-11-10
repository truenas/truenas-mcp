package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// Destructive tools that share a plan between preview and execute must
// register a Plan. Deleting the Plan field would silently go back to
// unbound confirmation tokens.
func TestPlanBackedDestructiveToolsRegisterAPlan(t *testing.T) {
	r := NewRegistry(nil, nil)
	for _, name := range []string{
		"stop_app",
		"delete_boot_environment",
		"delete_app",
		"delete_scrub_schedule",
		"upgrade_app",
		"update_app",
		"system_reboot",
		"apply_update",
		"configure_directory_service",
		"leave_directory_service",
	} {
		if safetyOf(name).Access != Destructive {
			t.Errorf("%s is listed as plan-backed but is not classified destructive", name)
			continue
		}
		tool, ok := r.tools[name]
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		if tool.Plan == nil {
			t.Errorf("%s must register a Plan so confirmation tokens stay bound to the planned operation", name)
		}
		if tool.Execute == nil {
			t.Errorf("%s must register Execute so a confirmed call runs the verified plan and does not fall back to the handler", name)
		}
	}
}

// After a token is issued, execute hashes a freshly built plan and then
// runs that same plan. Rebuilding again in the handler would issue a
// second query and could execute params that were never compared to the
// confirmed hash.
func TestStopAppExecuteUsesHashedPlanWithoutRebuild(t *testing.T) {
	r, fake := stopAppRegistry(t, "RUNNING")
	tok := previewToken(t, r, "stop_app", map[string]interface{}{"app_name": "plex"})
	queriesAfterPreview := len(fake.called("app.query"))
	out, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{
		"app_name": "plex", "confirmation_token": tok,
	})
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, out)
	}
	extra := len(fake.called("app.query")) - queriesAfterPreview
	if extra != 1 {
		t.Errorf("execute issued %d more app.query calls, want 1 (hash the verified plan and run it); a second query means the handler rebuilt the plan", extra)
	}
	if n := len(fake.called("app.stop")); n != 1 {
		t.Errorf("execute called app.stop %d times, want 1", n)
	}
}

// A ParamNames list shorter than CallParams fails the preview (it must
// not drop params or fall back to param_N).
func TestWritePlanShortParamNamesDoNotDropCallParams(t *testing.T) {
	p := &writePlan{
		ParamNames: []string{"a"},
		CallParams: []interface{}{"x", "y"},
	}
	details, err := p.previewDetails()
	if err == nil {
		raw, _ := json.Marshal(details)
		t.Fatalf("mismatched ParamNames must fail the preview, got %s", raw)
	}
}

// A plan error on the destructive call path (with or without a token)
// must say the tool was not executed.
func TestDestructivePlanErrorSaysNotExecuted(t *testing.T) {
	fake := &fakeMiddleware{
		handle: func(method string, _ json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method == "app.query" {
				return nil, map[string]interface{}{"message": "middleware down", "code": float64(1)}, true
			}
			return nil, nil, false
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	r := NewRegistry(client, mgr)

	out, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{"app_name": "plex"})
	if err == nil {
		t.Fatalf("expected a plan error, got %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "was NOT executed") || !strings.Contains(msg, "nothing was executed") {
		t.Errorf("plan error should say the tool was NOT executed, got %q", msg)
	}
	if !strings.Contains(msg, "failed to query") && !strings.Contains(msg, "middleware down") {
		t.Errorf("plan error should still name the underlying failure, got %q", msg)
	}
}

// The plan-mismatch reason must not already end in "nothing was executed",
// because the confirmation rejection wrapper adds that phrase.
func TestPlanMismatchMessageIsNotDoubled(t *testing.T) {
	r, _ := stopAppRegistry(t, "RUNNING")
	tool := r.tools["stop_app"]
	orig := tool.Plan
	var builds int
	tool.Plan = func(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
		builds++
		p, err := orig(ctx, client, args)
		if err != nil {
			return nil, err
		}
		if builds > 1 {
			cp := append([]interface{}{}, p.CallParams...)
			cp[0] = "other"
			p.CallParams = cp
		}
		return p, nil
	}
	r.tools["stop_app"] = tool

	tok := previewToken(t, r, "stop_app", map[string]interface{}{"app_name": "plex"})
	_, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{
		"app_name": "plex", "confirmation_token": tok,
	})
	if err == nil {
		t.Fatal("expected a plan-mismatch error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "different planned operation") {
		t.Errorf("error should name the plan change, got %q", msg)
	}
	if n := strings.Count(msg, "nothing was executed"); n != 1 {
		t.Errorf("want \"nothing was executed\" once, got %d in %q", n, msg)
	}
}

// A blocked dry run uses the same structured status:"blocked" preview
// for start_app and delete_boot_environment (an error-shaped result,
// no token).
func TestBlockedDryRunIsStructuredBlockedPreview(t *testing.T) {
	start, _ := startAppRegistry(t, "RUNNING")
	startOut, startErr := start.CallTool(t.Context(), "start_app", map[string]interface{}{
		"app_name": "plex", "dry_run": true,
	})

	bootEnvs := []interface{}{
		map[string]interface{}{"id": "running", "active": true, "activated": false, "keep": false},
	}
	client, fake := newFakeClientWith(t, map[string]interface{}{"boot.environment.query": bootEnvs}, nil)
	boot := NewRegistry(client, nil)
	bootOut, bootErr := boot.CallTool(t.Context(), "delete_boot_environment", map[string]interface{}{
		"id": "running", "dry_run": true,
	})

	for _, tc := range []struct {
		name string
		out  string
		err  error
		want string
	}{
		{"start_app", startOut, startErr, "already RUNNING"},
		{"delete_boot_environment", bootOut, bootErr, "currently running"},
	} {
		if tc.err == nil {
			t.Errorf("%s blocked dry run returned no error:\n%s", tc.name, tc.out)
			continue
		}
		var reply *mcp.ReplyError
		if !errors.As(tc.err, &reply) {
			t.Errorf("%s: want a ReplyError, got %T %v", tc.name, tc.err, tc.err)
			continue
		}
		if !strings.Contains(reply.Reply, `"status": "blocked"`) {
			t.Errorf("%s blocked dry run lacks status blocked: %s", tc.name, reply.Reply)
		}
		if strings.Contains(reply.Reply, "confirmation_token") {
			t.Errorf("%s blocked dry run issued a token: %s", tc.name, reply.Reply)
		}
		if !strings.Contains(reply.Reply, tc.want) && !strings.Contains(reply.Message, tc.want) {
			t.Errorf("%s blocked dry run should mention %q: %s / %s", tc.name, tc.want, reply.Message, reply.Reply)
		}
	}
	if calls := fake.called("boot.environment.destroy"); len(calls) != 0 {
		t.Fatalf("a blocked preview sent boot.environment.destroy: %v", calls)
	}
}

func deleteAppRegistry(t *testing.T) (*Registry, *fakeMiddleware) {
	t.Helper()
	app := appEntry("plex", "RUNNING")
	app["version"] = "1.2.3"
	fake := &fakeMiddleware{
		results: map[string]interface{}{"app.delete": 99},
		handle: func(method string, _ json.RawMessage) (interface{}, map[string]interface{}, bool) {
			switch method {
			case "app.query":
				return []interface{}{app}, nil, true
			case "core.get_jobs":
				return []interface{}{
					map[string]interface{}{
						"id":           float64(99),
						"method":       "app.delete",
						"state":        "RUNNING",
						"time_started": map[string]interface{}{"$date": float64(1767225600000)},
					},
				}, nil, true
			}
			return nil, nil, false
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake
}

func TestDeleteAppPlanMatchesExecutedPayload(t *testing.T) {
	r, fake := deleteAppRegistry(t)
	out, err := r.CallTool(t.Context(), "delete_app", map[string]interface{}{
		"app_name": "plex", "remove_images": true, "dry_run": true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := len(fake.called("app.delete")); n != 0 {
		t.Errorf("dry run called app.delete %d times", n)
	}
	details, actions := startAppPreview(t, out)
	if len(actions) != 1 {
		t.Fatalf("preview should list only the app.delete call, got %d actions: %s", len(actions), out)
	}
	if target, _ := actions[0]["target"].(string); target != "app.delete" {
		t.Errorf("planned target %q, want app.delete", target)
	}
	wantDetails, _ := json.Marshal(map[string]interface{}{
		"app_name": "plex",
		"options":  map[string]interface{}{"remove_images": true},
	})
	if !jsonBytesEqual(t, details, wantDetails) {
		t.Errorf("preview details %s, want %s", details, wantDetails)
	}

	r2, fake2 := deleteAppRegistry(t)
	got, err := r2.CallTool(t.Context(), "delete_app", approve(t, r2, "delete_app", map[string]interface{}{
		"app_name": "plex", "remove_images": true,
	}))
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, got)
	}
	calls := fake2.called("app.delete")
	if len(calls) != 1 {
		t.Fatalf("execute called app.delete %d times, want 1", len(calls))
	}
	wantParams, _ := json.Marshal([]interface{}{"plex", map[string]interface{}{"remove_images": true}})
	if !jsonBytesEqual(t, calls[0].Params, wantParams) {
		t.Errorf("executed app.delete params %s, want %s", calls[0].Params, wantParams)
	}
	if !strings.Contains(got, "task_id") {
		t.Errorf("execute should return a task_id, got %s", got)
	}
}

func TestDeleteScrubSchedulePlanMatchesExecutedPayload(t *testing.T) {
	schedules := []interface{}{
		map[string]interface{}{
			"id": 1.0, "pool_name": "tank", "enabled": true, "threshold": 35.0,
			"schedule": map[string]interface{}{"minute": "00", "hour": "00", "dom": "*", "dow": "7", "month": "*"},
		},
	}
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"pool.scrub.query":  schedules,
		"pool.scrub.delete": true,
	}, nil)
	r := NewRegistry(client, nil)

	out, err := r.CallTool(t.Context(), "delete_scrub_schedule", map[string]interface{}{
		"id": 1.0, "dry_run": true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := len(fake.called("pool.scrub.delete")); n != 0 {
		t.Errorf("dry run called pool.scrub.delete %d times", n)
	}
	details, actions := startAppPreview(t, out)
	if len(actions) != 1 {
		t.Fatalf("preview should list only the pool.scrub.delete call, got %d actions: %s", len(actions), out)
	}
	if target, _ := actions[0]["target"].(string); target != "pool.scrub.delete" {
		t.Errorf("planned target %q, want pool.scrub.delete", target)
	}
	wantDetails, _ := json.Marshal(map[string]interface{}{"id": 1})
	if !jsonBytesEqual(t, details, wantDetails) {
		t.Errorf("preview details %s, want %s", details, wantDetails)
	}

	client2, fake2 := newFakeClientWith(t, map[string]interface{}{
		"pool.scrub.query":  schedules,
		"pool.scrub.delete": true,
	}, nil)
	r2 := NewRegistry(client2, nil)
	got, err := r2.CallTool(t.Context(), "delete_scrub_schedule", approve(t, r2, "delete_scrub_schedule", map[string]interface{}{"id": 1.0}))
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, got)
	}
	calls := fake2.called("pool.scrub.delete")
	if len(calls) != 1 {
		t.Fatalf("execute called pool.scrub.delete %d times, want 1", len(calls))
	}
	wantParams, _ := json.Marshal([]interface{}{1})
	if !jsonBytesEqual(t, calls[0].Params, wantParams) {
		t.Errorf("executed pool.scrub.delete params %s, want %s", calls[0].Params, wantParams)
	}
	if !strings.Contains(got, `"deleted": true`) {
		t.Errorf("execute should report deleted, got %s", got)
	}
}

// A ParamNames list whose length does not match CallParams must fail the
// preview. The param_N fallback would skip positional redaction and can
// show a secret in clear.
func TestWritePlanMismatchedParamNamesFailsPreview(t *testing.T) {
	p := &writePlan{
		ParamNames: []string{"name"},
		CallParams: []interface{}{"a", "s3cret"},
	}
	details, err := p.previewDetails()
	if err == nil {
		raw, _ := json.Marshal(details)
		t.Fatalf("mismatched ParamNames must fail the preview, got %s", raw)
	}
	if details != nil {
		raw, _ := json.Marshal(details)
		if strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), "param_") {
			t.Errorf("failed preview still leaked a secret: %s", raw)
		}
	}
	if _, err := p.dryRunResult(); err == nil {
		t.Error("dryRunResult must also fail when ParamNames and CallParams differ in length")
	}
	if _, err := p.planHash(); err == nil {
		t.Error("planHash must also fail when ParamNames and CallParams differ in length")
	}
}

// A plan-backed destructive tool with Plan set but Execute nil must not
// silently fall back to the handler (which would rebuild the plan).
func TestCallDestructiveRefusesPlanWithoutExecute(t *testing.T) {
	r, fake := stopAppRegistry(t, "RUNNING")
	tok := previewToken(t, r, "stop_app", map[string]interface{}{"app_name": "plex"})
	tool := r.tools["stop_app"]
	tool.Execute = nil
	r.tools["stop_app"] = tool
	out, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{
		"app_name": "plex", "confirmation_token": tok,
	})
	if err == nil {
		t.Fatalf("expected an error when Execute is missing, got %s", out)
	}
	if !strings.Contains(err.Error(), "Execute") && !strings.Contains(err.Error(), "execute") {
		t.Errorf("error should say Execute is missing, got %q", err)
	}
	if !strings.Contains(err.Error(), "was NOT executed") || !strings.Contains(err.Error(), "nothing was executed") {
		t.Errorf("error should say the tool was not executed, got %q", err)
	}
	if n := len(fake.called("app.stop")); n != 0 {
		t.Errorf("missing Execute still called app.stop %d times", n)
	}
}

// An explicit dry_run of a blocked delete_boot_environment is documented
// as a breaking change: it now returns an error with status "blocked".
func TestREADMENotesBlockedDeleteBootEnvironmentDryRunIsBreakingChange(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, note := range readmeBreakingChangeNotes(string(data)) {
		if strings.Contains(note, "delete_boot_environment") && strings.Contains(note, "dry_run") && strings.Contains(note, "blocked") {
			found = true
			break
		}
	}
	if !found {
		t.Error(`README.md must have a "> **Breaking change:**" note that an explicit dry_run of a blocked delete_boot_environment now returns an error result with status "blocked"`)
	}
}

// readmeBreakingChangeNotes returns each README blockquote that begins
// with "> **Breaking change:". Only consecutive blockquote lines belong
// to a note, so later prose is not treated as part of the last note.
func readmeBreakingChangeNotes(text string) []string {
	var notes []string
	var cur []string
	in := false
	flush := func() {
		if len(cur) > 0 {
			notes = append(notes, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "> **Breaking change:") {
			if in {
				flush()
			}
			cur = []string{line}
			in = true
			continue
		}
		if in {
			if strings.HasPrefix(line, ">") {
				cur = append(cur, line)
				continue
			}
			flush()
			in = false
		}
	}
	if in {
		flush()
	}
	return notes
}

// Every registered Plan that uses ParamNames must have one name per
// CallParams entry, so a length mismatch cannot reach a preview.
func TestRegisteredPlansParamNamesMatchCallParams(t *testing.T) {
	bootEnvs := []interface{}{
		map[string]interface{}{"id": "25.04.1", "active": false, "activated": false, "keep": false, "used_bytes": 1048576.0},
	}
	schedules := []interface{}{
		map[string]interface{}{
			"id": 1.0, "pool_name": "tank", "enabled": true, "threshold": 35.0,
			"schedule": map[string]interface{}{"minute": "00", "hour": "00", "dom": "*", "dow": "7", "month": "*"},
		},
	}
	results := map[string]interface{}{
		"app.query":               []interface{}{appEntry("plex", "STOPPED")},
		"app.upgrade_summary":     upgradeSummaryFixture("x"),
		"catalog.get_app_details": hostPathCatalogDetails(),
		"app.available":           []interface{}{map[string]interface{}{"name": "plex", "train": "community"}},
		"app.config":              map[string]interface{}{},
		"pool.query": []interface{}{
			map[string]interface{}{"id": 1.0, "name": "tank", "status": "ONLINE", "size": 1.0e12},
			map[string]interface{}{"id": 2.0, "name": "spare", "status": "ONLINE", "size": 1.0e12},
		},
		"boot.environment.query":   bootEnvs,
		"directoryservices.status": map[string]interface{}{"type": "ACTIVEDIRECTORY", "status": "HEALTHY", "status_msg": nil},
		"update.status":            updateStatusAvailable("25.10.1"),
		"core.get_jobs":            []interface{}{},
		"alert.list": []interface{}{
			map[string]interface{}{"uuid": "alert-1", "level": "WARNING", "dismissed": false, "formatted": "disk"},
		},
	}
	fake := &fakeMiddleware{results: results}
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		switch method {
		case "pool.query":
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
		case "pool.scrub.query":
			if poolScrubQueryMatches(params, "id", 1) || poolScrubQueryMatches(params, "pool", 1) {
				return schedules, nil, true
			}
			return []interface{}{}, nil, true
		default:
			return nil, nil, false
		}
	}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	r := NewRegistry(client, mgr)

	checked := 0
	for name, tool := range r.tools {
		if tool.Plan == nil {
			continue
		}
		checked++
		args := toolArgsForSafetyFlagTest(name)
		if name == "create_scrub_schedule" {
			args["pool"] = "spare"
		}
		plan, err := tool.Plan(t.Context(), client, args)
		if err != nil {
			t.Errorf("%s Plan: %v", name, err)
			continue
		}
		if err := plan.paramNamesOK(); err != nil {
			t.Errorf("%s: %v (ParamNames=%q CallParams len %d)", name, err, plan.ParamNames, len(plan.CallParams))
		}
		if _, err := plan.previewDetails(); err != nil {
			t.Errorf("%s previewDetails: %v", name, err)
		}
	}
	if checked == 0 {
		t.Fatal("no registered tools have a Plan")
	}
}

// poolScrubQueryFilter returns the [field, "=", value] filter from a
// pool.scrub.query call, if the first positional argument is that tuple
// or a list containing it.
func poolScrubQueryFilter(params json.RawMessage) (field string, value float64, ok bool) {
	var wrap []interface{}
	if err := json.Unmarshal(params, &wrap); err != nil || len(wrap) == 0 {
		return "", 0, false
	}
	if f, v, found := asEqualsFilter(wrap[0]); found {
		return f, v, true
	}
	list, ok := wrap[0].([]interface{})
	if !ok {
		return "", 0, false
	}
	for _, item := range list {
		if f, v, found := asEqualsFilter(item); found {
			return f, v, true
		}
	}
	return "", 0, false
}

func asEqualsFilter(v interface{}) (string, float64, bool) {
	filter, ok := v.([]interface{})
	if !ok || len(filter) != 3 || filter[1] != "=" {
		return "", 0, false
	}
	field, _ := filter[0].(string)
	switch n := filter[2].(type) {
	case float64:
		return field, n, field != ""
	case int:
		return field, float64(n), field != ""
	default:
		return "", 0, false
	}
}

func poolScrubQueryMatches(params json.RawMessage, field string, id float64) bool {
	f, v, ok := poolScrubQueryFilter(params)
	return ok && f == field && v == id
}

func filterPoolScrubQuery(params json.RawMessage, all []interface{}) []interface{} {
	field, value, ok := poolScrubQueryFilter(params)
	if !ok {
		return all
	}
	var out []interface{}
	for _, item := range all {
		m, _ := item.(map[string]interface{})
		if queryNumberEquals(m[field], value) {
			out = append(out, item)
		}
	}
	return out
}

func queryNumberEquals(v interface{}, want float64) bool {
	switch n := v.(type) {
	case float64:
		return n == want
	case int:
		return float64(n) == want
	default:
		return false
	}
}

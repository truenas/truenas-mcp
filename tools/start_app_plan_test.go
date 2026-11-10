package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
)

const startAppJobID = 77

func startAppRegistry(t *testing.T, state string) (*Registry, *fakeMiddleware) {
	t.Helper()
	return startAppRegistryWithQuery(t, []interface{}{appEntry("plex", state)})
}

func startAppRegistryWithQuery(t *testing.T, queryResult interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"app.start": startAppJobID,
		},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			switch method {
			case "app.query":
				return queryResult, nil, true
			case "core.get_jobs":
				return []interface{}{
					map[string]interface{}{
						"id":           float64(startAppJobID),
						"method":       "app.start",
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

func startAppPreview(t *testing.T, out string) (details json.RawMessage, actions []map[string]interface{}) {
	t.Helper()
	var preview struct {
		PlannedActions []map[string]interface{} `json:"planned_actions"`
		Blocked        []string                 `json:"blocked"`
	}
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		t.Fatalf("preview is not JSON: %v\n%s", err, out)
	}
	if len(preview.PlannedActions) == 0 {
		t.Fatalf("preview has no planned_actions: %s", out)
	}
	raw, err := json.Marshal(preview.PlannedActions[0]["details"])
	if err != nil {
		t.Fatal(err)
	}
	return raw, preview.PlannedActions
}

func jsonBytesEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var xa, xb interface{}
	if err := json.Unmarshal(a, &xa); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &xb); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	sa, _ := json.Marshal(xa)
	sb, _ := json.Marshal(xb)
	return string(sa) == string(sb)
}

func blockedPreview(t *testing.T, err error) (msg string, blocked []string, reply string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	msg = err.Error()
	var replyErr *mcp.ReplyError
	if !errors.As(err, &replyErr) {
		t.Fatalf("want a ReplyError carrying the preview, got %T %v", err, err)
	}
	var preview struct {
		Blocked []string `json:"blocked"`
	}
	if err := json.Unmarshal([]byte(replyErr.Reply), &preview); err != nil {
		t.Fatalf("preview reply is not JSON: %v\n%s", err, replyErr.Reply)
	}
	if len(preview.Blocked) == 0 {
		t.Fatalf("preview lists no blocked reasons: %s", replyErr.Reply)
	}
	for _, b := range preview.Blocked {
		if !strings.Contains(msg, b) {
			t.Errorf("error %q does not contain blocked reason %q", msg, b)
		}
	}
	return msg, preview.Blocked, replyErr.Reply
}

// A successful start_app preview shows named, maskable details (not the raw
// positional params) and lists only the app.start call. The real call still
// sends those positional params. STOPPED and CRASHED stay startable.
func TestStartAppPlanMatchesExecutedPayload(t *testing.T) {
	wantDetails, _ := json.Marshal(map[string]interface{}{"app_name": "plex"})
	for _, state := range []string{"STOPPED", "CRASHED"} {
		t.Run(state, func(t *testing.T) {
			r, fake := startAppRegistry(t, state)
			out, err := r.CallTool(t.Context(), "start_app", map[string]interface{}{
				"app_name": "plex", "dry_run": true,
			})
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if strings.Contains(out, "must be STOPPED") {
				t.Errorf("%s preview should not warn that the app must be STOPPED, got %s", state, out)
			}
			if strings.Contains(out, `"blocked"`) && strings.Contains(out, `"blocked": [`) && !strings.Contains(out, `"blocked": []`) && !strings.Contains(out, `"blocked": null`) {
				var p struct {
					Blocked []string `json:"blocked"`
				}
				_ = json.Unmarshal([]byte(out), &p)
				if len(p.Blocked) != 0 {
					t.Errorf("%s preview should not be blocked, got %s", state, out)
				}
			}
			if n := len(fake.called("app.start")); n != 0 {
				t.Errorf("dry run called app.start %d times", n)
			}
			details, actions := startAppPreview(t, out)
			if len(actions) != 1 {
				t.Fatalf("preview should list only the app.start call, got %d actions: %s", len(actions), out)
			}
			if target, _ := actions[0]["target"].(string); target != "app.start" {
				t.Errorf("planned target %q, want app.start", target)
			}
			if !jsonBytesEqual(t, details, wantDetails) {
				t.Errorf("preview details %s, want named fields %s", details, wantDetails)
			}

			r2, fake2 := startAppRegistry(t, state)
			got, err := r2.CallTool(t.Context(), "start_app", map[string]interface{}{"app_name": "plex"})
			if err != nil {
				t.Fatalf("execute: %v\n%s", err, got)
			}
			calls := fake2.called("app.start")
			if len(calls) != 1 {
				t.Fatalf("execute called app.start %d times, want 1", len(calls))
			}
			wantParams, _ := json.Marshal([]interface{}{"plex"})
			if !jsonBytesEqual(t, calls[0].Params, wantParams) {
				t.Errorf("executed app.start params %s, want %s", calls[0].Params, wantParams)
			}
			if !strings.Contains(got, "task_id") {
				t.Errorf("execute should return a task_id, got %s", got)
			}
		})
	}
}

func TestStartAppRunningIsRefused(t *testing.T) {
	var previewBlocked []string
	for _, dry := range []bool{true, false} {
		name := "execute"
		if dry {
			name = "dry_run"
		}
		t.Run(name, func(t *testing.T) {
			r, fake := startAppRegistry(t, "RUNNING")
			args := map[string]interface{}{"app_name": "plex"}
			if dry {
				args["dry_run"] = true
			}
			out, err := r.CallTool(t.Context(), "start_app", args)
			if err == nil {
				t.Fatalf("expected an error for a RUNNING app, got %s", out)
			}
			msg, blocked, _ := blockedPreview(t, err)
			for _, want := range []string{`cannot start app "plex"`, "already RUNNING", "nothing was executed"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error should contain %q, got %q", want, msg)
				}
			}
			if n := len(fake.called("app.start")); n != 0 {
				t.Errorf("refused start called app.start %d times", n)
			}
			if dry {
				previewBlocked = append([]string{}, blocked...)
			} else if previewBlocked != nil {
				if strings.Join(blocked, "\n") != strings.Join(previewBlocked, "\n") {
					t.Errorf("execute blocked %q, preview blocked %q", blocked, previewBlocked)
				}
			}
		})
	}
}

func TestStartAppDeployingAndStoppingAreRefused(t *testing.T) {
	for _, state := range []string{"DEPLOYING", "STOPPING"} {
		for _, dry := range []bool{true, false} {
			name := state + "/execute"
			if dry {
				name = state + "/dry_run"
			}
			t.Run(name, func(t *testing.T) {
				r, fake := startAppRegistry(t, state)
				args := map[string]interface{}{"app_name": "plex"}
				if dry {
					args["dry_run"] = true
				}
				out, err := r.CallTool(t.Context(), "start_app", args)
				if err == nil {
					t.Fatalf("expected an error for a %s app, got %s", state, out)
				}
				msg, _, _ := blockedPreview(t, err)
				for _, want := range []string{`cannot start app "plex"`, state, "retry when it settles", "nothing was executed"} {
					if !strings.Contains(msg, want) {
						t.Errorf("error should contain %q, got %q", want, msg)
					}
				}
				if n := len(fake.called("app.start")); n != 0 {
					t.Errorf("refused start called app.start %d times", n)
				}
			})
		}
	}
}

// A non-string app state is a parse error, not a warning that prints map[...].
func TestStartAppNonStringStateIsParseError(t *testing.T) {
	app := appEntry("plex", "STOPPED")
	app["state"] = map[string]interface{}{"unexpected": true}
	r, fake := startAppRegistryWithQuery(t, []interface{}{app})
	out, err := r.CallTool(t.Context(), "start_app", map[string]interface{}{
		"app_name": "plex", "dry_run": true,
	})
	if err == nil {
		t.Fatalf("expected a parse error, got %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "failed to parse") {
		t.Errorf("error should say the state could not be parsed, got %q", msg)
	}
	if strings.Contains(msg, "map[") {
		t.Errorf("error should not print the raw state, got %q", msg)
	}
	if n := len(fake.called("app.start")); n != 0 {
		t.Errorf("parse failure called app.start %d times", n)
	}
}

// A malformed app.query result is a parse error, not "app not found".
func TestStartAppQueryParseErrorIsNotAppNotFound(t *testing.T) {
	r, fake := startAppRegistryWithQuery(t, map[string]interface{}{"unexpected": "object"})
	out, err := r.CallTool(t.Context(), "start_app", map[string]interface{}{
		"app_name": "plex", "dry_run": true,
	})
	if err == nil {
		t.Fatalf("expected a parse error, got %s", out)
	}
	msg := err.Error()
	if strings.Contains(msg, "app not found") {
		t.Errorf("parse failure was reported as app not found: %q", msg)
	}
	if !strings.Contains(msg, "failed to parse") {
		t.Errorf("error should say the query result could not be parsed, got %q", msg)
	}
	if n := len(fake.called("app.start")); n != 0 {
		t.Errorf("parse failure called app.start %d times", n)
	}
}

// blockedReasons reads the plan's blocked list, and still honours BLOCKED: warnings.
func TestBlockedReasonsReadsBlockedField(t *testing.T) {
	got := blockedReasons(`{"blocked":["already RUNNING"],"warnings":["just a warning"]}`)
	if len(got) != 1 || got[0] != "already RUNNING" {
		t.Errorf("blocked field: got %q", got)
	}
	got = blockedReasons(`{"warnings":["BLOCKED: cannot delete the active environment"]}`)
	if len(got) != 1 || got[0] != "cannot delete the active environment" {
		t.Errorf("BLOCKED: warning: got %q", got)
	}
}

// Preview details are derived from CallParams (via ParamNames or a single
// map argument), so they cannot drift from the payload that is executed.
func TestWritePlanPreviewIsDerivedFromCallParams(t *testing.T) {
	cases := []struct {
		name string
		plan *writePlan
		want interface{}
	}{
		{
			name: "positional with ParamNames",
			plan: &writePlan{
				Method:     startAppMethod,
				CallParams: []interface{}{"plex"},
				ParamNames: []string{"app_name"},
			},
			want: map[string]interface{}{"app_name": "plex"},
		},
		{
			name: "single map argument",
			plan: &writePlan{
				Method:     bootEnvironmentDestroyMethod,
				CallParams: []interface{}{map[string]interface{}{"id": "25.04.1"}},
			},
			want: map[string]interface{}{"id": "25.04.1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.plan.previewDetails())
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if !jsonBytesEqual(t, got, want) {
				t.Errorf("preview details %s, want %s (derived from CallParams)", got, want)
			}
		})
	}
}

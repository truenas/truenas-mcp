package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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

func startAppPreview(t *testing.T, out string) (payload json.RawMessage, actions []map[string]interface{}) {
	t.Helper()
	var preview struct {
		PlannedActions []map[string]interface{} `json:"planned_actions"`
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

// The dry-run plan and the executed app.start call use the same payload.
// A STOPPED (or CRASHED) app no longer gets a "must be STOPPED" warning,
// and the preview lists only the app.start call that execution performs.
func TestStartAppPlanMatchesExecutedPayload(t *testing.T) {
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
			if n := len(fake.called("app.start")); n != 0 {
				t.Errorf("dry run called app.start %d times", n)
			}
			planned, actions := startAppPreview(t, out)
			if len(actions) != 1 {
				t.Fatalf("preview should list only the app.start call, got %d actions: %s", len(actions), out)
			}
			if target, _ := actions[0]["target"].(string); target != "app.start" {
				t.Errorf("planned target %q, want app.start", target)
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
			if !jsonBytesEqual(t, planned, calls[0].Params) {
				t.Errorf("dry-run plan details %s do not match executed app.start params %s", planned, calls[0].Params)
			}
			if !strings.Contains(got, "task_id") {
				t.Errorf("execute should return a task_id, got %s", got)
			}
		})
	}
}

func TestStartAppRunningIsRefused(t *testing.T) {
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
			msg := err.Error()
			for _, want := range []string{`cannot start app "plex"`, "already RUNNING", "nothing was executed"} {
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

package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

const stopAppJobID = 88

func stopAppRegistry(t *testing.T, state string) (*Registry, *fakeMiddleware) {
	t.Helper()
	return stopAppRegistryWithQuery(t, []interface{}{appEntry("plex", state)})
}

func stopAppRegistryWithQuery(t *testing.T, queryResult interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"app.stop": stopAppJobID,
		},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			switch method {
			case "app.query":
				return queryResult, nil, true
			case "core.get_jobs":
				return []interface{}{
					map[string]interface{}{
						"id":           float64(stopAppJobID),
						"method":       "app.stop",
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

// A malformed app.query result is a parse error, not "app not found".
func TestStopAppQueryParseErrorIsNotAppNotFound(t *testing.T) {
	r, fake := stopAppRegistryWithQuery(t, map[string]interface{}{"unexpected": "object"})
	out, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{
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
	if n := len(fake.called("app.stop")); n != 0 {
		t.Errorf("parse failure called app.stop %d times", n)
	}
}

// A non-string app state is a parse error, not a warning that prints map[...].
func TestStopAppNonStringStateIsParseError(t *testing.T) {
	app := appEntry("plex", "RUNNING")
	app["state"] = map[string]interface{}{"unexpected": true}
	r, fake := stopAppRegistryWithQuery(t, []interface{}{app})
	out, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{
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
	if n := len(fake.called("app.stop")); n != 0 {
		t.Errorf("parse failure called app.stop %d times", n)
	}
}

// A successful stop_app preview shows named, maskable details derived from
// the executed payload, and lists only the app.stop call.
func TestStopAppPlanMatchesExecutedPayload(t *testing.T) {
	wantDetails, _ := json.Marshal(map[string]interface{}{"app_name": "plex"})
	for _, state := range []string{"RUNNING", "STOPPED"} {
		t.Run(state, func(t *testing.T) {
			r, fake := stopAppRegistry(t, state)
			out, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{
				"app_name": "plex", "dry_run": true,
			})
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if n := len(fake.called("app.stop")); n != 0 {
				t.Errorf("dry run called app.stop %d times", n)
			}
			details, actions := startAppPreview(t, out)
			if len(actions) != 1 {
				t.Fatalf("preview should list only the app.stop call, got %d actions: %s", len(actions), out)
			}
			if target, _ := actions[0]["target"].(string); target != "app.stop" {
				t.Errorf("planned target %q, want app.stop", target)
			}
			if !jsonBytesEqual(t, details, wantDetails) {
				t.Errorf("preview details %s, want named fields %s", details, wantDetails)
			}

			r2, fake2 := stopAppRegistry(t, state)
			got, err := r2.CallTool(t.Context(), "stop_app", approve(t, r2, "stop_app", map[string]interface{}{"app_name": "plex"}))
			if err != nil {
				t.Fatalf("execute: %v\n%s", err, got)
			}
			calls := fake2.called("app.stop")
			if len(calls) != 1 {
				t.Fatalf("execute called app.stop %d times, want 1", len(calls))
			}
			wantParams, _ := json.Marshal([]interface{}{"plex"})
			if !jsonBytesEqual(t, calls[0].Params, wantParams) {
				t.Errorf("executed app.stop params %s, want %s", calls[0].Params, wantParams)
			}
			if !strings.Contains(got, "task_id") {
				t.Errorf("execute should return a task_id, got %s", got)
			}
		})
	}
}

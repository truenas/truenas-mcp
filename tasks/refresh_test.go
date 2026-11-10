package tasks

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// refreshJobApp replaces task.Job rather than editing it: stored copies of
// the task share the old map.
func TestRefreshJobAppDoesNotMutateSharedJob(t *testing.T) {
	old := map[string]interface{}{"id": 1.0, "state": "SUCCESS", "app": map[string]interface{}{"name": "a", "state": "DEPLOYING"}, "result_truncated": true}
	task := &Task{Job: old}
	refreshJobApp(task, appHealthInfo{state: "RUNNING", app: map[string]interface{}{"name": "a", "state": "RUNNING"}}, nil)
	if old["app"].(map[string]interface{})["state"] != "DEPLOYING" || old["app_source"] != nil {
		t.Errorf("shared job map was modified: %v", old)
	}
	if task.Job["app"].(map[string]interface{})["state"] != "RUNNING" || task.Job["app_source"] != "app.query" || task.Job["result_truncated"] != true {
		t.Errorf("job not refreshed: %v", task.Job)
	}
	if task.Result != nil {
		t.Error("task.Result set: the result is reported in the job summary only")
	}

	// No job summary: nothing to do. A failed read keeps the copy, labelled.
	empty := &Task{}
	refreshJobApp(empty, appHealthInfo{}, nil)
	if empty.Job != nil {
		t.Errorf("job created from nothing: %v", empty.Job)
	}
	task = &Task{Job: old}
	refreshJobApp(task, appHealthInfo{}, errors.New("down"))
	if task.Job["app"].(map[string]interface{})["state"] != "DEPLOYING" || task.Job["app_source"] == "app.query" || task.Job["app_source"] == nil {
		t.Errorf("failed read: %v", task.Job)
	}
	// A job without an app copy gets no app_source on a failed read.
	task = &Task{Job: map[string]interface{}{"id": 1.0, "state": "SUCCESS"}}
	refreshJobApp(task, appHealthInfo{}, errors.New("down"))
	if _, ok := task.Job["app_source"]; ok {
		t.Errorf("app_source without app: %v", task.Job)
	}
}

// The refreshed summary is passed through FitJobSummary, so a job whose other
// fields are large still stays within MaxJobResultBytes.
func TestRefreshJobAppFitsSummary(t *testing.T) {
	task := &Task{Job: map[string]interface{}{
		"id": 1.0, "state": "SUCCESS", "method": "app.upgrade",
		"description": strings.Repeat("d", 3000),
		"error":       strings.Repeat("e", 3000),
	}}
	refreshJobApp(task, appHealthInfo{state: "RUNNING", app: map[string]interface{}{"name": "a", "state": "RUNNING"}}, nil)
	if n := summarySize(task.Job); n > MaxJobResultBytes {
		t.Errorf("refreshed summary is %d bytes, over %d", n, MaxJobResultBytes)
	}
	if task.Job["app_source"] != "app.query" {
		t.Errorf("app_source = %v", task.Job["app_source"])
	}
}

// A live read followed by a failed one keeps the last live copy but drops
// app_checked_at, and the label says why it was not refreshed: a read error
// and an app that app.query no longer lists are told apart.
func TestRefreshJobAppLiveThenFailure(t *testing.T) {
	live := appHealthInfo{state: "RUNNING", app: map[string]interface{}{"name": "a", "state": "RUNNING"}}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"read error", errors.New("connection reset"), "job result (the live app state could not be read)"},
		{"not found", fmt.Errorf("app a %w", errAppNotFound), "job result (app.query no longer lists the app)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &Task{Job: map[string]interface{}{"id": 1.0, "state": "SUCCESS", "app": map[string]interface{}{"name": "a", "state": "DEPLOYING"}}}
			refreshJobApp(task, live, nil)
			if _, ok := task.Job["app_checked_at"]; !ok {
				t.Fatalf("app_checked_at not set after a live read: %v", task.Job)
			}
			refreshJobApp(task, appHealthInfo{}, tc.err)
			if _, ok := task.Job["app_checked_at"]; ok {
				t.Errorf("stale app_checked_at kept after a failed read: %v", task.Job)
			}
			if task.Job["app_source"] != tc.want {
				t.Errorf("app_source = %v, want %q", task.Job["app_source"], tc.want)
			}
		})
	}
}

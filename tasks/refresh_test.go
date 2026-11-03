package tasks

import (
	"errors"
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
	if task.Result == nil {
		t.Error("task.Result not updated")
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

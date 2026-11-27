package tasks

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLocalTaskLifecycle(t *testing.T) {
	m := NewManager(nil, PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	defer m.Shutdown()

	task, err := m.CreateLocalTask("upload_file", map[string]interface{}{"path": "/mnt/tank/a.iso"}, time.Hour, "Starting", WithExpectedMethod("filesystem.put"))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskStatusWorking || task.JobID != nil || task.StatusMessage != "Starting" || task.TTL != 3600 {
		t.Fatalf("new local task = %+v", task)
	}
	if err := m.SetLocalMessage(task.TaskID, "Downloading: 10 MiB"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(task.TaskID); got.StatusMessage != "Downloading: 10 MiB" {
		t.Errorf("message = %q", got.StatusMessage)
	}

	if err := m.AttachJob(context.Background(), task.TaskID, 88, "Uploaded; job 88", WithJobOutcome(func(map[string]interface{}) (string, interface{}) { return "done", nil })); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(task.TaskID)
	if got.JobID == nil || *got.JobID != 88 || got.StatusMessage != "Uploaded; job 88" || got.Status != TaskStatusWorking {
		t.Errorf("attached task = %+v", got)
	}
	if !got.methodMatches("filesystem.put") || got.methodMatches("pool.scrub.scrub") {
		t.Error("attached task lost its expected method")
	}
	// Once a job is attached, only the poller updates the task.
	for name, err := range map[string]error{
		"SetLocalMessage": m.SetLocalMessage(task.TaskID, "x"),
		"FailLocal":       m.FailLocal(task.TaskID, "x"),
		"AttachJob":       m.AttachJob(context.Background(), task.TaskID, 89, "x"),
	} {
		if !errors.Is(err, errJobAttached) {
			t.Errorf("%s after AttachJob: %v", name, err)
		}
	}
	if got, _ := m.Get(task.TaskID); *got.JobID != 88 {
		t.Errorf("job id changed to %d", *got.JobID)
	}
}

func TestLocalTaskFail(t *testing.T) {
	m := NewManager(nil, PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	defer m.Shutdown()
	task, err := m.CreateLocalTask("upload_file", nil, time.Hour, "Starting")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.FailLocal(task.TaskID, "SHA-256 mismatch"); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(task.TaskID)
	if got.Status != TaskStatusFailed || got.StatusMessage != "SHA-256 mismatch" {
		t.Errorf("failed task = %+v", got)
	}
	for name, err := range map[string]error{
		"SetLocalMessage": m.SetLocalMessage(task.TaskID, "x"),
		"FailLocal":       m.FailLocal(task.TaskID, "x"),
		"AttachJob":       m.AttachJob(context.Background(), task.TaskID, 1, "x"),
	} {
		if !errors.Is(err, ErrTaskFinished) {
			t.Errorf("%s after FailLocal: %v", name, err)
		}
	}
	if err := m.SetLocalMessage("no-such-task", "x"); err == nil {
		t.Error("SetLocalMessage accepted an unknown task")
	}
}

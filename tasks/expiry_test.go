package tasks

import (
	"strings"
	"testing"
	"time"
)

// A task that completes shortly before its tracking TTL is still readable
// after that TTL: terminal tasks are kept for a retention period counted from
// when they finished, not from when they were created.
func TestCompletedTaskReadableAfterOriginalTTL(t *testing.T) {
	s := NewTaskStore()
	if err := s.Add(&Task{TaskID: "t1", Status: TaskStatusWorking, TTL: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	task, err := s.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	task.Status = TaskStatusCompleted
	task.StatusMessage = "Job completed successfully"
	if err := s.Update(task); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond) // past creation + TTL

	got, err := s.Get("t1")
	if err != nil {
		t.Fatalf("completed task unreadable after its original TTL: %v", err)
	}
	if got.Status != TaskStatusCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
	if l, _, _ := s.List("", 10); len(l) != 1 {
		t.Errorf("tasks_list shows %d tasks, want the completed one", len(l))
	}
	s.CleanExpired()
	if _, err := s.Get("t1"); err != nil {
		t.Errorf("cleanup removed a task within its retention period: %v", err)
	}
}

// A task still working when its TTL runs out is not dropped silently: it is
// marked failed ("tracking timed out") and stays readable for the retention
// period, and it is no longer polled.
func TestWorkingTaskPastTTLIsMarkedTimedOut(t *testing.T) {
	s := NewTaskStore()
	id := 7
	if err := s.Add(&Task{TaskID: "t1", Status: TaskStatusWorking, TTL: 1, CreatedAt: time.Now(),
		OperationType: OperationTypeJob, JobID: &id, ToolName: "run_scrub"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)

	got, err := s.Get("t1")
	if err != nil {
		t.Fatalf("task disappeared at its TTL instead of failing: %v", err)
	}
	if got.Status != TaskStatusFailed || !strings.Contains(got.StatusMessage, "tracking timed out") {
		t.Fatalf("task = %s / %q, want failed with tracking timed out", got.Status, got.StatusMessage)
	}
	if !strings.Contains(got.StatusMessage, "get_scrub_status") || !strings.Contains(got.StatusMessage, "7") {
		t.Errorf("message does not say how to check: %q", got.StatusMessage)
	}
	if n := len(s.GetActive()); n != 0 {
		t.Errorf("timed-out task still polled (%d active)", n)
	}
	s.CleanExpired()
	if _, err := s.Get("t1"); err != nil {
		t.Errorf("timed-out task removed within its retention period: %v", err)
	}
}

// A finished task is never changed again: a poller holding a copy from
// before the task timed out cannot revive it or overwrite its outcome.
func TestFinishedTaskIsNotRevived(t *testing.T) {
	s := NewTaskStore()
	if err := s.Add(&Task{TaskID: "t1", Status: TaskStatusWorking, TTL: 60}); err != nil {
		t.Fatal(err)
	}
	stale, _ := s.Get("t1")
	done, _ := s.Get("t1")
	done.Status = TaskStatusFailed
	done.StatusMessage = "tracking timed out"
	if err := s.Update(done); err != nil {
		t.Fatal(err)
	}
	stale.StatusMessage = "Progress: 50.0%"
	if err := s.Update(stale); err != ErrTaskFinished {
		t.Errorf("Update of a finished task = %v, want ErrTaskFinished", err)
	}
	if err := s.Save(stale); err != ErrTaskFinished {
		t.Errorf("Save of a finished task = %v, want ErrTaskFinished", err)
	}
	if got, _ := s.Get("t1"); got.Status != TaskStatusFailed || got.StatusMessage != "tracking timed out" {
		t.Errorf("finished task changed: %s / %q", got.Status, got.StatusMessage)
	}
}

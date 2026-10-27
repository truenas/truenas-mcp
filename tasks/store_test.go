package tasks

import (
	"sync"
	"testing"
	"time"
)

// The store hands out copies: changing what Get returned must not change the
// stored task, and a stored task is not affected by later edits to what was
// passed to Add or Update. This is what keeps the poller and a reader of the
// same task from sharing memory.
func TestStoreIsolatesCallersFromStoredTasks(t *testing.T) {
	s := NewTaskStore()
	orig := &Task{TaskID: "t1", Status: TaskStatusWorking, StatusMessage: "a", TTL: 60}
	if err := s.Add(orig); err != nil {
		t.Fatal(err)
	}
	orig.StatusMessage = "edited after Add"

	got, _ := s.Get("t1")
	if got.StatusMessage != "a" {
		t.Fatalf("Add kept the caller's pointer: %q", got.StatusMessage)
	}
	got.StatusMessage = "edited after Get"
	got.Status = TaskStatusFailed
	again, _ := s.Get("t1")
	if again.StatusMessage != "a" || again.Status != TaskStatusWorking {
		t.Errorf("Get returned the stored task: %q %v", again.StatusMessage, again.Status)
	}
	if l, _, _ := s.List("", 10); len(l) != 1 {
		t.Errorf("List returned %d tasks", len(l))
	} else {
		l[0].StatusMessage = "edited after List"
	}
	for _, a := range s.GetActive() {
		a.StatusMessage = "edited after GetActive"
	}
	if cur, _ := s.Get("t1"); cur.StatusMessage != "a" {
		t.Errorf("stored task changed through List/GetActive: %q", cur.StatusMessage)
	}

	upd := &Task{TaskID: "t1", Status: TaskStatusCompleted, StatusMessage: "done", TTL: 60}
	if err := s.Update(upd); err != nil {
		t.Fatal(err)
	}
	upd.StatusMessage = "edited after Update"
	if cur, _ := s.Get("t1"); cur.StatusMessage != "done" || cur.LastUpdatedAt.IsZero() {
		t.Errorf("Update: %q, updated at %v", cur.StatusMessage, cur.LastUpdatedAt)
	}
}

// Save persists bookkeeping without counting as a status update.
func TestStoreSaveKeepsLastUpdatedAt(t *testing.T) {
	s := NewTaskStore()
	if err := s.Add(&Task{TaskID: "t1", Status: TaskStatusWorking, TTL: 60}); err != nil {
		t.Fatal(err)
	}
	task, _ := s.Get("t1")
	_ = s.Update(task)
	before, _ := s.Get("t1")
	time.Sleep(5 * time.Millisecond)

	started := time.Now()
	before.VerifyStartedAt = started
	before.Job = map[string]interface{}{"state": "SUCCESS"}
	if err := s.Save(before); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get("t1")
	if !after.LastUpdatedAt.Equal(before.LastUpdatedAt) {
		t.Errorf("Save changed LastUpdatedAt: %v -> %v", before.LastUpdatedAt, after.LastUpdatedAt)
	}
	if !after.VerifyStartedAt.Equal(started) || after.Job["state"] != "SUCCESS" {
		t.Errorf("Save did not persist: %v %v", after.VerifyStartedAt, after.Job)
	}
	if err := s.Save(&Task{TaskID: "missing"}); err == nil {
		t.Error("Save of an unknown task should fail")
	}
}

// Readers and a writer on the same task, as tasks_get and the poller are.
// Meaningful under -race.
func TestStoreConcurrentReadersAndWriter(t *testing.T) {
	s := NewTaskStore()
	if err := s.Add(&Task{TaskID: "t1", Status: TaskStatusWorking, TTL: 60}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if task, err := s.Get("t1"); err == nil {
					_ = task.Job["state"]
					_ = task.StatusMessage
				}
				l, _, _ := s.List("", 10)
				for _, task := range l {
					_ = task.Status
				}
			}
		}()
	}
	for i := 0; i < 500; i++ {
		task, _ := s.Get("t1")
		task.StatusMessage = "m"
		task.Job = map[string]interface{}{"state": "RUNNING", "i": i}
		_ = s.Save(task)
		if i%10 == 0 {
			_ = s.Update(task)
		}
	}
	close(stop)
	wg.Wait()
}

package tasks

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type logBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *logBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *logBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func captureLog(t *testing.T) *logBuf {
	t.Helper()
	b := &logBuf{}
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return b
}

func workingTask(id string) *Task {
	return &Task{TaskID: id, Status: TaskStatusWorking, OperationType: OperationTypeStatus, StatusMethod: "x", TTL: 60}
}

// A panic while polling one task marks that task failed, still polls the
// others, and does not escape.
func TestPollerPanicMarksTaskFailedAndContinues(t *testing.T) {
	logs := captureLog(t)
	store := NewTaskStore()
	for _, id := range []string{"bad", "good1", "good2"} {
		if err := store.Add(workingTask(id)); err != nil {
			t.Fatal(err)
		}
	}
	var polled sync.Map
	p := NewPoller(nil, store, PollerConfig{PollInterval: time.Hour})
	p.pollTask = func(task *Task) {
		polled.Store(task.TaskID, true)
		if task.TaskID == "bad" {
			var s []int
			_ = s[3] // index out of range
		}
	}

	p.pollAllTasksSafely() // must not panic

	for _, id := range []string{"bad", "good1", "good2"} {
		if _, ok := polled.Load(id); !ok {
			t.Errorf("task %s was not polled", id)
		}
	}
	bad, err := store.Get("bad")
	if err != nil || bad.Status != TaskStatusFailed || !strings.Contains(bad.StatusMessage, "Internal error") {
		t.Errorf("bad task = %+v, %v; want failed with an internal error message", bad, err)
	}
	if g, _ := store.Get("good1"); g.Status != TaskStatusWorking {
		t.Errorf("good task status = %s", g.Status)
	}
	if !strings.Contains(logs.String(), "task poller panicked on task bad") || !strings.Contains(logs.String(), "index out of range") {
		t.Errorf("panic was not logged: %s", logs.String())
	}
	// The failed task is terminal, so it is not polled (and cannot panic) again.
	if n := len(store.GetActive()); n != 2 {
		t.Errorf("active tasks = %d; want 2", n)
	}
}

// Run keeps polling on later ticks after a pass panics outside any one task.
func TestPollerRunSurvivesPanicInPass(t *testing.T) {
	logs := captureLog(t)
	p := NewPoller(nil, nil, PollerConfig{PollInterval: 5 * time.Millisecond}) // nil store: every pass panics

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(logs.String(), "task poller panicked:") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("poller stopped after a panic; log: %s", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

// The cleanup loop logs a panic and keeps running.
func TestCleanupLoopSurvivesPanic(t *testing.T) {
	logs := captureLog(t)
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		store:  NewTaskStore(),
		config: PollerConfig{CleanupInterval: 5 * time.Millisecond},
		ctx:    ctx,
		cancel: cancel,
		cleanExpired: func() {
			if calls.Add(1) == 1 {
				panic("cleanup failed")
			}
		},
	}
	done := make(chan struct{})
	go func() { defer close(done); m.cleanupLoop() }()

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("cleanup loop died after a panic (calls = %d)", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.Shutdown()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanupLoop did not stop on Shutdown")
	}
	if !strings.Contains(logs.String(), "task cleanup panicked") {
		t.Errorf("panic was not logged: %s", logs.String())
	}
	if strings.Contains(logs.String(), "cleanup failed") {
		t.Errorf("non-runtime panic value was logged: %s", logs.String())
	}
}

// A cleanup pass really removes expired tasks, without a hook.
func TestCleanOnceRemovesExpired(t *testing.T) {
	m := &Manager{store: NewTaskStore()}
	exp := workingTask("old")
	exp.TTL = -1
	m.store.Add(exp)
	m.store.Add(workingTask("new"))
	m.cleanOnce()
	if _, err := m.store.Get("old"); err == nil {
		t.Error("expired task still present")
	}
	if _, err := m.store.Get("new"); err != nil {
		t.Errorf("live task removed: %v", err)
	}
}

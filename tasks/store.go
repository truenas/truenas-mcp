package tasks

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// TaskStore provides thread-safe storage for tasks.
//
// A working task is tracked until creation + TTL. If it is still working then,
// it is marked failed ("tracking timed out") rather than dropped. A finished
// task stays readable for the retention period after it finished, and is then
// removed.
//
// The store owns the tasks it holds: Add, Update and Save keep a private copy
// and Get, List and GetActive hand out copies. Callers (the poller, tools
// marshalling a task for tasks_get) therefore never share a *Task with each
// other, and nothing mutates a stored task outside the store lock. Map and
// pointer fields of a Task (Job, Result, Arguments, JobID) are shared between
// copies and must be replaced, not modified in place.
type TaskStore struct {
	mu        sync.Mutex
	tasks     map[string]*Task
	expiry    map[string]time.Time // working: end of tracking; finished: end of retention
	retention time.Duration
}

// ErrTaskFinished is returned by Update and Save for a task that has already
// reached a terminal state: a finished task is never changed again, so a
// poller holding an older copy cannot revive or overwrite it.
var ErrTaskFinished = errors.New("task already finished")

// NewTaskStore creates a new task store with DefaultRetention.
func NewTaskStore() *TaskStore {
	return &TaskStore{
		tasks:     make(map[string]*Task),
		expiry:    make(map[string]time.Time),
		retention: DefaultRetention,
	}
}

// SetRetention sets how long finished tasks stay readable; d <= 0 restores
// DefaultRetention.
func (s *TaskStore) SetRetention(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d <= 0 {
		d = DefaultRetention
	}
	s.retention = d
}

// Add stores a task and sets the end of its tracking window.
func (s *TaskStore) Add(task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if task.TaskID == "" {
		return fmt.Errorf("task ID cannot be empty")
	}

	now := time.Now()
	s.tasks[task.TaskID] = task.clone()
	if task.Status.Terminal() {
		s.expiry[task.TaskID] = now.Add(s.retention)
	} else {
		s.expiry[task.TaskID] = now.Add(time.Duration(task.TTL) * time.Second)
	}

	return nil
}

// timeOutLocked marks a working task whose tracking window has ended as
// failed, and starts its retention period. s.mu must be held.
func (s *TaskStore) timeOutLocked(taskID string, now time.Time) {
	task, ok := s.tasks[taskID]
	if !ok || task.Status.Terminal() {
		return
	}
	if expiry, ok := s.expiry[taskID]; !ok || !now.After(expiry) {
		return
	}
	t := task.clone()
	t.Status = TaskStatusFailed
	t.StatusMessage = timedOutMessage(t)
	t.LastUpdatedAt = now
	s.tasks[taskID] = t
	s.expiry[taskID] = now.Add(s.retention)
	log.Printf("task %s (%s): %s", taskID, t.ToolName, t.StatusMessage)
}

// timeOutAllLocked applies timeOutLocked to every task. s.mu must be held.
func (s *TaskStore) timeOutAllLocked(now time.Time) {
	for taskID := range s.tasks {
		s.timeOutLocked(taskID, now)
	}
}

// Get retrieves a task by ID
func (s *TaskStore) Get(taskID string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, exists := s.tasks[taskID]
	if !exists {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	now := time.Now()
	s.timeOutLocked(taskID, now)
	task = s.tasks[taskID]
	if expiry, ok := s.expiry[taskID]; ok && now.After(expiry) {
		return nil, fmt.Errorf("task expired: %s", taskID)
	}

	return task.clone(), nil
}

// Update stores task as the new state of an existing task and stamps
// LastUpdatedAt (on task and on the stored copy). A task that becomes
// terminal starts its retention period. A task that is already terminal is
// not changed (ErrTaskFinished).
func (s *TaskStore) Update(task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, exists := s.tasks[task.TaskID]
	if !exists {
		return fmt.Errorf("task not found: %s", task.TaskID)
	}
	if stored.Status.Terminal() {
		return ErrTaskFinished
	}

	now := time.Now()
	task.LastUpdatedAt = now
	s.tasks[task.TaskID] = task.clone()
	if task.Status.Terminal() {
		s.expiry[task.TaskID] = now.Add(s.retention)
	}

	return nil
}

// Save stores task as the new state of an existing task without changing
// LastUpdatedAt. The poller uses it for internal bookkeeping (the job summary,
// when the health wait began, poll counters) that is not a status change. A
// task that is already terminal is not changed (ErrTaskFinished).
func (s *TaskStore) Save(task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, exists := s.tasks[task.TaskID]
	if !exists {
		return fmt.Errorf("task not found: %s", task.TaskID)
	}
	if stored.Status.Terminal() {
		return ErrTaskFinished
	}
	s.tasks[task.TaskID] = task.clone()

	return nil
}

// List returns tasks with pagination support
func (s *TaskStore) List(cursor string, limit int) ([]*Task, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.timeOutAllLocked(now)

	// Collect all tasks still within their tracking or retention window
	var validTasks []*Task
	for taskID, task := range s.tasks {
		if expiry, ok := s.expiry[taskID]; ok && now.After(expiry) {
			continue // Skip expired
		}
		validTasks = append(validTasks, task.clone())
	}

	// Sort by creation time (newest first)
	sort.Slice(validTasks, func(i, j int) bool {
		return validTasks[i].CreatedAt.After(validTasks[j].CreatedAt)
	})

	// Apply cursor
	startIdx := 0
	if cursor != "" {
		for i, task := range validTasks {
			if task.TaskID == cursor {
				startIdx = i + 1
				break
			}
		}
	}

	// Apply limit
	if limit <= 0 {
		limit = 50 // Default
	}

	endIdx := startIdx + limit
	if endIdx > len(validTasks) {
		endIdx = len(validTasks)
	}

	result := validTasks[startIdx:endIdx]

	// Calculate next cursor
	nextCursor := ""
	if endIdx < len(validTasks) {
		nextCursor = validTasks[endIdx-1].TaskID
	}

	return result, nextCursor, nil
}

// GetActive returns all non-terminal tasks for polling. Tasks whose tracking
// window has ended are marked timed out first and are not returned.
func (s *TaskStore) GetActive() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.timeOutAllLocked(time.Now())

	var active []*Task
	for _, task := range s.tasks {
		if !task.Status.Terminal() {
			active = append(active, task.clone())
		}
	}

	return active
}

// CleanExpired marks working tasks past their tracking window as timed out
// and removes finished tasks whose retention period has ended.
func (s *TaskStore) CleanExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.timeOutAllLocked(now)
	for taskID, expiry := range s.expiry {
		if now.After(expiry) {
			delete(s.tasks, taskID)
			delete(s.expiry, taskID)
		}
	}
}

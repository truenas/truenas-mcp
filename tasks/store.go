package tasks

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// TaskStore provides thread-safe storage for tasks with TTL-based expiry.
//
// The store owns the tasks it holds: Add, Update and Save keep a private copy
// and Get, List and GetActive hand out copies. Callers (the poller, tools
// marshalling a task for tasks_get) therefore never share a *Task with each
// other, and nothing mutates a stored task outside the store lock. Map and
// pointer fields of a Task (Job, Result, Arguments, JobID) are shared between
// copies and must be replaced, not modified in place.
type TaskStore struct {
	mu     sync.RWMutex
	tasks  map[string]*Task
	expiry map[string]time.Time
}

// NewTaskStore creates a new task store
func NewTaskStore() *TaskStore {
	return &TaskStore{
		tasks:  make(map[string]*Task),
		expiry: make(map[string]time.Time),
	}
}

// Add stores a task and sets its expiry time
func (s *TaskStore) Add(task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if task.TaskID == "" {
		return fmt.Errorf("task ID cannot be empty")
	}

	s.tasks[task.TaskID] = task.clone()
	s.expiry[task.TaskID] = time.Now().Add(time.Duration(task.TTL) * time.Second)

	return nil
}

// Get retrieves a task by ID
func (s *TaskStore) Get(taskID string) (*Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, exists := s.tasks[taskID]
	if !exists {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	// Check if expired
	if expiry, ok := s.expiry[taskID]; ok && time.Now().After(expiry) {
		return nil, fmt.Errorf("task expired: %s", taskID)
	}

	return task.clone(), nil
}

// Update stores task as the new state of an existing task and stamps
// LastUpdatedAt (on task and on the stored copy).
func (s *TaskStore) Update(task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.tasks[task.TaskID]; !exists {
		return fmt.Errorf("task not found: %s", task.TaskID)
	}

	task.LastUpdatedAt = time.Now()
	s.tasks[task.TaskID] = task.clone()

	return nil
}

// Save stores task as the new state of an existing task without changing
// LastUpdatedAt. The poller uses it for internal bookkeeping (the job summary,
// when the health wait began) that is not a status change.
func (s *TaskStore) Save(task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.tasks[task.TaskID]; !exists {
		return fmt.Errorf("task not found: %s", task.TaskID)
	}
	s.tasks[task.TaskID] = task.clone()

	return nil
}

// List returns tasks with pagination support
func (s *TaskStore) List(cursor string, limit int) ([]*Task, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Collect all non-expired tasks
	var validTasks []*Task
	now := time.Now()
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

// GetActive returns all non-terminal tasks for polling
func (s *TaskStore) GetActive() []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var active []*Task
	now := time.Now()

	for taskID, task := range s.tasks {
		// Skip expired
		if expiry, ok := s.expiry[taskID]; ok && now.After(expiry) {
			continue
		}

		// Include only non-terminal states
		if task.Status == TaskStatusWorking || task.Status == TaskStatusInputRequired {
			active = append(active, task.clone())
		}
	}

	return active
}

// CleanExpired removes expired tasks from storage
func (s *TaskStore) CleanExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for taskID, expiry := range s.expiry {
		if now.After(expiry) {
			delete(s.tasks, taskID)
			delete(s.expiry, taskID)
		}
	}
}

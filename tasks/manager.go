package tasks

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/truenas/truenas-mcp/truenas"
)

// Manager orchestrates task lifecycle and background polling
type Manager struct {
	client *truenas.Client
	store  *TaskStore
	poller *Poller
	config PollerConfig
	ctx    context.Context
	cancel context.CancelFunc
}

// NewManager creates a new task manager
func NewManager(client *truenas.Client, config PollerConfig) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	store := NewTaskStore()
	poller := NewPoller(client, store, config)

	return &Manager{
		client: client,
		store:  store,
		poller: poller,
		config: config,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start begins background polling and cleanup
func (m *Manager) Start() {
	// Start the poller
	go m.poller.Run(m.ctx)

	// Start cleanup routine
	go m.cleanupLoop()
}

// Shutdown gracefully stops background operations
func (m *Manager) Shutdown() {
	m.cancel()
}

// cleanupLoop periodically removes expired tasks
func (m *Manager) cleanupLoop() {
	ticker := time.NewTicker(m.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.store.CleanExpired()
		}
	}
}

// CreateJobTask creates a task for a job-based operation
func (m *Manager) CreateJobTask(toolName string, args map[string]interface{}, jobID int, ttl time.Duration) (*Task, error) {
	task := &Task{
		TaskID:        uuid.New().String(),
		Status:        TaskStatusWorking,
		CreatedAt:     time.Now(),
		LastUpdatedAt: time.Now(),
		TTL:           int64(ttl.Seconds()),
		PollInterval:  int64(m.config.PollInterval.Seconds()),
		OperationType: OperationTypeJob,
		JobID:         &jobID,
		ToolName:      toolName,
		Arguments:     args,
	}

	if err := m.store.Add(task); err != nil {
		return nil, fmt.Errorf("failed to store task: %w", err)
	}

	return task, nil
}

// CreateStatusTask creates a task for a status-based operation
func (m *Manager) CreateStatusTask(toolName string, args map[string]interface{}, statusMethod string, ttl time.Duration) (*Task, error) {
	task := &Task{
		TaskID:        uuid.New().String(),
		Status:        TaskStatusWorking,
		CreatedAt:     time.Now(),
		LastUpdatedAt: time.Now(),
		TTL:           int64(ttl.Seconds()),
		PollInterval:  int64(m.config.PollInterval.Seconds()),
		OperationType: OperationTypeStatus,
		StatusMethod:  statusMethod,
		ToolName:      toolName,
		Arguments:     args,
	}

	if err := m.store.Add(task); err != nil {
		return nil, fmt.Errorf("failed to store task: %w", err)
	}

	return task, nil
}

// Get retrieves a task by ID
func (m *Manager) Get(taskID string) (*Task, error) {
	return m.store.Get(taskID)
}

// List returns tasks with pagination
func (m *Manager) List(cursor string, limit int) ([]*Task, string, error) {
	return m.store.List(cursor, limit)
}

// Cancel attempts to cancel a task
func (m *Manager) Cancel(taskID string) (*Task, error) {
	task, err := m.store.Get(taskID)
	if err != nil {
		return nil, err
	}

	// Only cancel non-terminal tasks
	if task.Status == TaskStatusCompleted || task.Status == TaskStatusFailed || task.Status == TaskStatusCancelled {
		return nil, fmt.Errorf("task is already in terminal state: %s", task.Status)
	}

	// For job-based tasks, try to abort the job
	if task.OperationType == OperationTypeJob && task.JobID != nil {
		_, err := m.client.Call("core.job_abort", *task.JobID)
		if err != nil {
			// Log but don't fail - job might already be done
		}
	}

	// Update task status
	task.Status = TaskStatusCancelled
	task.StatusMessage = "Cancelled by user"
	if err := m.store.Update(task); err != nil {
		return nil, fmt.Errorf("failed to update task: %w", err)
	}

	return task, nil
}

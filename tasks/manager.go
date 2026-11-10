package tasks

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/truenas/truenas-mcp/internal/safego"
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

	// cleanExpired removes expired tasks; nil means the store's. Tests
	// replace it to inject failures.
	cleanExpired func()
}

// NewManager creates a new task manager
func NewManager(client *truenas.Client, config PollerConfig) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	store := NewTaskStore()
	store.SetRetention(config.Retention)
	store.SetMaxFinished(config.MaxFinished)
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
			m.cleanOnce()
		}
	}
}

// TaskOption customises a task at creation.
type TaskOption func(*Task)

// WithAppHealthCheck makes a job task verify the app after the job succeeds:
// the task stays "working" until app.query reports the app RUNNING (with no crashed or
// starting container), and fails with "job finished but app is <state>"
// if that does not happen within timeout. It also records a bounded job
// summary on the task.
func WithAppHealthCheck(appName string, timeout time.Duration) TaskOption {
	return func(t *Task) {
		t.VerifyApp = appName
		t.VerifyTimeout = timeout
	}
}

// WithExpectedMethod sets the job method the tool's start call creates,
// so identity checks use the same source as the write plan.
func WithExpectedMethod(method string) TaskOption {
	return func(t *Task) {
		t.expectedMethod = method
	}
}

// PollNow polls every active task once, outside the background schedule.
func (m *Manager) PollNow() {
	m.poller.pollAllTasks(m.ctx)
}

// cleanOnce runs one cleanup pass. A panic is logged to stderr and the next
// tick tries again, so the loop never dies silently.
func (m *Manager) cleanOnce() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("task cleanup panicked: %s\n%s", safego.Describe(r), debug.Stack())
		}
	}()
	if m.cleanExpired != nil {
		m.cleanExpired()
		return
	}
	m.store.CleanExpired()
}

// CreateJobTask creates a task for a job-based operation. It reads the job
// once to record its identity (method and start time) and the client's
// session generation, so that the poller can tell the job apart from a later
// job that reuses its id after a middleware restart.
func (m *Manager) CreateJobTask(ctx context.Context, toolName string, args map[string]interface{}, jobID int, ttl time.Duration, opts ...TaskOption) (*Task, error) {
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

	for _, opt := range opts {
		opt(task)
	}

	if m.client != nil {
		m.poller.recordIdentity(ctx, task)
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

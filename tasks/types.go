package tasks

import (
	"time"
)

// TaskStatus represents the current state of a task
type TaskStatus string

const (
	TaskStatusWorking       TaskStatus = "working"
	TaskStatusInputRequired TaskStatus = "input_required"
	TaskStatusCompleted     TaskStatus = "completed"
	TaskStatusFailed        TaskStatus = "failed"
	TaskStatusCancelled     TaskStatus = "cancelled"
)

// OperationType indicates how to poll for task updates
type OperationType string

const (
	OperationTypeJob    OperationType = "job"    // Poll core.get_jobs
	OperationTypeStatus OperationType = "status" // Poll custom status endpoint
)

// Task represents a long-running operation
type Task struct {
	TaskID        string     `json:"taskId"`
	Status        TaskStatus `json:"status"`
	StatusMessage string     `json:"statusMessage,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastUpdatedAt time.Time  `json:"lastUpdatedAt"`
	TTL           int64      `json:"ttl"`          // Seconds until expiry
	PollInterval  int64      `json:"pollInterval"` // Seconds between polls

	// Internal fields (not exposed in JSON)
	OperationType OperationType          `json:"-"`
	JobID         *int                   `json:"-"` // For job-based ops
	StatusMethod  string                 `json:"-"` // For status-based ops
	ToolName      string                 `json:"-"`
	Arguments     map[string]interface{} `json:"-"`
	Result        interface{}            `json:"-"`
	Error         error                  `json:"-"`
}

// PollerConfig configures the background polling behavior
type PollerConfig struct {
	PollInterval    time.Duration // How often to poll TrueNAS
	MaxPollAttempts int           // 0 = unlimited
	CleanupInterval time.Duration // How often to clean expired tasks
}

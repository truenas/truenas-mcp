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
	OperationTypeJob OperationType = "job" // Poll core.get_jobs
)

// Task represents a long-running operation tracked through a TrueNAS job.
//
// TTL is how long the task is tracked while it is still working, counted from
// CreatedAt. A task that is still working when it runs out is marked failed
// ("tracking timed out"). A task that has finished (completed, failed or
// cancelled) stays readable for the store's retention period, counted from
// when it finished.
type Task struct {
	TaskID        string     `json:"taskId"`
	Status        TaskStatus `json:"status"`
	StatusMessage string     `json:"statusMessage,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastUpdatedAt time.Time  `json:"lastUpdatedAt"`
	TTL           int64      `json:"ttl"`          // Seconds the task is tracked while working
	PollInterval  int64      `json:"pollInterval"` // Seconds between polls

	// ToolName is the tool that started the task; JobID the TrueNAS job it
	// tracks.
	ToolName string `json:"tool,omitempty"`
	JobID    *int   `json:"job_id,omitempty"`

	// Job is a bounded summary of the underlying middleware job (state,
	// error, progress, RFC 3339 times, app name and versions). Only tasks
	// created with WithAppHealthCheck carry it.
	Job map[string]interface{} `json:"job,omitempty"`

	// Result is the job's result once it succeeded, bounded by BoundResult.
	// ResultTruncated says it was cut; ResultSizeBytes is then the size of
	// the full result. Tasks with a Job summary report the result there.
	Result          interface{} `json:"result,omitempty"`
	ResultTruncated bool        `json:"result_truncated,omitempty"`
	ResultSizeBytes int         `json:"result_size_bytes,omitempty"`

	// Internal fields (not exposed in JSON)
	OperationType OperationType          `json:"-"`
	Arguments     map[string]interface{} `json:"-"` // may hold credentials; never returned

	// Job identity, recorded when the task is created. Job ids restart at 1
	// when the middleware restarts, so the id alone does not identify the
	// job: every poll also requires the same method and start time.
	jobMethod     string
	jobStarted    string // time_started as JSON
	identityKnown bool
	sessionGen    uint64 // client session generation when created
	genKnown      bool

	// Consecutive polls that found no job row, and that failed outright.
	missingPolls int
	pollErrors   int

	// App health check, set by WithAppHealthCheck: once the job succeeds the
	// task only completes after this app reports RUNNING.
	VerifyApp       string        `json:"-"`
	VerifyTimeout   time.Duration `json:"-"`
	VerifyStartedAt time.Time     `json:"-"`
}

// Terminal reports whether status is final: the task is no longer polled.
func (s TaskStatus) Terminal() bool {
	return s == TaskStatusCompleted || s == TaskStatusFailed || s == TaskStatusCancelled
}

// clone returns a shallow copy of t. Maps and pointers inside are shared and
// treated as immutable (see TaskStore).
func (t *Task) clone() *Task {
	c := *t
	return &c
}

// PollerConfig configures the background polling behavior
type PollerConfig struct {
	PollInterval    time.Duration // How often to poll TrueNAS
	CleanupInterval time.Duration // How often to clean expired tasks

	// MissingJobPolls is how many consecutive polls may find no row for the
	// job before the task fails (0 means DefaultMissingJobPolls).
	MissingJobPolls int
	// PollErrorPolls is how many consecutive failed polls are reported in
	// statusMessage and logged (0 means DefaultPollErrorPolls). The task
	// keeps working; it is still bounded by its TTL.
	PollErrorPolls int
	// Retention is how long a finished task stays readable (0 means
	// DefaultRetention).
	Retention time.Duration
}

const (
	DefaultMissingJobPolls = 3
	DefaultPollErrorPolls  = 3
	DefaultRetention       = 24 * time.Hour
)

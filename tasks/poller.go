package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/truenas/truenas-mcp/truenas"
)

// Poller handles background polling of TrueNAS for task updates
type Poller struct {
	client *truenas.Client
	store  *TaskStore
	config PollerConfig
}

// NewPoller creates a new poller
func NewPoller(client *truenas.Client, store *TaskStore, config PollerConfig) *Poller {
	return &Poller{
		client: client,
		store:  store,
		config: config,
	}
}

// Run is the main polling loop
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollAllTasks()
		}
	}
}

// pollAllTasks polls all active tasks
func (p *Poller) pollAllTasks() {
	activeTasks := p.store.GetActive()

	for _, task := range activeTasks {
		switch task.OperationType {
		case OperationTypeJob:
			p.pollJobTask(task)
		case OperationTypeStatus:
			p.pollStatusTask(task)
		}
	}
}

// pollJobTask polls a job-based task using core.get_jobs
func (p *Poller) pollJobTask(task *Task) {
	if task.JobID == nil {
		return
	}

	// Query job status
	result, err := p.client.Call("core.get_jobs", []interface{}{
		[]interface{}{"id", "=", *task.JobID},
	})
	if err != nil {
		// Don't fail the task on network errors, just skip this poll
		return
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(result, &jobs); err != nil {
		return
	}

	if len(jobs) == 0 {
		return
	}

	p.updateTaskFromJob(task, jobs[0])
}

// pollStatusTask polls a status-based task using custom status endpoint
func (p *Poller) pollStatusTask(task *Task) {
	if task.StatusMethod == "" {
		return
	}

	// Call the status method
	result, err := p.client.Call(task.StatusMethod)
	if err != nil {
		return
	}

	var status map[string]interface{}
	if err := json.Unmarshal(result, &status); err != nil {
		return
	}

	p.updateTaskFromStatus(task, status)
}

// updateTaskFromJob updates task state based on TrueNAS job state
func (p *Poller) updateTaskFromJob(task *Task, job map[string]interface{}) {
	state, ok := job["state"].(string)
	if !ok {
		return
	}

	var newStatus TaskStatus
	var statusMessage string

	switch state {
	case "RUNNING", "WAITING":
		newStatus = TaskStatusWorking
		if progress, ok := job["progress"].(map[string]interface{}); ok {
			if percent, ok := progress["percent"].(float64); ok {
				statusMessage = fmt.Sprintf("Progress: %.1f%%", percent)
			}
			if desc, ok := progress["description"].(string); ok && desc != "" {
				statusMessage = desc
			}
		}

	case "SUCCESS":
		newStatus = TaskStatusCompleted
		statusMessage = "Job completed successfully"
		if result, ok := job["result"]; ok {
			task.Result = result
		}

	case "FAILED":
		newStatus = TaskStatusFailed
		if errMsg, ok := job["error"].(string); ok {
			statusMessage = errMsg
		} else {
			statusMessage = "Job failed"
		}

	case "ABORTED":
		newStatus = TaskStatusCancelled
		statusMessage = "Job was aborted"

	default:
		return // Unknown state, don't update
	}

	// Update task if state changed
	if task.Status != newStatus || task.StatusMessage != statusMessage {
		task.Status = newStatus
		task.StatusMessage = statusMessage
		p.store.Update(task)
	}
}

// updateTaskFromStatus updates task state based on custom status endpoint
func (p *Poller) updateTaskFromStatus(task *Task, status map[string]interface{}) {
	// Generic status parsing - can be extended per status endpoint
	// Look for common fields like "state", "status", "progress"

	var newStatus TaskStatus
	var statusMessage string

	// Try to find state/status field
	if state, ok := status["state"].(string); ok {
		switch state {
		case "RUNNING", "IN_PROGRESS":
			newStatus = TaskStatusWorking
		case "FINISHED", "COMPLETED":
			newStatus = TaskStatusCompleted
		case "FAILED", "ERROR":
			newStatus = TaskStatusFailed
		default:
			return
		}
	} else if statusStr, ok := status["status"].(string); ok {
		switch statusStr {
		case "RUNNING", "IN_PROGRESS":
			newStatus = TaskStatusWorking
		case "FINISHED", "COMPLETED":
			newStatus = TaskStatusCompleted
		case "FAILED", "ERROR":
			newStatus = TaskStatusFailed
		default:
			return
		}
	} else {
		// No recognized status field
		return
	}

	// Try to get message
	if msg, ok := status["message"].(string); ok {
		statusMessage = msg
	} else if desc, ok := status["description"].(string); ok {
		statusMessage = desc
	}

	// Update task if state changed
	if task.Status != newStatus || task.StatusMessage != statusMessage {
		task.Status = newStatus
		task.StatusMessage = statusMessage
		task.Result = status
		p.store.Update(task)
	}
}

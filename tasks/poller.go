package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

	if task.VerifyApp != "" {
		// Bounded, readable view of the job; the raw result can be an entire
		// app entry.
		task.Job = SummarizeJob(job)
	}

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
		if task.VerifyApp != "" {
			// A finished job is not a healthy app: hold the task open until
			// the app is observed running, or report what state it is in.
			task.Result = task.Job
			newStatus, statusMessage = p.verifyApp(task)
		}

	case "FAILED":
		newStatus = TaskStatusFailed
		if errMsg, ok := job["error"].(string); ok {
			statusMessage = errMsg
			if task.VerifyApp != "" {
				statusMessage, _ = TruncateText(errMsg, MaxJobTextChars)
			}
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

// verifyApp decides the task status after its job succeeded, from the app's
// live state. It returns working while the app is still coming up and the
// timeout has not elapsed, completed once the app is RUNNING with no crashed
// or starting container, and failed otherwise.
func (p *Poller) verifyApp(task *Task) (TaskStatus, string) {
	if task.VerifyStartedAt.IsZero() {
		task.VerifyStartedAt = time.Now()
	}
	expired := time.Since(task.VerifyStartedAt) >= task.VerifyTimeout

	state, notRunning, version, err := p.appHealth(task.VerifyApp)
	if err == nil && state == "RUNNING" && len(notRunning) == 0 {
		msg := fmt.Sprintf("Upgrade completed; app %s is RUNNING", task.VerifyApp)
		if version != "" {
			msg += " (version " + version + ")"
		}
		return TaskStatusCompleted, msg
	}

	var what string
	switch {
	case err != nil:
		what = "unknown"
		if state != "" {
			what = state
		}
	case state != "RUNNING":
		what = state
	default:
		what = "RUNNING with containers not healthy: " + strings.Join(notRunning, ", ")
	}
	if expired {
		if err != nil {
			return TaskStatusFailed, fmt.Sprintf("upgrade job finished but app state could not be verified: %v", err)
		}
		return TaskStatusFailed, fmt.Sprintf("upgrade job finished but app is %s", what)
	}
	return TaskStatusWorking, fmt.Sprintf("Upgrade job finished; waiting for app %s to be RUNNING (currently %s)", task.VerifyApp, what)
}

// appHealth queries the app and returns its state, the containers that are
// crashed or still starting, and its human version. TrueNAS exposes no container health
// probe in app.query, so container state is the health signal.
func (p *Poller) appHealth(appName string) (state string, notRunning []string, version string, err error) {
	raw, err := p.client.Call("app.query", []interface{}{
		[]interface{}{"name", "=", appName},
	})
	if err != nil {
		return "", nil, "", err
	}
	var apps []map[string]interface{}
	if err := json.Unmarshal(raw, &apps); err != nil {
		return "", nil, "", err
	}
	if len(apps) == 0 {
		return "", nil, "", fmt.Errorf("app %s not found", appName)
	}
	app := apps[0]
	state, _ = app["state"].(string)
	version, _ = app["human_version"].(string)
	if aw, ok := app["active_workloads"].(map[string]interface{}); ok {
		details, _ := aw["container_details"].([]interface{})
		for _, d := range details {
			c, ok := d.(map[string]interface{})
			if !ok {
				continue
			}
			// Init and one-shot containers legitimately end up "exited" or
			// "created" in a healthy app; only crashed or still-starting
			// containers count against it.
			if cs, _ := c["state"].(string); cs == "crashed" || cs == "starting" {
				name, _ := c["service_name"].(string)
				if name == "" {
					name = "container"
				}
				notRunning = append(notRunning, fmt.Sprintf("%s (%s)", name, cs))
			}
		}
	}
	if state == "" {
		return "", nil, version, fmt.Errorf("app %s reported no state", appName)
	}
	return state, notRunning, version, nil
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

package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/internal/safego"
	"github.com/truenas/truenas-mcp/truenas"
)

// Poller handles background polling of TrueNAS for task updates
type Poller struct {
	client *truenas.Client
	store  *TaskStore
	config PollerConfig

	// pollTask polls one task; nil means the default. Tests replace it to
	// inject failures.
	pollTask func(*Task)
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
			p.pollAllTasksSafely()
		}
	}
}

// pollAllTasksSafely runs one polling pass. A panic must not end the loop
// (and with it all task updates, or the whole process): it is logged to
// stderr and the next tick polls again.
func (p *Poller) pollAllTasksSafely() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("task poller panicked: %s\n%s", safego.Describe(r), debug.Stack())
		}
	}()
	p.pollAllTasks()
}

// pollAllTasks polls all active tasks
func (p *Poller) pollAllTasks() {
	activeTasks := p.store.GetActive()

	for _, task := range activeTasks {
		p.pollOne(task)
	}
}

// pollOne polls a single task. If that panics, the task is marked failed so
// it is not retried (and does not panic again) on every tick, and the
// remaining tasks are still polled.
func (p *Poller) pollOne(task *Task) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("task poller panicked on task %s: %s\n%s", task.TaskID, safego.Describe(r), debug.Stack())
			task.Status = TaskStatusFailed
			task.StatusMessage = "Internal error while polling this task; its state on TrueNAS is unknown"
			p.store.Update(task)
		}
	}()
	if p.pollTask != nil {
		p.pollTask(task)
		return
	}
	switch task.OperationType {
	case OperationTypeJob:
		p.pollJobTask(task)
	case OperationTypeStatus:
		p.pollStatusTask(task)
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
				statusMessage, _ = TruncateText(errMsg, MaxJobTextBytes)
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
	} else if task.VerifyApp != "" {
		// Not a status change, but the job summary and the start of the
		// health wait live on the task and must reach the stored copy.
		p.store.Save(task)
	}
}

// verifyApp decides the task status after its job succeeded, from the app's
// live state. It returns working while the app is still coming up and the
// timeout has not elapsed, completed once the app is RUNNING with no crashed
// or starting container, and failed otherwise. A failure because the app was
// still starting is worded as such: the upgrade itself succeeded and the app
// may simply be slow, so the caller does not roll back or retry a healthy
// upgrade.
func (p *Poller) verifyApp(task *Task) (TaskStatus, string) {
	if task.VerifyStartedAt.IsZero() {
		task.VerifyStartedAt = time.Now()
	}
	expired := time.Since(task.VerifyStartedAt) >= task.VerifyTimeout

	h, err := p.appHealth(task.VerifyApp)
	if err == nil && h.state == "RUNNING" && len(h.crashed) == 0 && len(h.starting) == 0 {
		msg := fmt.Sprintf("Upgrade completed; app %s is RUNNING", task.VerifyApp)
		if h.version != "" {
			msg += " (version " + h.version + ")"
		}
		return TaskStatusCompleted, msg
	}

	var what string
	switch {
	case err != nil:
		what = "unknown"
		if h.state != "" {
			what = h.state
		}
	case h.state != "RUNNING":
		what = h.state
	default:
		what = "RUNNING with containers not healthy: " + strings.Join(append(append([]string{}, h.crashed...), h.starting...), ", ")
	}
	if expired {
		if err != nil {
			return TaskStatusFailed, fmt.Sprintf("upgrade job finished but app state could not be verified: %v", err)
		}
		// DEPLOYING, or RUNNING with only containers still starting (a
		// healthcheck that has not passed yet), and nothing crashed.
		if len(h.crashed) == 0 && (h.state == "DEPLOYING" || (h.state == "RUNNING" && len(h.starting) > 0)) {
			return TaskStatusFailed, fmt.Sprintf(
				"upgrade job succeeded, but app %s was still starting (%s) when the %ds health wait ended. "+
					"This is not an upgrade failure: the app may just be slow to start. "+
					"Check it with query_apps, or raise health_timeout_seconds (maximum 600) on upgrade_app",
				task.VerifyApp, what, int(task.VerifyTimeout/time.Second))
		}
		return TaskStatusFailed, fmt.Sprintf("upgrade job finished but app is %s", what)
	}
	return TaskStatusWorking, fmt.Sprintf("Upgrade job finished; waiting for app %s to be RUNNING (currently %s)", task.VerifyApp, what)
}

// appHealthInfo is an app's state as seen by the health wait.
type appHealthInfo struct {
	state    string   // app state (RUNNING, DEPLOYING, ...)
	crashed  []string // "service (crashed)" for each crashed container
	starting []string // "service (starting)" for each starting container
	version  string   // human version
}

// appHealth queries the app and returns its state, the containers that are
// crashed or starting, and its human version. app.query reflects container
// health itself: a running container whose healthcheck is not "healthy" is
// reported as "starting", which also makes the app DEPLOYING rather than
// RUNNING. The container check here is therefore a safeguard on top of the
// app state, not a separate signal. "created" containers also make an app
// DEPLOYING, so they cannot appear in a RUNNING app; "exited" ones (init and
// one-shot containers) are legitimate and are not counted.
func (p *Poller) appHealth(appName string) (appHealthInfo, error) {
	var h appHealthInfo
	raw, err := p.client.Call("app.query", []interface{}{
		[]interface{}{"name", "=", appName},
	})
	if err != nil {
		return h, err
	}
	var apps []map[string]interface{}
	if err := json.Unmarshal(raw, &apps); err != nil {
		return h, err
	}
	if len(apps) == 0 {
		return h, fmt.Errorf("app %s not found", appName)
	}
	app := apps[0]
	h.state, _ = app["state"].(string)
	h.version, _ = app["human_version"].(string)
	if aw, ok := app["active_workloads"].(map[string]interface{}); ok {
		details, _ := aw["container_details"].([]interface{})
		for _, d := range details {
			c, ok := d.(map[string]interface{})
			if !ok {
				continue
			}
			cs, _ := c["state"].(string)
			if cs != "crashed" && cs != "starting" {
				continue
			}
			name, _ := c["service_name"].(string)
			if name == "" {
				name = "container"
			}
			entry := fmt.Sprintf("%s (%s)", name, cs)
			if cs == "crashed" {
				h.crashed = append(h.crashed, entry)
			} else {
				h.starting = append(h.starting, entry)
			}
		}
	}
	if h.state == "" {
		return h, fmt.Errorf("app %s reported no state", appName)
	}
	return h, nil
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

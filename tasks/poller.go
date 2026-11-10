package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/internal/safego"
	"github.com/truenas/truenas-mcp/redact"
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
	if task.OperationType == OperationTypeJob {
		p.pollJobTask(task)
	}
}

func (p *Poller) missingJobPolls() int {
	if p.config.MissingJobPolls > 0 {
		return p.config.MissingJobPolls
	}
	return DefaultMissingJobPolls
}

func (p *Poller) pollErrorPolls() int {
	if p.config.PollErrorPolls > 0 {
		return p.config.PollErrorPolls
	}
	return DefaultPollErrorPolls
}

// readJob reads one job row by id. It returns nil (and no error) when
// core.get_jobs has no such job, and the client's session generation after
// the read, so a reconnect during the read is seen as a new session.
func (p *Poller) readJob(jobID int) (job map[string]interface{}, gen uint64, err error) {
	result, err := p.client.Call("core.get_jobs", []interface{}{
		[]interface{}{"id", "=", jobID},
	})
	if err != nil {
		return nil, 0, err
	}
	var jobs []map[string]interface{}
	if err := json.Unmarshal(result, &jobs); err != nil {
		return nil, 0, fmt.Errorf("unexpected core.get_jobs result: %w", err)
	}
	gen, err = p.client.SessionGeneration()
	if err != nil {
		return nil, 0, err
	}
	if len(jobs) == 0 {
		return nil, gen, nil
	}
	return jobs[0], gen, nil
}

// jobIdentity returns the fields that tell one job apart from a later job
// with the same id: its method and its start time (as JSON).
func jobIdentity(job map[string]interface{}) (method, started string) {
	method, _ = job["method"].(string)
	if v, ok := job["time_started"]; ok && v != nil {
		if b, err := json.Marshal(v); err == nil {
			started = string(b)
		}
	}
	return method, started
}

// recordIdentity reads the task's job once, at creation, and records its
// identity and the session it was read in. If the read fails the identity is
// recorded by the first poll that still sees the same session.
func (p *Poller) recordIdentity(task *Task) {
	if task.JobID == nil {
		return
	}
	job, gen, err := p.readJob(*task.JobID)
	if err != nil {
		log.Printf("task %s (%s): could not read job %d at creation: %s", task.TaskID, task.ToolName, *task.JobID, redact.Text(err.Error()))
		if g, gerr := p.client.SessionGeneration(); gerr == nil {
			task.sessionGen, task.genKnown = g, true
		}
		return
	}
	task.sessionGen, task.genKnown = gen, true
	if job != nil {
		task.jobMethod, task.jobStarted = jobIdentity(job)
		task.identityKnown = true
		if !methodMatches(task.ToolName, task.jobMethod) {
			// TrueNAS restarted between the start call and this read, and
			// the id now belongs to another job.
			task.Status = TaskStatusFailed
			task.StatusMessage = restartMessage(task, true)
			log.Printf("task %s (%s): %s", task.TaskID, task.ToolName, task.StatusMessage)
		}
	}
}

// failTask ends a task as failed with msg and logs why.
func (p *Poller) failTask(task *Task, msg string) {
	task.Status = TaskStatusFailed
	task.StatusMessage = msg
	log.Printf("task %s (%s): %s", task.TaskID, task.ToolName, msg)
	p.store.Update(task)
}

// pollJobTask polls a job-based task using core.get_jobs. The row must be the
// same job the task was created for (same method and start time); a job that
// is gone, or whose id now belongs to another job, ends the task as failed.
// Read errors keep the task working but are reported once they repeat.
func (p *Poller) pollJobTask(task *Task) {
	if task.JobID == nil {
		return
	}

	job, gen, err := p.readJob(*task.JobID)
	if err != nil {
		p.pollFailed(task, err)
		return
	}

	// The job was read, so a "could not read" message is no longer true.
	staleErrMsg := task.pollErrors >= p.pollErrorPolls()
	dirty := task.pollErrors != 0
	task.pollErrors = 0

	if job == nil {
		task.missingPolls++
		if task.missingPolls >= p.missingJobPolls() {
			p.failTask(task, restartMessage(task, false))
			return
		}
		if staleErrMsg {
			task.StatusMessage = ""
			p.store.Update(task)
			return
		}
		p.store.Save(task)
		return
	}
	if task.missingPolls != 0 {
		task.missingPolls = 0
		dirty = true
	}

	method, started := jobIdentity(job)
	switch {
	case task.identityKnown:
		if method != task.jobMethod || started != task.jobStarted {
			p.failTask(task, restartMessage(task, true))
			return
		}
	case task.genKnown && gen == task.sessionGen:
		// The creation read failed, but this is still the session the job
		// was started in, so the row is that job if it runs the method the
		// tool starts.
		if !methodMatches(task.ToolName, method) {
			p.failTask(task, restartMessage(task, true))
			return
		}
		task.jobMethod, task.jobStarted, task.identityKnown = method, started, true
		dirty = true
	default:
		// The session changed before the job's identity was recorded: the
		// row may belong to a job started after a restart.
		p.failTask(task, unconfirmedMessage(task))
		return
	}

	switch {
	case staleErrMsg:
		task.StatusMessage = ""
		p.store.Update(task)
	case dirty:
		p.store.Save(task)
	}
	p.updateTaskFromJob(task, job)
}

// pollFailed records a failed poll. After PollErrorPolls consecutive
// failures the task's statusMessage says so (it stays working) and the
// failure is logged, then again every PollErrorPolls failures.
func (p *Poller) pollFailed(task *Task, err error) {
	task.pollErrors++
	n := p.pollErrorPolls()
	if task.pollErrors < n {
		p.store.Save(task)
		return
	}
	text, _ := TruncateText(redact.Text(err.Error()), 256)
	if task.pollErrors%n == 0 {
		log.Printf("task %s (%s): could not read %s for %d consecutive polls: %s", task.TaskID, task.ToolName, jobIDText(task), task.pollErrors, text)
	}
	task.StatusMessage = fmt.Sprintf("Could not read %s from TrueNAS for %d consecutive polls (last error: %s); still tracking", jobIDText(task), task.pollErrors, text)
	p.store.Update(task)
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
		if task.VerifyApp != "" {
			// A finished job is not a healthy app: hold the task open until
			// the app is observed running, or report what state it is in.
			// The result is reported in the bounded job summary.
			newStatus, statusMessage = p.verifyApp(task)
		} else if result, ok := job["result"]; ok && result != nil {
			task.Result, task.ResultTruncated, task.ResultSizeBytes = BoundResult(result)
			if !task.ResultTruncated {
				task.ResultSizeBytes = 0
			}
		}

	case "FAILED":
		newStatus = TaskStatusFailed
		if errMsg, ok := job["error"].(string); ok {
			statusMessage, _ = BoundText(errMsg, MaxJobTextBytes)
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
	refreshJobApp(task, h, err)
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

// refreshJobApp replaces the app copy in the job summary with the live app
// just read by the health wait. The job result of app.upgrade is the app as
// it was when the job finished, typically still DEPLOYING with containers
// starting, which contradicts a task that has since seen the app RUNNING.
// app_source says where job.app came from; result_truncated and
// result_size_bytes keep describing the job's own result, which is still not
// returned in full. If the app could not be read, or app.query no longer
// lists it, the job's copy (or the last live copy) is kept and labelled as
// such, and app_checked_at is dropped. task.Job is replaced, not modified: stored copies share it.
func refreshJobApp(task *Task, h appHealthInfo, err error) {
	if task.Job == nil {
		return
	}
	job := make(map[string]interface{}, len(task.Job)+2)
	for k, v := range task.Job {
		job[k] = v
	}
	if err == nil && h.app != nil {
		job["app"] = SummarizeApp(h.app)
		job["app_source"] = "app.query"
		job["app_checked_at"] = time.Now().UTC().Format(time.RFC3339)
	} else {
		// The previous live copy, if any, is no longer current.
		delete(job, "app_checked_at")
		if _, ok := job["app"]; ok {
			if errors.Is(err, errAppNotFound) {
				job["app_source"] = "job result (app.query no longer lists the app)"
			} else {
				job["app_source"] = "job result (the live app state could not be read)"
			}
		}
	}
	task.Job = FitJobSummary(job)
}

// errAppNotFound is returned by appHealth when app.query succeeded but listed
// no app of that name.
var errAppNotFound = errors.New("not found")

// appHealthInfo is an app's state as seen by the health wait.
type appHealthInfo struct {
	state    string                 // app state (RUNNING, DEPLOYING, ...)
	crashed  []string               // "service (crashed)" for each crashed container
	starting []string               // "service (starting)" for each starting container
	version  string                 // human version
	app      map[string]interface{} // the app.query entry, when found
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
		return h, fmt.Errorf("app %s %w", appName, errAppNotFound)
	}
	app := apps[0]
	h.app = app
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

package tasks

import (
	"fmt"
	"time"
)

// checkTools names the tool that shows the current state of what a task's
// tool changed, for messages that tell the caller where to look once the task
// can no longer say.
var checkTools = map[string]string{
	"install_app":                 "query_apps",
	"update_app":                  "query_apps",
	"upgrade_app":                 "query_apps",
	"start_app":                   "query_apps",
	"stop_app":                    "query_apps",
	"delete_app":                  "query_apps",
	"run_scrub":                   "get_scrub_status",
	"download_update":             "update_status",
	"apply_update":                "system_info and update_status",
	"configure_directory_service": "get_directory_service_status",
	"leave_directory_service":     "get_directory_service_status",
}

// expectedMethods is the job method each tool's start call creates. A job
// row with another method under the returned id is not the job the tool
// started (TrueNAS restarted in between and the id was reused).
var expectedMethods = map[string]string{
	"install_app":                 "app.create",
	"update_app":                  "app.update",
	"upgrade_app":                 "app.upgrade",
	"start_app":                   "app.start",
	"stop_app":                    "app.stop",
	"delete_app":                  "app.delete",
	"run_scrub":                   "pool.scrub.scrub",
	"download_update":             "update.download",
	"apply_update":                "update.run",
	"configure_directory_service": "directoryservices.update",
	"leave_directory_service":     "directoryservices.leave",
}

// methodMatches reports whether a job with this method can be the one
// toolName started. Tools without an entry accept any method.
func methodMatches(toolName, method string) bool {
	want, ok := expectedMethods[toolName]
	return !ok || method == want
}

func checkTool(toolName string) string {
	if c, ok := checkTools[toolName]; ok {
		return c
	}
	return "query_jobs"
}

func jobIDText(task *Task) string {
	if task.JobID == nil {
		return "the job"
	}
	return fmt.Sprintf("job %d", *task.JobID)
}

// restartMessage is the terminal message for a task whose job is gone or
// whose id now belongs to a different job. reused says which.
func restartMessage(task *Task, reused bool) string {
	if task.ToolName == "apply_update" {
		return fmt.Sprintf("TrueNAS restarted, as expected when apply_update reboots, and %s can no longer be read; "+
			"the outcome of the update is not known from this task. Check the running version with system_info and the update state with update_status", jobIDText(task))
	}
	what := fmt.Sprintf("%s no longer exists on TrueNAS", jobIDText(task))
	if reused {
		what = fmt.Sprintf("%s on TrueNAS is now a different job", jobIDText(task))
	}
	return fmt.Sprintf("%s (the system or its middleware restarted), so the outcome of this operation is unknown; check the state with %s",
		what, checkTool(task.ToolName))
}

// unconfirmedMessage is the terminal message for a task whose job could not
// be read while the session it was started in lasted, so it cannot be told
// apart from a job started after a restart.
func unconfirmedMessage(task *Task) string {
	return fmt.Sprintf("the connection to TrueNAS was re-established before %s could be read, so it could not be confirmed that it is still the job this task started (TrueNAS or its middleware may have restarted); "+
		"the outcome of this operation is unknown; check the state with %s", jobIDText(task), checkTool(task.ToolName))
}

// timedOutMessage is the terminal message for a task still working when its
// TTL ran out.
func timedOutMessage(task *Task) string {
	ttl := time.Duration(task.TTL) * time.Second
	return fmt.Sprintf("tracking timed out after %s; the operation may still be running on TrueNAS. "+
		"Check the state with %s, or query_jobs for %s", ttl, checkTool(task.ToolName), jobIDText(task))
}

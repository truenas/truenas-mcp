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

// timedOutMessage is the terminal message for a task still working when its
// TTL ran out.
func timedOutMessage(task *Task) string {
	ttl := time.Duration(task.TTL) * time.Second
	return fmt.Sprintf("tracking timed out after %s; the operation may still be running on TrueNAS. "+
		"Check the state with %s, or query_jobs for %s", ttl, checkTool(task.ToolName), jobIDText(task))
}

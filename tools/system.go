package tools

// System information, health, update, and reboot tools and their handlers
// (system_info, system_health, check_updates, download_update, apply_update,
// update_status, system_reboot). system_reboot handlers live in
// dryrun_maintenance.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerSystemTools() {
	// System info tool
	r.tools["system_info"] = Tool{
		Definition: mcp.Tool{
			Name:        "system_info",
			Description: "Get TrueNAS system information including version, hostname, and platform details",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleSystemInfo,
	}

	// System health tool
	r.tools["system_health"] = Tool{
		Definition: mcp.Tool{
			Name:        "system_health",
			Description: "Get system health status including alerts and diagnostics",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleSystemHealth,
	}

	// System update tools
	r.tools["check_updates"] = Tool{
		Definition: mcp.Tool{
			Name:        "check_updates",
			Description: "Check for available TrueNAS system updates. Returns allow-listed fields for each available version (train, version, release notes and URL).",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleCheckUpdates,
	}

	r.tools["download_update"] = Tool{
		Definition: mcp.Tool{
			Name:        "download_update",
			Description: "Download TrueNAS system update. Supports dry-run mode to preview changes. Returns a task ID for tracking download progress. This is a write operation.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"train": map[string]interface{}{
						"type":        "string",
						"description": "Optional train name passed to update.download. Must be set together with version, or both omitted. When both are omitted, the download uses the update offered by the system's configured profile",
					},
					"version": map[string]interface{}{
						"type":        "string",
						"description": "Optional version passed to update.download. Must be set together with train, or both omitted",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview changes without executing (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: r.handleDownloadUpdateWithDryRun,
		Plan:    planDownloadUpdate,
	}

	r.tools["apply_update"] = Tool{
		Definition: mcp.Tool{
			Name:        "apply_update",
			Description: "Apply downloaded TrueNAS system update. System will reboot if reboot parameter is true. Supports dry-run mode to preview changes. Returns a task ID for tracking progress. This is a write operation. **Best Practice**: After successful update and reboot, use query_boot_environments to check for old boot environments that can be safely pruned with delete_boot_environment. Recommend keeping 2-3 recent boot environments for rollback safety.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"reboot": map[string]interface{}{
						"type":        "boolean",
						"description": "Reboot after update completes (default: false for safety)",
						"default":     false,
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview changes without executing (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: r.handleApplyUpdateWithDryRun,
		Plan:    planApplyUpdate,
		Execute: r.executeApplyUpdate,
	}

	r.tools["update_status"] = Tool{
		Definition: mcp.Tool{
			Name:        "update_status",
			Description: "Get current TrueNAS system update status and progress",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleUpdateStatus,
	}

	// System reboot tool
	r.tools["system_reboot"] = Tool{
		Definition: mcp.Tool{
			Name:        "system_reboot",
			Description: "Reboot the TrueNAS system. This will disconnect all active sessions and services. Use after applying system updates. Supports dry-run mode and an optional delay of up to 86400 seconds. A pending delayed reboot cannot be cancelled through this server.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"delay": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: seconds to wait before rebooting, 0-86400 (default: reboot immediately). Cannot be cancelled through this server once scheduled.",
						"minimum":     0,
						"maximum":     maxRebootDelay,
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview the reboot without executing (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: handleSystemRebootWithDryRun,
		Plan:    planSystemReboot,
		Execute: executeSystemReboot,
	}
}

// Tool handlers

func handleSystemInfo(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	result, err := client.Call(ctx, "system.info")
	if err != nil {
		return "", err
	}

	var info map[string]interface{}
	if err := json.Unmarshal(result, &info); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	// Datetimes (buildtime, boottime, datetime) arrive as {"$date": ms};
	// report them as ISO-8601 UTC strings like the other tools.
	formatted, err := json.MarshalIndent(readableDatesMap(info), "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleSystemHealth(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	// Get alerts
	result, err := client.Call(ctx, "alert.list")
	if err != nil {
		return "", err
	}

	var alerts []map[string]interface{}
	if err := json.Unmarshal(result, &alerts); err != nil {
		return "", fmt.Errorf("failed to parse alerts: %w", err)
	}

	// Get active jobs
	jobsResult, err := client.Call(ctx, "core.get_jobs", []interface{}{
		[]interface{}{"state", "=", "RUNNING"},
	})
	if err != nil {
		return "", fmt.Errorf("failed to get jobs: %w", err)
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(jobsResult, &jobs); err != nil {
		return "", fmt.Errorf("failed to parse jobs: %w", err)
	}

	// Create summary of active jobs
	activeTasks := make([]map[string]interface{}, 0)
	for _, job := range jobs {
		taskSummary := map[string]interface{}{
			"id":          job["id"],
			"method":      job["method"],
			"state":       job["state"],
			"description": job["description"],
		}
		if progress, ok := job["progress"]; ok {
			taskSummary["progress"] = progress
		}
		activeTasks = append(activeTasks, taskSummary)
	}

	// Add capacity warnings
	capacityWarnings := make([]string, 0)

	// Quick capacity check using reporting data (last hour)
	cpuResult, err := client.Call(ctx, "reporting.get_data", []interface{}{
		map[string]interface{}{
			"name":       "cpu",
			"identifier": nil,
		},
	}, map[string]interface{}{"unit": "HOUR"})
	if err == nil {
		var cpuData []map[string]interface{}
		if err := json.Unmarshal(cpuResult, &cpuData); err == nil && len(cpuData) > 0 {
			if dataPoints := columnPoints(cpuData[0], legendColumn(cpuData[0], "cpu", 1), true); len(dataPoints) > 0 {
				avgCPU := calculateAverage(dataPoints)
				if avgCPU > 85 {
					capacityWarnings = append(capacityWarnings,
						fmt.Sprintf("CPU utilization critical: %.1f%%", avgCPU))
				} else if avgCPU > 70 {
					capacityWarnings = append(capacityWarnings,
						fmt.Sprintf("CPU utilization elevated: %.1f%%", avgCPU))
				}
			}
		}
	}

	// Check memory
	sysInfoResult, err := client.Call(ctx, "system.info")
	var totalMemory float64
	if err == nil {
		var sysInfo map[string]interface{}
		if err := json.Unmarshal(sysInfoResult, &sysInfo); err == nil {
			if physMem, ok := sysInfo["physmem"].(float64); ok {
				totalMemory = physMem
			}
		}
	}

	arcFreshnessNote := ""
	if totalMemory > 0 {
		memResult, err := client.Call(ctx, "reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       "memory",
				"identifier": nil,
			},
		}, map[string]interface{}{"unit": "HOUR"})
		if err == nil {
			var memData []map[string]interface{}
			if err := json.Unmarshal(memResult, &memData); err == nil && len(memData) > 0 {
				if mem, err := availableMemorySamples(memData[0]); err == nil {
					dataPoints := mem.all
					// The "memory" graph reports available bytes, not used.
					// The ZFS ARC is reclaimable, so exclude it when known.
					// Only the latest ARC size is known, so it is compared with
					// the latest complete available sample, not the hourly average.
					avgAvailable := calculateAverage(dataPoints)
					label, suffix := "Memory utilization", " (may include ZFS ARC cache)"
					if arcBytes, freshKnown, err := currentArcBytes(ctx, client, nasReferenceTime(memData[0])); err == nil {
						avgAvailable = mem.latest + arcBytes
						label, suffix = "Memory utilization excluding ZFS ARC", ""
						if !freshKnown {
							arcFreshnessNote = arcFreshnessUnknownNote
						}
					}
					avgMemPct := memoryUsedPct(totalMemory, avgAvailable)
					if avgMemPct > 85 {
						capacityWarnings = append(capacityWarnings,
							fmt.Sprintf("%s critical: %.1f%%%s", label, avgMemPct, suffix))
					} else if avgMemPct > 70 {
						capacityWarnings = append(capacityWarnings,
							fmt.Sprintf("%s elevated: %.1f%%%s", label, avgMemPct, suffix))
					}
				}
			}
		}
	}

	// Check pool capacity
	poolResult, err := client.Call(ctx, "pool.query")
	if err == nil {
		var pools []map[string]interface{}
		if err := json.Unmarshal(poolResult, &pools); err == nil {
			for _, pool := range pools {
				poolName, _ := pool["name"].(string)
				capacity := calculatePoolCapacity(pool)

				if utilPct, ok := capacity["utilization_pct"].(float64); ok {
					if utilPct > 85 {
						capacityWarnings = append(capacityWarnings,
							fmt.Sprintf("Pool '%s' capacity critical: %.1f%%", poolName, utilPct))
					} else if utilPct > 70 {
						capacityWarnings = append(capacityWarnings,
							fmt.Sprintf("Pool '%s' capacity elevated: %.1f%%", poolName, utilPct))
					}
				}
			}
		}
	}

	// Check directory service status
	var directoryServiceStatus map[string]interface{}
	dirStatusResult, err := client.Call(ctx, "directoryservices.status")
	if err == nil {
		var dirStatus map[string]interface{}
		if err := json.Unmarshal(dirStatusResult, &dirStatus); err == nil {
			directoryServiceStatus = dirStatus

			// Add warnings for directory service issues
			if status, ok := dirStatus["status"].(string); ok && status != "" {
				if status == "FAULTED" {
					statusMsg := "connection error"
					if msg, ok := dirStatus["status_msg"].(string); ok && msg != "" {
						statusMsg = msg
					}
					serviceType := "directory service"
					if svcType, ok := dirStatus["type"].(string); ok && svcType != "" {
						serviceType = svcType
					}
					capacityWarnings = append(capacityWarnings,
						fmt.Sprintf("CRITICAL: Directory service (%s) is FAULTED: %s", serviceType, statusMsg))
				} else if status == "JOINING" || status == "LEAVING" {
					capacityWarnings = append(capacityWarnings,
						fmt.Sprintf("Directory service operation in progress: %s", status))
				}
			}
		}
	}

	// Dismissed alerts are acknowledged: count them separately, not as
	// active.
	activeAlerts, criticalAlerts, dismissedAlerts := 0, 0, 0
	for _, alert := range alerts {
		if dismissed, _ := alert["dismissed"].(bool); dismissed {
			dismissedAlerts++
			continue
		}
		activeAlerts++
		switch level, _ := alert["level"].(string); strings.ToUpper(level) {
		case "CRITICAL", "ALERT", "EMERGENCY":
			criticalAlerts++
		}
	}

	// Only active alerts are listed; dismissed ones are only counted
	// (list_alerts returns them with include_dismissed=true or dismissed=true).
	activeAlertList := make([]map[string]interface{}, 0, activeAlerts)
	for _, alert := range alerts {
		if dismissed, _ := alert["dismissed"].(bool); !dismissed {
			activeAlertList = append(activeAlertList, readableDatesMap(alert))
		}
	}

	response := map[string]interface{}{
		"alerts":                activeAlertList,
		"alert_count":           activeAlerts,
		"critical_alert_count":  criticalAlerts,
		"dismissed_alert_count": dismissedAlerts,
		"active_jobs":           activeTasks,
		"job_count":             len(activeTasks),
		"capacity_warnings":     capacityWarnings,
		"directory_service":     directoryServiceStatus,
		"health_check":          "OK",
	}

	if arcFreshnessNote != "" {
		response["arc_freshness_note"] = arcFreshnessNote
	}

	if activeAlerts > 0 {
		response["health_check"] = "ALERTS_PRESENT"
	}

	if len(activeTasks) > 0 {
		if response["health_check"] == "OK" {
			response["health_check"] = "ACTIVE_TASKS"
		} else {
			response["health_check"] = "ALERTS_AND_ACTIVE_TASKS"
		}
	}

	if len(capacityWarnings) > 0 {
		if response["health_check"] == "OK" {
			response["health_check"] = "CAPACITY_WARNINGS"
		} else {
			response["health_check"] = response["health_check"].(string) + "_AND_CAPACITY"
		}
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const updateRunMethod = "update.run"
const updateDownloadMethod = "update.download"

// System Update Handlers

// handleCheckUpdates checks for available TrueNAS system updates
func handleCheckUpdates(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	result, err := client.Call(ctx, "update.available_versions")
	if err != nil {
		return "", fmt.Errorf("failed to check for updates: %w", err)
	}

	var updates []map[string]interface{}
	if err := json.Unmarshal(result, &updates); err != nil {
		return "", fmt.Errorf("failed to parse update information: %w", err)
	}
	simplified := make([]map[string]interface{}, 0, len(updates))
	for _, item := range updates {
		simplified = append(simplified, simplifyAvailableVersion(item))
	}

	formatted, err := json.MarshalIndent(simplified, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// handleUpdateStatus gets current system update status
func handleUpdateStatus(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	result, err := client.Call(ctx, "update.status")
	if err != nil {
		return "", fmt.Errorf("failed to get update status: %w", err)
	}

	var status interface{}
	if err := json.Unmarshal(result, &status); err != nil {
		return "", fmt.Errorf("failed to parse update status: %w", err)
	}

	formatted, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// handleDownloadUpdate downloads a TrueNAS system update
func (r *Registry) handleDownloadUpdate(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planDownloadUpdate(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	return r.executeDownloadUpdate(ctx, client, args, plan)
}

// handleDownloadUpdateWithDryRun uses the shared plan for preview and the real call.
func (r *Registry) handleDownloadUpdateWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planDownloadUpdate(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeDownloadUpdate(ctx, client, args, plan)
}

// optionalNonEmptyString reports a trimmed string argument. ok is false when
// the key is missing, null, or a blank string. A non-string value is an error.
func optionalNonEmptyString(args map[string]interface{}, key string) (string, bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("%s must be a string", key)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false, nil
	}
	return s, true, nil
}

// downloadUpdateTrainVersion reads the optional train and version arguments.
// 25.10 update.download requires both or neither
// (src/middlewared/middlewared/plugins/update_/download.py).
func downloadUpdateTrainVersion(args map[string]interface{}) (train, version string, set bool, err error) {
	train, hasTrain, err := optionalNonEmptyString(args, "train")
	if err != nil {
		return "", "", false, err
	}
	version, hasVersion, err := optionalNonEmptyString(args, "version")
	if err != nil {
		return "", "", false, err
	}
	if hasTrain != hasVersion {
		return "", "", false, fmt.Errorf("train and version must both be set, or both omitted (TrueNAS update.download requires both or neither)")
	}
	return train, version, hasTrain, nil
}

// updateAlreadyDownloaded reports a completed download of the requested
// version (or of the current profile's update when no version was requested).
// progress is the 25.10 update.status update_download_progress object
// ({percent, description, version}).
func updateAlreadyDownloaded(status map[string]interface{}, requestedVersion string) (string, bool) {
	if status == nil {
		return "", false
	}
	progress, _ := status["update_download_progress"].(map[string]interface{})
	if progress == nil {
		return "", false
	}
	percent, _ := progress["percent"].(float64)
	version, _ := progress["version"].(string)
	if percent != 100 || version == "" {
		return "", false
	}
	if requestedVersion != "" && requestedVersion != version {
		return "", false
	}
	return version, true
}

func planDownloadUpdate(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	train, version, set, err := downloadUpdateTrainVersion(args)
	if err != nil {
		return nil, err
	}

	sysInfoResult, err := client.Call(ctx, "system.info")
	if err != nil {
		return nil, fmt.Errorf("failed to get system info: %w", err)
	}
	var sysInfo map[string]interface{}
	if err := json.Unmarshal(sysInfoResult, &sysInfo); err != nil {
		return nil, fmt.Errorf("failed to parse system info: %w", err)
	}
	currentVersion, ok := sysInfo["version"].(string)
	if !ok || currentVersion == "" {
		return nil, fmt.Errorf("system.info returned no usable version")
	}

	current := map[string]interface{}{
		"current_version": currentVersion,
	}
	if set {
		current["requested_train"] = train
		current["requested_version"] = version
	}

	var callParams []interface{}
	var paramNames []string
	if set {
		callParams = []interface{}{train, version}
		paramNames = []string{"train", "version"}
	}

	var status map[string]interface{}
	statusResult, statusErr := client.Call(ctx, "update.status")
	if statusErr == nil {
		if err := json.Unmarshal(statusResult, &status); err != nil {
			return nil, fmt.Errorf("failed to parse update status: %w", err)
		}
		current["update_status"] = status
	}

	if ver, already := updateAlreadyDownloaded(status, version); already {
		return &writePlan{
			Tool:         "download_update",
			Method:       updateDownloadMethod,
			CallParams:   callParams,
			ParamNames:   paramNames,
			CurrentState: current,
			Actions: []PlannedAction{{
				Step:        1,
				Description: fmt.Sprintf("Update %s is already downloaded (100%%); no download will be started", ver),
				Operation:   "download",
				Target:      ver,
			}},
			Result: map[string]interface{}{
				"version":            ver,
				"already_downloaded": true,
				"download_percent":   100,
				"message":            fmt.Sprintf("Update %s is already downloaded (100%%). Ready to apply.", ver),
			},
		}, nil
	}

	desc := "Download the update offered by the system's configured update train"
	target := "configured update train"
	if set {
		desc = fmt.Sprintf("Download %s from train %s", version, train)
		target = version
	}
	return &writePlan{
		Tool:         "download_update",
		Method:       updateDownloadMethod,
		CallParams:   callParams,
		ParamNames:   paramNames,
		JobMethod:    updateDownloadMethod,
		CurrentState: current,
		Actions: []PlannedAction{
			{
				Step:        1,
				Description: "Call update.download to " + strings.ToLower(desc[:1]) + desc[1:],
				Operation:   "download",
				Target:      target,
				Details: map[string]interface{}{
					"current_version":   currentVersion,
					"requested_train":   train,
					"requested_version": version,
				},
			},
		},
		EstimatedTime: &EstimatedTime{
			MinSeconds: 120,
			MaxSeconds: 1800,
			Note:       "Time varies based on update size and network speed",
		},
	}, nil
}

func (r *Registry) executeDownloadUpdate(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if already, _ := plan.Result["already_downloaded"].(bool); already {
		formatted, err := json.MarshalIndent(plan.Result, "", "  ")
		if err != nil {
			return "", err
		}
		return string(formatted), nil
	}

	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to start update download: %w", err)
	}

	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("failed to parse job ID: %w", err)
	}

	opts := []tasks.TaskOption{}
	if plan.JobMethod != "" {
		opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
	}
	task, err := r.taskManager.CreateJobTask(ctx,
		"download_update",
		withoutKeys(args, "dry_run", "confirm", "confirmation_token"),
		jobID,
		2*time.Hour,
		opts...,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"task_id":       task.TaskID,
		"task_status":   task.Status,
		"poll_interval": task.PollInterval,
		"job_id":        jobID,
		"message":       fmt.Sprintf("Update download started. Track progress with tasks_get using task_id: %s", task.TaskID),
	}
	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}
	return string(formatted), nil
}

// applyUpdateBoundVersion returns the trimmed new_version.version from a
// 25.10 update.status result, or "" when there is no version to apply.
// The official shape (middlewared/api/v25_10_0/update.py, UpdateStatus, and
// https://api.truenas.com/v25.10/api_methods_update.status.html) is:
//
//	{code, error, update_download_progress, status: {current_version, new_version: {version, ...} | null} | null}
//
// new_version is under status, not at the top level.
func applyUpdateBoundVersion(result map[string]interface{}) string {
	st, _ := result["status"].(map[string]interface{})
	nv, _ := st["new_version"].(map[string]interface{})
	v, _ := nv["version"].(string)
	return strings.TrimSpace(v)
}

// applyUpdateBind returns the extra plan identity for apply_update, or nil
// when there is no version so hashes stay {method, params}.
func applyUpdateBind(result map[string]interface{}) interface{} {
	if v := applyUpdateBoundVersion(result); v != "" {
		return map[string]interface{}{"new_version": v}
	}
	return nil
}

func applyUpdateStatusErrorReason(result map[string]interface{}) string {
	errObj, _ := result["error"].(map[string]interface{})
	if r, _ := errObj["reason"].(string); strings.TrimSpace(r) != "" {
		return strings.TrimSpace(r)
	}
	if name, _ := errObj["errname"].(string); strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	return ""
}

// applyUpdateBlockReasons returns why apply_update cannot proceed against
// this update.status result, or nil when an update is available.
func applyUpdateBlockReasons(result map[string]interface{}) []string {
	code, _ := result["code"].(string)
	if code == "ERROR" {
		reason := applyUpdateStatusErrorReason(result)
		if reason == "" {
			reason = "see error for details"
		}
		return []string{"update.status is ERROR: " + reason}
	}
	if _, ok := result["status"].(map[string]interface{}); !ok {
		return []string{"update.status has no status details (status is null)"}
	}
	if applyUpdateBoundVersion(result) == "" {
		return []string{"No update is available to apply (update.status has no new_version)"}
	}
	return nil
}

// planApplyUpdate builds the apply_update plan used by both the dry-run
// preview and the real call. The middleware write is a single update.run;
// the preview lists that middleware call and the versions and reboot flag
// read from the system.
func planApplyUpdate(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	reboot := false
	if r, ok := args["reboot"].(bool); ok {
		reboot = r
	}

	sysInfoResult, err := client.Call(ctx, "system.info")
	if err != nil {
		return nil, fmt.Errorf("failed to get system info: %w", err)
	}

	var sysInfo map[string]interface{}
	if err := json.Unmarshal(sysInfoResult, &sysInfo); err != nil {
		return nil, fmt.Errorf("failed to parse system info: %w", err)
	}

	currentVersion, ok := sysInfo["version"].(string)
	if !ok || currentVersion == "" {
		return nil, fmt.Errorf("system.info returned no usable version")
	}

	statusResult, err := client.Call(ctx, "update.status")
	if err != nil {
		return nil, fmt.Errorf("failed to get update status: %w", err)
	}

	var status map[string]interface{}
	if err := json.Unmarshal(statusResult, &status); err != nil {
		return nil, fmt.Errorf("failed to parse update status: %w", err)
	}

	newVersion := applyUpdateBoundVersion(status)
	actions := []PlannedAction{
		{
			Step:        1,
			Description: "Call update.run to apply the available system update",
			Operation:   "update",
			Target:      "system",
			Details: map[string]interface{}{
				"current_version": currentVersion,
				"new_version":     newVersion,
				"reboot":          reboot,
			},
		},
	}

	warnings := []string{
		"CRITICAL: This operation will update the TrueNAS system software.",
		"Services may be interrupted during the update process.",
	}
	if reboot {
		warnings = append(warnings,
			"REBOOT ENABLED: System will automatically reboot after update completes.",
			"All connections will be lost during reboot.",
		)
	} else {
		warnings = append(warnings,
			"Manual reboot required after update to complete the process.",
		)
	}

	blocked := applyUpdateBlockReasons(status)

	return &writePlan{
		Tool:       "apply_update",
		Method:     updateRunMethod,
		CallParams: []interface{}{map[string]interface{}{"reboot": reboot}},
		JobMethod:  updateRunMethod,
		ParamNames: []string{"options"},
		CurrentState: map[string]interface{}{
			"current_version": currentVersion,
			"update_status":   status,
		},
		Actions:  actions,
		Warnings: warnings,
		Blocked:  blocked,
		Bind:     applyUpdateBind(status),
		EstimatedTime: &EstimatedTime{
			MinSeconds: 180,
			MaxSeconds: 900,
			Note:       "Time varies based on system configuration. Add 60-120s for reboot if enabled.",
		},
		TargetName: "system",
		Result:     map[string]interface{}{"reboot": reboot},
		ConfirmSummary: newConfirmSummary("Apply a system update",
			confirmFact("current version", currentVersion),
			confirmFact("new version", newVersion),
			confirmBoolFact("reboot", reboot),
		),
	}, nil
}

func (r *Registry) handleApplyUpdateWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planApplyUpdate(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeApplyUpdate(ctx, client, args, plan)
}

func (r *Registry) executeApplyUpdate(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to start update: %w", err)
	}

	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("failed to parse job ID: %w", err)
	}

	opts := []tasks.TaskOption{}
	if plan.JobMethod != "" {
		opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
	}
	task, err := r.taskManager.CreateJobTask(ctx,
		"apply_update",
		args,
		jobID,
		2*time.Hour, // 2 hour TTL
		opts...,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	reboot, _ := plan.Result["reboot"].(bool)
	response := map[string]interface{}{
		"reboot":        reboot,
		"task_id":       task.TaskID,
		"task_status":   task.Status,
		"poll_interval": task.PollInterval,
		"job_id":        jobID,
		"message":       fmt.Sprintf("Update started. Track progress with tasks_get using task_id: %s", task.TaskID),
	}

	if reboot {
		response["warning"] = "System will reboot after update completes. Connection will be lost."
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

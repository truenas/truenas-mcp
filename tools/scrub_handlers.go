package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/truenas"
)

// Pool scrub management handlers

func handleQueryScrubSchedules(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Query all scrub schedules
	result, err := client.Call("pool.scrub.query", []interface{}{})
	if err != nil {
		return "", fmt.Errorf("failed to query scrub schedules: %w", err)
	}

	var schedules []map[string]interface{}
	if err := json.Unmarshal(result, &schedules); err != nil {
		return "", fmt.Errorf("failed to parse schedules: %w", err)
	}

	// Pool scan end times are needed for next_scrub_estimate.
	poolsResult, err := client.Call("pool.query", []interface{}{})
	if err != nil {
		return "", fmt.Errorf("failed to query pools: %w", err)
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(poolsResult, &pools); err != nil {
		return "", fmt.Errorf("failed to parse pools: %w", err)
	}

	lastEndByPoolID := map[int]*time.Time{}
	runningByPoolID := map[int]bool{}
	for _, pool := range pools {
		poolID, _ := pool["id"].(float64)
		id := int(poolID)
		if end := lastFinishedScrubEnd(pool); end != nil {
			lastEndByPoolID[id] = end
		}
		if scrubScanRunning(pool) {
			runningByPoolID[id] = true
		}
	}

	// Apply filters
	poolFilter, hasPoolFilter := args["pool"].(string)
	enabledOnly, _ := args["enabled_only"].(bool)

	filtered := []map[string]interface{}{}
	poolsWithSchedules := make(map[string]bool)
	loc, _, assumedUTC := nasTimezone(client)

	for _, schedule := range schedules {
		poolName, _ := schedule["pool_name"].(string)
		enabled, _ := schedule["enabled"].(bool)

		// Apply pool filter
		if hasPoolFilter && poolName != poolFilter {
			continue
		}

		// Apply enabled filter
		if enabledOnly && !enabled {
			continue
		}

		poolsWithSchedules[poolName] = true
		poolID, _ := schedule["pool"].(float64)
		id := int(poolID)
		simplified := simplifyScrubSchedule(schedule, loc, lastEndByPoolID[id], runningByPoolID[id])
		filtered = append(filtered, simplified)
	}

	poolsWithoutSchedules := []string{}
	for _, pool := range pools {
		poolName, _ := pool["name"].(string)
		if !poolsWithSchedules[poolName] {
			poolsWithoutSchedules = append(poolsWithoutSchedules, poolName)
		}
	}

	response := map[string]interface{}{
		"scrub_schedules":         filtered,
		"count":                   len(filtered),
		"pools_with_schedules":    mapKeys(poolsWithSchedules),
		"pools_without_schedules": poolsWithoutSchedules,
	}
	if assumedUTC {
		response["next_run_timezone_note"] = nextRunTimezoneNote
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleGetScrubStatus(client *truenas.Client, args map[string]interface{}) (string, error) {
	poolFilter, hasPoolFilter := args["pool"].(string)

	// Query all pools
	poolsResult, err := client.Call("pool.query", []interface{}{})
	if err != nil {
		return "", fmt.Errorf("failed to query pools: %w", err)
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(poolsResult, &pools); err != nil {
		return "", fmt.Errorf("failed to parse pools: %w", err)
	}

	// Query scrub schedules
	schedulesResult, err := client.Call("pool.scrub.query", []interface{}{})
	if err != nil {
		return "", fmt.Errorf("failed to query schedules: %w", err)
	}

	var schedules []map[string]interface{}
	if err := json.Unmarshal(schedulesResult, &schedules); err != nil {
		return "", fmt.Errorf("failed to parse schedules: %w", err)
	}

	// Query running jobs (25.x pool.scrub.scrub and 26+ zpool.scrub.run)
	jobs, err := queryScrubJobs(client, true)
	if err != nil {
		return "", fmt.Errorf("failed to query jobs: %w", err)
	}

	// Build status for each pool
	poolStatuses := []map[string]interface{}{}
	scrubNowCount := 0
	withSchedules := 0
	withoutSchedules := 0
	loc, _, assumedUTC := nasTimezone(client)

	for _, pool := range pools {
		poolName, _ := pool["name"].(string)
		poolID, _ := pool["id"].(float64)

		// Apply pool filter
		if hasPoolFilter && poolName != poolFilter {
			continue
		}

		sizeBytes, sizeHuman := poolSizeFields(pool)
		status := map[string]interface{}{
			"name":       poolName,
			"id":         int(poolID),
			"size_bytes": sizeBytes,
			"size_human": sizeHuman,
			"status":     pool["status"],
		}

		// Find schedule for this pool
		for _, schedule := range schedules {
			schedPoolID, _ := schedule["pool"].(float64)
			if int(schedPoolID) == int(poolID) {
				withSchedules++
				enabled, _ := schedule["enabled"].(bool)
				threshold, _ := schedule["threshold"].(float64)

				schedObj, _ := schedule["schedule"].(map[string]interface{})
				scheduleHuman := formatCronSchedule(schedObj)
				nextRun := calculateNextRun(schedObj, time.Now(), loc)

				schedOut := map[string]interface{}{
					"enabled":        enabled,
					"schedule_human": scheduleHuman,
					"next_run":       nextRun,
					"threshold_days": int(threshold),
				}
				attachNextScrubEstimate(schedOut, schedObj, enabled, int(threshold), loc, time.Now(), lastFinishedScrubEnd(pool), scrubScanRunning(pool))
				status["schedule"] = schedOut
				break
			}
		}

		if status["schedule"] == nil {
			withoutSchedules++
		}

		// Find running scrub job
		for _, job := range jobs {
			if jobPoolName, ok := scrubJobPoolName(job); ok && jobPoolName == poolName {
				scrubNowCount++
				progress, _ := job["progress"].(map[string]interface{})
				percent, _ := progress["percent"].(float64)
				description, _ := progress["description"].(string)

				timeStarted, _ := job["time_started"].(map[string]interface{})
				startedSec, _ := timeStarted["$date"].(float64)
				started := time.Unix(int64(startedSec/1000), 0)

				cur := map[string]interface{}{
					"running":     true,
					"progress":    percent,
					"description": description,
					"started":     formatInLocation(started, loc),
				}
				if id, ok := floatField(job, "id"); ok {
					cur["job_id"] = int(id)
				}
				status["current_scrub"] = cur
				break
			}
		}

		if status["current_scrub"] == nil {
			status["current_scrub"] = map[string]interface{}{
				"running": false,
			}
		}

		// Extract last scrub info from pool scan data
		if scan, ok := pool["scan"].(map[string]interface{}); ok {
			if scanFunc, ok := scan["function"].(string); ok && scanFunc == "SCRUB" {
				state, _ := scan["state"].(string)
				errors, _ := scan["errors"].(float64)

				lastScrub := map[string]interface{}{
					"state":  state,
					"errors": int(errors),
				}

				if endTime, ok := scan["end_time"].(map[string]interface{}); ok {
					if endSec, ok := endTime["$date"].(float64); ok {
						completed := time.Unix(int64(endSec/1000), 0)
						lastScrub["completed"] = formatInLocation(completed, loc)
						lastScrub["days_ago"] = int(time.Since(completed).Hours() / 24)
					}
				}

				if startTime, ok := scan["start_time"].(map[string]interface{}); ok {
					if endTime, ok := scan["end_time"].(map[string]interface{}); ok {
						startSec, _ := startTime["$date"].(float64)
						endSec, _ := endTime["$date"].(float64)
						durationHours := (endSec - startSec) / 1000 / 3600
						lastScrub["duration_hours"] = fmt.Sprintf("%.2f", durationHours)
					}
				}

				status["last_scrub"] = lastScrub
			}
		}

		poolStatuses = append(poolStatuses, status)
	}

	response := map[string]interface{}{
		"pools": poolStatuses,
		"summary": map[string]interface{}{
			"total_pools":       len(poolStatuses),
			"scrubbing_now":     scrubNowCount,
			"with_schedules":    withSchedules,
			"without_schedules": withoutSchedules,
		},
	}
	if assumedUTC {
		response["next_run_timezone_note"] = nextRunTimezoneNote
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func (r *Registry) handleCreateScrubSchedule(client *truenas.Client, args map[string]interface{}) (string, error) {
	poolName, ok := args["pool"].(string)
	if !ok || poolName == "" {
		return "", fmt.Errorf("pool is required")
	}

	scheduleObj, ok := args["schedule"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("schedule is required")
	}

	// Get pool ID
	poolInfo, err := getPoolByName(client, poolName)
	if err != nil {
		return "", err
	}

	threshold := 35
	if t, ok := args["threshold"].(float64); ok {
		threshold = int(t)
	}

	description := ""
	if d, ok := args["description"].(string); ok {
		description = d
	}

	enabled := true
	if e, ok := args["enabled"].(bool); ok {
		enabled = e
	}

	// Check if schedule already exists
	existingResult, err := client.Call("pool.scrub.query", []interface{}{
		[]interface{}{"pool", "=", poolInfo["id"]},
	})
	if err != nil {
		return "", fmt.Errorf("failed to check existing schedules: %w", err)
	}

	var existing []map[string]interface{}
	if err := json.Unmarshal(existingResult, &existing); err != nil {
		return "", fmt.Errorf("failed to parse existing schedules: %w", err)
	}

	if len(existing) > 0 {
		return "", fmt.Errorf("pool '%s' already has a scrub schedule (id: %v). Delete it first or use a different pool", poolName, existing[0]["id"])
	}

	// Create schedule
	createArgs := map[string]interface{}{
		"pool":        poolInfo["id"],
		"threshold":   threshold,
		"description": description,
		"enabled":     enabled,
		"schedule":    scheduleObj,
	}

	result, err := client.Call("pool.scrub.create", createArgs)
	if err != nil {
		return "", fmt.Errorf("failed to create schedule: %w", err)
	}

	var created map[string]interface{}
	if err := json.Unmarshal(result, &created); err != nil {
		return "", fmt.Errorf("failed to parse result: %w", err)
	}

	loc, _, assumedUTC := nasTimezone(client)
	nextRun := calculateNextRun(scheduleObj, time.Now(), loc)
	response := map[string]interface{}{
		"pool":           poolName,
		"schedule_id":    created["id"],
		"enabled":        enabled,
		"threshold_days": threshold,
		"schedule_human": formatCronSchedule(scheduleObj),
		"next_run":       nextRun,
		"message":        fmt.Sprintf("Scrub schedule created for pool '%s'. First run: %s", poolName, nextRun),
	}
	attachNextScrubEstimate(response, scheduleObj, enabled, threshold, loc, time.Now(), lastFinishedScrubEnd(poolInfo), scrubScanRunning(poolInfo))
	if assumedUTC {
		response["next_run_timezone_note"] = nextRunTimezoneNote
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func (r *Registry) handleRunScrub(client *truenas.Client, args map[string]interface{}) (string, error) {
	poolName, ok := args["pool"].(string)
	if !ok || poolName == "" {
		return "", fmt.Errorf("pool is required")
	}

	threshold := 7
	if t, ok := args["threshold"].(float64); ok {
		threshold = int(t)
	}

	// Get pool info
	poolInfo, err := getPoolByName(client, poolName)
	if err != nil {
		return "", err
	}
	if err := errIfPoolCannotScrub(poolInfo, poolName); err != nil {
		return "", err
	}
	if err := errIfPoolResilvering(poolInfo, poolName); err != nil {
		return "", err
	}

	jobs, err := queryScrubJobs(client, true)
	if err != nil {
		return "", fmt.Errorf("failed to check running scrubs: %w", err)
	}
	for _, job := range jobs {
		if jobPoolName, ok := scrubJobPoolName(job); ok && jobPoolName == poolName {
			return "", fmt.Errorf("scrub already running on pool '%s' (job id: %v)", poolName, job["id"])
		}
	}
	if scrubScanRunning(poolInfo) {
		return "", fmt.Errorf("scrub already running on pool '%s'", poolName)
	}

	if threshold > 0 {
		if end := lastFinishedScrubEnd(poolInfo); end != nil {
			daysAgo := int(time.Since(*end).Hours() / 24)
			if daysAgo < threshold {
				response := map[string]interface{}{
					"pool":          poolName,
					"scrub_started": false,
					"reason":        fmt.Sprintf("last scrub finished %d days ago, within the threshold of %d days", daysAgo, threshold),
					"message":       fmt.Sprintf("No scrub started on pool '%s': last scrub finished %d days ago, within the threshold of %d days", poolName, daysAgo, threshold),
				}
				formatted, err := json.MarshalIndent(response, "", "  ")
				if err != nil {
					return "", err
				}
				return string(formatted), nil
			}
		}
	}

	jobID, err := startScrubJob(client, poolName)
	if err != nil {
		return "", err
	}

	// Create task for tracking
	task, err := r.taskManager.CreateJobTask(
		"run_scrub",
		args,
		jobID,
		48*time.Hour, // Scrubs can take days on large pools
	)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"pool":          poolName,
		"scrub_started": true,
		"job_id":        jobID,
		"task_id":       task.TaskID,
		"task_status":   task.Status,
		"poll_interval": task.PollInterval,
		"message":       fmt.Sprintf("Scrub started on pool '%s'. Track progress: (1) tasks_get with task_id: %s, or (2) get_scrub_status", poolName, task.TaskID),
	}
	if size, ok := poolSizeBytes(poolInfo); ok {
		response["estimated_duration_hours"] = estimateScrubDuration(size)
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleDeleteScrubSchedule(client *truenas.Client, args map[string]interface{}) (string, error) {
	scheduleID, ok := args["id"].(float64)
	if !ok {
		return "", fmt.Errorf("id is required")
	}

	id := int(scheduleID)

	// Query schedule to verify it exists
	result, err := client.Call("pool.scrub.query", []interface{}{
		[]interface{}{"id", "=", id},
	})
	if err != nil {
		return "", fmt.Errorf("failed to query schedule: %w", err)
	}

	var schedules []map[string]interface{}
	if err := json.Unmarshal(result, &schedules); err != nil {
		return "", fmt.Errorf("failed to parse schedules: %w", err)
	}

	if len(schedules) == 0 {
		return "", fmt.Errorf("schedule with id %d not found", id)
	}

	schedule := schedules[0]
	poolName, _ := schedule["pool_name"].(string)

	// Delete schedule
	_, err = client.Call("pool.scrub.delete", id)
	if err != nil {
		return "", fmt.Errorf("failed to delete schedule: %w", err)
	}

	response := map[string]interface{}{
		"deleted":        true,
		"id":             id,
		"pool":           poolName,
		"message":        fmt.Sprintf("Scrub schedule deleted for pool '%s'. IMPORTANT: Run manual scrubs monthly to maintain data integrity.", poolName),
		"recommendation": "Use run_scrub tool for manual scrubs, or create a new schedule with create_scrub_schedule",
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// Dry-run wrappers

func (r *Registry) handleCreateScrubScheduleWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &createScrubScheduleDryRun{}, r.handleCreateScrubSchedule)
}

func (r *Registry) handleRunScrubWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &runScrubDryRun{}, r.handleRunScrub)
}

func (r *Registry) handleDeleteScrubScheduleWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &deleteScrubScheduleDryRun{}, handleDeleteScrubSchedule)
}

// Dry-run implementations

type createScrubScheduleDryRun struct{}

func (c *createScrubScheduleDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	poolName, ok := args["pool"].(string)
	if !ok || poolName == "" {
		return nil, fmt.Errorf("pool is required")
	}

	scheduleObj, ok := args["schedule"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("schedule is required")
	}

	// Get pool info
	poolInfo, err := getPoolByName(client, poolName)
	if err != nil {
		return nil, err
	}

	threshold := 35
	if t, ok := args["threshold"].(float64); ok {
		threshold = int(t)
	}

	enabled := true
	if e, ok := args["enabled"].(bool); ok {
		enabled = e
	}

	// Check existing schedule
	existingResult, err := client.Call("pool.scrub.query", []interface{}{
		[]interface{}{"pool", "=", poolInfo["id"]},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to check existing schedules: %w", err)
	}

	var existing []map[string]interface{}
	if err := json.Unmarshal(existingResult, &existing); err != nil {
		return nil, fmt.Errorf("failed to parse existing schedules: %w", err)
	}

	var existingSchedule map[string]interface{}
	if len(existing) > 0 {
		existingSchedule = existing[0]
	}

	// Get last scrub info
	poolsResult, err := client.Call("pool.query", []interface{}{
		[]interface{}{"name", "=", poolName},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query pool: %w", err)
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(poolsResult, &pools); err != nil {
		return nil, fmt.Errorf("failed to parse pool: %w", err)
	}

	scheduleHuman := formatCronSchedule(scheduleObj)
	loc, _, assumedUTC := nasTimezone(client)

	var lastScrubDate string
	if len(pools) > 0 {
		if scan, ok := pools[0]["scan"].(map[string]interface{}); ok {
			if endTime, ok := scan["end_time"].(map[string]interface{}); ok {
				if endSec, ok := endTime["$date"].(float64); ok {
					lastScrubDate = formatInLocation(time.Unix(int64(endSec/1000), 0), loc)
				}
			}
		}
	}
	firstRun := calculateNextRun(scheduleObj, time.Now(), loc)
	scrubEstimate, scrubEstimateNote, hasScrubEstimate := estimateNextScrub(scheduleObj, time.Now(), loc, enabled, threshold, lastFinishedScrubEnd(poolInfo), scrubScanRunning(poolInfo))
	sizeBytes, hasSize := poolSizeBytes(poolInfo)
	_, sizeHuman := poolSizeFields(poolInfo)
	var estimatedHours int
	if hasSize {
		estimatedHours = estimateScrubDuration(sizeBytes)
	}

	warnings := []string{}
	if existingSchedule != nil {
		warnings = append(warnings, fmt.Sprintf("ERROR: Pool '%s' already has a scrub schedule (id: %v)", poolName, existingSchedule["id"]))
		warnings = append(warnings, "Delete the existing schedule first, or choose a different pool")
	} else {
		warnings = append(warnings, fmt.Sprintf("Next cron tick: %s", firstRun))
		if hasScrubEstimate {
			if scrubEstimate != "" {
				warnings = append(warnings, fmt.Sprintf("Earliest possible scrub: %s (%s)", scrubEstimate, scrubEstimateNote))
			} else {
				warnings = append(warnings, scrubEstimateNote)
			}
		}
		if hasSize {
			warnings = append(warnings, fmt.Sprintf("Scrub may take %d-%d hours based on pool size", estimatedHours, estimatedHours*3))
		} else {
			warnings = append(warnings, "Pool size is unknown; duration cannot be estimated")
		}
		warnings = append(warnings, "Ensure system is powered on at scheduled time")

		// Check if schedule is during reasonable hours
		hour, _ := scheduleObj["hour"].(string)
		if hour != "*" {
			hourInt := 0
			fmt.Sscanf(hour, "%d", &hourInt)
			if hourInt >= 2 && hourInt <= 4 {
				warnings = append(warnings, "Schedule runs during low-activity hours (recommended)")
			} else if hourInt >= 8 && hourInt <= 18 {
				warnings = append(warnings, "WARNING: Schedule runs during typical business hours - may impact performance")
			}
		}
	}

	actionDetails := map[string]interface{}{
		"schedule_human": scheduleHuman,
		"first_run":      firstRun,
		"threshold_days": threshold,
		"enabled":        enabled,
	}
	if hasScrubEstimate {
		if scrubEstimate != "" {
			actionDetails["next_scrub_estimate"] = scrubEstimate
		}
		actionDetails["next_scrub_estimate_note"] = scrubEstimateNote
	}

	actions := []PlannedAction{}
	if existingSchedule == nil {
		actions = append(actions, PlannedAction{
			Step:        1,
			Description: fmt.Sprintf("Create scrub schedule for pool '%s'", poolName),
			Operation:   "create",
			Target:      poolName,
			Details:     actionDetails,
		})
	}

	if assumedUTC {
		warnings = append(warnings, nextRunTimezoneNote)
	}

	result := &DryRunResult{
		Tool: "create_scrub_schedule",
		CurrentState: map[string]interface{}{
			"pool":              poolName,
			"pool_id":           poolInfo["id"],
			"pool_size":         sizeHuman,
			"existing_schedule": existingSchedule,
			"last_scrub":        lastScrubDate,
		},
		PlannedActions: actions,
		Warnings:       warnings,
	}
	if hasSize {
		result.EstimatedTime = &EstimatedTime{
			MinSeconds: estimatedHours * 3600,
			MaxSeconds: estimatedHours * 3 * 3600,
			Note:       fmt.Sprintf("Scrub duration: %d-%d hours for %s pools", estimatedHours, estimatedHours*3, sizeHuman),
		}
	}
	return result, nil
}

type runScrubDryRun struct{}

func (r *runScrubDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	poolName, ok := args["pool"].(string)
	if !ok || poolName == "" {
		return nil, fmt.Errorf("pool is required")
	}

	threshold := 7
	if t, ok := args["threshold"].(float64); ok {
		threshold = int(t)
	}

	// Get pool info
	poolInfo, err := getPoolByName(client, poolName)
	if err != nil {
		return nil, err
	}
	if err := errIfPoolCannotScrub(poolInfo, poolName); err != nil {
		return nil, err
	}
	if err := errIfPoolResilvering(poolInfo, poolName); err != nil {
		return nil, err
	}

	status, _ := poolInfo["status"].(string)
	sizeBytes, hasSize := poolSizeBytes(poolInfo)
	_, sizeHuman := poolSizeFields(poolInfo)

	jobs, err := queryScrubJobs(client, true)
	if err != nil {
		return nil, fmt.Errorf("failed to check running scrubs: %w", err)
	}

	scrubRunning := false
	for _, job := range jobs {
		if jobPoolName, ok := scrubJobPoolName(job); ok && jobPoolName == poolName {
			scrubRunning = true
			break
		}
	}
	if scrubScanRunning(poolInfo) {
		scrubRunning = true
	}

	// Get last scrub info
	poolsResult, err := client.Call("pool.query", []interface{}{
		[]interface{}{"name", "=", poolName},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query pool: %w", err)
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(poolsResult, &pools); err != nil {
		return nil, fmt.Errorf("failed to parse pool: %w", err)
	}

	loc, _, _ := nasTimezone(client)

	var lastScrub map[string]interface{}
	if len(pools) > 0 {
		if scan, ok := pools[0]["scan"].(map[string]interface{}); ok {
			lastScrub = map[string]interface{}{
				"state":  scan["state"],
				"errors": scan["errors"],
			}
			if endTime, ok := scan["end_time"].(map[string]interface{}); ok {
				if endSec, ok := endTime["$date"].(float64); ok {
					completed := time.Unix(int64(endSec/1000), 0)
					lastScrub["date"] = formatInLocation(completed, loc)
					lastScrub["days_ago"] = int(time.Since(completed).Hours() / 24)
				}
			}
		}
	}

	var estimatedHours, minSeconds, maxSeconds int
	if hasSize {
		estimatedHours = estimateScrubDuration(sizeBytes)
		minSeconds = estimatedHours * 3600
		maxSeconds = estimatedHours * 3 * 3600
	}

	warnings := []string{}
	if scrubRunning {
		warnings = append(warnings, fmt.Sprintf("ERROR: Scrub already running on pool '%s'", poolName))
		warnings = append(warnings, "Wait for current scrub to complete, or stop it first")
	} else {
		warnings = append(warnings, "Scrub will verify all data blocks on the pool")
		warnings = append(warnings, "Performance may be impacted during scrub")
		if lastScrub != nil {
			daysAgo, _ := lastScrub["days_ago"].(int)
			if daysAgo < threshold {
				warnings = append(warnings, fmt.Sprintf("Last scrub was %d days ago (within threshold of %d days)", daysAgo, threshold))
			} else {
				warnings = append(warnings, fmt.Sprintf("Last scrub was %d days ago (exceeds threshold of %d days)", daysAgo, threshold))
			}
		}
		warnings = append(warnings, "Scrub can be safely interrupted (resumes from checkpoint)")
	}

	actions := []PlannedAction{}
	if !scrubRunning {
		details := map[string]interface{}{
			"pool_size": sizeHuman,
		}
		if hasSize {
			details["estimated_duration_hours"] = estimatedHours
		}
		actions = append(actions, PlannedAction{
			Step:        1,
			Description: fmt.Sprintf("Start manual scrub of pool '%s'", poolName),
			Operation:   "scrub",
			Target:      poolName,
			Details:     details,
		})
	}

	requirements := &Requirements{
		Conditions: []string{
			fmt.Sprintf("Pool must be ONLINE or DEGRADED (current: %s)", status),
			fmt.Sprintf("No existing scrub running (current: %v)", map[bool]string{true: "running", false: "none"}[scrubRunning]),
		},
	}

	result := &DryRunResult{
		Tool: "run_scrub",
		CurrentState: map[string]interface{}{
			"pool":          poolName,
			"pool_id":       poolInfo["id"],
			"size":          sizeHuman,
			"status":        status,
			"last_scrub":    lastScrub,
			"scrub_running": scrubRunning,
		},
		PlannedActions: actions,
		Warnings:       warnings,
		Requirements:   requirements,
	}
	if hasSize {
		result.EstimatedTime = &EstimatedTime{
			MinSeconds: minSeconds,
			MaxSeconds: maxSeconds,
			Note:       "Duration varies by pool size, data amount, and system load",
		}
	}
	return result, nil
}

type deleteScrubScheduleDryRun struct{}

func (d *deleteScrubScheduleDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	scheduleID, ok := args["id"].(float64)
	if !ok {
		return nil, fmt.Errorf("id is required")
	}

	id := int(scheduleID)

	// Query schedule
	result, err := client.Call("pool.scrub.query", []interface{}{
		[]interface{}{"id", "=", id},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query schedule: %w", err)
	}

	var schedules []map[string]interface{}
	if err := json.Unmarshal(result, &schedules); err != nil {
		return nil, fmt.Errorf("failed to parse schedules: %w", err)
	}

	if len(schedules) == 0 {
		return nil, fmt.Errorf("schedule with id %d not found", id)
	}

	schedule := schedules[0]
	poolName, _ := schedule["pool_name"].(string)
	schedObj, _ := schedule["schedule"].(map[string]interface{})

	simplified := map[string]interface{}{
		"id":             id,
		"pool":           poolName,
		"schedule_human": formatCronSchedule(schedObj),
		"enabled":        schedule["enabled"],
		"threshold":      schedule["threshold"],
	}

	warnings := []string{
		fmt.Sprintf("PERMANENT: Pool '%s' will no longer have automatic scrubbing", poolName),
		"RECOMMENDATION: Run manual scrubs monthly to maintain data integrity",
		"You can still run scrubs with run_scrub tool",
		"Consider creating new schedule instead if adjusting timing",
	}

	actions := []PlannedAction{
		{
			Step:        1,
			Description: fmt.Sprintf("Delete scrub schedule for pool '%s'", poolName),
			Operation:   "delete",
			Target:      poolName,
		},
	}

	return &DryRunResult{
		Tool: "delete_scrub_schedule",
		CurrentState: map[string]interface{}{
			"schedule": simplified,
		},
		PlannedActions: actions,
		Warnings:       warnings,
	}, nil
}

// Helper functions for scrub management

func simplifyScrubSchedule(schedule map[string]interface{}, loc *time.Location, lastScrubEnd *time.Time, scrubRunning bool) map[string]interface{} {
	scheduleObj, _ := mapField(schedule, "schedule")
	enabled, _ := schedule["enabled"].(bool)
	threshold := 0
	switch t := schedule["threshold"].(type) {
	case float64:
		threshold = int(t)
	case int:
		threshold = t
	}

	out := map[string]interface{}{
		"id":             schedule["id"],
		"pool":           schedule["pool_name"],
		"pool_id":        schedule["pool"],
		"enabled":        schedule["enabled"],
		"threshold":      schedule["threshold"],
		"description":    schedule["description"],
		"schedule":       scheduleObj,
		"schedule_human": formatCronSchedule(scheduleObj),
		"next_run":       calculateNextRun(scheduleObj, time.Now(), loc),
	}
	attachNextScrubEstimate(out, scheduleObj, enabled, threshold, loc, time.Now(), lastScrubEnd, scrubRunning)
	return out
}

func formatCronSchedule(schedule map[string]interface{}) string {
	minute, _ := schedule["minute"].(string)
	hour, _ := schedule["hour"].(string)
	dom, _ := schedule["dom"].(string)
	dow, _ := schedule["dow"].(string)

	// Weekly pattern (specific day of week)
	if dow != "*" && dom == "*" {
		dayMap := map[string]string{
			"0": "Sunday", "1": "Monday", "2": "Tuesday",
			"3": "Wednesday", "4": "Thursday", "5": "Friday",
			"6": "Saturday", "7": "Sunday",
		}
		dayName := dayMap[dow]
		return fmt.Sprintf("Weekly on %s at %s:%s", dayName, hour, minute)
	}

	// Monthly pattern (specific day of month)
	if dom != "*" && dow == "*" {
		suffix := "th"
		domInt := 0
		fmt.Sscanf(dom, "%d", &domInt)
		if domInt == 1 || domInt == 21 || domInt == 31 {
			suffix = "st"
		} else if domInt == 2 || domInt == 22 {
			suffix = "nd"
		} else if domInt == 3 || domInt == 23 {
			suffix = "rd"
		}
		return fmt.Sprintf("Monthly on %s%s at %s:%s", dom, suffix, hour, minute)
	}

	// Daily pattern
	if hour != "*" && minute != "*" {
		return fmt.Sprintf("Daily at %s:%s", hour, minute)
	}

	// Hourly pattern
	if hour == "*" && minute != "*" {
		return fmt.Sprintf("Hourly at :%s", minute)
	}

	// Custom pattern
	return fmt.Sprintf("Custom: %s %s %s * %s", minute, hour, dom, dow)
}

// nextRunTimezoneNote is added to scrub responses only when the NAS
// timezone could not be read or loaded. next_run is then computed in UTC
// so we never silently present the MCP host's local zone as the NAS zone.
const nextRunTimezoneNote = "NAS timezone unavailable; next_run computed in UTC"

// nasTimezone returns the NAS system timezone from system.info.
// Both TrueNAS 25.x and 27.x expose timezone as an IANA name on that call
// (same value as system.general.config). On any failure, falls back to UTC
// with assumedUTC=true so callers can surface an honest note.
// formatInLocation renders t as RFC3339 in loc (NAS zone, or the UTC
// fallback from nasTimezone). Same instant as time.Unix(...).Format would
// show, but with the NAS offset instead of the MCP host zone.
func formatInLocation(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format(time.RFC3339)
}

func nasTimezone(client *truenas.Client) (loc *time.Location, name string, assumedUTC bool) {
	result, err := client.Call("system.info")
	if err != nil {
		return time.UTC, "UTC", true
	}
	var info map[string]interface{}
	if err := json.Unmarshal(result, &info); err != nil {
		return time.UTC, "UTC", true
	}
	tz, _ := info["timezone"].(string)
	if tz == "" {
		return time.UTC, "UTC", true
	}
	loc, err = time.LoadLocation(tz)
	if err != nil {
		return time.UTC, "UTC", true
	}
	return loc, tz, false
}

// nextScrubEstimateNoteWithEnd explains next_scrub_estimate when a finished
// scrub end_time is known (TrueNAS 26+ rule; approximate on 25.x).
const nextScrubEstimateNoteWithEnd = "Earliest possible start: first cron tick on or after last finished scrub end + (threshold-1)*24h; may be later on TrueNAS 25.x or if create/import/scrub activity falls in the window"

// nextScrubEstimateNoteNoEnd is used when we have no finished-scrub end_time
// (new/imported pool, resilver, canceled scrub, etc.).
const nextScrubEstimateNoteNoEnd = "Earliest possible start (next cron tick); may be later if the pool was created, imported, or scrubbed within the threshold window"

// nextScrubEstimateNoteRunning explains why no timestamp is given while a
// scrub is in progress.
const nextScrubEstimateNoteRunning = "A scrub is currently running; the next cron tick is skipped"

// lastFinishedScrubEnd returns the pool's last finished SCRUB scan end_time
// when present. On TrueNAS 26/27, middleware's threshold check uses
// scan.end_time for a FINISHED SCRUB. TrueNAS 25.x instead uses the start
// of the latest zpool scrub|create|import from pool history, so an estimate
// based on end_time is only approximate there. Returns nil when there is no
// finished scrub end to use (including resilver, canceled, or in-progress).
func lastFinishedScrubEnd(pool map[string]interface{}) *time.Time {
	scan, ok := pool["scan"].(map[string]interface{})
	if !ok {
		return nil
	}
	if fn, _ := scan["function"].(string); fn != "SCRUB" {
		return nil
	}
	if state, _ := scan["state"].(string); state != "FINISHED" {
		return nil
	}
	endTime, ok := scan["end_time"].(map[string]interface{})
	if !ok {
		return nil
	}
	endSec, ok := endTime["$date"].(float64)
	if !ok {
		return nil
	}
	t := time.Unix(int64(endSec/1000), 0).UTC()
	return &t
}

// scrubScanRunning reports an in-progress (not paused) SCRUB scan.
func scrubScanRunning(pool map[string]interface{}) bool {
	scan, ok := pool["scan"].(map[string]interface{})
	if !ok {
		return false
	}
	if fn, _ := scan["function"].(string); fn != "SCRUB" {
		return false
	}
	if state, _ := scan["state"].(string); state != "SCANNING" {
		return false
	}
	if pause, exists := scan["pause"]; exists && pause != nil {
		return false
	}
	return true
}

// nextCronAtOrAfter returns the first cron tick at or after at, in loc.
func nextCronAtOrAfter(schedule map[string]interface{}, at time.Time, loc *time.Location) string {
	// calculateNextRun treats an exact tick as already passed; step 1ns
	// earlier so an exact eligibility instant still counts.
	return calculateNextRun(schedule, at.Add(-time.Nanosecond), loc)
}

// estimateNextScrub returns when a scrub may actually start.
// Middleware is due once (threshold-1)*86400 seconds have passed since the
// last scrub reference time; threshold ≤ 1 means the next cron tick.
// Disabled schedules yield ok=false. When a scrub is running, ok=true with
// an empty estimate and a note that the next tick is skipped.
func estimateNextScrub(schedule map[string]interface{}, fromTime time.Time, loc *time.Location, enabled bool, thresholdDays int, lastEnd *time.Time, scrubRunning bool) (estimate, note string, ok bool) {
	if !enabled {
		return "", "", false
	}
	if scrubRunning {
		return "", nextScrubEstimateNoteRunning, true
	}
	if loc == nil {
		loc = time.UTC
	}
	if lastEnd == nil {
		return calculateNextRun(schedule, fromTime, loc), nextScrubEstimateNoteNoEnd, true
	}
	if thresholdDays <= 1 {
		return calculateNextRun(schedule, fromTime, loc), nextScrubEstimateNoteWithEnd, true
	}
	// Match middleware: due when tick > last + (threshold-1)*86400 (fixed
	// seconds, not calendar AddDate, so DST does not shift the window).
	eligible := lastEnd.Add(time.Duration(thresholdDays-1) * 24 * time.Hour)
	anchor := fromTime
	if eligible.After(anchor) {
		anchor = eligible
	}
	return nextCronAtOrAfter(schedule, anchor.In(loc), loc), nextScrubEstimateNoteWithEnd, true
}

// attachNextScrubEstimate adds next_scrub_estimate fields when applicable.
// A running scrub sets only the note (no timestamp).
func attachNextScrubEstimate(out map[string]interface{}, schedule map[string]interface{}, enabled bool, thresholdDays int, loc *time.Location, fromTime time.Time, lastEnd *time.Time, scrubRunning bool) {
	estimate, note, ok := estimateNextScrub(schedule, fromTime, loc, enabled, thresholdDays, lastEnd, scrubRunning)
	if !ok {
		return
	}
	if estimate != "" {
		out["next_scrub_estimate"] = estimate
	}
	out["next_scrub_estimate_note"] = note
}

func calculateNextRun(schedule map[string]interface{}, fromTime time.Time, loc *time.Location) string {
	// Simplified calculation - just add one week/month/day based on pattern
	// In production, would use a proper cron library
	if loc == nil {
		loc = time.UTC
	}
	minute, _ := schedule["minute"].(string)
	hour, _ := schedule["hour"].(string)
	dom, _ := schedule["dom"].(string)
	dow, _ := schedule["dow"].(string)

	minuteInt, hourInt := 0, 0
	fmt.Sscanf(minute, "%d", &minuteInt)
	fmt.Sscanf(hour, "%d", &hourInt)

	// Evaluate cron fields in the NAS timezone. TrueNAS pool.scrub
	// schedules fire in the system timezone, not the MCP host's local zone.
	now := fromTime.In(loc)

	// Weekly
	if dow != "*" && dom == "*" {
		dowInt := 0
		fmt.Sscanf(dow, "%d", &dowInt)
		if dowInt == 7 {
			dowInt = 0 // Sunday
		}

		// Find next occurrence of this weekday
		daysUntil := (int(dowInt) - int(now.Weekday()) + 7) % 7
		if daysUntil == 0 && (now.Hour() > hourInt || (now.Hour() == hourInt && now.Minute() >= minuteInt)) {
			daysUntil = 7
		}

		// Build the candidate with time.Date on loc, not AddDate. AddDate on a
		// wall-clock that fell in a spring-forward gap keeps the normalized
		// (wrong) hour; Date(y,m,d+n,hour,min,...,loc) re-applies the schedule.
		y, m, d := now.Date()
		next := time.Date(y, m, d+daysUntil, hourInt, minuteInt, 0, 0, loc)
		return next.Format(time.RFC3339)
	}

	// Monthly
	if dom != "*" && dow == "*" {
		domInt := 0
		fmt.Sscanf(dom, "%d", &domInt)

		next := time.Date(now.Year(), now.Month(), domInt, hourInt, minuteInt, 0, 0, loc)
		if next.Before(now) {
			// Same as weekly: rebuild with the schedule hour/minute rather than
			// AddDate on a gap-normalized instant.
			next = time.Date(now.Year(), now.Month()+1, domInt, hourInt, minuteInt, 0, 0, loc)
		}
		return next.Format(time.RFC3339)
	}

	// Daily
	next := time.Date(now.Year(), now.Month(), now.Day(), hourInt, minuteInt, 0, 0, loc)
	if next.Before(now) {
		y, m, d := now.Date()
		next = time.Date(y, m, d+1, hourInt, minuteInt, 0, 0, loc)
	}
	return next.Format(time.RFC3339)
}

// floatField returns the JSON number at key. Middleware data is decoded
// into map[string]interface{}, so numbers are float64; a missing key or
// a null / non-number value reports ok=false.
func floatField(m map[string]interface{}, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key].(float64)
	return v, ok
}

// mapField returns the JSON object at key, if any.
func mapField(m map[string]interface{}, key string) (map[string]interface{}, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m[key].(map[string]interface{})
	return v, ok
}

// poolSizeBytes returns the pool's size in bytes when middleware provided
// a number. Size is null for a configured but not-imported (OFFLINE) pool.
func poolSizeBytes(pool map[string]interface{}) (int64, bool) {
	v, ok := floatField(pool, "size")
	if !ok {
		return 0, false
	}
	return int64(v), true
}

// poolSizeFields returns size_bytes (a number or nil) and size_human
// ("unknown" when size is not a number) for tool output.
func poolSizeFields(pool map[string]interface{}) (interface{}, string) {
	v, ok := poolSizeBytes(pool)
	if !ok {
		return nil, "unknown"
	}
	return float64(v), formatBytes(v)
}

// poolIsOffline reports whether a pool.query record is not imported
// (status OFFLINE: exported, missing disks, or failed import). Size is
// typically null in that case.
func poolIsOffline(pool map[string]interface{}) bool {
	status, _ := pool["status"].(string)
	return status == "OFFLINE"
}

// errIfPoolOffline refuses a start when the pool is not imported.
func errIfPoolOffline(pool map[string]interface{}, poolName string) error {
	if !poolIsOffline(pool) {
		return nil
	}
	if name, _ := pool["name"].(string); name != "" {
		poolName = name
	}
	return fmt.Errorf("cannot run a scrub on pool %q: the pool is OFFLINE (not imported); nothing was started", poolName)
}

// errIfPoolCannotScrub refuses run_scrub (and its dry run) unless the pool
// is ONLINE or DEGRADED. FAULTED, UNAVAIL, REMOVED, and OFFLINE are rejected
// before any start call.
func errIfPoolCannotScrub(pool map[string]interface{}, poolName string) error {
	if name, _ := pool["name"].(string); name != "" {
		poolName = name
	}
	if err := errIfPoolOffline(pool, poolName); err != nil {
		return err
	}
	status, _ := pool["status"].(string)
	if status == "ONLINE" || status == "DEGRADED" {
		return nil
	}
	if status == "" {
		status = "unknown"
	}
	return fmt.Errorf("cannot run a scrub on pool %q: the pool is %s (only ONLINE or DEGRADED pools can be scrubbed); nothing was started", poolName, status)
}

func errIfPoolResilvering(pool map[string]interface{}, poolName string) error {
	scan, ok := mapField(pool, "scan")
	if !ok {
		return nil
	}
	fn, _ := scan["function"].(string)
	state, _ := scan["state"].(string)
	if fn != "RESILVER" || state != "SCANNING" {
		return nil
	}
	if name, _ := pool["name"].(string); name != "" {
		poolName = name
	}
	return fmt.Errorf("cannot run a scrub on pool %q: a resilver is running; nothing was started", poolName)
}

func getPoolByName(client *truenas.Client, poolName string) (map[string]interface{}, error) {
	result, err := client.Call("pool.query", []interface{}{
		[]interface{}{"name", "=", poolName},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query pool: %w", err)
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(result, &pools); err != nil {
		return nil, fmt.Errorf("failed to parse pools: %w", err)
	}

	if len(pools) == 0 {
		return nil, fmt.Errorf("pool '%s' not found", poolName)
	}

	return pools[0], nil
}

// scrubJobFilters is the core.get_jobs filter list that matches both the
// 25.x job method and the 26+ zpool.scrub.run method.
func scrubJobFilters(runningOnly bool) []interface{} {
	methods := []interface{}{
		"OR",
		[]interface{}{
			[]interface{}{"method", "=", "pool.scrub.scrub"},
			[]interface{}{"method", "=", "zpool.scrub.run"},
		},
	}
	if runningOnly {
		return []interface{}{methods, []interface{}{"state", "in", []string{"RUNNING", "WAITING"}}}
	}
	return []interface{}{methods}
}

func queryScrubJobs(client *truenas.Client, runningOnly bool) ([]map[string]interface{}, error) {
	result, err := client.Call("core.get_jobs", scrubJobFilters(runningOnly))
	if err != nil {
		return nil, err
	}
	var jobs []map[string]interface{}
	if err := json.Unmarshal(result, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// scrubJobPoolName reads the pool from a pool.scrub.scrub job (string
// argument) or a zpool.scrub.run job ({"pool_name": ...}).
func scrubJobPoolName(job map[string]interface{}) (string, bool) {
	args, ok := job["arguments"].([]interface{})
	if !ok || len(args) == 0 {
		return "", false
	}
	if s, ok := args[0].(string); ok && s != "" {
		return s, true
	}
	if m, ok := args[0].(map[string]interface{}); ok {
		if s, ok := m["pool_name"].(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

func useZpoolScrubRun(client *truenas.Client) bool {
	result, err := client.Call("system.info")
	if err != nil {
		return false
	}
	var info map[string]interface{}
	if err := json.Unmarshal(result, &info); err != nil {
		return false
	}
	ver, _ := info["version"].(string)
	major, _, ok := parseVersionMajorMinor(ver)
	return ok && major >= 26
}

func parseVersionMajorMinor(v string) (int, int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	var major, minor int
	n, err := fmt.Sscanf(v, "%d.%d", &major, &minor)
	return major, minor, err == nil && n == 2
}

// startScrubJob starts a scrub with a method that returns a job id and has
// no threshold (threshold is enforced in the handler first).
func startScrubJob(client *truenas.Client, poolName string) (int, error) {
	var result json.RawMessage
	var err error
	if useZpoolScrubRun(client) {
		result, err = client.Call("zpool.scrub.run", map[string]interface{}{
			"pool_name": poolName,
			"action":    "START",
			"threshold": 0,
		})
	} else {
		result, err = client.Call("pool.scrub.scrub", poolName, "START")
	}
	if err != nil {
		return 0, fmt.Errorf("failed to start scrub: %w", err)
	}
	jobID, err := parseScrubJobID(result)
	if err != nil {
		return 0, err
	}
	return jobID, nil
}

func parseScrubJobID(result json.RawMessage) (int, error) {
	errNoID := fmt.Errorf("scrub start returned no job id; nothing was confirmed started")
	if len(result) == 0 || string(result) == "null" {
		return 0, errNoID
	}
	var id int
	if err := json.Unmarshal(result, &id); err == nil {
		if id == 0 {
			return 0, errNoID
		}
		return id, nil
	}
	var f float64
	if err := json.Unmarshal(result, &f); err == nil {
		if f == 0 {
			return 0, errNoID
		}
		return int(f), nil
	}
	var arr []int
	if err := json.Unmarshal(result, &arr); err == nil && len(arr) > 0 {
		if arr[0] == 0 {
			return 0, errNoID
		}
		return arr[0], nil
	}
	return 0, errNoID
}

func estimateScrubDuration(poolSizeBytes int64) int {
	// Assume 500 MB/s average scrub speed
	// This is conservative; actual speed varies by hardware
	mbPerSec := 500.0
	bytesPerSec := mbPerSec * 1024 * 1024
	seconds := float64(poolSizeBytes) / bytesPerSec
	hours := int(seconds / 3600)

	// Minimum 1 hour
	if hours < 1 {
		hours = 1
	}

	return hours
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

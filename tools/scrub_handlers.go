package tools

import (
	"encoding/json"
	"fmt"
	"sort"
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

	// Apply filters
	poolFilter, hasPoolFilter := args["pool"].(string)
	enabledOnly, _ := args["enabled_only"].(bool)

	filtered := []map[string]interface{}{}
	poolsWithSchedules := make(map[string]bool)

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
		simplified := simplifyScrubSchedule(schedule)
		filtered = append(filtered, simplified)
	}

	// Get all pools to show pools without schedules
	poolsResult, err := client.Call("pool.query", []interface{}{})
	if err != nil {
		return "", fmt.Errorf("failed to query pools: %w", err)
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(poolsResult, &pools); err != nil {
		return "", fmt.Errorf("failed to parse pools: %w", err)
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

	// Query running jobs
	jobsResult, err := client.Call("core.get_jobs", []interface{}{
		[]interface{}{"method", "=", "pool.scrub.scrub"},
		[]interface{}{"state", "in", []string{"RUNNING", "WAITING"}},
	})
	if err != nil {
		return "", fmt.Errorf("failed to query jobs: %w", err)
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(jobsResult, &jobs); err != nil {
		return "", fmt.Errorf("failed to parse jobs: %w", err)
	}

	// Build status for each pool
	poolStatuses := []map[string]interface{}{}
	scrubNowCount := 0
	withSchedules := 0
	withoutSchedules := 0

	for _, pool := range pools {
		poolName, _ := pool["name"].(string)
		poolID, _ := pool["id"].(float64)

		// Apply pool filter
		if hasPoolFilter && poolName != poolFilter {
			continue
		}

		status := map[string]interface{}{
			"name":       poolName,
			"id":         int(poolID),
			"size_bytes": pool["size"],
			"size_human": formatBytes(int64(pool["size"].(float64))),
			"status":     pool["status"],
		}

		// Find schedule for this pool
		for _, schedule := range schedules {
			schedPoolID, _ := schedule["pool"].(float64)
			if int(schedPoolID) == int(poolID) {
				withSchedules++
				enabled, _ := schedule["enabled"].(bool)
				threshold, _ := schedule["threshold"].(float64)

				schedObj := schedule["schedule"].(map[string]interface{})
				scheduleHuman := formatCronSchedule(schedObj)
				nextRun := calculateNextRun(schedObj, time.Now())

				status["schedule"] = map[string]interface{}{
					"enabled":        enabled,
					"schedule_human": scheduleHuman,
					"next_run":       nextRun,
					"threshold_days": int(threshold),
				}
				break
			}
		}

		if status["schedule"] == nil {
			withoutSchedules++
		}

		// Find running scrub job
		for _, job := range jobs {
			jobArgs, ok := job["arguments"].([]interface{})
			if ok && len(jobArgs) > 0 {
				if jobPoolName, ok := jobArgs[0].(string); ok && jobPoolName == poolName {
					scrubNowCount++
					progress, _ := job["progress"].(map[string]interface{})
					percent, _ := progress["percent"].(float64)
					description, _ := progress["description"].(string)

					timeStarted, _ := job["time_started"].(map[string]interface{})
					startedSec, _ := timeStarted["$date"].(float64)
					started := time.Unix(int64(startedSec/1000), 0)

					status["current_scrub"] = map[string]interface{}{
						"running":     true,
						"job_id":      int(job["id"].(float64)),
						"progress":    percent,
						"description": description,
						"started":     started.Format(time.RFC3339),
					}
					break
				}
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
						lastScrub["completed"] = completed.Format(time.RFC3339)
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

	response := map[string]interface{}{
		"pool":           poolName,
		"schedule_id":    created["id"],
		"enabled":        enabled,
		"threshold_days": threshold,
		"schedule_human": formatCronSchedule(scheduleObj),
		"next_run":       calculateNextRun(scheduleObj, time.Now()),
		"message":        fmt.Sprintf("Scrub schedule created for pool '%s'. First run: %s", poolName, calculateNextRun(scheduleObj, time.Now())),
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

	// Check if scrub already running
	jobsResult, err := client.Call("core.get_jobs", []interface{}{
		[]interface{}{"method", "=", "pool.scrub.scrub"},
		[]interface{}{"state", "in", []string{"RUNNING", "WAITING"}},
	})
	if err != nil {
		return "", fmt.Errorf("failed to check running scrubs: %w", err)
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(jobsResult, &jobs); err != nil {
		return "", fmt.Errorf("failed to parse jobs: %w", err)
	}

	for _, job := range jobs {
		if jobArgs, ok := job["arguments"].([]interface{}); ok && len(jobArgs) > 0 {
			if jobPoolName, ok := jobArgs[0].(string); ok && jobPoolName == poolName {
				return "", fmt.Errorf("scrub already running on pool '%s' (job id: %v)", poolName, job["id"])
			}
		}
	}

	// Start scrub
	_, err = client.Call("pool.scrub.run", poolName, threshold)
	if err != nil {
		return "", fmt.Errorf("failed to start scrub: %w", err)
	}

	// Wait a moment for job to be created
	time.Sleep(500 * time.Millisecond)

	// Find the newly created job
	jobID, err := findLatestScrubJob(client, poolName)
	if err != nil {
		return "", fmt.Errorf("scrub started but failed to find job: %w", err)
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

	estimatedHours := estimateScrubDuration(int64(poolInfo["size"].(float64)))

	response := map[string]interface{}{
		"pool":                     poolName,
		"scrub_started":            true,
		"job_id":                   jobID,
		"task_id":                  task.TaskID,
		"task_status":              task.Status,
		"estimated_duration_hours": estimatedHours,
		"poll_interval":            30,
		"message":                  fmt.Sprintf("Scrub started on pool '%s'. Track progress: (1) tasks_get with task_id: %s, or (2) get_scrub_status", poolName, task.TaskID),
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

	var lastScrubDate string
	if len(pools) > 0 {
		if scan, ok := pools[0]["scan"].(map[string]interface{}); ok {
			if endTime, ok := scan["end_time"].(map[string]interface{}); ok {
				if endSec, ok := endTime["$date"].(float64); ok {
					lastScrubDate = time.Unix(int64(endSec/1000), 0).Format(time.RFC3339)
				}
			}
		}
	}

	scheduleHuman := formatCronSchedule(scheduleObj)
	firstRun := calculateNextRun(scheduleObj, time.Now())
	estimatedHours := estimateScrubDuration(int64(poolInfo["size"].(float64)))

	warnings := []string{}
	if existingSchedule != nil {
		warnings = append(warnings, fmt.Sprintf("ERROR: Pool '%s' already has a scrub schedule (id: %v)", poolName, existingSchedule["id"]))
		warnings = append(warnings, "Delete the existing schedule first, or choose a different pool")
	} else {
		warnings = append(warnings, fmt.Sprintf("First scrub will run on %s", firstRun))
		warnings = append(warnings, fmt.Sprintf("Scrub may take %d-%d hours based on pool size", estimatedHours, estimatedHours*3))
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

	actions := []PlannedAction{}
	if existingSchedule == nil {
		actions = append(actions, PlannedAction{
			Step:        1,
			Description: fmt.Sprintf("Create scrub schedule for pool '%s'", poolName),
			Operation:   "create",
			Target:      poolName,
			Details: map[string]interface{}{
				"schedule_human": scheduleHuman,
				"first_run":      firstRun,
				"threshold_days": threshold,
				"enabled":        enabled,
			},
		})
	}

	return &DryRunResult{
		Tool: "create_scrub_schedule",
		CurrentState: map[string]interface{}{
			"pool":              poolName,
			"pool_id":           poolInfo["id"],
			"pool_size":         formatBytes(int64(poolInfo["size"].(float64))),
			"existing_schedule": existingSchedule,
			"last_scrub":        lastScrubDate,
		},
		PlannedActions: actions,
		Warnings:       warnings,
		EstimatedTime: &EstimatedTime{
			MinSeconds: estimatedHours * 3600,
			MaxSeconds: estimatedHours * 3 * 3600,
			Note:       fmt.Sprintf("Scrub duration: %d-%d hours for %s pools", estimatedHours, estimatedHours*3, formatBytes(int64(poolInfo["size"].(float64)))),
		},
	}, nil
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

	status, _ := poolInfo["status"].(string)
	sizeBytes := int64(poolInfo["size"].(float64))

	// Check running scrub
	jobsResult, err := client.Call("core.get_jobs", []interface{}{
		[]interface{}{
			[]interface{}{"method", "=", "pool.scrub.scrub"},
			[]interface{}{"state", "in", []string{"RUNNING", "WAITING"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to check running scrubs: %w", err)
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(jobsResult, &jobs); err != nil {
		return nil, fmt.Errorf("failed to parse jobs: %w", err)
	}

	scrubRunning := false
	for _, job := range jobs {
		if jobArgs, ok := job["arguments"].([]interface{}); ok && len(jobArgs) > 0 {
			if jobPoolName, ok := jobArgs[0].(string); ok && jobPoolName == poolName {
				scrubRunning = true
				break
			}
		}
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
					lastScrub["date"] = completed.Format(time.RFC3339)
					lastScrub["days_ago"] = int(time.Since(completed).Hours() / 24)
				}
			}
		}
	}

	estimatedHours := estimateScrubDuration(sizeBytes)
	minSeconds := estimatedHours * 3600
	maxSeconds := estimatedHours * 3 * 3600

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
		actions = append(actions, PlannedAction{
			Step:        1,
			Description: fmt.Sprintf("Start manual scrub of pool '%s'", poolName),
			Operation:   "scrub",
			Target:      poolName,
			Details: map[string]interface{}{
				"pool_size":                formatBytes(sizeBytes),
				"estimated_duration_hours": estimatedHours,
			},
		})
	}

	requirements := &Requirements{
		Conditions: []string{
			fmt.Sprintf("Pool must be ONLINE (current: %s)", status),
			fmt.Sprintf("No existing scrub running (current: %v)", map[bool]string{true: "running", false: "none"}[scrubRunning]),
		},
	}

	return &DryRunResult{
		Tool: "run_scrub",
		CurrentState: map[string]interface{}{
			"pool":          poolName,
			"pool_id":       poolInfo["id"],
			"size":          formatBytes(sizeBytes),
			"status":        status,
			"last_scrub":    lastScrub,
			"scrub_running": scrubRunning,
		},
		PlannedActions: actions,
		Warnings:       warnings,
		Requirements:   requirements,
		EstimatedTime: &EstimatedTime{
			MinSeconds: minSeconds,
			MaxSeconds: maxSeconds,
			Note:       "Duration varies by pool size, data amount, and system load",
		},
	}, nil
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
	schedObj := schedule["schedule"].(map[string]interface{})

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

func simplifyScrubSchedule(schedule map[string]interface{}) map[string]interface{} {
	scheduleObj := schedule["schedule"].(map[string]interface{})

	return map[string]interface{}{
		"id":             schedule["id"],
		"pool":           schedule["pool_name"],
		"pool_id":        schedule["pool"],
		"enabled":        schedule["enabled"],
		"threshold":      schedule["threshold"],
		"description":    schedule["description"],
		"schedule":       scheduleObj,
		"schedule_human": formatCronSchedule(scheduleObj),
		"next_run":       calculateNextRun(scheduleObj, time.Now()),
	}
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

func calculateNextRun(schedule map[string]interface{}, fromTime time.Time) string {
	// Simplified calculation - just add one week/month/day based on pattern
	// In production, would use a proper cron library
	minute, _ := schedule["minute"].(string)
	hour, _ := schedule["hour"].(string)
	dom, _ := schedule["dom"].(string)
	dow, _ := schedule["dow"].(string)

	minuteInt, hourInt := 0, 0
	fmt.Sscanf(minute, "%d", &minuteInt)
	fmt.Sscanf(hour, "%d", &hourInt)

	now := fromTime

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

		next := now.AddDate(0, 0, daysUntil)
		next = time.Date(next.Year(), next.Month(), next.Day(), hourInt, minuteInt, 0, 0, next.Location())
		return next.Format(time.RFC3339)
	}

	// Monthly
	if dom != "*" && dow == "*" {
		domInt := 0
		fmt.Sscanf(dom, "%d", &domInt)

		next := time.Date(now.Year(), now.Month(), domInt, hourInt, minuteInt, 0, 0, now.Location())
		if next.Before(now) {
			next = next.AddDate(0, 1, 0)
		}
		return next.Format(time.RFC3339)
	}

	// Daily
	next := time.Date(now.Year(), now.Month(), now.Day(), hourInt, minuteInt, 0, 0, now.Location())
	if next.Before(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Format(time.RFC3339)
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

func findLatestScrubJob(client *truenas.Client, poolName string) (int, error) {
	result, err := client.Call("core.get_jobs", []interface{}{
		[]interface{}{"method", "=", "pool.scrub.scrub"},
	})
	if err != nil {
		return 0, fmt.Errorf("failed to query jobs: %w", err)
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(result, &jobs); err != nil {
		return 0, fmt.Errorf("failed to parse jobs: %w", err)
	}

	// Find most recent job for this pool
	var latestJob map[string]interface{}
	var latestTime float64

	for _, job := range jobs {
		if jobArgs, ok := job["arguments"].([]interface{}); ok && len(jobArgs) > 0 {
			if jobPoolName, ok := jobArgs[0].(string); ok && jobPoolName == poolName {
				if timeStarted, ok := job["time_started"].(map[string]interface{}); ok {
					if startSec, ok := timeStarted["$date"].(float64); ok {
						if startSec > latestTime {
							latestTime = startSec
							latestJob = job
						}
					}
				}
			}
		}
	}

	if latestJob == nil {
		return 0, fmt.Errorf("no scrub job found for pool '%s'", poolName)
	}

	jobID, _ := latestJob["id"].(float64)
	return int(jobID), nil
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

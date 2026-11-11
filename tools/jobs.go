package tools

// Job query tool and its handler (query_jobs).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerJobsTools() {
	// Query jobs
	r.tools["query_jobs"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_jobs",
			Description: "Query system jobs (running, pending, or completed tasks like replication, snapshots, scrubs, app upgrades, etc.), most recent first. Returns jobs in every state unless state is given; method narrows to one job method (e.g. app.upgrade). state is case-insensitive, including all; with no state or all the response reports state_filter \"ALL\". Reports has_more and total when the list was cut by limit; long results and error text are truncated with a flag. A job state of SUCCESS means the job finished, not that the app or service it acted on is healthy: an app embedded in a job result is a copy from when the job ended. Use tasks_get or query_apps for the current app state.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					// No enum: state is case-insensitive, which an enum
					// cannot express. The handler checks the value.
					"state": map[string]interface{}{
						"type":        "string",
						"description": "Filter by job state: RUNNING, WAITING, SUCCESS, FAILED, ABORTED or all, case-insensitive (default: all states; all is reported as state_filter \"ALL\")",
						"default":     "all",
					},
					"method": map[string]interface{}{
						"type":        "string",
						"description": "Optional: only return jobs whose method is exactly this name, e.g. app.upgrade or pool.scrub.scrub",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of jobs to return (default: 50). The response has has_more (and total, the number of jobs matching the state and method filters) when more exist than were returned. Each job entry is at most 4 KiB. Must be an integer from 1 to 9999; other values are rejected with an error and nothing is queried.",
						"default":     50,
						"minimum":     1,
						"maximum":     maxQueryJobsLimit,
					},
				},
			},
		},
		Handler: handleQueryJobs,
	}
}

// maxQueryJobsLimit is the largest query_jobs limit: one below the
// middleware's 10000-row maximum, as one extra row is requested.
const maxQueryJobsLimit = 9999

// jobStates are the TrueNAS middleware job states query_jobs filters on.
var jobStates = map[string]bool{"WAITING": true, "RUNNING": true, "SUCCESS": true, "FAILED": true, "ABORTED": true}

const validJobStatesText = "RUNNING, WAITING, SUCCESS, FAILED, ABORTED, or all (case-insensitive)"

func handleQueryJobs(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	// No state (or "all", any case) means every state, reported as "ALL".
	// Values are checked before anything is sent; they are not echoed.
	state := "ALL"
	if v, present := args["state"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("invalid arguments for tool query_jobs: state must be a string, one of %s; nothing was executed", validJobStatesText)
		}
		s = strings.ToUpper(strings.TrimSpace(s))
		if s != "" && s != "ALL" {
			if !jobStates[s] {
				return "", fmt.Errorf("invalid arguments for tool query_jobs: unknown state (valid: %s); nothing was executed", validJobStatesText)
			}
			state = s
		}
	}
	method := ""
	if v, present := args["method"]; present && v != nil {
		m, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("invalid arguments for tool query_jobs: method must be a string, an exact job method name such as app.upgrade; nothing was executed")
		}
		method = m
	}

	// limit must be a whole number from 1 to maxQueryJobsLimit; anything
	// else is rejected rather than coerced, and the value is not echoed.
	limit := 50
	if v, present := args["limit"]; present && v != nil {
		l, ok := toInt(v)
		if !ok || l < 1 || l > maxQueryJobsLimit {
			return "", fmt.Errorf("invalid arguments for tool query_jobs: limit must be an integer from 1 to %d; nothing was executed", maxQueryJobsLimit)
		}
		limit = l
	}

	// Filter server-side so limit, has_more and total apply to the
	// matching jobs only.
	filters := []interface{}{}
	if state != "ALL" {
		filters = append(filters, []interface{}{"state", "=", state})
	}
	if method != "" {
		filters = append(filters, []interface{}{"method", "=", method})
	}

	// Build options
	// One row more than asked for tells whether the list was cut. limit is
	// capped at maxQueryJobsLimit so this stays within the middleware's
	// 10000-row maximum.
	options := map[string]interface{}{
		"limit":    limit + 1,
		"order_by": []string{"-time_started"}, // Most recent first
	}

	result, err := client.Call(ctx, "core.get_jobs", filters, options)
	if err != nil {
		return "", fmt.Errorf("failed to query jobs: %w", err)
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(result, &jobs); err != nil {
		return "", fmt.Errorf("failed to parse jobs: %w", err)
	}
	hasMore := len(jobs) > limit
	if hasMore {
		jobs = jobs[:limit]
	}
	total := len(jobs)
	totalKnown := true
	if hasMore {
		// The full count is a separate cheap query; if it fails the list is
		// still returned, flagged has_more, without a total.
		totalKnown = false
		if raw, err := client.Call(ctx, "core.get_jobs", filters, map[string]interface{}{"count": true}); err == nil {
			var n float64
			if json.Unmarshal(raw, &n) == nil && int(n) >= len(jobs) {
				total = int(n)
				totalKnown = true
			}
		}
	}

	// Create simplified response with relevant fields
	simplified := make([]map[string]interface{}, 0, len(jobs))
	for _, job := range jobs {
		jobInfo := map[string]interface{}{
			"id":          job["id"],
			"method":      job["method"],
			"state":       job["state"],
			"description": job["description"],
		}

		// Add optional fields if present
		if progress, ok := job["progress"]; ok && progress != nil {
			jobInfo["progress"] = progress
		}
		if timeStarted, ok := job["time_started"]; ok && timeStarted != nil {
			jobInfo["time_started"] = readableDates(timeStarted)
		}
		if timeFinished, ok := job["time_finished"]; ok && timeFinished != nil {
			jobInfo["time_finished"] = readableDates(timeFinished)
		}
		if result, ok := job["result"]; ok && result != nil {
			// A job result can be a whole app entry or similar: bound it and
			// say so when it was cut.
			bounded, truncated, size := tasks.BoundResult(readableDates(result))
			jobInfo["result"] = bounded
			if truncated {
				jobInfo["result_truncated"] = true
				jobInfo["result_size_bytes"] = size
			}
		}
		if errorMsg, ok := job["error"]; ok && errorMsg != nil {
			if text, isText := errorMsg.(string); isText {
				cut, truncated := tasks.BoundText(text, tasks.MaxJobTextBytes)
				jobInfo["error"] = cut
				if truncated {
					jobInfo["error_truncated"] = true
				}
			} else {
				jobInfo["error"] = errorMsg
			}
		}
		if exception, ok := job["exception"]; ok && exception != nil {
			if text, isText := exception.(string); isText {
				cut, truncated := tasks.BoundText(text, tasks.MaxJobTextBytes)
				jobInfo["exception"] = cut
				if truncated {
					jobInfo["exception_truncated"] = true
				}
			} else {
				jobInfo["exception"] = exception
			}
		}
		if abortable, ok := job["abortable"]; ok {
			jobInfo["abortable"] = abortable
		}

		// Each entry as a whole stays within tasks.MaxJobResultBytes.
		simplified = append(simplified, tasks.FitJobSummary(jobInfo))
	}

	response := map[string]interface{}{
		"jobs":         simplified,
		"job_count":    len(simplified),
		"state_filter": state,
		"has_more":     hasMore,
		"limit":        limit,
	}
	if method != "" {
		response["method_filter"] = method
	}
	if totalKnown {
		response["total"] = total
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

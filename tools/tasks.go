package tools

// Task tools and their handlers (tasks_list, tasks_get).

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerTaskTools() {
	// Task management tools
	r.tools["tasks_list"] = Tool{
		Definition: mcp.Tool{
			Name:        "tasks_list",
			Description: "List tasks started by this server (newest first): running tasks, and finished ones for 24 hours after they finish (at most 500; the oldest finished are dropped first). Each task names the tool that started it (tool), the TrueNAS job it tracks (job_id), its status and statusMessage, and, once the job succeeded, its result (bounded; result_truncated is set when cut). Tasks are held in this server's memory only and are lost when it restarts.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"cursor": map[string]interface{}{
						"type":        "string",
						"description": "Pagination cursor from previous response",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of tasks to return (default: 50)",
						"minimum":     1,
						"default":     50,
					},
				},
			},
		},
		Handler: r.handleTasksList,
	}

	r.tools["tasks_get"] = Tool{
		Definition: mcp.Tool{
			Name:        "tasks_get",
			Description: "Get the status of a task by ID: tool, job_id, status (working, completed, failed or cancelled), statusMessage and, once the job succeeded, its result (bounded; result_truncated is set when cut). If TrueNAS or its middleware restarts, the tracked job is gone and the task fails with a message naming the tool to check the state with. A task still working when its ttl (seconds since createdAt) runs out fails with \"tracking timed out\"; the operation may still be running. Finished tasks stay readable for 24 hours.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task_id": map[string]interface{}{
						"type":        "string",
						"description": "Task ID to retrieve",
					},
				},
				"required": []string{"task_id"},
			},
		},
		Handler: r.handleTasksGet,
	}
}

// handleTasksList lists all active and recent tasks
func (r *Registry) handleTasksList(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	cursor := ""
	if c, ok := args["cursor"].(string); ok {
		cursor = c
	}

	limit := 50
	if l, ok := args["limit"].(float64); ok && l >= 1 {
		limit = int(l)
	}

	taskList, nextCursor, err := r.taskManager.List(cursor, limit)
	if err != nil {
		return "", fmt.Errorf("failed to list tasks: %w", err)
	}

	response := map[string]interface{}{
		"tasks": taskList,
	}
	if nextCursor != "" {
		response["next_cursor"] = nextCursor
	}

	formatted, _ := json.MarshalIndent(response, "", "  ")
	return string(formatted), nil
}

// handleTasksGet retrieves a specific task by ID
func (r *Registry) handleTasksGet(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	taskID, ok := args["task_id"].(string)
	if !ok || taskID == "" {
		return "", fmt.Errorf("task_id is required")
	}

	task, err := r.taskManager.Get(taskID)
	if err != nil {
		return "", fmt.Errorf("failed to get task: %w", err)
	}

	formatted, _ := json.MarshalIndent(task, "", "  ")
	return string(formatted), nil
}

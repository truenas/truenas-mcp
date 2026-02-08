package tools

import (
	"encoding/json"

	"github.com/truenas/truenas-mcp/truenas"
)

// DryRunnable is implemented by tools that support dry-run mode
type DryRunnable interface {
	ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error)
}

// DryRunResult represents the preview of changes that would be made
type DryRunResult struct {
	Tool           string          `json:"tool"`
	CurrentState   interface{}     `json:"current_state"`
	PlannedActions []PlannedAction `json:"planned_actions"`
	Warnings       []string        `json:"warnings,omitempty"`
	Requirements   *Requirements   `json:"requirements,omitempty"`
	EstimatedTime  *EstimatedTime  `json:"estimated_time,omitempty"`
}

// PlannedAction describes a single step in the operation
type PlannedAction struct {
	Step        int         `json:"step"`
	Description string      `json:"description"`
	Operation   string      `json:"operation"` // "update", "restart", "create", "delete", etc.
	Target      string      `json:"target"`
	Details     interface{} `json:"details,omitempty"`
}

// Requirements describes prerequisites or dependencies
type Requirements struct {
	DiskSpace  *int64   `json:"disk_space_bytes,omitempty"`
	Memory     *int64   `json:"memory_bytes,omitempty"`
	Services   []string `json:"services,omitempty"`
	Conditions []string `json:"conditions,omitempty"`
}

// EstimatedTime provides time estimates for the operation
type EstimatedTime struct {
	MinSeconds int    `json:"min_seconds"`
	MaxSeconds int    `json:"max_seconds"`
	Note       string `json:"note,omitempty"`
}

// ExecuteWithDryRun wraps a handler to support dry-run mode
// If dry_run is true, calls ExecuteDryRun; otherwise calls normalHandler
func ExecuteWithDryRun(
	client *truenas.Client,
	args map[string]interface{},
	dryRunnable DryRunnable,
	normalHandler func(*truenas.Client, map[string]interface{}) (string, error),
) (string, error) {
	// Check if dry_run is requested
	dryRun, ok := args["dry_run"].(bool)
	if !ok || !dryRun {
		// Normal execution
		return normalHandler(client, args)
	}

	// Dry-run execution
	result, err := dryRunnable.ExecuteDryRun(client, args)
	if err != nil {
		return "", err
	}

	// Format the result as JSON
	formatted, err := marshalJSON(result)
	if err != nil {
		return "", err
	}

	return formatted, nil
}

// marshalJSON is a helper to format results as indented JSON
func marshalJSON(v interface{}) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

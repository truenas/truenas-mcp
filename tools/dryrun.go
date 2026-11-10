package tools

import (
	"context"
	"encoding/json"

	"github.com/truenas/truenas-mcp/truenas"
)

// DryRunnable is implemented by tools that support dry-run mode
type DryRunnable interface {
	ExecuteDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*DryRunResult, error)
}

// DryRunResult represents the preview of changes that would be made
type DryRunResult struct {
	Tool           string          `json:"tool"`
	CurrentState   interface{}     `json:"current_state"`
	PlannedActions []PlannedAction `json:"planned_actions"`
	Warnings       []string        `json:"warnings,omitempty"`
	// Blocked is the list of reasons the operation cannot proceed. It is
	// separate from warnings: a warning is informational, a blocked reason
	// is refused on both the preview and the real call.
	Blocked       []string       `json:"blocked,omitempty"`
	Requirements  *Requirements  `json:"requirements,omitempty"`
	EstimatedTime *EstimatedTime `json:"estimated_time,omitempty"`
	// DryRun and Executed mark the result explicitly as a preview. They are
	// optional so tools that do not set them keep their output unchanged.
	DryRun   *bool `json:"dry_run,omitempty"`
	Executed *bool `json:"executed,omitempty"`
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
	ctx context.Context,
	client *truenas.Client,
	args map[string]interface{},
	dryRunnable DryRunnable,
	normalHandler func(context.Context, *truenas.Client, map[string]interface{}) (string, error),
) (string, error) {
	// Check if dry_run is requested
	dryRun, ok := args["dry_run"].(bool)
	if !ok || !dryRun {
		// Normal execution
		return normalHandler(ctx, client, args)
	}

	// Dry-run execution
	result, err := dryRunnable.ExecuteDryRun(ctx, client, args)
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

package tools

// Alert tools and the list_alerts handler (list_alerts, dismiss_alert,
// restore_alert). dismiss_alert and restore_alert handlers live in
// dryrun_maintenance.go.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerAlertTools() {
	// Alert list with filtering
	r.tools["list_alerts"] = Tool{
		Definition: mcp.Tool{
			Name:        "list_alerts",
			Description: "List system alerts. By default only active alerts are listed: dismissed alerts are excluded. Set include_dismissed=true to list all alerts, or dismissed=true to list only dismissed ones. Returns allow-listed alert fields (uuid, level, text, and so on). The middleware dedup key is omitted because it would be masked by output redaction; use uuid with dismiss_alert or restore_alert.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dismissed": map[string]interface{}{
						"type":        "boolean",
						"description": "Filter by dismissed status (true=dismissed only, false=active only, omit=active only unless include_dismissed=true)",
					},
					"include_dismissed": map[string]interface{}{
						"type":        "boolean",
						"description": "Also list dismissed alerts when dismissed is not set (default: false, dismissed alerts are excluded)",
						"default":     false,
					},
				},
			},
		},
		Handler: handleListAlerts,
	}

	// Dismiss alert
	r.tools["dismiss_alert"] = Tool{
		Definition: mcp.Tool{
			Name:        "dismiss_alert",
			Description: "Dismiss a system alert by UUID",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"uuid": map[string]interface{}{
						"type":        "string",
						"description": "UUID of the alert to dismiss",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview without changing the alert (default: false)",
						"default":     false,
					},
				},
				"required": []string{"uuid"},
			},
		},
		Handler: handleDismissAlertWithDryRun,
		Plan:    planDismissAlert,
	}

	// Restore alert
	r.tools["restore_alert"] = Tool{
		Definition: mcp.Tool{
			Name:        "restore_alert",
			Description: "Restore (un-dismiss) a previously dismissed alert by UUID",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"uuid": map[string]interface{}{
						"type":        "string",
						"description": "UUID of the alert to restore",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview without changing the alert (default: false)",
						"default":     false,
					},
				},
				"required": []string{"uuid"},
			},
		},
		Handler: handleRestoreAlertWithDryRun,
		Plan:    planRestoreAlert,
	}
}

// Alert management handlers

func handleListAlerts(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	// Validate every argument before calling the API. A value that is not a
	// boolean is rejected rather than ignored, and values are not echoed.
	dismissed, hasDismissed := false, false
	if v, present := args["dismissed"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("invalid arguments for tool list_alerts: dismissed must be a boolean (true or false); nothing was executed")
		}
		dismissed, hasDismissed = b, true
	}
	includeDismissed, hasInclude := false, false
	if v, present := args["include_dismissed"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("invalid arguments for tool list_alerts: include_dismissed must be a boolean (true or false); nothing was executed")
		}
		includeDismissed, hasInclude = b, true
	}
	if hasDismissed && hasInclude && dismissed != includeDismissed {
		if dismissed {
			return "", fmt.Errorf("invalid arguments for tool list_alerts: dismissed=true lists only dismissed alerts and cannot be combined with include_dismissed=false; nothing was executed")
		}
		return "", fmt.Errorf("invalid arguments for tool list_alerts: dismissed=false lists only active alerts and cannot be combined with include_dismissed=true; nothing was executed")
	}

	// alert.list doesn't take filter parameters in the same way as other queries
	// It just returns all alerts, so we'll filter in post-processing if needed
	result, err := client.Call(ctx, "alert.list")
	if err != nil {
		return "", err
	}

	var alerts []map[string]interface{}
	if err := json.Unmarshal(result, &alerts); err != nil {
		return "", fmt.Errorf("failed to parse alerts: %w", err)
	}

	// Post-filter by dismissed status. An explicit dismissed filter wins and
	// keeps only alerts whose dismissed field matches it; otherwise only
	// alerts marked dismissed are excluded unless include_dismissed=true.
	switch {
	case hasDismissed:
		filtered := make([]map[string]interface{}, 0)
		for _, alert := range alerts {
			if isDismissed, ok := alert["dismissed"].(bool); ok && isDismissed == dismissed {
				filtered = append(filtered, alert)
			}
		}
		alerts = filtered
	case !includeDismissed:
		filtered := make([]map[string]interface{}, 0)
		for _, alert := range alerts {
			if isDismissed, _ := alert["dismissed"].(bool); !isDismissed {
				filtered = append(filtered, alert)
			}
		}
		alerts = filtered
	}
	for i, alert := range alerts {
		alerts[i] = simplifyAlert(alert)
	}

	formatted, err := json.MarshalIndent(alerts, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

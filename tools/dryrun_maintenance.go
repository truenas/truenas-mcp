package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/truenas/truenas-mcp/truenas"
)

// Dry-run support for system_reboot, dismiss_alert, restore_alert and
// refresh_directory_cache.

func handleSystemRebootWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &systemRebootDryRun{}, handleSystemReboot)
}

// maxRebootDelay caps system_reboot's delay at one day: no tool can cancel a
// pending reboot.
const maxRebootDelay = 86400

// rebootDelay returns the optional delay argument in seconds.
func rebootDelay(args map[string]interface{}) (int, error) {
	v, ok := args["delay"]
	if !ok || v == nil {
		return 0, nil
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f != math.Trunc(f) || f > maxRebootDelay {
		return 0, fmt.Errorf("delay must be an integer number of seconds between 0 and %d", maxRebootDelay)
	}
	return int(f), nil
}

type systemRebootDryRun struct{}

func (s *systemRebootDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	delay, err := rebootDelay(args)
	if err != nil {
		return nil, err
	}
	result, err := client.Call("system.info")
	if err != nil {
		return nil, fmt.Errorf("failed to get system info: %w", err)
	}
	var info map[string]interface{}
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("failed to parse system info: %w", err)
	}
	when := "immediately"
	if delay > 0 {
		when = fmt.Sprintf("after %d seconds", delay)
	}
	return &DryRunResult{
		Tool: "system_reboot",
		CurrentState: map[string]interface{}{
			"hostname":       info["hostname"],
			"version":        info["version"],
			"uptime_seconds": info["uptime_seconds"],
		},
		PlannedActions: []PlannedAction{{
			Step:        1,
			Description: "Reboot the system " + when,
			Operation:   "reboot",
			Target:      "system.reboot",
			Details:     map[string]interface{}{"delay_seconds": delay},
		}},
		Warnings: []string{
			"All services (shares, apps, VMs) will be interrupted and every client connection will be dropped.",
			"This MCP server will lose its connection until TrueNAS is back up.",
		},
		EstimatedTime: &EstimatedTime{MinSeconds: 120, MaxSeconds: 600, Note: "Time for the system to come back up"},
	}, nil
}

func handleDismissAlertWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &alertDryRun{tool: "dismiss_alert", method: "alert.dismiss"}, handleDismissAlert)
}

func handleRestoreAlertWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &alertDryRun{tool: "restore_alert", method: "alert.restore"}, handleRestoreAlert)
}

type alertDryRun struct{ tool, method string }

func (a *alertDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	uuid, ok := args["uuid"].(string)
	if !ok || uuid == "" {
		return nil, fmt.Errorf("uuid parameter is required")
	}
	result, err := client.Call("alert.list")
	if err != nil {
		return nil, err
	}
	var alerts []map[string]interface{}
	if err := json.Unmarshal(result, &alerts); err != nil {
		return nil, fmt.Errorf("failed to parse alerts: %w", err)
	}
	var current map[string]interface{}
	for _, alert := range alerts {
		if alert["uuid"] == uuid {
			current = alert
			break
		}
	}
	if current == nil {
		return nil, fmt.Errorf("alert not found: %s", uuid)
	}
	return &DryRunResult{
		Tool:         a.tool,
		CurrentState: current,
		PlannedActions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Call %s for alert %s", a.method, uuid),
			Operation:   "update",
			Target:      a.method,
		}},
	}, nil
}

func handleRefreshDirectoryCacheWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &refreshDirectoryCacheDryRun{}, handleRefreshDirectoryCache)
}

type refreshDirectoryCacheDryRun struct{}

func (d *refreshDirectoryCacheDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	status, err := getDirectoryServiceStatus(context.Background(), client)
	if err != nil {
		return nil, err
	}
	result := &DryRunResult{
		Tool:         "refresh_directory_cache",
		CurrentState: status,
		PlannedActions: []PlannedAction{{
			Step:        1,
			Description: "Refresh cached directory users and groups",
			Operation:   "refresh",
			Target:      "directoryservices.cache_refresh",
		}},
	}
	if status.Type == "none" || !status.Enabled {
		result.Warnings = []string{"No directory service is enabled; the refresh would fail."}
	}
	return result, nil
}

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

const systemRebootMethod = "system.reboot"
const systemRebootReason = "System reboot requested via MCP"

func handleSystemRebootWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planSystemReboot(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeSystemReboot(ctx, client, args, plan)
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

// planSystemReboot builds the system_reboot plan used by both the dry-run
// preview and the real call. A delay of 0 is omitted from the middleware
// payload, matching the previous handler.
func planSystemReboot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	delay, err := rebootDelay(args)
	if err != nil {
		return nil, err
	}
	result, err := client.Call(ctx, "system.info")
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
	params := []interface{}{systemRebootReason}
	names := []string{"reason"}
	if delay > 0 {
		params = append(params, map[string]interface{}{"delay": delay})
		names = append(names, "options")
	}
	return &writePlan{
		Tool:       "system_reboot",
		Method:     systemRebootMethod,
		CallParams: params,
		ParamNames: names,
		CurrentState: map[string]interface{}{
			"hostname":       info["hostname"],
			"version":        info["version"],
			"uptime_seconds": info["uptime_seconds"],
		},
		Actions: []PlannedAction{{
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
		Result:        map[string]interface{}{"delay": delay},
	}, nil
}

func executeSystemReboot(ctx context.Context, client *truenas.Client, _ map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to initiate system reboot: %w", err)
	}

	delay := 0
	if plan.Result != nil {
		if d, ok := plan.Result["delay"].(int); ok {
			delay = d
		}
	}

	var response map[string]interface{}
	if len(result) > 0 {
		_ = json.Unmarshal(result, &response)
	}

	returnMsg := map[string]interface{}{
		"status":  "reboot_initiated",
		"message": "System reboot initiated. All connections will be lost.",
		"warning": "TrueNAS system is rebooting. Wait approximately 2-3 minutes before reconnecting.",
	}
	if delay > 0 {
		returnMsg["delay_seconds"] = delay
		returnMsg["message"] = fmt.Sprintf("System reboot scheduled in %d seconds. All connections will be lost.", delay)
	}

	formatted, err := json.MarshalIndent(returnMsg, "", "  ")
	if err != nil {
		return "", err
	}
	return string(formatted), nil
}

func handleDismissAlertWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(ctx, client, args, &alertDryRun{tool: "dismiss_alert", method: "alert.dismiss"}, handleDismissAlert)
}

func handleRestoreAlertWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(ctx, client, args, &alertDryRun{tool: "restore_alert", method: "alert.restore"}, handleRestoreAlert)
}

type alertDryRun struct{ tool, method string }

func (a *alertDryRun) ExecuteDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	uuid, ok := args["uuid"].(string)
	if !ok || uuid == "" {
		return nil, fmt.Errorf("uuid parameter is required")
	}
	result, err := client.Call(ctx, "alert.list")
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

func handleRefreshDirectoryCacheWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(ctx, client, args, &refreshDirectoryCacheDryRun{}, handleRefreshDirectoryCache)
}

type refreshDirectoryCacheDryRun struct{}

func (d *refreshDirectoryCacheDryRun) ExecuteDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	status, err := getDirectoryServiceStatus(ctx, client)
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

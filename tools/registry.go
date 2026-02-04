package tools

import (
	"encoding/json"
	"fmt"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

type Registry struct {
	client *truenas.Client
	tools  map[string]Tool
}

type Tool struct {
	Definition mcp.Tool
	Handler    func(*truenas.Client, map[string]interface{}) (string, error)
}

func NewRegistry(client *truenas.Client) *Registry {
	r := &Registry{
		client: client,
		tools:  make(map[string]Tool),
	}
	r.registerTools()
	return r
}

func (r *Registry) registerTools() {
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

	// Storage pools query
	r.tools["query_pools"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_pools",
			Description: "Query storage pools with their status, capacity, and health information",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleQueryPools,
	}

	// Dataset query
	r.tools["query_datasets"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_datasets",
			Description: "Query datasets with optional filtering. Provide 'pool' parameter to filter by pool name.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pool": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter datasets by pool name",
					},
				},
			},
		},
		Handler: handleQueryDatasets,
	}

	// Shares query
	r.tools["query_shares"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_shares",
			Description: "Query SMB and NFS shares configuration",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"share_type": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"smb", "nfs", "all"},
						"description": "Type of shares to query (default: all)",
						"default":     "all",
					},
				},
			},
		},
		Handler: handleQueryShares,
	}

	// Alert list with filtering
	r.tools["list_alerts"] = Tool{
		Definition: mcp.Tool{
			Name:        "list_alerts",
			Description: "List system alerts with optional filtering by dismissed status",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dismissed": map[string]interface{}{
						"type":        "boolean",
						"description": "Filter by dismissed status (true=dismissed only, false=active only, omit=all)",
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
				},
				"required": []string{"uuid"},
			},
		},
		Handler: handleDismissAlert,
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
				},
				"required": []string{"uuid"},
			},
		},
		Handler: handleRestoreAlert,
	}

	// System reporting metrics
	r.tools["get_system_metrics"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_system_metrics",
			Description: "Get system performance metrics (CPU, memory, load average)",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"graphs": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
							"enum": []string{"cpu", "memory", "load"},
						},
						"description": "Metrics to retrieve (default: all)",
					},
					"unit": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"HOUR", "DAY", "WEEK", "MONTH", "YEAR"},
						"description": "Time range for metrics (default: HOUR)",
						"default":     "HOUR",
					},
				},
			},
		},
		Handler: handleGetSystemMetrics,
	}

	// Network reporting metrics
	r.tools["get_network_metrics"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_network_metrics",
			Description: "Get network interface traffic metrics",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"interface": map[string]interface{}{
						"type":        "string",
						"description": "Network interface name (e.g., 'eth0'). If omitted, returns all interfaces.",
					},
					"unit": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"HOUR", "DAY", "WEEK", "MONTH", "YEAR"},
						"description": "Time range for metrics (default: HOUR)",
						"default":     "HOUR",
					},
				},
			},
		},
		Handler: handleGetNetworkMetrics,
	}

	// Disk I/O reporting metrics
	r.tools["get_disk_metrics"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_disk_metrics",
			Description: "Get disk I/O performance metrics",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"disk": map[string]interface{}{
						"type":        "string",
						"description": "Disk name (e.g., 'sda'). If omitted, returns all disks.",
					},
					"unit": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"HOUR", "DAY", "WEEK", "MONTH", "YEAR"},
						"description": "Time range for metrics (default: HOUR)",
						"default":     "HOUR",
					},
				},
			},
		},
		Handler: handleGetDiskMetrics,
	}
}

func (r *Registry) ListTools() []mcp.Tool {
	tools := make([]mcp.Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool.Definition)
	}
	return tools
}

func (r *Registry) CallTool(name string, args map[string]interface{}) (string, error) {
	tool, exists := r.tools[name]
	if !exists {
		return "", fmt.Errorf("unknown tool: %s", name)
	}

	return tool.Handler(r.client, args)
}

// Tool handlers

func handleSystemInfo(client *truenas.Client, args map[string]interface{}) (string, error) {
	result, err := client.Call("system.info")
	if err != nil {
		return "", err
	}

	var info map[string]interface{}
	if err := json.Unmarshal(result, &info); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	formatted, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleSystemHealth(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Get alerts
	result, err := client.Call("alert.list")
	if err != nil {
		return "", err
	}

	var alerts []map[string]interface{}
	if err := json.Unmarshal(result, &alerts); err != nil {
		return "", fmt.Errorf("failed to parse alerts: %w", err)
	}

	response := map[string]interface{}{
		"alerts":       alerts,
		"alert_count":  len(alerts),
		"health_check": "OK",
	}

	if len(alerts) > 0 {
		response["health_check"] = "ALERTS_PRESENT"
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleQueryPools(client *truenas.Client, args map[string]interface{}) (string, error) {
	result, err := client.Call("pool.query")
	if err != nil {
		return "", err
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(result, &pools); err != nil {
		return "", fmt.Errorf("failed to parse pools (raw response: %s): %w", string(result), err)
	}

	formatted, err := json.MarshalIndent(pools, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleQueryDatasets(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Build query filters
	var filters []interface{}
	if pool, ok := args["pool"].(string); ok && pool != "" {
		filters = append(filters, []interface{}{"name", "^", pool})
	}

	result, err := client.Call("pool.dataset.query", filters)
	if err != nil {
		return "", err
	}

	var datasets []map[string]interface{}
	if err := json.Unmarshal(result, &datasets); err != nil {
		return "", fmt.Errorf("failed to parse datasets: %w", err)
	}

	formatted, err := json.MarshalIndent(datasets, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleQueryShares(client *truenas.Client, args map[string]interface{}) (string, error) {
	shareType := "all"
	if st, ok := args["share_type"].(string); ok && st != "" {
		shareType = st
	}

	response := make(map[string]interface{})

	// Query SMB shares
	if shareType == "smb" || shareType == "all" {
		result, err := client.Call("sharing.smb.query")
		if err != nil {
			return "", fmt.Errorf("failed to query SMB shares: %w", err)
		}

		var smbShares []map[string]interface{}
		if err := json.Unmarshal(result, &smbShares); err != nil {
			return "", fmt.Errorf("failed to parse SMB shares: %w", err)
		}
		response["smb_shares"] = smbShares
	}

	// Query NFS shares
	if shareType == "nfs" || shareType == "all" {
		result, err := client.Call("sharing.nfs.query")
		if err != nil {
			return "", fmt.Errorf("failed to query NFS shares: %w", err)
		}

		var nfsShares []map[string]interface{}
		if err := json.Unmarshal(result, &nfsShares); err != nil {
			return "", fmt.Errorf("failed to parse NFS shares: %w", err)
		}
		response["nfs_shares"] = nfsShares
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// Alert management handlers

func handleListAlerts(client *truenas.Client, args map[string]interface{}) (string, error) {
	// alert.list doesn't take filter parameters in the same way as other queries
	// It just returns all alerts, so we'll filter in post-processing if needed
	result, err := client.Call("alert.list")
	if err != nil {
		return "", err
	}

	var alerts []map[string]interface{}
	if err := json.Unmarshal(result, &alerts); err != nil {
		return "", fmt.Errorf("failed to parse alerts: %w", err)
	}

	// Post-filter by dismissed status if requested
	if dismissed, ok := args["dismissed"].(bool); ok {
		filtered := make([]map[string]interface{}, 0)
		for _, alert := range alerts {
			if isDismissed, ok := alert["dismissed"].(bool); ok && isDismissed == dismissed {
				filtered = append(filtered, alert)
			}
		}
		alerts = filtered
	}

	formatted, err := json.MarshalIndent(alerts, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleDismissAlert(client *truenas.Client, args map[string]interface{}) (string, error) {
	uuid, ok := args["uuid"].(string)
	if !ok || uuid == "" {
		return "", fmt.Errorf("uuid parameter is required")
	}

	result, err := client.Call("alert.dismiss", uuid)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Alert %s dismissed successfully: %s", uuid, string(result)), nil
}

func handleRestoreAlert(client *truenas.Client, args map[string]interface{}) (string, error) {
	uuid, ok := args["uuid"].(string)
	if !ok || uuid == "" {
		return "", fmt.Errorf("uuid parameter is required")
	}

	result, err := client.Call("alert.restore", uuid)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Alert %s restored successfully: %s", uuid, string(result)), nil
}

// Reporting handlers

func handleGetSystemMetrics(client *truenas.Client, args map[string]interface{}) (string, error) {
	unit := "HOUR"
	if u, ok := args["unit"].(string); ok && u != "" {
		unit = u
	}

	// Default graphs if not specified
	graphs := []string{"cpu", "memory", "load"}
	if g, ok := args["graphs"].([]interface{}); ok && len(g) > 0 {
		graphs = make([]string, len(g))
		for i, v := range g {
			if s, ok := v.(string); ok {
				graphs[i] = s
			}
		}
	}

	response := make(map[string]interface{})

	for _, graph := range graphs {
		var apiGraph string
		switch graph {
		case "cpu":
			apiGraph = "cpu"
		case "memory":
			apiGraph = "memory"
		case "load":
			apiGraph = "load"
		default:
			continue
		}

		result, err := client.Call("reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       apiGraph,
				"identifier": nil,
			},
		}, map[string]interface{}{"unit": unit})
		if err != nil {
			response[graph] = map[string]string{"error": err.Error()}
			continue
		}

		var data interface{}
		if err := json.Unmarshal(result, &data); err != nil {
			response[graph] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}
		response[graph] = data
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleGetNetworkMetrics(client *truenas.Client, args map[string]interface{}) (string, error) {
	unit := "HOUR"
	if u, ok := args["unit"].(string); ok && u != "" {
		unit = u
	}

	iface, _ := args["interface"].(string)

	result, err := client.Call("reporting.get_data", []interface{}{
		map[string]interface{}{
			"name":       "interface",
			"identifier": iface,
		},
	}, map[string]interface{}{"unit": unit})
	if err != nil {
		return "", err
	}

	var data interface{}
	if err := json.Unmarshal(result, &data); err != nil {
		return "", fmt.Errorf("failed to parse network metrics: %w", err)
	}

	formatted, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleGetDiskMetrics(client *truenas.Client, args map[string]interface{}) (string, error) {
	unit := "HOUR"
	if u, ok := args["unit"].(string); ok && u != "" {
		unit = u
	}

	disk, _ := args["disk"].(string)

	result, err := client.Call("reporting.get_data", []interface{}{
		map[string]interface{}{
			"name":       "disk",
			"identifier": disk,
		},
	}, map[string]interface{}{"unit": unit})
	if err != nil {
		return "", err
	}

	var data interface{}
	if err := json.Unmarshal(result, &data); err != nil {
		return "", fmt.Errorf("failed to parse disk metrics: %w", err)
	}

	formatted, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

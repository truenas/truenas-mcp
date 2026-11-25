package tools

// System service query tool (query_services).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerServiceTools() {
	r.tools["query_services"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_services",
			Description: "List system services (for example cifs for SMB, nfs, ssh, ups, snmp, ftp) with their state and whether each starts at boot. 'state' is RUNNING, STOPPED, or UNKNOWN when TrueNAS could not read it in time. Use 'name' for one service.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Service name (exact match, e.g. 'cifs', 'nfs', 'ssh') to return only that service",
						"minLength":   1,
					},
				},
			},
		},
		Handler: handleQueryServices,
	}
}

// simplifyService projects a service.query entry onto the fields
// query_services returns.
func simplifyService(svc map[string]interface{}) map[string]interface{} {
	state, _ := svc["state"].(string)
	if state == "" {
		state = "UNKNOWN"
	}
	out := map[string]interface{}{
		"name":    svc["service"],
		"state":   state,
		"running": state == "RUNNING",
	}
	if enable, ok := svc["enable"].(bool); ok {
		out["enabled_at_boot"] = enable
	}
	return out
}

func handleQueryServices(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	filters := []interface{}{}
	name, _ := args["name"].(string)
	if name != "" {
		filters = append(filters, []interface{}{"service", "=", name})
	}
	raw, err := client.Call(ctx, "service.query", filters, map[string]interface{}{})
	if err != nil {
		return "", fmt.Errorf("failed to query services: %w", err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", fmt.Errorf("failed to parse services: unexpected result")
	}
	if name != "" && len(rows) == 0 {
		return "", fmt.Errorf("service %q not found", name)
	}
	list := make([]map[string]interface{}, 0, len(rows))
	running, unknown := 0, 0
	for _, row := range rows {
		svc := simplifyService(row)
		switch svc["state"] {
		case "RUNNING":
			running++
		case "UNKNOWN":
			unknown++
		}
		list = append(list, svc)
	}
	sort.Slice(list, func(i, j int) bool {
		a, _ := list[i]["name"].(string)
		b, _ := list[j]["name"].(string)
		return a < b
	})
	response := map[string]interface{}{
		"services": list,
		"count":    len(list),
		"running":  running,
	}
	if unknown > 0 {
		response["note"] = fmt.Sprintf("%d service(s) report UNKNOWN: TrueNAS could not read their state within its 15 second limit", unknown)
	}
	return marshalJSON(response)
}

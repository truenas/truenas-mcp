package tools

// Virtual machine query tool and its handler (query_vms).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerVMTools() {
	// VM query
	r.tools["query_vms"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_vms",
			Description: "Query virtual machines with optional filtering and sorting. Returns simplified VM information with resource allocation, status, and device summary. Excludes sensitive data like display passwords.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter VMs by name (partial match)",
					},
					"state": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter by VM state (default: all)",
						"enum":        []string{"RUNNING", "STOPPED", "all"},
					},
					"autostart": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Filter by autostart setting",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of VMs to return (default: 50)",
						"minimum":     1,
					},
					"order_by": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Sort by 'name' (default, alphabetical), 'memory' (descending), or 'status' (running first)",
						"enum":        []string{"name", "memory", "status"},
					},
				},
			},
		},
		Handler: handleQueryVMs,
	}
}

var vmQuerySelect = []interface{}{
	"id", "name", "uuid", "description", "vcpus", "cores", "threads", "cpu_mode",
	"memory", "min_memory", "bootloader", "autostart", "status", "devices",
}

func handleQueryVMs(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	// Request only the fields simplifyVM uses; filter and limit stay
	// client-side because name is a partial match.
	result, err := client.Call(ctx, "vm.query", []interface{}{}, map[string]interface{}{"select": vmQuerySelect})
	if err != nil {
		return "", err
	}

	var vms []map[string]interface{}
	if err := json.Unmarshal(result, &vms); err != nil {
		return "", fmt.Errorf("failed to parse VMs: %w", err)
	}

	// Simplify response
	simplified := make([]map[string]interface{}, 0, len(vms))
	for _, vm := range vms {
		summary := simplifyVM(vm)
		simplified = append(simplified, summary)
	}

	// Filter by name (partial match)
	if name, ok := args["name"].(string); ok && name != "" {
		filtered := make([]map[string]interface{}, 0)
		nameLower := strings.ToLower(name)
		for _, vm := range simplified {
			if vmName, ok := vm["name"].(string); ok {
				if strings.Contains(strings.ToLower(vmName), nameLower) {
					filtered = append(filtered, vm)
				}
			}
		}
		simplified = filtered
	}

	// Filter by state
	if state, ok := args["state"].(string); ok && state != "" && state != "all" {
		filtered := make([]map[string]interface{}, 0)
		for _, vm := range simplified {
			if vmState, ok := vm["state"].(string); ok && vmState == state {
				filtered = append(filtered, vm)
			}
		}
		simplified = filtered
	}

	// Filter by autostart
	if autostart, ok := args["autostart"].(bool); ok {
		filtered := make([]map[string]interface{}, 0)
		for _, vm := range simplified {
			if vmAutostart, ok := vm["autostart"].(bool); ok && vmAutostart == autostart {
				filtered = append(filtered, vm)
			}
		}
		simplified = filtered
	}

	// Sort VMs
	orderBy := "name" // default to sorting by name
	if order, ok := args["order_by"].(string); ok && order != "" {
		orderBy = order
	}
	sortVMs(simplified, orderBy)

	// Apply limit (default to 50)
	limit := 50
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	totalVMs := len(simplified)
	if len(simplified) > limit {
		simplified = simplified[:limit]
	}

	// Add metadata wrapper
	response := map[string]interface{}{
		"vms":       simplified,
		"vm_count":  len(simplified),
		"total_vms": totalVMs,
	}
	if name, ok := args["name"].(string); ok && name != "" {
		response["name_filter"] = name
	}
	if state, ok := args["state"].(string); ok && state != "" && state != "all" {
		response["state_filter"] = state
	}
	if autostart, ok := args["autostart"].(bool); ok {
		response["autostart_filter"] = autostart
	}
	if len(simplified) < totalVMs {
		response["note"] = fmt.Sprintf("Showing %d of %d VMs (limited)", len(simplified), totalVMs)
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// simplifyVM extracts the most relevant fields from a raw VM object
func simplifyVM(vm map[string]interface{}) map[string]interface{} {
	summary := map[string]interface{}{
		"id":   vm["id"],
		"name": vm["name"],
		"uuid": vm["uuid"],
	}

	// Description (only if not empty)
	if desc, ok := vm["description"].(string); ok && desc != "" {
		summary["description"] = desc
	}

	// CPU topology: the API's "vcpus" is the number of sockets, "cores"
	// is cores per socket and "threads" threads per core, so the guest
	// sees vcpus*cores*threads virtual CPUs.
	sockets, sOk := vm["vcpus"].(float64)
	cores, cOk := vm["cores"].(float64)
	threads, tOk := vm["threads"].(float64)
	if sOk {
		summary["sockets"] = int(sockets)
	}
	if cOk {
		summary["cores_per_socket"] = int(cores)
	}
	if tOk {
		summary["threads_per_core"] = int(threads)
	}
	if sOk || cOk || tOk {
		// Each value defaults to 1 in the VM schema.
		summary["total_vcpus"] = atLeastOne(sockets) * atLeastOne(cores) * atLeastOne(threads)
	}
	if cpuMode, ok := vm["cpu_mode"].(string); ok {
		summary["cpu_mode"] = cpuMode
	}

	// Memory: the API value is in MiB.
	if memory, ok := vm["memory"].(float64); ok {
		summary["memory_mib"] = int(memory)
		summary["memory_gib"] = formatGiB(memory)
	}
	if minMemory, ok := vm["min_memory"].(float64); ok && minMemory > 0 {
		summary["min_memory_mib"] = int(minMemory)
	}

	// Boot configuration
	if bootloader, ok := vm["bootloader"].(string); ok {
		summary["bootloader"] = bootloader
	}
	if autostart, ok := vm["autostart"].(bool); ok {
		summary["autostart"] = autostart
	}

	// Status information
	if status, ok := vm["status"].(map[string]interface{}); ok {
		if state, ok := status["state"].(string); ok {
			summary["state"] = state
		}
		if pid, ok := status["pid"].(float64); ok && pid > 0 {
			summary["pid"] = int(pid)
		}
	}

	// Device summary (sanitized - no passwords or sensitive data)
	if devices, ok := vm["devices"].([]interface{}); ok {
		deviceSummary := simplifyVMDevices(devices)
		for k, v := range deviceSummary {
			summary[k] = v
		}
	}

	return summary
}

// simplifyVMDevices extracts device information without sensitive data
func simplifyVMDevices(devices []interface{}) map[string]interface{} {
	summary := map[string]interface{}{
		"device_count": len(devices),
	}

	var disks []map[string]interface{}
	var nics []map[string]interface{}
	var displays []map[string]interface{}

	for _, dev := range devices {
		device, ok := dev.(map[string]interface{})
		if !ok {
			continue
		}

		attrs, ok := device["attributes"].(map[string]interface{})
		if !ok {
			continue
		}

		dtype, _ := attrs["dtype"].(string)

		switch dtype {
		case "DISK":
			disk := map[string]interface{}{}
			if path, ok := attrs["path"].(string); ok {
				disk["path"] = path
			}
			if diskType, ok := attrs["type"].(string); ok {
				disk["type"] = diskType
			}
			if serial, ok := attrs["serial"].(string); ok {
				disk["serial"] = serial
			}
			disks = append(disks, disk)

		case "NIC":
			nic := map[string]interface{}{}
			if nicType, ok := attrs["type"].(string); ok {
				nic["type"] = nicType
			}
			if attach, ok := attrs["nic_attach"].(string); ok {
				nic["attached_to"] = attach
			}
			if mac, ok := attrs["mac"].(string); ok {
				nic["mac"] = mac
			}
			nics = append(nics, nic)

		case "DISPLAY":
			display := map[string]interface{}{}
			if displayType, ok := attrs["type"].(string); ok {
				display["type"] = displayType
			}
			if port, ok := attrs["port"].(float64); ok {
				display["port"] = int(port)
			}
			if webPort, ok := attrs["web_port"].(float64); ok {
				display["web_port"] = int(webPort)
			}
			if bind, ok := attrs["bind"].(string); ok {
				display["bind"] = bind
			}
			// Explicitly exclude password field for security
			displays = append(displays, display)
		}
	}

	if len(disks) > 0 {
		summary["disks"] = disks
		summary["disk_count"] = len(disks)
	}
	if len(nics) > 0 {
		summary["nics"] = nics
		summary["nic_count"] = len(nics)
	}
	if len(displays) > 0 {
		summary["displays"] = displays
		summary["display_count"] = len(displays)
	}

	return summary
}

// sortVMs sorts a slice of simplified VMs by the specified field
func sortVMs(vms []map[string]interface{}, orderBy string) {
	sort.Slice(vms, func(i, j int) bool {
		switch orderBy {
		case "name":
			// Sort by name alphabetically ascending
			iName, iOk := vms[i]["name"].(string)
			jName, jOk := vms[j]["name"].(string)
			if iOk && jOk {
				return iName < jName
			}
			return false
		case "memory":
			// Sort by memory descending (largest first)
			iMem, iOk := vms[i]["memory_mib"].(int)
			jMem, jOk := vms[j]["memory_mib"].(int)
			if iOk && jOk {
				return iMem > jMem
			}
			return false
		case "status":
			// Sort by state (RUNNING first, then others)
			iState, iOk := vms[i]["state"].(string)
			jState, jOk := vms[j]["state"].(string)
			if iOk && jOk {
				if iState == "RUNNING" && jState != "RUNNING" {
					return true
				}
				if jState == "RUNNING" && iState != "RUNNING" {
					return false
				}
				// If both same state, sort by name
				iName, _ := vms[i]["name"].(string)
				jName, _ := vms[j]["name"].(string)
				return iName < jName
			}
			return false
		default:
			// Default to name
			iName, iOk := vms[i]["name"].(string)
			jName, jOk := vms[j]["name"].(string)
			if iOk && jOk {
				return iName < jName
			}
			return false
		}
	})
}

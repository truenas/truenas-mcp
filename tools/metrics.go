package tools

// Metrics and reporting tools and their handlers (get_system_metrics,
// get_network_metrics, get_disk_metrics, get_arc_metrics, get_ups_metrics).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/truenas/truenas-mcp/internal/safego"
	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerMetricsTools() {
	// System reporting metrics
	r.tools["get_system_metrics"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_system_metrics",
			Description: "Get system performance metrics (CPU, memory, load average, CPU temperature, uptime)",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"graphs": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
							"enum": []string{"cpu", "cputemp", "memory", "load", "uptime"},
						},
						"description": "Metrics to retrieve (default: cpu, memory, load)",
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
			Description: "Get disk performance metrics (I/O or temperature)",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"disk": map[string]interface{}{
						"type":        "string",
						"description": "Disk name (e.g., 'sda'). If omitted, returns all disks.",
					},
					"type": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"disk", "disktemp"},
						"description": "Metric type: disk I/O or disk temperature (default: disk)",
						"default":     "disk",
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

	// ZFS ARC reporting metrics
	r.tools["get_arc_metrics"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_arc_metrics",
			Description: "Get ZFS ARC (Adaptive Replacement Cache) reporting metrics such as ARC size. Which ARC graphs exist depends on the TrueNAS version; the supported set is discovered from the server.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"graphs": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
						},
						"description": "ARC graphs to retrieve, e.g. 'arcsize'. Other ARC graphs are accepted when the server supports them. Default: every ARC graph the server supports.",
					},
					"unit": map[string]interface{}{
						"type":    "string",
						"enum":    []string{"HOUR", "DAY", "WEEK", "MONTH", "YEAR"},
						"default": "HOUR",
					},
				},
			},
		},
		Handler: handleGetArcMetrics,
	}

	// UPS reporting metrics
	r.tools["get_ups_metrics"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_ups_metrics",
			Description: "Get UPS (Uninterruptible Power Supply) metrics. For upsvoltage, returns battery, input, and output voltage. Requires a UPS configured in TrueNAS.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"graphs": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
							"enum": []string{
								"upscharge", "upsruntime", "upsvoltage",
								"upscurrent", "upsfrequency", "upsload", "upstemperature",
							},
						},
						"description": "UPS metrics to retrieve (default: all)",
					},
					"unit": map[string]interface{}{
						"type":    "string",
						"enum":    []string{"HOUR", "DAY", "WEEK", "MONTH", "YEAR"},
						"default": "HOUR",
					},
				},
			},
		},
		Handler: handleGetUpsMetrics,
	}
}

// Reporting handlers

func handleGetSystemMetrics(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
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
		case "cputemp":
			apiGraph = "cputemp"
		case "memory":
			apiGraph = "memory"
		case "load":
			apiGraph = "load"
		case "uptime":
			apiGraph = "uptime"
		default:
			continue
		}

		result, err := client.Call(ctx, "reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       apiGraph,
				"identifier": nil,
			},
		}, map[string]interface{}{"unit": unit})
		if err != nil {
			response[graph] = map[string]string{"error": err.Error()}
			continue
		}

		var fullData []map[string]interface{}
		if err := json.Unmarshal(result, &fullData); err != nil {
			response[graph] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}

		summary := make(map[string]interface{})
		if len(fullData) > 0 {
			summary = summarizeSystemGraph(fullData[0], apiGraph)
		}
		response[graph] = summary
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleGetNetworkMetrics(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	unit := "HOUR"
	if u, ok := args["unit"].(string); ok && u != "" {
		unit = u
	}

	iface, _ := args["interface"].(string)

	// If no interface specified, get all interfaces
	var interfaces []string
	if iface != "" {
		interfaces = []string{iface}
	} else {
		// Query for available network interfaces
		result, err := client.Call(ctx, "interface.query")
		if err != nil {
			return "", fmt.Errorf("failed to query interfaces: %w", err)
		}

		var ifaceList []map[string]interface{}
		if err := json.Unmarshal(result, &ifaceList); err != nil {
			return "", fmt.Errorf("failed to parse interface list: %w", err)
		}

		// Extract interface names
		for _, iface := range ifaceList {
			if name, ok := iface["name"].(string); ok && name != "" {
				interfaces = append(interfaces, name)
			}
		}

		if len(interfaces) == 0 {
			return `{"error": "no network interfaces found"}`, nil
		}
	}

	// Get metrics for each interface
	allMetrics := make(map[string]interface{})

	for _, ifaceName := range interfaces {
		result, err := client.Call(ctx, "reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       "interface",
				"identifier": ifaceName,
			},
		}, map[string]interface{}{"unit": unit})

		if err != nil {
			allMetrics[ifaceName] = map[string]string{"error": err.Error()}
			continue
		}

		var fullData []map[string]interface{}
		if err := json.Unmarshal(result, &fullData); err != nil {
			allMetrics[ifaceName] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}

		// Keep aggregations and metadata, sample data points to reduce size
		summaries := make([]map[string]interface{}, 0, len(fullData))
		for _, item := range fullData {
			summary := make(map[string]interface{})
			for key, value := range item {
				if key == "data" {
					// Include sample: first 10 and last 10 data points
					if dataArray, ok := value.([]interface{}); ok {
						summary["data_points_total"] = len(dataArray)
						if len(dataArray) > 0 {
							sample := make([]interface{}, 0)

							for i := 0; i < 10 && i < len(dataArray); i++ {
								sample = append(sample, dataArray[i])
							}

							if len(dataArray) > 20 {
								for i := len(dataArray) - 10; i < len(dataArray); i++ {
									sample = append(sample, dataArray[i])
								}
							}

							summary["data_sample"] = sample
						}
					}
				} else {
					summary[key] = value
				}
			}
			// Units of every series in "data" and "aggregations".
			summary["unit"] = reportingUnits["interface"]
			summaries = append(summaries, summary)
		}

		if len(summaries) == 1 {
			allMetrics[ifaceName] = summaries[0]
		} else {
			allMetrics[ifaceName] = summaries
		}
	}

	formatted, err := json.MarshalIndent(allMetrics, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleGetDiskMetrics(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	unit := "HOUR"
	if u, ok := args["unit"].(string); ok && u != "" {
		unit = u
	}

	requestedDisk, _ := args["disk"].(string)

	graphType := "disk"
	if t, ok := args["type"].(string); ok && t != "" {
		graphType = t
	}

	// First, get available reporting graphs
	graphsResult, err := client.Call(ctx, "reporting.graphs")
	if err != nil {
		return "", fmt.Errorf("failed to query reporting graphs: %w", err)
	}

	var graphs []map[string]interface{}
	if err := json.Unmarshal(graphsResult, &graphs); err != nil {
		return "", fmt.Errorf("failed to parse reporting graphs: %w", err)
	}

	// Find the requested graph type and extract identifiers
	var diskIdentifiers []string
	graphUnit := reportingUnits[graphType]
	for _, graph := range graphs {
		graphName, nameOk := graph["name"].(string)
		if nameOk && graphName == graphType {
			if label, ok := graph["vertical_label"].(string); ok && label != "" {
				graphUnit = label
			}
			// Get the identifiers array
			if identifiersRaw, ok := graph["identifiers"]; ok && identifiersRaw != nil {
				if identifiersArray, ok := identifiersRaw.([]interface{}); ok {
					for _, idRaw := range identifiersArray {
						if idStr, ok := idRaw.(string); ok {
							// Extract disk name from identifier string (e.g., "sda | Type: SSD...")
							diskName := idStr
							if idx := strings.Index(idStr, " |"); idx != -1 {
								diskName = idStr[:idx]
							}

							// If specific disk requested, filter by name
							if requestedDisk == "" || diskName == requestedDisk {
								diskIdentifiers = append(diskIdentifiers, idStr)
							}
						}
					}
				}
			}
			break
		}
	}

	if len(diskIdentifiers) == 0 {
		return fmt.Sprintf(`{"error": "no disk identifiers found for graph type %q"}`, graphType), nil
	}

	// Get metrics for each disk identifier
	allMetrics := make(map[string]interface{})

	for _, identifier := range diskIdentifiers {
		// Extract disk name for the key (e.g., "sda" from "sda | Type: SSD...")
		diskName := identifier
		if idx := strings.Index(identifier, " |"); idx != -1 {
			diskName = identifier[:idx]
		}

		result, err := client.Call(ctx, "reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       graphType,
				"identifier": identifier,
			},
		}, map[string]interface{}{"unit": unit})

		if err != nil {
			allMetrics[diskName] = map[string]string{"error": err.Error()}
			continue
		}

		var fullData []map[string]interface{}
		if err := json.Unmarshal(result, &fullData); err != nil {
			allMetrics[diskName] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}

		// Keep aggregations and metadata, sample data points to reduce size
		summaries := make([]map[string]interface{}, 0, len(fullData))
		for _, item := range fullData {
			summary := make(map[string]interface{})
			for key, value := range item {
				if key == "data" {
					// Include sample: first 10 and last 10 data points
					if dataArray, ok := value.([]interface{}); ok {
						summary["data_points_total"] = len(dataArray)
						if len(dataArray) > 0 {
							sample := make([]interface{}, 0)

							for i := 0; i < 10 && i < len(dataArray); i++ {
								sample = append(sample, dataArray[i])
							}

							if len(dataArray) > 20 {
								for i := len(dataArray) - 10; i < len(dataArray); i++ {
									sample = append(sample, dataArray[i])
								}
							}

							summary["data_sample"] = sample
						}
					}
				} else {
					summary[key] = value
				}
			}
			// Units of every series in "data" and "aggregations".
			if graphUnit != "" {
				summary["unit"] = graphUnit
			}
			summaries = append(summaries, summary)
		}

		if len(summaries) == 1 {
			allMetrics[diskName] = summaries[0]
		} else {
			allMetrics[diskName] = summaries
		}
	}

	formatted, err := json.MarshalIndent(allMetrics, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// fallbackArcGraphs is used when the server's supported graphs cannot be
// discovered. 'arcsize' is accepted by reporting.get_data on 25.10 through
// 27.0; other ARC graphs vary between releases.
var fallbackArcGraphs = []string{"arcsize"}

// arcGraphCache holds a successful ARC graph discovery per client, tagged
// with the session generation it was made on so a reconnect or new login
// discovers again (the server may have been upgraded meanwhile).
var arcGraphCache sync.Map // *truenas.Client -> arcGraphEntry

type arcGraphEntry struct {
	gen    uint64
	graphs []string
}

// discoveryTimeout bounds each discovery call; both run concurrently, so
// discovery adds at most this much to the first get_arc_metrics call.
var discoveryTimeout = 10 * time.Second

// supportedArcGraphs returns the ARC graphs (names starting with "arc")
// that reporting.get_data accepts on this server. reporting.graphs lists
// the graph plugins the server has, but on 27.0 it lists plugins that
// reporting.get_data's schema rejects, so the accepted names from
// core.get_methods are intersected with it. Only a complete discovery is
// cached (per session); a failed or partial one is retried next call.
func supportedArcGraphs(ctx context.Context, client *truenas.Client) []string {
	gen, err := client.SessionGeneration(ctx)
	if err != nil {
		return fallbackArcGraphs
	}
	if cached, ok := arcGraphCache.Load(client); ok {
		if e := cached.(arcGraphEntry); e.gen == gen {
			return e.graphs
		}
	}
	graphs, complete := discoverArcGraphs(ctx, client)
	if complete {
		arcGraphCache.Store(client, arcGraphEntry{gen: gen, graphs: graphs})
	}
	return graphs
}

// recoverDiscovery is deferred by the ARC graph discovery goroutines.
func recoverDiscovery(source string) {
	if r := recover(); r != nil {
		log.Printf("ARC graph discovery (%s) panicked: %s\n%s", source, safego.Describe(r), debug.Stack())
	}
}

// discoverArcGraphs returns the supported ARC graphs and whether discovery
// was complete (both sources answered and at least one ARC graph matched).
func discoverArcGraphs(ctx context.Context, client *truenas.Client) ([]string, bool) {
	var accepted, offered []string
	var wg sync.WaitGroup
	wg.Add(2)
	// CallTool's recover does not cover these goroutines. A panic in either
	// is logged and leaves that source unanswered (nil), like an API error.
	go func() {
		defer wg.Done()
		defer recoverDiscovery("get_data schema")
		accepted = getDataAcceptedGraphs(ctx, client)
	}()
	go func() {
		defer wg.Done()
		defer recoverDiscovery("reporting.graphs")
		offered = reportingGraphNames(ctx, client)
	}()
	wg.Wait()

	var names []string
	switch {
	case accepted != nil && offered != nil:
		for _, n := range offered {
			if slices.Contains(accepted, n) {
				names = append(names, n)
			}
		}
	case accepted != nil:
		// The schema alone is authoritative for what get_data accepts.
		names = accepted
	}
	// reporting.graphs alone is not used: on 27.0 it lists ARC graphs that
	// reporting.get_data rejects.
	arc := make([]string, 0, len(names))
	for _, n := range names {
		if strings.HasPrefix(n, "arc") && !slices.Contains(arc, n) {
			arc = append(arc, n)
		}
	}
	if len(arc) == 0 {
		return fallbackArcGraphs, false
	}
	sort.Strings(arc)
	return arc, accepted != nil && offered != nil
}

// reportingGraphNames returns the names from reporting.graphs, or nil.
func reportingGraphNames(ctx context.Context, client *truenas.Client) []string {
	result, err := client.CallWithTimeout(ctx, discoveryTimeout, "reporting.graphs")
	if err != nil {
		return nil
	}
	var items []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(result, &items); err != nil || len(items) == 0 {
		return nil
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		if it.Name != "" {
			names = append(names, it.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// getDataAcceptedGraphs returns the graph names allowed by the
// reporting.get_data argument schema (graphs[].name enum) as published by
// core.get_methods, or nil if unavailable.
func getDataAcceptedGraphs(ctx context.Context, client *truenas.Client) []string {
	result, err := client.CallWithTimeout(ctx, discoveryTimeout, "core.get_methods", "reporting")
	if err != nil {
		return nil
	}
	var methods map[string]struct {
		Accepts json.RawMessage `json:"accepts"`
	}
	if err := json.Unmarshal(result, &methods); err != nil {
		return nil
	}
	method, ok := methods["reporting.get_data"]
	if !ok {
		return nil
	}
	var accepts []interface{}
	if err := json.Unmarshal(method.Accepts, &accepts); err != nil || len(accepts) == 0 {
		return nil
	}
	// accepts is one JSON schema per argument; the first is graphs, an
	// array of {name, identifier}. Read accepts[0].items.properties.name
	// and only walk the schema if that path is absent.
	if graphs, ok := accepts[0].(map[string]interface{}); ok {
		if items, ok := graphs["items"].(map[string]interface{}); ok {
			if out := nameEnum(items); out != nil {
				return out
			}
		}
	}
	return findNameEnum(accepts[0])
}

// nameEnum returns schema.properties.name.enum as strings, or nil.
func nameEnum(schema map[string]interface{}) []string {
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return nil
	}
	name, ok := props["name"].(map[string]interface{})
	if !ok {
		return nil
	}
	enum, ok := name["enum"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(enum))
	for _, e := range enum {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// findNameEnum walks a JSON schema for the first properties.name.enum list.
func findNameEnum(node interface{}) []string {
	switch v := node.(type) {
	case map[string]interface{}:
		if out := nameEnum(v); out != nil {
			return out
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if out := findNameEnum(v[k]); out != nil {
				return out
			}
		}
	case []interface{}:
		for _, e := range v {
			if out := findNameEnum(e); out != nil {
				return out
			}
		}
	}
	return nil
}

// conciseError returns the first line of err's message, dropping the
// request context and middleware traceback the client appends after it.
func conciseError(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "\n"); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg)
}

func handleGetArcMetrics(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	unit := "HOUR"
	if u, ok := args["unit"].(string); ok && u != "" {
		unit = u
	}

	// Default to every ARC graph the server supports.
	supported := supportedArcGraphs(ctx, client)
	graphs := supported
	if g, ok := args["graphs"].([]interface{}); ok && len(g) > 0 {
		graphs = make([]string, 0, len(g))
		for _, v := range g {
			s, _ := v.(string)
			if !slices.Contains(supported, s) {
				return "", fmt.Errorf("unknown or unsupported ARC graph %q; supported graphs on this server: %s", s, strings.Join(supported, ", "))
			}
			graphs = append(graphs, s)
		}
	}

	response := make(map[string]interface{})
	failures := make(map[string]string)

	for _, graph := range graphs {
		result, err := client.Call(ctx, "reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       graph,
				"identifier": nil,
			},
		}, map[string]interface{}{"unit": unit})
		if err != nil {
			failures[graph] = conciseError(err)
			continue
		}

		var fullData []map[string]interface{}
		if err := json.Unmarshal(result, &fullData); err != nil {
			failures[graph] = fmt.Sprintf("unexpected response: %v", err)
			continue
		}
		if len(fullData) == 0 {
			failures[graph] = "no data returned"
			continue
		}

		summary := make(map[string]interface{})
		for key, value := range fullData[0] {
			if key == "data" {
				if dataArray, ok := value.([]interface{}); ok {
					summary["data_points_total"] = len(dataArray)
					sample := make([]interface{}, 0)

					for i := 0; i < 10 && i < len(dataArray); i++ {
						sample = append(sample, dataArray[i])
					}

					if len(dataArray) > 20 {
						for i := len(dataArray) - 10; i < len(dataArray); i++ {
							sample = append(sample, dataArray[i])
						}
					}

					summary["data_sample"] = sample
				}
			} else {
				summary[key] = value
			}
		}
		response[graph] = summary
	}

	// Every requested graph failed: report a real tool error.
	if len(response) == 0 && len(failures) > 0 {
		parts := make([]string, 0, len(graphs))
		for _, graph := range graphs {
			if msg, ok := failures[graph]; ok {
				parts = append(parts, graph+": "+msg)
			}
		}
		return "", fmt.Errorf("failed to get ARC metrics: %s", strings.Join(parts, "; "))
	}
	if len(failures) > 0 {
		response["errors"] = failures
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleGetUpsMetrics(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	unit := "HOUR"
	if u, ok := args["unit"].(string); ok && u != "" {
		unit = u
	}

	// Default to all UPS graphs
	graphs := []string{"upscharge", "upsruntime", "upsvoltage", "upscurrent", "upsfrequency", "upsload", "upstemperature"}
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
		if graph == "upsvoltage" {
			// upsvoltage has per-identifier data (battery, input, output)
			// Discover identifiers from reporting.graphs
			graphsResult, err := client.Call(ctx, "reporting.graphs")
			if err != nil {
				response[graph] = map[string]string{"error": fmt.Sprintf("failed to query reporting graphs: %v", err)}
				continue
			}

			var reportingGraphs []map[string]interface{}
			if err := json.Unmarshal(graphsResult, &reportingGraphs); err != nil {
				response[graph] = map[string]string{"error": fmt.Sprintf("failed to parse reporting graphs: %v", err)}
				continue
			}

			var voltageIdentifiers []string
			for _, rg := range reportingGraphs {
				if name, ok := rg["name"].(string); ok && name == "upsvoltage" {
					if identifiersRaw, ok := rg["identifiers"]; ok && identifiersRaw != nil {
						if identifiersArray, ok := identifiersRaw.([]interface{}); ok {
							for _, idRaw := range identifiersArray {
								if idStr, ok := idRaw.(string); ok {
									voltageIdentifiers = append(voltageIdentifiers, idStr)
								}
							}
						}
					}
					break
				}
			}

			if len(voltageIdentifiers) == 0 {
				// No identifiers found — try nil identifier
				voltageIdentifiers = []string{""}
			}

			voltageData := make(map[string]interface{})
			for _, identifier := range voltageIdentifiers {
				var callIdentifier interface{}
				if identifier != "" {
					callIdentifier = identifier
				}
				result, err := client.Call(ctx, "reporting.get_data", []interface{}{
					map[string]interface{}{
						"name":       "upsvoltage",
						"identifier": callIdentifier,
					},
				}, map[string]interface{}{"unit": unit})
				if err != nil {
					voltageData[identifier] = map[string]string{"error": err.Error()}
					continue
				}

				var fullData []map[string]interface{}
				if err := json.Unmarshal(result, &fullData); err != nil {
					voltageData[identifier] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
					continue
				}

				summary := make(map[string]interface{})
				if len(fullData) > 0 {
					for key, value := range fullData[0] {
						if key == "data" {
							if dataArray, ok := value.([]interface{}); ok {
								summary["data_points_total"] = len(dataArray)
								sample := make([]interface{}, 0)

								for i := 0; i < 10 && i < len(dataArray); i++ {
									sample = append(sample, dataArray[i])
								}

								if len(dataArray) > 20 {
									for i := len(dataArray) - 10; i < len(dataArray); i++ {
										sample = append(sample, dataArray[i])
									}
								}

								summary["data_sample"] = sample
							}
						} else {
							summary[key] = value
						}
					}
				}
				key := identifier
				if key == "" {
					key = "default"
				}
				voltageData[key] = summary
			}
			response[graph] = voltageData
		} else {
			// All other UPS graphs use nil identifier
			result, err := client.Call(ctx, "reporting.get_data", []interface{}{
				map[string]interface{}{
					"name":       graph,
					"identifier": nil,
				},
			}, map[string]interface{}{"unit": unit})
			if err != nil {
				response[graph] = map[string]string{"error": err.Error()}
				continue
			}

			var fullData []map[string]interface{}
			if err := json.Unmarshal(result, &fullData); err != nil {
				response[graph] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
				continue
			}

			summary := make(map[string]interface{})
			if len(fullData) > 0 {
				for key, value := range fullData[0] {
					if key == "data" {
						if dataArray, ok := value.([]interface{}); ok {
							summary["data_points_total"] = len(dataArray)
							sample := make([]interface{}, 0)

							for i := 0; i < 10 && i < len(dataArray); i++ {
								sample = append(sample, dataArray[i])
							}

							if len(dataArray) > 20 {
								for i := len(dataArray) - 10; i < len(dataArray); i++ {
									sample = append(sample, dataArray[i])
								}
							}

							summary["data_sample"] = sample
						}
					} else {
						summary[key] = value
					}
				}
			}
			response[graph] = summary
		}
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

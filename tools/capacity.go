package tools

// Capacity analysis tools and their handlers (analyze_capacity,
// get_pool_capacity_details). Shared helpers used by system_health
// (formatBytes, calculateAverage, calculatePoolCapacity, ARC/memory
// sampling) stay in registry.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerCapacityTools() {
	// Capacity analysis tool
	r.tools["analyze_capacity"] = Tool{
		Definition: mcp.Tool{
			Name:        "analyze_capacity",
			Description: "Analyze system capacity utilization and trends for capacity planning. Provides utilization percentages, growth rates, and projections based on historical metrics. Includes CPU, memory, network, and disk I/O analysis.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"time_range": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"DAY", "WEEK", "MONTH", "YEAR"},
						"description": "Historical time range for trend analysis (default: MONTH, about 30 days; note the get_*_metrics tools default to HOUR). The range used is returned as time_range in each section",
						"default":     "MONTH",
					},
					"metrics": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
							"enum": []string{"cpu", "memory", "network", "disk", "all"},
						},
						"description": "Metrics to analyze (default: all)",
					},
				},
			},
		},
		Handler: handleAnalyzeCapacity,
	}

	// Pool capacity details tool
	r.tools["get_pool_capacity_details"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_pool_capacity_details",
			Description: "Get detailed pool and dataset capacity information with utilization analysis. Returns current capacity snapshot with breakdown by dataset. Note: Historical capacity trends are not available from TrueNAS API; use Netdata graphs if available.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pool_name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Specific pool to analyze",
					},
				},
			},
		},
		Handler: handleGetPoolCapacityDetails,
	}
}

// Capacity analysis handlers

func handleAnalyzeCapacity(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	timeRange := "MONTH"
	if tr, ok := args["time_range"].(string); ok && tr != "" {
		timeRange = tr
	}

	// Default to all metrics
	metrics := []string{"cpu", "memory", "network", "disk"}
	if m, ok := args["metrics"].([]interface{}); ok && len(m) > 0 {
		metrics = make([]string, 0, len(m))
		for _, v := range m {
			if s, ok := v.(string); ok {
				if s == "all" {
					metrics = []string{"cpu", "memory", "network", "disk"}
					break
				}
				metrics = append(metrics, s)
			}
		}
	}

	analysis := make(map[string]interface{})

	// Analyze each metric
	for _, metric := range metrics {
		switch metric {
		case "cpu":
			cpuAnalysis, err := analyzeCPUCapacity(ctx, client, timeRange)
			if err != nil {
				analysis["cpu"] = map[string]string{"error": err.Error()}
			} else {
				analysis["cpu"] = cpuAnalysis
			}
		case "memory":
			memAnalysis, err := analyzeMemoryCapacity(ctx, client, timeRange)
			if err != nil {
				analysis["memory"] = map[string]string{"error": err.Error()}
			} else {
				analysis["memory"] = memAnalysis
			}
		case "network":
			netAnalysis, err := analyzeNetworkCapacity(ctx, client, timeRange)
			if err != nil {
				analysis["network"] = map[string]string{"error": err.Error()}
			} else {
				analysis["network"] = netAnalysis
			}
		case "disk":
			diskAnalysis, err := analyzeDiskCapacity(ctx, client, timeRange)
			if err != nil {
				analysis["disk"] = map[string]string{"error": err.Error()}
			} else {
				analysis["disk"] = diskAnalysis
			}
		}
	}

	// Add summary and recommendations
	analysis["summary"] = generateCapacityRecommendations(analysis)

	formatted, err := json.MarshalIndent(analysis, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func analyzeCPUCapacity(ctx context.Context, client *truenas.Client, timeRange string) (map[string]interface{}, error) {
	// Get CPU metrics for time range
	result, err := client.Call(ctx, "reporting.get_data", []interface{}{
		map[string]interface{}{
			"name":       "cpu",
			"identifier": nil,
		},
	}, map[string]interface{}{"unit": timeRange})

	if err != nil {
		return nil, err
	}

	var metricsData []map[string]interface{}
	if err := json.Unmarshal(result, &metricsData); err != nil {
		return nil, err
	}

	if len(metricsData) == 0 {
		return nil, fmt.Errorf("no CPU metrics data available")
	}

	// Overall CPU usage is the legend series "cpu" (column 1 if the legend
	// has none). Rows with no sample (all zero/null, as the middleware
	// zero-fills gaps) are skipped.
	samples := columnSamples(metricsData[0], legendColumn(metricsData[0], "cpu", 1), true, nil)
	if len(samples) == 0 {
		return nil, fmt.Errorf("no valid data points")
	}
	dataPoints := sampleValues(samples)
	complete, partialNote := completeSamples(metricsData[0], samples)

	// Calculate statistics; current is the last 5 complete samples.
	current := calculateRecentAverage(sampleValues(complete), 5)
	average := calculateAverage(dataPoints)
	peak := calculateMax(dataPoints)
	trend := calculateTrendDirection(dataPoints)
	status := determineCapacityStatus(current, 70.0, 85.0)

	analysis := map[string]interface{}{
		"metric":                  "CPU",
		"time_range":              timeRange,
		"current_utilization_pct": fmt.Sprintf("%.2f", current),
		"average_utilization_pct": fmt.Sprintf("%.2f", average),
		"peak_utilization_pct":    fmt.Sprintf("%.2f", peak),
		"trend":                   trend,
		"capacity_status":         status,
	}
	if partialNote != "" {
		analysis["partial_sample_note"] = partialNote
	}

	// Add projections if trending up
	if trend == "increasing" {
		projections := calculateProjections(dataPoints, current, 70.0, 85.0)
		if len(projections) > 0 {
			analysis["projections"] = projections
		}
	}

	return analysis, nil
}

func analyzeMemoryCapacity(ctx context.Context, client *truenas.Client, timeRange string) (map[string]interface{}, error) {
	// Get system info to find total memory
	sysInfoResult, err := client.Call(ctx, "system.info")
	if err != nil {
		return nil, fmt.Errorf("failed to get system info: %w", err)
	}

	var sysInfo map[string]interface{}
	if err := json.Unmarshal(sysInfoResult, &sysInfo); err != nil {
		return nil, fmt.Errorf("failed to parse system info: %w", err)
	}

	// Get total physical memory in bytes
	totalMemory := 0.0
	if physMem, ok := sysInfo["physmem"].(float64); ok {
		totalMemory = physMem
	} else {
		return nil, fmt.Errorf("could not determine total system memory")
	}

	// Get memory metrics
	result, err := client.Call(ctx, "reporting.get_data", []interface{}{
		map[string]interface{}{
			"name":       "memory",
			"identifier": nil,
		},
	}, map[string]interface{}{"unit": timeRange})

	if err != nil {
		return nil, err
	}

	var metricsData []map[string]interface{}
	if err := json.Unmarshal(result, &metricsData); err != nil {
		return nil, err
	}

	if len(metricsData) == 0 {
		return nil, fmt.Errorf("no memory metrics data available")
	}

	// Extract data points: the "memory" graph (truenas_meminfo.available)
	// reports AVAILABLE bytes, so used = total - available.
	mem, err := availableMemorySamples(metricsData[0])
	if err != nil {
		return nil, err
	}
	availablePoints := mem.all

	// Convert to used percentages
	dataPointsPct := make([]float64, len(availablePoints))
	for i, avail := range availablePoints {
		dataPointsPct[i] = memoryUsedPct(totalMemory, avail)
	}

	// All current figures use one basis: the latest complete sample. Byte
	// counts are integers first, so current_used_bytes - arc_bytes equals
	// used_excluding_arc_bytes exactly.
	usedBytes := int64(math.Max(0, totalMemory-mem.latest))
	currentPct := bytesPct(usedBytes, totalMemory)

	// Calculate statistics
	average := calculateAverage(dataPointsPct)
	peak := calculateMax(dataPointsPct)
	trend := calculateTrendDirection(dataPointsPct)

	analysis := map[string]interface{}{
		"metric":                  "Memory",
		"time_range":              timeRange,
		"current_utilization_pct": fmt.Sprintf("%.2f", currentPct),
		"average_utilization_pct": fmt.Sprintf("%.2f", average),
		"peak_utilization_pct":    fmt.Sprintf("%.2f", peak),
		"trend":                   trend,
		"total_memory_bytes":      int64(totalMemory),
		"current_available_bytes": int64(mem.latest),
		"current_used_bytes":      usedBytes,
		"samples_used":            len(availablePoints),
	}
	if mem.partialNote != "" {
		analysis["partial_sample_note"] = mem.partialNote
	}

	// The ZFS ARC is reclaimable cache that the available figure does not
	// count, so status is based on memory used excluding the ARC. Only the
	// latest ARC size is known, so it is compared with the latest complete
	// available sample rather than an average over the range.
	statusPct := currentPct
	arcBytes, arcFreshKnown, arcErr := currentArcBytes(ctx, client, nasReferenceTime(metricsData[0]))
	if arcErr == nil {
		if !arcFreshKnown {
			analysis["arc_freshness_note"] = arcFreshnessUnknownNote
		}
		arc := int64(arcBytes)
		usedExclArc := usedBytes - arc
		if usedExclArc < 0 {
			usedExclArc = 0
			analysis["arc_clamped"] = true
			analysis["clamp_note"] = "ZFS ARC size exceeds memory used, so used_excluding_arc_bytes was clamped to 0; the ARC and memory samples may be from different times."
		}
		statusPct = bytesPct(usedExclArc, totalMemory)
		analysis["arc_bytes"] = arc
		analysis["used_excluding_arc_bytes"] = usedExclArc
		analysis["utilization_excluding_arc_pct"] = fmt.Sprintf("%.2f", statusPct)
		// The headline figure matches capacity_status: excluding the ARC.
		analysis["current_utilization_pct"] = fmt.Sprintf("%.2f", statusPct)
		analysis["current_utilization_including_arc_pct"] = fmt.Sprintf("%.2f", currentPct)
		analysis["current_utilization_basis"] = "excluding_arc"
		analysis["utilization_basis"] = "used = total_memory_bytes - available (reporting graph 'memory'). current_used_bytes, current_available_bytes, current_utilization_including_arc_pct, used_excluding_arc_bytes, current_utilization_pct and capacity_status all use the latest complete sample; current_utilization_pct and capacity_status subtract the latest ZFS ARC size (arcsize), which is reclaimable cache. average_utilization_pct, peak_utilization_pct and trend include the ARC."
		// Only the latest ARC size is known, not its history, so a trend
		// excluding the ARC can't be computed. Projecting the ARC-inclusive
		// trend would report ARC growth as a threshold risk, so none is given.
		if trend == "increasing" {
			analysis["projection_note"] = "No threshold projection: the trend includes the ZFS ARC, which grows as the cache warms, while capacity_status excludes it."
		}
	} else {
		analysis["utilization_basis"] = "used = total_memory_bytes - available (reporting graph 'memory' reports available memory). current_used_bytes, current_available_bytes, current_utilization_pct and capacity_status use the latest complete sample; all figures may include the ZFS ARC"
		analysis["current_utilization_basis"] = "including_arc"
		analysis["note"] = arcUnavailableNote(arcErr)
		// Status and trend are both ARC-inclusive here, so projecting is consistent.
		if trend == "increasing" {
			if projections := calculateProjections(dataPointsPct, statusPct, 70.0, 85.0); len(projections) > 0 {
				analysis["projections"] = projections
			}
		}
	}
	analysis["capacity_status"] = determineCapacityStatus(statusPct, 70.0, 85.0)

	return analysis, nil
}

func analyzeNetworkCapacity(ctx context.Context, client *truenas.Client, timeRange string) (map[string]interface{}, error) {
	// Get all network interfaces
	ifaceResult, err := client.Call(ctx, "interface.query")
	if err != nil {
		return nil, fmt.Errorf("failed to query interfaces: %w", err)
	}

	var ifaceList []map[string]interface{}
	if err := json.Unmarshal(ifaceResult, &ifaceList); err != nil {
		return nil, fmt.Errorf("failed to parse interface list: %w", err)
	}

	interfaceAnalysis := make(map[string]interface{})

	for _, iface := range ifaceList {
		ifaceName, ok := iface["name"].(string)
		if !ok || ifaceName == "" {
			continue
		}

		// Get link speed if available
		var linkSpeed float64
		if state, ok := iface["state"].(map[string]interface{}); ok {
			if speed, ok := state["link_speed"].(float64); ok {
				linkSpeed = speed // In Mbps
			}
		}

		// Get network metrics for this interface
		result, err := client.Call(ctx, "reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       "interface",
				"identifier": ifaceName,
			},
		}, map[string]interface{}{"unit": timeRange})

		if err != nil {
			interfaceAnalysis[ifaceName] = map[string]string{"error": err.Error()}
			continue
		}

		var metricsData []map[string]interface{}
		if err := json.Unmarshal(result, &metricsData); err != nil {
			interfaceAnalysis[ifaceName] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}

		if len(metricsData) == 0 {
			continue
		}

		// Analyze both TX and RX
		ifaceInfo := map[string]interface{}{"time_range": timeRange}
		if linkSpeed > 0 {
			ifaceInfo["link_speed_mbps"] = linkSpeed
		}

		// Each series (legend "received", "sent") is analysed separately.
		// The "interface" graph is in Kilobits/s.
		ifaceInfo["unit_source"] = "reporting graph 'interface' (" + reportingUnits["interface"] + "), converted to Mbps"
		for _, metric := range metricsData {
			legend := legendNames(metric)
			for col := 1; col < len(legend); col++ {
				samples := columnSamples(metric, col, false, nil)
				if len(samples) == 0 || legend[col] == "" {
					continue
				}
				for i := range samples {
					samples[i].v = math.Abs(samples[i].v) / 1000.0
				}
				complete, note := completeSamples(metric, samples)
				if note != "" {
					ifaceInfo["partial_sample_note"] = note
				}
				ifaceInfo[legend[col]] = networkSeriesInfo(sampleValues(samples), sampleValues(complete), linkSpeed)
			}
		}

		interfaceAnalysis[ifaceName] = ifaceInfo
	}

	return interfaceAnalysis, nil
}

// networkSeriesInfo summarizes one interface series in Mbps, with
// utilization against linkSpeed (Mbps) when known. current_mbps is the
// mean of the last 5 complete samples (completeMbps excludes a partial
// last bucket); average and peak use every sample.
func networkSeriesInfo(dataPointsMbps, completeMbps []float64, linkSpeed float64) map[string]interface{} {
	current := calculateRecentAverage(completeMbps, 5)
	average := calculateAverage(dataPointsMbps)
	peak := calculateMax(dataPointsMbps)

	metricInfo := map[string]interface{}{
		"current_mbps": fmt.Sprintf("%.2f", current),
		"average_mbps": fmt.Sprintf("%.2f", average),
		"peak_mbps":    fmt.Sprintf("%.2f", peak),
	}

	// Calculate utilization percentage if we have link speed
	if linkSpeed > 0 {
		currentPct := (current / linkSpeed) * 100
		metricInfo["current_utilization_pct"] = fmt.Sprintf("%.2f", currentPct)
		metricInfo["average_utilization_pct"] = fmt.Sprintf("%.2f", average/linkSpeed*100)
		metricInfo["peak_utilization_pct"] = fmt.Sprintf("%.2f", peak/linkSpeed*100)
		metricInfo["capacity_status"] = determineCapacityStatus(currentPct, 70.0, 85.0)
	}

	return metricInfo
}

func analyzeDiskCapacity(ctx context.Context, client *truenas.Client, timeRange string) (map[string]interface{}, error) {
	// Get available disk graphs
	graphsResult, err := client.Call(ctx, "reporting.graphs")
	if err != nil {
		return nil, fmt.Errorf("failed to query reporting graphs: %w", err)
	}

	var graphs []map[string]interface{}
	if err := json.Unmarshal(graphsResult, &graphs); err != nil {
		return nil, fmt.Errorf("failed to parse reporting graphs: %w", err)
	}

	// Find disk identifiers
	var diskIdentifiers []string
	for _, graph := range graphs {
		if graphName, ok := graph["name"].(string); ok && graphName == "disk" {
			if identifiersRaw, ok := graph["identifiers"]; ok && identifiersRaw != nil {
				if identifiersArray, ok := identifiersRaw.([]interface{}); ok {
					for _, idRaw := range identifiersArray {
						if idStr, ok := idRaw.(string); ok {
							diskIdentifiers = append(diskIdentifiers, idStr)
						}
					}
				}
			}
			break
		}
	}

	if len(diskIdentifiers) == 0 {
		return nil, fmt.Errorf("no disk identifiers found")
	}

	diskAnalysis := make(map[string]interface{})

	for _, identifier := range diskIdentifiers {
		diskName := identifier
		if idx := strings.Index(identifier, " |"); idx != -1 {
			diskName = identifier[:idx]
		}

		result, err := client.Call(ctx, "reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       "disk",
				"identifier": identifier,
			},
		}, map[string]interface{}{"unit": timeRange})

		if err != nil {
			diskAnalysis[diskName] = map[string]string{"error": err.Error()}
			continue
		}

		var metricsData []map[string]interface{}
		if err := json.Unmarshal(result, &metricsData); err != nil {
			diskAnalysis[diskName] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}

		if len(metricsData) == 0 {
			continue
		}

		// Analyze throughput per series (legend "reads", "writes"). The
		// "disk" graph is in Kibibytes/s.
		// current is the mean of the last 5 complete samples.
		diskInfo := map[string]interface{}{"unit": reportingUnits["disk"], "time_range": timeRange}
		for _, metric := range metricsData {
			legend := legendNames(metric)
			for col := 1; col < len(legend); col++ {
				samples := columnSamples(metric, col, false, nil)
				if len(samples) == 0 || legend[col] == "" {
					continue
				}
				for i := range samples {
					samples[i].v = math.Abs(samples[i].v)
				}
				complete, note := completeSamples(metric, samples)
				if note != "" {
					diskInfo["partial_sample_note"] = note
				}
				dataPoints := sampleValues(samples)
				diskInfo[legend[col]] = map[string]interface{}{
					"current": fmt.Sprintf("%.2f", calculateRecentAverage(sampleValues(complete), 5)),
					"average": fmt.Sprintf("%.2f", calculateAverage(dataPoints)),
					"peak":    fmt.Sprintf("%.2f", calculateMax(dataPoints)),
					"trend":   calculateTrendDirection(dataPoints),
				}
			}
		}

		diskAnalysis[diskName] = diskInfo
	}

	return diskAnalysis, nil
}

func handleGetPoolCapacityDetails(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	poolName, _ := args["pool_name"].(string)

	// Get pool information
	poolResult, err := client.Call(ctx, "pool.query")
	if err != nil {
		return "", err
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(poolResult, &pools); err != nil {
		return "", err
	}

	// Filter by pool name if specified
	var targetPools []map[string]interface{}
	for _, pool := range pools {
		if poolName == "" || pool["name"] == poolName {
			targetPools = append(targetPools, pool)
		}
	}

	analysis := make([]map[string]interface{}, 0, len(targetPools))

	for _, pool := range targetPools {
		poolAnalysis := make(map[string]interface{})

		poolAnalysis["name"] = pool["name"]
		poolAnalysis["status"] = pool["status"]
		poolAnalysis["healthy"] = pool["healthy"]

		// Same dataset query as query_datasets: exact pool match,
		// retrieve_children=false, and only the properties simplifyDataset
		// reads. Errors are reported on the pool instead of dropping datasets.
		poolAnalysis["datasets"] = []map[string]interface{}{}
		name, _ := pool["name"].(string)
		datasetResult, err := client.Call(ctx, "pool.dataset.query",
			[]interface{}{datasetTypeFilter, []interface{}{"pool", "=", name}},
			map[string]interface{}{
				"extra": map[string]interface{}{
					"retrieve_children": false,
					"properties":        datasetQueryProperties,
				},
			},
		)
		if err != nil {
			poolAnalysis["datasets_error"] = fmt.Sprintf("failed to query datasets: %v", err)
		} else {
			var datasets []map[string]interface{}
			if err := json.Unmarshal(datasetResult, &datasets); err != nil {
				poolAnalysis["datasets_error"] = fmt.Sprintf("failed to parse datasets: %v", err)
			} else {
				top, total := analyzeDatasetCapacity(datasets)
				poolAnalysis["datasets"] = top
				poolAnalysis["dataset_count"] = len(top)
				poolAnalysis["total_datasets"] = total
				if len(top) < total {
					poolAnalysis["datasets_note"] = fmt.Sprintf("Showing %d of %d datasets (largest by used space)", len(top), total)
				}
			}
		}

		// Calculate capacity metrics from topology
		capacity := calculatePoolCapacity(pool)
		poolAnalysis["capacity"] = capacity

		// Determine warning level
		if utilPct, ok := capacity["utilization_pct"].(float64); ok {
			poolAnalysis["capacity_warning"] = determineCapacityStatus(utilPct, 70.0, 85.0)
		}

		analysis = append(analysis, poolAnalysis)
	}

	result := map[string]interface{}{
		"pools": analysis,
		"note":  "Historical capacity trends are not available from TrueNAS API. This shows current snapshot only. For growth trend analysis, query this tool periodically and track results externally.",
	}

	formatted, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func determineCapacityStatus(current, warningThreshold, criticalThreshold float64) string {
	if current >= criticalThreshold {
		return "critical"
	} else if current >= warningThreshold {
		return "warning"
	}
	return "healthy"
}

func calculateProjections(values []float64, current, warningThreshold, criticalThreshold float64) []string {
	projections := make([]string, 0)

	if len(values) < 2 {
		return projections
	}

	// Calculate growth rate (% per time unit)
	n := float64(len(values))
	sumX := 0.0
	sumY := 0.0
	sumXY := 0.0
	sumX2 := 0.0

	for i, y := range values {
		x := float64(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}

	slope := (n*sumXY - sumX*sumY) / (n*sumX2 - sumX*sumX)

	if slope <= 0 {
		return projections
	}

	// Project when we'll hit thresholds
	if current < warningThreshold {
		timeToWarning := (warningThreshold - current) / slope
		if timeToWarning > 0 && timeToWarning < 1000 {
			projections = append(projections, fmt.Sprintf("Warning threshold (%.0f%%) projected in ~%.0f time units", warningThreshold, timeToWarning))
		}
	}

	if current < criticalThreshold {
		timeToCritical := (criticalThreshold - current) / slope
		if timeToCritical > 0 && timeToCritical < 1000 {
			projections = append(projections, fmt.Sprintf("Critical threshold (%.0f%%) projected in ~%.0f time units", criticalThreshold, timeToCritical))
		}
	}

	return projections
}

func generateCapacityRecommendations(analysis map[string]interface{}) map[string]interface{} {
	recommendations := make([]string, 0)
	overallStatuses := make([]string, 0)

	// Check CPU
	if cpuAnalysis, ok := analysis["cpu"].(map[string]interface{}); ok {
		if status, ok := cpuAnalysis["capacity_status"].(string); ok {
			overallStatuses = append(overallStatuses, status)
			if status == "warning" {
				recommendations = append(recommendations,
					"CPU utilization is elevated (>70%). Consider reviewing workloads or planning CPU upgrade.")
			} else if status == "critical" {
				recommendations = append(recommendations,
					"CPU utilization is critical (>85%). Immediate action recommended: optimize workloads or upgrade hardware.")
			}
		}
	}

	// Check memory
	if memAnalysis, ok := analysis["memory"].(map[string]interface{}); ok {
		if status, ok := memAnalysis["capacity_status"].(string); ok {
			overallStatuses = append(overallStatuses, status)
			if status == "warning" {
				recommendations = append(recommendations,
					"Memory utilization (excluding ZFS ARC cache when known) is elevated (>70%). Consider adding more RAM or optimizing memory usage.")
			} else if status == "critical" {
				recommendations = append(recommendations,
					"Memory utilization (excluding ZFS ARC cache when known) is critical (>85%). Immediate action recommended: add more RAM or reduce workload.")
			}
		}
	}

	// Check network interfaces
	if netAnalysis, ok := analysis["network"].(map[string]interface{}); ok {
		for ifaceName, ifaceData := range netAnalysis {
			if ifaceName == "error" {
				continue
			}
			if ifaceInfo, ok := ifaceData.(map[string]interface{}); ok {
				for metric, metricData := range ifaceInfo {
					if metric == "link_speed_mbps" {
						continue
					}
					if metricInfo, ok := metricData.(map[string]interface{}); ok {
						if status, ok := metricInfo["capacity_status"].(string); ok {
							overallStatuses = append(overallStatuses, status)
							if status == "warning" || status == "critical" {
								recommendations = append(recommendations,
									fmt.Sprintf("Network interface %s (%s) is nearing capacity. Consider upgrading link speed or load balancing.", ifaceName, metric))
							}
						}
					}
				}
			}
		}
	}

	// Determine overall status
	overallStatus := "healthy"
	for _, status := range overallStatuses {
		if status == "critical" {
			overallStatus = "critical"
			break
		} else if status == "warning" {
			overallStatus = "warning"
		}
	}

	if len(recommendations) == 0 {
		recommendations = append(recommendations, "All monitored capacity metrics are within healthy ranges.")
	}

	return map[string]interface{}{
		"recommendations": recommendations,
		"overall_status":  overallStatus,
	}
}

// analyzeDatasetCapacity summarises datasets for get_pool_capacity_details
// using the same parser as query_datasets (top-level used/available, not a
// nested properties object). It returns the top defaultQueryLimit datasets
// by used space and the total count before the limit.
func analyzeDatasetCapacity(datasets []map[string]interface{}) (top []map[string]interface{}, total int) {
	names := make([]string, 0, len(datasets))
	for _, ds := range datasets {
		if n, ok := ds["name"].(string); ok {
			names = append(names, n)
		}
	}
	simplified := make([]map[string]interface{}, 0, len(datasets))
	for _, ds := range datasets {
		summary := simplifyDataset(ds)
		if name, ok := summary["name"].(string); ok {
			summary["children_count"] = datasetChildrenCount(name, names)
		}
		used, usedOK := summary["used_bytes"].(float64)
		avail, availOK := summary["available_bytes"].(float64)
		if usedOK && availOK && used+avail > 0 {
			summary["utilization_pct"] = (used / (used + avail)) * 100
		}
		simplified = append(simplified, summary)
	}
	sortDatasets(simplified, "used")
	total = len(simplified)
	if len(simplified) > defaultQueryLimit {
		simplified = simplified[:defaultQueryLimit]
	}
	return simplified, total
}

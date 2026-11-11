package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"runtime"
	"runtime/debug"
	"sort"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

type Registry struct {
	client      *truenas.Client
	taskManager *tasks.Manager
	tools       map[string]Tool

	// Safety policy, set by SetPolicy (tools/safety.go)
	readOnly bool
	allow    map[string]bool
	deny     map[string]bool

	// confirmTokens holds the confirmation tokens of destructive tools
	// (tools/confirm_token.go), including elicit-only handles.
	confirmTokens *confirmationTokens

	// requireHumanConfirm refuses destructive tools on clients that did
	// not declare form elicitation, instead of falling back to a token.
	requireHumanConfirm bool
}

type Tool struct {
	Definition mcp.Tool
	Handler    func(context.Context, *truenas.Client, map[string]interface{}) (string, error)
	// Plan, when set, is the shared preview/execute plan. Destructive
	// tools with a Plan issue and redeem confirmation tokens bound to
	// that plan (issueWithPlan/redeemWithPlan) so a token cannot be
	// used for a different planned operation.
	Plan func(context.Context, *truenas.Client, map[string]interface{}) (*writePlan, error)
	// Execute, when set, runs the verified plan after a token is
	// redeemed. callDestructive passes the plan it just hashed, so
	// the handler does not rebuild it.
	Execute func(context.Context, *truenas.Client, map[string]interface{}, *writePlan) (string, error)
}

func NewRegistry(client *truenas.Client, taskManager *tasks.Manager) *Registry {
	r := &Registry{
		client:        client,
		taskManager:   taskManager,
		tools:         make(map[string]Tool),
		confirmTokens: newConfirmationTokens(),
	}
	r.registerTools()
	r.applySafety()
	return r
}

func (r *Registry) registerTools() {
	r.registerSystemTools()

	r.registerBootTools()

	r.registerScrubTools()

	r.registerDirectoryServiceTools()

	r.registerPoolTools()

	r.registerQueryTools()

	r.registerShareTools()

	r.registerVMTools()

	r.registerDatasetTools()

	r.registerSMBShareTools()

	r.registerNFSShareTools()

	r.registerAlertTools()

	r.registerMetricsTools()

	r.registerAppTools()

	r.registerJobsTools()

	r.registerCapacityTools()

	r.registerTaskTools()
}

func (r *Registry) ListTools() []mcp.Tool {
	tools := make([]mcp.Tool, 0, len(r.tools))
	for name, tool := range r.tools {
		if r.checkPolicy(name) != nil {
			continue
		}
		if r.toolHiddenByVersion(name) {
			continue
		}
		if r.readOnly && readOnlyDryRunTools[name] {
			tools = append(tools, readOnlyDryRunDefinition(tool.Definition))
		} else {
			tools = append(tools, tool.Definition)
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

// HasTool reports whether name is a registered tool, whether or not the
// safety policy hides it.
func (r *Registry) HasTool(name string) bool {
	_, ok := r.tools[name]
	return ok
}

// CallTool runs a tool. It is safe for concurrent use once SetPolicy has
// returned. A call whose ctx is already done runs nothing. Handlers and
// the TrueNAS client observe ctx, so a cancelled call stops waiting for
// an in-flight middleware request.
func (r *Registry) CallTool(ctx context.Context, name string, args map[string]interface{}) (result string, err error) {
	// A panicking handler must not take down the server. Log the details to
	// stderr (the panic value only for runtime errors, which carry no
	// argument values) and return a generic error to the client.
	defer func() {
		if p := recover(); p != nil {
			detail := fmt.Sprintf("%T", p)
			if re, ok := p.(runtime.Error); ok {
				detail = re.Error()
			}
			log.Printf("tool %s panicked: %s\n%s", name, detail, debug.Stack())
			result, err = "", fmt.Errorf("internal error in tool %s; the operation may not have completed, check the system state before retrying", name)
		}
	}()

	tool, exists := r.tools[name]
	if !exists {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	// Enforce the policy here, not only in ListTools: clients may call tools
	// they were never shown.
	if err := r.checkPolicy(name); err != nil {
		return "", err
	}
	// Reject a non-boolean dry_run or confirm before any policy that
	// interprets those flags, any preview, or the handler. A string
	// "true" must not fall through to a real write.
	if err := validateSafetyBooleans(name, args); err != nil {
		return "", err
	}
	if err := r.checkReadOnlyDryRun(name, args); err != nil {
		return "", err
	}
	// confirm is no longer declared by destructive tools (they take
	// confirmation_token), but a client with a stale schema may still send
	// it. Accept it, so the call gets the preview and an explanation of the
	// token flow instead of an unknown-argument error, and drop it here.
	destructive := safetyOf(name).Access == Destructive
	legacyConfirm := false
	if _, sent := args["confirm"]; sent && destructive && !hasParam(tool, "confirm") {
		legacyConfirm, _ = args["confirm"].(bool)
		args = withoutKeys(args, "confirm")
	}
	// Reject arguments the schema does not declare before anything runs,
	// including the dry-run preview of a destructive tool.
	if err := validateArgs(name, tool, args); err != nil {
		return "", withMigrationHint(name, args, err)
	}
	// Then check the values of the declared arguments against the schema:
	// types, enums, required arguments and ranges. A wrong-typed value
	// must not be dropped in favour of a default.
	// null on an optional argument means omitted. Drop it (in a copy)
	// before validation, so the handler sees the default.
	args = dropNullOptionals(tool.Definition.InputSchema, args)
	if err := validateArgValues(name, tool, args); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("tool %s was not run: %w", name, err)
	}
	// Hide-and-refuse tools the connected TrueNAS release cannot run,
	// and refuse every tool if the NAS is below the 25.10 minimum.
	if err := r.checkVersion(ctx, name); err != nil {
		return "", err
	}
	if destructive {
		return r.callDestructive(ctx, name, tool, args, legacyConfirm)
	}

	return tool.Handler(ctx, r.client, args)
}

// withoutKeys returns a copy of args without the named keys.
func withoutKeys(args map[string]interface{}, keys ...string) map[string]interface{} {
	out := make(map[string]interface{}, len(args))
	for k, v := range args {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// availableMemory holds the available-memory samples of a "memory" graph.
type availableMemory struct {
	all         []float64 // every valid sample, oldest first
	latest      float64   // latest complete sample (latest sample if all are partial)
	partialNote string    // completeSamples note about a partial last bucket
}

// availableMemorySamples returns the available-memory samples of a
// "memory" graph, skipping gaps: null, non-finite and non-positive values
// (a running system never has zero available memory).
func availableMemorySamples(metric map[string]interface{}) (availableMemory, error) {
	if _, ok := metric["data"].([]interface{}); !ok {
		return availableMemory{}, fmt.Errorf("no data field in metric")
	}
	samples := columnSamples(metric, 1, false, func(v float64) bool { return v > 0 })
	if len(samples) == 0 {
		return availableMemory{}, fmt.Errorf("no valid data points")
	}
	complete, note := completeSamples(metric, samples)
	return availableMemory{
		all:         sampleValues(samples),
		latest:      complete[len(complete)-1].v,
		partialNote: note,
	}, nil
}

// bytesPct is used as a percentage of total, clamped to 0-100.
func bytesPct(used int64, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Max(0, math.Min(100, float64(used)/total*100))
}

// arcMaxAge is how old the latest arcsize sample may be, relative to the
// NAS's own reference time, before it is treated as unavailable rather than
// compared with current memory figures.
const arcMaxAge = 15 * time.Minute

// errArcStale reports that the latest arcsize sample is older than arcMaxAge.
var errArcStale = errors.New("latest ZFS ARC sample is stale")

// nasReferenceTime returns the newest timestamp in a reporting.get_data
// graph, in the NAS's clock: the later of the graph's "end" and its newest
// sample with a value. It returns 0 when neither is present.
func nasReferenceTime(graph map[string]interface{}) float64 {
	ref := 0.0
	if graph == nil {
		return ref
	}
	if end, ok := graph["end"].(float64); ok && end > ref && !math.IsInf(end, 0) {
		ref = end
	}
	rows, _ := graph["data"].([]interface{})
	for i := len(rows) - 1; i >= 0; i-- {
		row, ok := rows[i].([]interface{})
		if !ok || len(row) < 2 {
			continue
		}
		if val, ok := row[1].(float64); !ok || math.IsNaN(val) || math.IsInf(val, 0) || val <= 0 {
			continue // gap
		}
		if ts, ok := row[0].(float64); ok && ts > ref && !math.IsInf(ts, 0) {
			ref = ts
		}
		break
	}
	return ref
}

// currentArcBytes returns the latest ZFS ARC size from the "arcsize"
// reporting graph. Its age is judged only against timestamps from the NAS
// itself, never this host's clock, so clock skew between the two does not
// matter: the reference is the later of memRef (the newest timestamp of a
// graph the caller fetched from the same NAS, 0 if none) and the arcsize
// graph's "end". A sample more than arcMaxAge older than the
// reference yields errArcStale. With no reference at all, freshness is
// unknown: the sample is returned with freshKnown false.
func currentArcBytes(ctx context.Context, client *truenas.Client, memRef float64) (bytes float64, freshKnown bool, err error) {
	result, err := client.Call(ctx, "reporting.get_data", []interface{}{
		map[string]interface{}{"name": "arcsize", "identifier": nil},
	}, map[string]interface{}{"unit": "HOUR"})
	if err != nil {
		return 0, false, err
	}
	var data []map[string]interface{}
	if err := json.Unmarshal(result, &data); err != nil {
		return 0, false, err
	}
	if len(data) == 0 {
		return 0, false, fmt.Errorf("no arcsize data")
	}
	// Only the graph's "end" counts here: the ARC sample can't vouch for its
	// own freshness.
	ref := memRef
	if end, ok := data[0]["end"].(float64); ok && end > ref && !math.IsInf(end, 0) {
		ref = end
	}
	rows, _ := data[0]["data"].([]interface{})
	for i := len(rows) - 1; i >= 0; i-- {
		row, ok := rows[i].([]interface{})
		if !ok || len(row) < 2 {
			continue
		}
		val, ok := row[1].(float64)
		if !ok || math.IsNaN(val) || math.IsInf(val, 0) || val <= 0 {
			continue // gap
		}
		ts, ok := row[0].(float64)
		if !ok || ts <= 0 || ref <= 0 {
			return val, false, nil
		}
		if ref-ts > arcMaxAge.Seconds() {
			return 0, false, errArcStale
		}
		return val, true, nil
	}
	return 0, false, fmt.Errorf("no arcsize data")
}

// arcFreshnessUnknownNote is the caveat given when the ARC sample was used
// but its age could not be checked.
const arcFreshnessUnknownNote = "The TrueNAS reporting data had no reference timestamp, so the age of the ZFS ARC sample could not be checked; it was used as is."

// arcUnavailableNote explains why memory figures may include the ARC.
func arcUnavailableNote(err error) string {
	if errors.Is(err, errArcStale) {
		return "Latest ZFS ARC sample is more than 15 minutes older than the newest TrueNAS reporting timestamp, so it was not subtracted; utilization may include the ZFS ARC cache, which is reclaimable."
	}
	return "ZFS ARC size unavailable; utilization may include the ZFS ARC cache, which is reclaimable."
}

// memoryUsedPct converts an available-memory sample into percent used,
// clamped to 0-100.
func memoryUsedPct(total, available float64) float64 {
	if total <= 0 {
		return 0
	}
	pct := (total - available) / total * 100
	return math.Max(0, math.Min(100, pct))
}

// Helper functions for capacity analysis

func calculateAverage(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func calculateRecentAverage(values []float64, count int) float64 {
	if len(values) == 0 {
		return 0
	}

	start := len(values) - count
	if start < 0 {
		start = 0
	}

	return calculateAverage(values[start:])
}

func calculateMax(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	max := values[0]
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	return max
}

func calculateTrendDirection(values []float64) string {
	if len(values) < 2 {
		return "stable"
	}

	// Simple linear regression to determine trend
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

	// Calculate slope
	slope := (n*sumXY - sumX*sumY) / (n*sumX2 - sumX*sumX)

	// Determine trend based on slope
	avgValue := sumY / n
	if avgValue == 0 {
		return "stable"
	}

	// Calculate relative slope (% change per time unit)
	relativeSlope := (slope / avgValue) * 100

	if relativeSlope > 1.0 {
		return "increasing"
	} else if relativeSlope < -1.0 {
		return "decreasing"
	}
	return "stable"
}

func calculatePoolCapacity(pool map[string]interface{}) map[string]interface{} {
	capacity := make(map[string]interface{})

	// Try to get capacity from topology
	if topology, ok := pool["topology"].(map[string]interface{}); ok {
		// Look for data vdevs
		if data, ok := topology["data"].([]interface{}); ok && len(data) > 0 {
			totalBytes := int64(0)
			for _, vdevRaw := range data {
				if vdev, ok := vdevRaw.(map[string]interface{}); ok {
					if stats, ok := vdev["stats"].(map[string]interface{}); ok {
						if size, ok := stats["size"].(float64); ok {
							totalBytes += int64(size)
						}
					}
				}
			}
			if totalBytes > 0 {
				capacity["total_bytes"] = totalBytes
			}
		}
	}

	// Get used/available from root dataset if available
	if name, ok := pool["name"].(string); ok {
		capacity["pool_name"] = name
	}

	// Try to get usage from pool-level stats
	if usedBytes, ok := pool["allocated"].(float64); ok {
		capacity["used_bytes"] = int64(usedBytes)
	}

	if freeBytes, ok := pool["free"].(float64); ok {
		capacity["available_bytes"] = int64(freeBytes)
	}

	// Calculate utilization percentage
	if used, ok := capacity["used_bytes"].(int64); ok {
		if available, ok := capacity["available_bytes"].(int64); ok {
			total := used + available
			if total > 0 {
				utilPct := (float64(used) / float64(total)) * 100
				capacity["utilization_pct"] = utilPct
				capacity["total_bytes"] = total
			}
		}
	}

	return capacity
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	return fmt.Sprintf("%.2f %s", float64(bytes)/float64(div), units[exp])
}

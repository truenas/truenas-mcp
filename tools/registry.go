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
	"strings"
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

	// query_datasets and query_snapshots stay here so this split does not
	// move handlers that are being changed on other branches.
	r.tools["query_datasets"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_datasets",
			Description: "Query datasets with optional filtering and sorting. Returns simplified dataset information with capacity, encryption status, and usage details. Use 'limit' to control result size (default: 50, maximum: 1000), 'order_by' to sort by size, and 'encrypted_only' to filter.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pool": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter datasets by pool name (exact match; tank does not include tank2)",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of datasets to return (default: 50, maximum: 1000)",
						"minimum":     1,
					},
					"order_by": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Sort by 'used' (space usage), 'available', or 'name' (default: used descending)",
						"enum":        []string{"used", "available", "name"},
					},
					"encrypted_only": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Return only encrypted datasets (default: false)",
					},
				},
			},
		},
		Handler: handleQueryDatasets,
	}

	// Snapshots query
	r.tools["query_snapshots"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_snapshots",
			Description: "Query ZFS snapshots with optional filtering and sorting. Returns simplified snapshot information with creation info, sizes and dataset. Holds are included only with 'include_holds' or 'holds_only' (they cost an extra lookup per snapshot). Use 'limit' to control result size (default: 50, maximum: 1000), 'order_by' to sort.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dataset": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter snapshots by parent dataset name",
					},
					"pool": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter snapshots by pool name",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of snapshots to return (default: 50, maximum: 1000)",
						"minimum":     1,
					},
					"order_by": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Sort by 'name' (snapshot name, default descending), 'dataset' (parent dataset), or 'created' (parsed from name if available)",
						"enum":        []string{"name", "dataset", "created"},
					},
					"holds_only": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Return only snapshots with holds that prevent deletion (default: false). Implies include_holds.",
					},
					"include_holds": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Include each snapshot's holds (default: false; one extra lookup per snapshot on the NAS)",
					},
				},
			},
		},
		Handler: handleQuerySnapshots,
	}

	r.registerShareTools()

	r.registerVMTools()

	r.registerDatasetTools()

	r.registerSMBShareTools()

	r.registerNFSShareTools()

	r.registerAlertTools()

	r.registerMetricsTools()

	r.registerAppTools()

	r.registerJobsTools()

	// analyze_capacity and get_pool_capacity_details stay here so this
	// split does not move handlers that are being changed on other branches.
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

// datasetQueryProperties are the ZFS properties query_datasets requests
// (what simplifyDataset reads). Combined with retrieve_children=false,
// this keeps pool.dataset.query from returning every property and a
// nested children tree for each row.
var datasetQueryProperties = []interface{}{
	"used", "available", "usedbysnapshots", "usedbydataset", "usedbychildren",
	"compression", "compressratio", "dedup", "quota", "refquota",
	"encryption", "keystatus",
}

// datasetTypeFilter is always sent with retrieve_children=false so
// middleware still walks child datasets. On 25.10 and 27.0, an empty
// filter list plus retrieve_children=false returns only pool roots.
var datasetTypeFilter = []interface{}{"type", "in", []interface{}{"FILESYSTEM", "VOLUME"}}

const (
	defaultQueryLimit = 50
	maxQueryLimit     = 1000
)

// datasetChildrenCount returns how many names are direct children of name
// (one path component under it). Used when retrieve_children is false.
func datasetChildrenCount(name string, names []string) int {
	prefix := name + "/"
	n := 0
	for _, other := range names {
		if strings.HasPrefix(other, prefix) && !strings.Contains(other[len(prefix):], "/") {
			n++
		}
	}
	return n
}

// queryCount runs method(filters, {count: true}) and returns the integer
// total when the middleware answers with a JSON number.
func joinNote(existing interface{}, add string) string {
	if s, ok := existing.(string); ok && s != "" {
		return s + "; " + add
	}
	return add
}

func queryLimit(args map[string]interface{}) (limit int, capped bool) {
	l, ok := args["limit"].(float64)
	// !(l >= 1) also rejects NaN and -Inf, which a plain l < 1 would not.
	if !ok || !(l >= 1) {
		return defaultQueryLimit, false
	}
	if l > float64(maxQueryLimit) {
		return maxQueryLimit, true
	}
	return int(l), false
}

func queryCount(ctx context.Context, client *truenas.Client, method string, filters []interface{}) (int, bool) {
	raw, err := client.Call(ctx, method, filters, map[string]interface{}{"count": true})
	if err != nil {
		return 0, false
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return int(n), true
}

func handleQueryDatasets(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	// Always include a structured filter. With retrieve_children=false and
	// no filters, 25.10 and 27.0 middleware do not recurse below pool roots.
	filters := []interface{}{datasetTypeFilter}
	if pool, ok := args["pool"].(string); ok && pool != "" {
		// Equality on the pool field: a name prefix of "tank" would also
		// match tank2/... datasets.
		filters = append(filters, []interface{}{"pool", "=", pool})
	}

	options := map[string]interface{}{
		"extra": map[string]interface{}{
			"retrieve_children": false,
			"properties":        datasetQueryProperties,
		},
	}

	result, err := client.Call(ctx, "pool.dataset.query", filters, options)
	if err != nil {
		return "", err
	}

	var datasets []map[string]interface{}
	if err := json.Unmarshal(result, &datasets); err != nil {
		return "", fmt.Errorf("failed to parse datasets: %w", err)
	}

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
		simplified = append(simplified, summary)
	}

	// Filter by encryption after children_count so the count reflects the
	// full hierarchy, not only encrypted neighbours.
	if encryptedOnly, ok := args["encrypted_only"].(bool); ok && encryptedOnly {
		filtered := make([]map[string]interface{}, 0)
		for _, ds := range simplified {
			if encrypted, ok := ds["encrypted"].(bool); ok && encrypted {
				filtered = append(filtered, ds)
			}
		}
		simplified = filtered
	}

	orderBy := "used"
	if order, ok := args["order_by"].(string); ok && order != "" {
		orderBy = order
	}
	sortDatasets(simplified, orderBy)

	limit, capped := queryLimit(args)
	total := len(simplified)
	if len(simplified) > limit {
		simplified = simplified[:limit]
	}

	response := map[string]interface{}{
		"datasets":       simplified,
		"dataset_count":  len(simplified),
		"total_datasets": total,
	}
	if pool, ok := args["pool"].(string); ok && pool != "" {
		response["pool_filter"] = pool
	}
	if len(simplified) < total {
		response["note"] = fmt.Sprintf("Showing %d of %d datasets (limited)", len(simplified), total)
	}
	if capped {
		response["note"] = joinNote(response["note"], "limit was capped to 1000")
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// simplifyDataset extracts the most relevant fields from a raw dataset object
func simplifyDataset(ds map[string]interface{}) map[string]interface{} {
	summary := map[string]interface{}{
		"name": ds["name"],
		"type": ds["type"],
		"pool": ds["pool"],
	}

	// Helper to extract parsed value from property object
	getParsed := func(prop interface{}) interface{} {
		if propMap, ok := prop.(map[string]interface{}); ok {
			return propMap["parsed"]
		}
		return nil
	}

	// Helper to extract human-readable value from property object
	getValue := func(prop interface{}) interface{} {
		if propMap, ok := prop.(map[string]interface{}); ok {
			if val := propMap["value"]; val != nil {
				return val
			}
			return propMap["parsed"]
		}
		return nil
	}

	// Mountpoint (direct field, not nested)
	if mp, ok := ds["mountpoint"].(string); ok && mp != "" {
		summary["mountpoint"] = mp
	}

	// Capacity fields (CRITICAL for user queries)
	if used := getParsed(ds["used"]); used != nil {
		summary["used_bytes"] = used
		summary["used"] = getValue(ds["used"]) // Human readable like "1008.3 GiB"
	}
	if avail := getParsed(ds["available"]); avail != nil {
		summary["available_bytes"] = avail
		summary["available"] = getValue(ds["available"]) // Human readable like "5.87 TiB"
	}

	// Usage breakdown (useful for understanding where space goes)
	if snapUsed := getParsed(ds["usedbysnapshots"]); snapUsed != nil {
		if bytes, ok := snapUsed.(float64); ok && bytes > 0 {
			summary["used_by_snapshots"] = getValue(ds["usedbysnapshots"])
		}
	}
	if dsUsed := getParsed(ds["usedbydataset"]); dsUsed != nil {
		summary["used_by_dataset"] = getValue(ds["usedbydataset"])
	}
	if childUsed := getParsed(ds["usedbychildren"]); childUsed != nil {
		if bytes, ok := childUsed.(float64); ok && bytes > 0 {
			summary["used_by_children"] = getValue(ds["usedbychildren"])
		}
	}

	// Compression
	if comp := getParsed(ds["compression"]); comp != nil {
		summary["compression"] = comp
		if ratio := getParsed(ds["compressratio"]); ratio != nil {
			summary["compression_ratio"] = ratio
		}
	}

	// Deduplication (only if enabled)
	if dedup := getParsed(ds["deduplication"]); dedup != nil {
		if dedupStr, ok := dedup.(string); ok && dedupStr != "off" {
			summary["deduplication"] = dedup
		}
	}

	// Quotas (only if set)
	if quota := getParsed(ds["quota"]); quota != nil {
		summary["quota"] = getValue(ds["quota"])
	}
	if refquota := getParsed(ds["refquota"]); refquota != nil {
		summary["refquota"] = getValue(ds["refquota"])
	}

	// Encryption
	if encrypted, ok := ds["encrypted"].(bool); ok {
		summary["encrypted"] = encrypted
		if encrypted {
			if locked, ok := ds["locked"].(bool); ok {
				summary["locked"] = locked
			}
			if keyLoaded, ok := ds["key_loaded"].(bool); ok && keyLoaded {
				summary["key_loaded"] = keyLoaded
			}
		}
	}

	// Children count (useful for understanding hierarchy)
	if children, ok := ds["children"].([]interface{}); ok {
		summary["children_count"] = len(children)
	}

	return summary
}

// sortDatasets sorts a slice of simplified datasets by the specified field
func sortDatasets(datasets []map[string]interface{}, orderBy string) {
	sort.Slice(datasets, func(i, j int) bool {
		switch orderBy {
		case "used":
			// Sort by used_bytes descending (largest first)
			iUsed, iOk := datasets[i]["used_bytes"].(float64)
			jUsed, jOk := datasets[j]["used_bytes"].(float64)
			if iOk && jOk {
				return iUsed > jUsed
			}
			return false
		case "available":
			// Sort by available_bytes descending (most available first)
			iAvail, iOk := datasets[i]["available_bytes"].(float64)
			jAvail, jOk := datasets[j]["available_bytes"].(float64)
			if iOk && jOk {
				return iAvail > jAvail
			}
			return false
		case "name":
			// Sort by name alphabetically
			iName, iOk := datasets[i]["name"].(string)
			jName, jOk := datasets[j]["name"].(string)
			if iOk && jOk {
				return iName < jName
			}
			return false
		default:
			// Default to name
			iName, iOk := datasets[i]["name"].(string)
			jName, jOk := datasets[j]["name"].(string)
			if iOk && jOk {
				return iName < jName
			}
			return false
		}
	})
}

func handleQuerySnapshots(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	// Build query filters - initialize as empty array, not nil (API expects [] not null)
	filters := []interface{}{}
	if dataset, ok := args["dataset"].(string); ok && dataset != "" {
		filters = append(filters, []interface{}{"dataset", "=", dataset})
	}
	if pool, ok := args["pool"].(string); ok && pool != "" {
		filters = append(filters, []interface{}{"pool", "=", pool})
	}

	// Request creation time and sizes explicitly: TrueNAS 27.0 only returns
	// the properties named in extra.properties. Holds are only returned
	// with extra.holds, which costs one extra ZFS lookup per snapshot, so
	// they are requested only for holds_only or include_holds.
	holdsOnly, _ := args["holds_only"].(bool)
	includeHolds, _ := args["include_holds"].(bool)
	wantHolds := holdsOnly || includeHolds
	extra := map[string]interface{}{"properties": snapshotProperties}
	if wantHolds {
		extra["holds"] = true
	}
	options := map[string]interface{}{"extra": extra}

	orderBy := "name"
	if order, ok := args["order_by"].(string); ok && order != "" {
		orderBy = order
	}
	limit, capped := queryLimit(args)

	// Unfiltered calls can return every snapshot on the NAS. Push limit
	// and order_by when the sort key is a middleware field. holds_only
	// and "created" still need the full list (post-filter or computed sort).
	unfiltered := true
	if dataset, ok := args["dataset"].(string); ok && dataset != "" {
		unfiltered = false
	}
	if pool, ok := args["pool"].(string); ok && pool != "" {
		unfiltered = false
	}
	pushLimit := unfiltered && !holdsOnly && (orderBy == "name" || orderBy == "dataset")
	if pushLimit {
		options["limit"] = limit
		if orderBy == "name" {
			options["order_by"] = []string{"-snapshot_name"}
		} else {
			options["order_by"] = []string{"dataset"}
		}
	}

	result, err := client.Call(ctx, "pool.snapshot.query", filters, options)
	if err != nil {
		return "", err
	}

	var snapshots []map[string]interface{}
	if err := json.Unmarshal(result, &snapshots); err != nil {
		return "", fmt.Errorf("failed to parse snapshots: %w", err)
	}

	// Simplify response
	simplified := make([]map[string]interface{}, 0, len(snapshots))
	for _, snap := range snapshots {
		summary := simplifySnapshot(snap)
		simplified = append(simplified, summary)
	}

	// Filter by holds_only if requested
	if holdsOnly {
		filtered := make([]map[string]interface{}, 0)
		for _, snap := range simplified {
			if holdsCount, ok := snap["holds_count"].(int); ok && holdsCount > 0 {
				filtered = append(filtered, snap)
			}
		}
		simplified = filtered
	}

	sortSnapshots(simplified, orderBy)

	totalSnapshots := len(simplified)
	totalKnown := true
	// A count query is only needed when the page was full; otherwise the
	// returned rows are the complete result.
	if pushLimit && len(snapshots) >= limit {
		if n, ok := queryCount(ctx, client, "pool.snapshot.query", filters); ok {
			totalSnapshots = n
		} else {
			totalKnown = false
		}
	}
	if len(simplified) > limit {
		simplified = simplified[:limit]
	}

	// Add metadata wrapper
	response := map[string]interface{}{
		"snapshots":      simplified,
		"snapshot_count": len(simplified),
		"holds_included": wantHolds,
	}
	if totalKnown {
		response["total_snapshots"] = totalSnapshots
	} else {
		response["note"] = "Snapshot total is unknown; the count query failed"
	}
	if dataset, ok := args["dataset"].(string); ok && dataset != "" {
		response["dataset_filter"] = dataset
	}
	if pool, ok := args["pool"].(string); ok && pool != "" {
		response["pool_filter"] = pool
	}
	if holdsOnly {
		response["holds_filter"] = "only snapshots with holds"
	}
	if totalKnown && len(simplified) < totalSnapshots {
		response["note"] = fmt.Sprintf("Showing %d of %d snapshots (limited)", len(simplified), totalSnapshots)
	}
	if capped {
		response["note"] = joinNote(response["note"], "limit was capped to 1000")
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// simplifySnapshot extracts the most relevant fields from a raw snapshot object
func simplifySnapshot(snap map[string]interface{}) map[string]interface{} {
	summary := map[string]interface{}{
		"snapshot_name": snap["snapshot_name"],
		"dataset":       snap["dataset"],
		"pool":          snap["pool"],
	}

	// Creation time: the ZFS "creation" property when present, else a
	// date parsed from the snapshot name.
	props, _ := snap["properties"].(map[string]interface{})
	if created, ts := snapshotCreated(props); ts > 0 {
		summary["created"] = created
		summary["created_timestamp"] = ts
		summary["created_source"] = "zfs_property"
	} else if snapName, ok := snap["snapshot_name"].(string); ok {
		if parsedDate := parseSnapshotDate(snapName); parsedDate != "" {
			summary["created_date"] = parsedDate
			summary["created_source"] = "snapshot_name"
		}
	}

	// Space: "used" is space unique to the snapshot, "referenced" the
	// data it refers to.
	for _, prop := range []string{"used", "referenced"} {
		if n, ok := snapshotPropertyBytes(props, prop); ok {
			summary[prop+"_bytes"] = n
			summary[prop] = formatBytes(n)
		}
	}

	// Add createtxg for reference
	if txg, ok := snap["createtxg"].(string); ok {
		summary["createtxg"] = txg
	}

	// Count holds and extract names
	if holds, ok := snap["holds"].(map[string]interface{}); ok {
		if len(holds) > 0 {
			summary["holds_count"] = len(holds)
			holdNames := make([]string, 0, len(holds))
			for name := range holds {
				holdNames = append(holdNames, name)
			}
			summary["holds"] = holdNames
		}
	}

	// Include full snapshot ID for reference
	if id, ok := snap["id"].(string); ok {
		summary["full_name"] = id
	}

	return summary
}

// parseSnapshotDate attempts to extract date information from snapshot names
func parseSnapshotDate(name string) string {
	// Common patterns used by automatic snapshot tasks
	patterns := []struct {
		layout string
		prefix string
	}{
		{"2006-01-02_15-04", "auto-"},    // auto-YYYY-MM-DD_HH-MM
		{"2006-01-02", "auto-"},          // auto-YYYY-MM-DD
		{"2006-01-02_15-04", ""},         // YYYY-MM-DD_HH-MM
		{"2006-01-02", ""},               // YYYY-MM-DD
		{"20060102-1504", "auto-"},       // auto-YYYYMMDD-HHMM
		{"20060102", "auto-"},            // auto-YYYYMMDD
		{"2006-01-02_15-04-05", "auto-"}, // auto-YYYY-MM-DD_HH-MM-SS
		{"2006-01-02_1504", ""},          // YYYY-MM-DD_HHMM
	}

	for _, p := range patterns {
		// Try to extract date substring
		dateStr := name
		if p.prefix != "" && strings.HasPrefix(name, p.prefix) {
			dateStr = strings.TrimPrefix(name, p.prefix)
		}

		// Try parsing with this layout
		if t, err := time.Parse(p.layout, dateStr); err == nil {
			return t.Format("2006-01-02 15:04")
		}

		// Also try just the first part before any underscore
		if idx := strings.Index(dateStr, "_"); idx > 0 {
			if t, err := time.Parse("2006-01-02", dateStr[:idx]); err == nil {
				return t.Format("2006-01-02")
			}
		}
	}

	return "" // No date found
}

// sortSnapshots sorts a slice of simplified snapshots by the specified field
func sortSnapshots(snapshots []map[string]interface{}, orderBy string) {
	sort.Slice(snapshots, func(i, j int) bool {
		switch orderBy {
		case "name":
			// Sort by snapshot_name descending (newest automatic snapshots first)
			iName, iOk := snapshots[i]["snapshot_name"].(string)
			jName, jOk := snapshots[j]["snapshot_name"].(string)
			if iOk && jOk {
				return iName > jName // Descending
			}
			return false
		case "dataset":
			// Sort by dataset path alphabetically ascending
			iDataset, iOk := snapshots[i]["dataset"].(string)
			jDataset, jOk := snapshots[j]["dataset"].(string)
			if iOk && jOk {
				return iDataset < jDataset
			}
			return false
		case "created":
			// Newest first: creation property timestamp, then the date
			// parsed from the name, then the name. Snapshots with a
			// timestamp sort before those without.
			iTS, iTSOk := snapshots[i]["created_timestamp"].(int64)
			jTS, jTSOk := snapshots[j]["created_timestamp"].(int64)
			if iTSOk && jTSOk && iTS != jTS {
				return iTS > jTS
			}
			if iTSOk != jTSOk {
				return iTSOk
			}
			iCreated, iOk := snapshots[i]["created_date"].(string)
			jCreated, jOk := snapshots[j]["created_date"].(string)
			if iOk && jOk {
				return iCreated > jCreated
			}
			// Fallback to name comparison
			iName, iOk := snapshots[i]["snapshot_name"].(string)
			jName, jOk := snapshots[j]["snapshot_name"].(string)
			if iOk && jOk {
				return iName > jName
			}
			return false
		default:
			// Default to name descending
			iName, iOk := snapshots[i]["snapshot_name"].(string)
			jName, jOk := snapshots[j]["snapshot_name"].(string)
			if iOk && jOk {
				return iName > jName
			}
			return false
		}
	})
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

		// Get datasets for this pool
		var datasets []map[string]interface{}
		datasetResult, err := client.Call(ctx, "pool.dataset.query",
			[]interface{}{[]interface{}{"name", "^", pool["name"]}})
		if err == nil {
			if err := json.Unmarshal(datasetResult, &datasets); err == nil {
				poolAnalysis["datasets"] = analyzeDatasetCapacity(datasets)
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

func analyzeDatasetCapacity(datasets []map[string]interface{}) []map[string]interface{} {
	analysis := make([]map[string]interface{}, 0, len(datasets))

	for _, ds := range datasets {
		dsAnalysis := map[string]interface{}{
			"name": ds["name"],
			"type": ds["type"],
		}

		// Get properties
		if props, ok := ds["properties"].(map[string]interface{}); ok {
			// Extract used space
			if used, ok := props["used"].(map[string]interface{}); ok {
				if usedVal, ok := used["rawvalue"].(string); ok {
					dsAnalysis["used_bytes"] = usedVal
				}
				if usedParsed, ok := used["parsed"].(float64); ok {
					dsAnalysis["used_bytes_numeric"] = int64(usedParsed)
				}
			}

			// Extract available space
			if available, ok := props["available"].(map[string]interface{}); ok {
				if availVal, ok := available["rawvalue"].(string); ok {
					dsAnalysis["available_bytes"] = availVal
				}
				if availParsed, ok := available["parsed"].(float64); ok {
					dsAnalysis["available_bytes_numeric"] = int64(availParsed)
				}
			}

			// Extract referenced space
			if referenced, ok := props["referenced"].(map[string]interface{}); ok {
				if refVal, ok := referenced["rawvalue"].(string); ok {
					dsAnalysis["referenced_bytes"] = refVal
				}
			}

			// Calculate utilization if we have both used and available
			if usedNum, usedOk := dsAnalysis["used_bytes_numeric"].(int64); usedOk {
				if availNum, availOk := dsAnalysis["available_bytes_numeric"].(int64); availOk {
					total := usedNum + availNum
					if total > 0 {
						utilPct := (float64(usedNum) / float64(total)) * 100
						dsAnalysis["utilization_pct"] = fmt.Sprintf("%.2f", utilPct)
					}
				}
			}
		}

		analysis = append(analysis, dsAnalysis)
	}

	return analysis
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

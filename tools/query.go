package tools

// Dataset and snapshot query tools and their handlers (query_datasets,
// query_snapshots). Helpers such as simplifyDataset, sortDatasets,
// datasetChildrenCount, datasetQueryProperties, datasetTypeFilter and
// defaultQueryLimit also live here; get_pool_capacity_details reuses them.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerQueryTools() {
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
	// snapshotHoldsScanBudget is the maximum number of snapshot rows
	// holds_only will page through when filtering for holds.
	snapshotHoldsScanBudget = 1000
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

// queryCount runs method(filters, {count: true}) and returns the integer
// total when the middleware answers with a JSON number.
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

func datasetServerOrder(orderBy string) []string {
	// On 25.10 and 27.0, used and available are flattened to the top
	// level of each pool.dataset.query row. There is no properties
	// object, so -properties.used.parsed is ignored and limit returns
	// collection order. Snapshot rows still nest creation under
	// properties.creation.parsed; see snapshotServerOrder.
	switch orderBy {
	case "available":
		return []string{"-available.parsed"}
	case "name":
		return []string{"name"}
	default:
		return []string{"-used.parsed"}
	}
}

func snapshotServerOrder(orderBy string) []string {
	switch orderBy {
	case "dataset":
		return []string{"dataset"}
	case "created":
		// createtxg is a string and is not zero-padded, so lexical
		// -createtxg mis-orders values such as "9" and "10". Creation
		// time lives under properties.creation.parsed as a number.
		return []string{"-properties.creation.parsed"}
	default:
		return []string{"-snapshot_name"}
	}
}

// queryDatasetNames lists dataset names (and encryption) with a compact
// query so query_datasets can compute children_count and totals without
// fetching every property for every row.
//
// This listing is intentionally unbounded. children_count and the
// total_datasets figure are computed from the complete name list, so a
// cap would silently under-count on large systems. The query is already
// compact (select name, encrypted; no nested children; no full property
// set), and the client's 10 MiB response read limit still applies.
func queryDatasetNames(ctx context.Context, client *truenas.Client, filters []interface{}) ([]string, map[string]bool, error) {
	raw, err := client.Call(ctx, "pool.dataset.query", filters, map[string]interface{}{
		"select": []interface{}{"name", "encrypted"},
		"extra": map[string]interface{}{
			"retrieve_children":   false,
			"properties":          []interface{}{"encryption"},
			"retrieve_user_props": false,
		},
	})
	if err != nil {
		return nil, nil, err
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, nil, fmt.Errorf("failed to parse dataset names: %w", err)
	}
	names := make([]string, 0, len(rows))
	encrypted := make(map[string]bool, len(rows))
	for _, row := range rows {
		name, _ := row["name"].(string)
		if name == "" {
			continue
		}
		names = append(names, name)
		if enc, ok := row["encrypted"].(bool); ok && enc {
			encrypted[name] = true
		}
	}
	return names, encrypted, nil
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

	// Compact name listing for children_count and totals, so the
	// full-property query can be limited without losing hierarchy.
	names, encryptedByName, err := queryDatasetNames(ctx, client, filters)
	if err != nil {
		return "", err
	}

	orderBy := "used"
	if order, ok := args["order_by"].(string); ok && order != "" {
		orderBy = order
	}
	limit, capped := queryLimit(args)
	encryptedOnly, _ := args["encrypted_only"].(bool)

	heavyFilters := filters
	if encryptedOnly {
		heavyFilters = append(append([]interface{}{}, filters...), []interface{}{"encrypted", "=", true})
	}

	options := map[string]interface{}{
		"extra": map[string]interface{}{
			"retrieve_children":   false,
			"properties":          datasetQueryProperties,
			"retrieve_user_props": false,
		},
		"limit":    limit,
		"order_by": datasetServerOrder(orderBy),
	}

	result, err := client.Call(ctx, "pool.dataset.query", heavyFilters, options)
	if err != nil {
		return "", err
	}

	var datasets []map[string]interface{}
	if err := json.Unmarshal(result, &datasets); err != nil {
		return "", fmt.Errorf("failed to parse datasets: %w", err)
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
	if encryptedOnly {
		filtered := make([]map[string]interface{}, 0)
		for _, ds := range simplified {
			if encrypted, ok := ds["encrypted"].(bool); ok && encrypted {
				filtered = append(filtered, ds)
			}
		}
		simplified = filtered
	}

	sortDatasets(simplified, orderBy)

	total := len(names)
	if encryptedOnly {
		total = 0
		for _, name := range names {
			if encryptedByName[name] {
				total++
			}
		}
	}
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

// sortDatasets sorts a slice of simplified datasets by the specified field.
// used and available are descending (largest first); name is ascending.
// Ties, including equal used or available values, break by name so the
// order is deterministic.
func sortDatasets(datasets []map[string]interface{}, orderBy string) {
	sort.SliceStable(datasets, func(i, j int) bool {
		switch orderBy {
		case "used":
			iUsed, iOk := datasets[i]["used_bytes"].(float64)
			jUsed, jOk := datasets[j]["used_bytes"].(float64)
			if iOk && jOk && iUsed != jUsed {
				return iUsed > jUsed
			}
			if iOk != jOk {
				return iOk
			}
		case "available":
			iAvail, iOk := datasets[i]["available_bytes"].(float64)
			jAvail, jOk := datasets[j]["available_bytes"].(float64)
			if iOk && jOk && iAvail != jAvail {
				return iAvail > jAvail
			}
			if iOk != jOk {
				return iOk
			}
		case "name":
			// Fall through to the name comparison.
		}
		iName, _ := datasets[i]["name"].(string)
		jName, _ := datasets[j]["name"].(string)
		return iName < jName
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

	var (
		simplified     []map[string]interface{}
		totalSnapshots int
		totalKnown     = true
		note           string
	)

	if holdsOnly {
		var budgetHit bool
		var scanned int
		var err error
		simplified, totalSnapshots, totalKnown, budgetHit, scanned, err = queryHeldSnapshots(ctx, client, filters, extra, orderBy, limit)
		if err != nil {
			return "", err
		}
		if budgetHit {
			note = fmt.Sprintf("Stopped after scanning %d snapshots; more snapshots with holds may exist. Narrow the query with a pool or dataset filter", scanned)
		} else if !totalKnown && len(simplified) == limit {
			note = fmt.Sprintf("Showing %d snapshots with holds (limited)", len(simplified))
		}
	} else {
		options["limit"] = limit
		options["order_by"] = snapshotServerOrder(orderBy)

		result, err := client.Call(ctx, "pool.snapshot.query", filters, options)
		if err != nil {
			return "", err
		}

		var snapshots []map[string]interface{}
		if err := json.Unmarshal(result, &snapshots); err != nil {
			return "", fmt.Errorf("failed to parse snapshots: %w", err)
		}

		simplified = make([]map[string]interface{}, 0, len(snapshots))
		for _, snap := range snapshots {
			simplified = append(simplified, simplifySnapshot(snap))
		}

		sortSnapshots(simplified, orderBy)

		totalSnapshots = len(simplified)
		if len(snapshots) >= limit {
			if n, ok := queryCount(ctx, client, "pool.snapshot.query", filters); ok {
				totalSnapshots = n
			} else {
				totalKnown = false
				note = "Snapshot total is unknown; the count query failed"
			}
		}
		if len(simplified) > limit {
			simplified = simplified[:limit]
		}
	}

	// Add metadata wrapper
	response := map[string]interface{}{
		"snapshots":      simplified,
		"snapshot_count": len(simplified),
		"holds_included": wantHolds,
	}
	if totalKnown {
		response["total_snapshots"] = totalSnapshots
	} else if note == "" {
		note = "Snapshot total is unknown; the count query failed"
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
		note = joinNote(note, fmt.Sprintf("Showing %d of %d snapshots (limited)", len(simplified), totalSnapshots))
	}
	if capped {
		note = joinNote(note, "limit was capped to 1000")
	}
	if note != "" {
		response["note"] = note
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// queryHeldSnapshots pages through pool.snapshot.query with a server-side
// limit and order, keeping snapshots that have holds, up to
// snapshotHoldsScanBudget rows.
func queryHeldSnapshots(ctx context.Context, client *truenas.Client, filters []interface{}, extra map[string]interface{}, orderBy string, limit int) (rows []map[string]interface{}, total int, known, budgetHit bool, scanned int, err error) {
	held := make([]map[string]interface{}, 0)
	offset := 0
	exhausted := false
	for scanned < snapshotHoldsScanBudget {
		remaining := snapshotHoldsScanBudget - scanned
		pageSize := limit
		if pageSize < defaultQueryLimit {
			pageSize = defaultQueryLimit
		}
		if pageSize > remaining {
			pageSize = remaining
		}
		options := map[string]interface{}{
			"extra":    extra,
			"limit":    pageSize,
			"order_by": snapshotServerOrder(orderBy),
		}
		if offset > 0 {
			options["offset"] = offset
		}
		result, callErr := client.Call(ctx, "pool.snapshot.query", filters, options)
		if callErr != nil {
			return nil, 0, false, false, scanned, callErr
		}
		var page []map[string]interface{}
		if err := json.Unmarshal(result, &page); err != nil {
			return nil, 0, false, false, scanned, fmt.Errorf("failed to parse snapshots: %w", err)
		}
		if len(page) == 0 {
			exhausted = true
			break
		}
		scanned += len(page)
		offset += len(page)
		for _, snap := range page {
			summary := simplifySnapshot(snap)
			if holdsCount, ok := summary["holds_count"].(int); ok && holdsCount > 0 {
				held = append(held, summary)
			}
		}
		if len(page) < pageSize {
			exhausted = true
			break
		}
		if len(held) >= limit {
			break
		}
	}
	budgetHit = !exhausted && scanned >= snapshotHoldsScanBudget
	sortSnapshots(held, orderBy)
	total = len(held)
	known = exhausted
	if len(held) > limit {
		held = held[:limit]
	}
	return held, total, known, budgetHit, scanned, nil
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

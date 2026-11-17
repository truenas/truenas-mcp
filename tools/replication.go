package tools

// Replication and periodic snapshot task query tools (query_replications,
// query_snapshot_tasks). Both project middleware records onto fixed field
// lists: replication tasks embed the full keychain SSH credential entry,
// and only its id is kept.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/truenas/truenas-mcp/internal/mwtime"
	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerReplicationTools() {
	r.tools["query_replications"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_replications",
			Description: "List configured ZFS replication tasks and the state of each: direction (PUSH/PULL), transport, source datasets, target dataset, recursion, schedule or the periodic snapshot tasks it is bound to, retention policy, and the last run (state, time, error, warnings, last snapshot sent). SSH connection details are not returned; only the keychain connection id is. Use 'task_id' for one task and 'limit' to bound the list (default: 50, maximum: 1000).",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task_id": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Replication task id (from this tool) to return only that task",
						"minimum":     1,
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of tasks to return (default: 50, maximum: 1000)",
						"minimum":     1,
					},
				},
			},
		},
		Handler: handleQueryReplications,
	}

	r.tools["query_snapshot_tasks"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_snapshot_tasks",
			Description: "List periodic snapshot tasks: dataset, recursion and excludes, naming schema, schedule, retention, the last run and last_snapshot (the snapshot the task last created, which proves it ran). For a recursive task, 'coverage' lists the datasets the task covers today and which of them have no snapshot from the last run (for example children created after it ran), so a gap in what a replication will ship is visible. 'bound_replications' lists the replication tasks that run after this task. Use 'task_id' for one task, 'dataset' for an exact dataset, and 'limit' to bound the list (default: 50, maximum: 1000).",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task_id": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Periodic snapshot task id (from this tool) to return only that task",
						"minimum":     1,
					},
					"dataset": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Return only tasks on this dataset (exact match)",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of tasks to return (default: 50, maximum: 1000)",
						"minimum":     1,
					},
				},
			},
		},
		Handler: handleQuerySnapshotTasks,
	}
}

const (
	// maxCoverageDatasets bounds each per-task dataset list in
	// query_snapshot_tasks coverage. Counts are always complete.
	maxCoverageDatasets = 100
	// maxTaskStateText bounds free-text task state fields (error, reason,
	// each warning).
	maxTaskStateText = 1000
	// maxTaskStateWarnings bounds the warnings list of a task state.
	maxTaskStateWarnings = 20
)

var (
	// taskScheduleFields are the cron fields of a replication or periodic
	// snapshot task schedule (25.10 / 27.0 CronModel and TimeCronModel).
	taskScheduleFields = []string{"minute", "hour", "dom", "month", "dow", "begin", "end"}
	// taskProgressFields are the fields of a RUNNING replication state's
	// progress object.
	taskProgressFields = []string{"dataset", "snapshot", "snapshots_sent", "snapshots_total", "bytes_sent", "bytes_total"}
	// jobProgressFields are the fields of a middleware job's progress.
	jobProgressFields = []string{"percent", "description"}
)

// taskIDFilter returns the id filter for an optional integer task_id.
func taskIDFilter(args map[string]interface{}) ([]interface{}, int, bool) {
	v, ok := args["task_id"].(float64)
	if !ok {
		return []interface{}{}, 0, false
	}
	id := int(v)
	return []interface{}{[]interface{}{"id", "=", id}}, id, true
}

// queryTaskPage runs one page of a task query (fetch is called with the
// options: a single order_by key and limit) and returns the rows plus the
// total, from count when the page is full. totalKnown is false when that
// count query failed.
func queryTaskPage(limit int, fetch func(options map[string]interface{}) (json.RawMessage, error), count func() (int, bool)) (rows []map[string]interface{}, total int, totalKnown bool, err error) {
	raw, err := fetch(map[string]interface{}{
		"order_by": []string{"id"},
		"limit":    limit,
	})
	if err != nil {
		return nil, 0, false, err
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, 0, false, fmt.Errorf("unexpected result: %w", err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	total, totalKnown = len(rows), true
	if len(rows) >= limit {
		total, totalKnown = count()
	}
	return rows, total, totalKnown, nil
}

// taskSchedule returns a task schedule's cron fields and a readable form,
// or nil when the task has no schedule.
func taskSchedule(v interface{}) map[string]interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	out := pickAllowed(m, taskScheduleFields)
	human := formatCronSchedule(m)
	if month, _ := m["month"].(string); month != "" && month != "*" {
		human += fmt.Sprintf(" (months: %s)", month)
	}
	begin, _ := m["begin"].(string)
	end, _ := m["end"].(string)
	if begin != "" && end != "" && !(begin == "00:00" && end == "23:59") {
		human += fmt.Sprintf(", only between %s and %s", begin, end)
	}
	out["human"] = human
	return out
}

// boundTaskText redacts and bounds a free-text task state field.
func boundTaskText(v interface{}) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	cut, _ := tasks.BoundText(s, maxTaskStateText)
	return cut, true
}

// simplifyTaskState is the last-run summary of a zettarepl task state
// ({state, datetime, error, warnings, last_snapshot, progress, reason}).
func simplifyTaskState(v interface{}) map[string]interface{} {
	state, ok := v.(map[string]interface{})
	if !ok {
		return map[string]interface{}{"state": "UNKNOWN"}
	}
	out := map[string]interface{}{}
	if s, ok := state["state"].(string); ok && s != "" {
		out["state"] = s
	} else {
		out["state"] = "UNKNOWN"
	}
	if when, ts := mwtime.Normalize(state["datetime"]); ts > 0 {
		out["datetime"] = when
	}
	for _, k := range []string{"error", "reason"} {
		if s, ok := boundTaskText(state[k]); ok {
			out[k] = s
		}
	}
	if list, ok := state["warnings"].([]interface{}); ok && len(list) > 0 {
		warnings := make([]string, 0, len(list))
		for _, w := range list {
			if len(warnings) == maxTaskStateWarnings {
				break
			}
			if s, ok := boundTaskText(w); ok {
				warnings = append(warnings, s)
			}
		}
		out["warnings"] = warnings
		if len(list) > maxTaskStateWarnings {
			out["warnings_total"] = len(list)
		}
	}
	if s, ok := state["last_snapshot"].(string); ok && s != "" {
		out["last_snapshot"] = s
	} else {
		out["last_snapshot"] = nil
	}
	if p := pickAllowedMap(state["progress"], taskProgressFields); p != nil {
		if pm, ok := p.(map[string]interface{}); ok && len(pm) > 0 {
			out["progress"] = pm
		}
	}
	return out
}

// lastSnapshotName splits "dataset@name" into its dataset and snapshot name.
func lastSnapshotName(s string) (dataset, name string, ok bool) {
	i := strings.LastIndex(s, "@")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func stringList(v interface{}) []string {
	list, _ := v.([]interface{})
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func intID(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// simplifyBoundSnapshotTask is a periodic snapshot task as embedded in a
// replication task.
func simplifyBoundSnapshotTask(v interface{}) map[string]interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	return pickAllowed(m, []string{"id", "dataset", "recursive", "naming_schema", "enabled"})
}

// simplifyReplication projects a replication.query entry onto the fields
// query_replications returns. ssh_credentials (a full keychain entry with
// host, user, private key id and host key) is reduced to its id; netcat
// addresses, encryption keys and key locations, properties_override and
// the raw job are dropped.
func simplifyReplication(task map[string]interface{}) map[string]interface{} {
	out := pickAllowed(task, []string{
		"id", "name", "enabled", "direction", "transport", "source_datasets", "target_dataset",
		"recursive", "exclude", "replicate", "auto", "only_matching_schedule", "naming_schema",
		"also_include_naming_schema", "name_regex", "readonly", "allow_from_scratch",
		"hold_pending_snapshots", "encryption", "encryption_inherit", "sudo", "netcat_active_side",
		"compression", "speed_limit", "retries", "properties", "properties_exclude", "large_block",
		"compressed", "embed",
	})
	if creds, ok := task["ssh_credentials"].(map[string]interface{}); ok {
		if id, ok := intID(creds["id"]); ok {
			out["ssh_connection_id"] = id
		}
	}
	out["schedule"] = taskSchedule(task["schedule"])
	if rs := taskSchedule(task["restrict_schedule"]); rs != nil {
		out["restrict_schedule"] = rs
	}

	bound := []map[string]interface{}{}
	if list, ok := task["periodic_snapshot_tasks"].([]interface{}); ok {
		for _, item := range list {
			if b := simplifyBoundSnapshotTask(item); b != nil {
				bound = append(bound, b)
			}
		}
	}
	out["bound_snapshot_tasks"] = bound
	out["trigger"] = replicationTrigger(task, bound)

	retention := map[string]interface{}{"policy": task["retention_policy"]}
	if policy, _ := task["retention_policy"].(string); policy == "CUSTOM" {
		retention["lifetime_value"] = task["lifetime_value"]
		retention["lifetime_unit"] = task["lifetime_unit"]
	}
	if lifetimes, ok := task["lifetimes"].([]interface{}); ok && len(lifetimes) > 0 {
		retention["additional_lifetimes"] = len(lifetimes)
	}
	out["retention"] = retention

	out["last_run"] = simplifyTaskState(task["state"])
	if job, ok := task["job"].(map[string]interface{}); ok {
		cur := pickAllowed(job, []string{"id", "state"})
		if p, ok := pickAllowedMap(job["progress"], jobProgressFields).(map[string]interface{}); ok && len(p) > 0 {
			if s, ok := boundTaskText(p["description"]); ok {
				p["description"] = s
			}
			cur["progress"] = p
		}
		out["current_job"] = cur
	}
	return out
}

// replicationTrigger says in words what starts a replication task.
func replicationTrigger(task map[string]interface{}, bound []map[string]interface{}) string {
	auto, _ := task["auto"].(bool)
	if !auto {
		return "manual only (auto is off)"
	}
	if len(bound) > 0 {
		ids := make([]string, 0, len(bound))
		for _, b := range bound {
			ids = append(ids, fmt.Sprint(b["id"]))
		}
		return fmt.Sprintf("runs after periodic snapshot task(s) %s; it ships existing snapshots and does not create any", strings.Join(ids, ", "))
	}
	if task["schedule"] != nil {
		return "runs on its own schedule"
	}
	return "automatic, but has no schedule and no bound periodic snapshot task"
}

func handleQueryReplications(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	filters, id, byID := taskIDFilter(args)
	limit, capped := queryLimit(args)
	rows, total, totalKnown, err := queryTaskPage(limit,
		func(options map[string]interface{}) (json.RawMessage, error) {
			return client.Call(ctx, "replication.query", filters, options)
		},
		func() (int, bool) { return queryCount(ctx, client, "replication.query", filters) })
	if err != nil {
		return "", fmt.Errorf("failed to query replication tasks: %w", err)
	}
	if byID && len(rows) == 0 {
		return "", fmt.Errorf("replication task %d not found", id)
	}
	list := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		list = append(list, simplifyReplication(row))
	}
	response := map[string]interface{}{
		"replications": list,
		"count":        len(list),
	}
	note := ""
	if totalKnown {
		response["total_replications"] = total
		if len(list) < total {
			note = fmt.Sprintf("Showing %d of %d replication tasks (limited)", len(list), total)
		}
	} else {
		note = "Replication task total is unknown; the count query failed"
	}
	if capped {
		note = joinNote(note, "limit was capped to 1000")
	}
	if note != "" {
		response["note"] = note
	}
	return marshalJSON(response)
}

// simplifySnapshotTask projects a pool.snapshottask.query entry onto the
// fields query_snapshot_tasks returns.
func simplifySnapshotTask(task map[string]interface{}) map[string]interface{} {
	out := pickAllowed(task, []string{"id", "dataset", "recursive", "exclude", "enabled", "naming_schema", "allow_empty", "vmware_sync"})
	out["schedule"] = taskSchedule(task["schedule"])
	retention := map[string]interface{}{
		"lifetime_value": task["lifetime_value"],
		"lifetime_unit":  task["lifetime_unit"],
	}
	if v, ok := task["lifetime_value"].(float64); ok {
		if u, ok := task["lifetime_unit"].(string); ok {
			retention["human"] = fmt.Sprintf("%d %s(s)", int(v), strings.ToLower(u))
		}
	}
	out["retention"] = retention
	state := simplifyTaskState(task["state"])
	out["last_run"] = state
	out["last_snapshot"] = state["last_snapshot"]
	return out
}

// coveredDatasets returns the datasets a periodic snapshot task snapshots
// today: the dataset itself and, for a recursive task, every descendant
// that is not excluded. A child of an excluded dataset is excluded too.
func coveredDatasets(dataset string, recursive bool, exclude []string, names []string) []string {
	if !recursive {
		return []string{dataset}
	}
	excluded := func(name string) bool {
		for _, e := range exclude {
			if name == e || strings.HasPrefix(name, e+"/") {
				return true
			}
		}
		return false
	}
	out := []string{}
	for _, name := range names {
		if (name == dataset || strings.HasPrefix(name, dataset+"/")) && !excluded(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func capStrings(list []string, max int) []string {
	if len(list) <= max {
		return list
	}
	return list[:max]
}

// snapshotTaskCoverage reports which datasets a recursive task covers and
// which of them have no snapshot from the task's last run.
func snapshotTaskCoverage(ctx context.Context, client *truenas.Client, task map[string]interface{}, names []string, namesErr error) map[string]interface{} {
	dataset, _ := task["dataset"].(string)
	recursive, _ := task["recursive"].(bool)
	if !recursive {
		return nil
	}
	if namesErr != nil {
		msg, _ := tasks.BoundText(namesErr.Error(), maxTaskStateText)
		return map[string]interface{}{"error": "dataset list unavailable: " + msg}
	}
	covered := coveredDatasets(dataset, recursive, stringList(task["exclude"]), names)
	cov := map[string]interface{}{
		"datasets_covered": len(covered),
		"datasets":         capStrings(covered, maxCoverageDatasets),
	}
	if len(covered) > maxCoverageDatasets {
		cov["datasets_truncated"] = true
	}
	if len(covered) == 0 {
		cov["note"] = "The task's dataset does not exist"
		return cov
	}
	state, _ := task["state"].(map[string]interface{})
	last, _ := state["last_snapshot"].(string)
	_, snapName, ok := lastSnapshotName(last)
	if !ok {
		cov["note"] = "The task has no recorded last snapshot, so coverage of its last run cannot be checked"
		return cov
	}
	raw, err := client.Call(ctx, "pool.snapshot.query", []interface{}{
		[]interface{}{"dataset", "in", covered},
		[]interface{}{"snapshot_name", "=", snapName},
	}, map[string]interface{}{"extra": map[string]interface{}{"properties": []interface{}{"createtxg"}}})
	if err != nil {
		msg, _ := tasks.BoundText(err.Error(), maxTaskStateText)
		cov["error"] = "snapshot lookup failed: " + msg
		return cov
	}
	var snaps []map[string]interface{}
	if err := json.Unmarshal(raw, &snaps); err != nil {
		cov["error"] = "snapshot lookup returned an unexpected result"
		return cov
	}
	have := map[string]bool{}
	for _, s := range snaps {
		if ds, ok := s["dataset"].(string); ok {
			have[ds] = true
		}
	}
	missing := []string{}
	for _, ds := range covered {
		if !have[ds] {
			missing = append(missing, ds)
		}
	}
	cov["last_run_snapshot"] = snapName
	cov["datasets_with_last_run_snapshot"] = len(covered) - len(missing)
	cov["datasets_missing_last_run_snapshot"] = capStrings(missing, maxCoverageDatasets)
	if len(missing) > maxCoverageDatasets {
		cov["datasets_missing_truncated"] = true
	}
	if len(missing) > 0 {
		cov["note"] = fmt.Sprintf("%d covered dataset(s) have no @%s snapshot: they were created (or un-excluded) after the last run, or that snapshot was removed. They have nothing for a replication of this task to send until the task runs again", len(missing), snapName)
	}
	return cov
}

// boundReplications maps periodic snapshot task id to the ids of the
// replication tasks bound to it.
func boundReplications(ctx context.Context, client *truenas.Client) (map[int][]int, error) {
	raw, err := client.Call(ctx, "replication.query", []interface{}{}, map[string]interface{}{"order_by": []string{"id"}})
	if err != nil {
		return nil, err
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected replication.query result")
	}
	out := map[int][]int{}
	for _, row := range rows {
		replID, ok := intID(row["id"])
		if !ok {
			continue
		}
		list, _ := row["periodic_snapshot_tasks"].([]interface{})
		for _, item := range list {
			m, _ := item.(map[string]interface{})
			if pst, ok := intID(m["id"]); ok {
				out[pst] = append(out[pst], replID)
			}
		}
	}
	return out, nil
}

// datasetPool returns the pool of a dataset name.
func datasetPool(name string) string {
	if i := strings.Index(name, "/"); i >= 0 {
		return name[:i]
	}
	return name
}

func handleQuerySnapshotTasks(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	filters, id, byID := taskIDFilter(args)
	dataset, _ := args["dataset"].(string)
	if dataset != "" {
		filters = append(filters, []interface{}{"dataset", "=", dataset})
	}
	limit, capped := queryLimit(args)
	rows, total, totalKnown, err := queryTaskPage(limit,
		func(options map[string]interface{}) (json.RawMessage, error) {
			return client.Call(ctx, "pool.snapshottask.query", filters, options)
		},
		func() (int, bool) { return queryCount(ctx, client, "pool.snapshottask.query", filters) })
	if err != nil {
		return "", fmt.Errorf("failed to query periodic snapshot tasks: %w", err)
	}
	if byID && len(rows) == 0 {
		return "", fmt.Errorf("periodic snapshot task %d not found", id)
	}

	// One compact dataset name listing per pool that has a recursive task.
	poolNames := map[string][]string{}
	poolErrs := map[string]error{}
	for _, row := range rows {
		ds, _ := row["dataset"].(string)
		if rec, _ := row["recursive"].(bool); !rec || ds == "" {
			continue
		}
		pool := datasetPool(ds)
		if _, done := poolNames[pool]; done {
			continue
		}
		if _, failed := poolErrs[pool]; failed {
			continue
		}
		names, _, err := queryDatasetNames(ctx, client, []interface{}{datasetTypeFilter, []interface{}{"pool", "=", pool}})
		if err != nil {
			poolErrs[pool] = err
			continue
		}
		poolNames[pool] = names
	}

	bound, boundErr := boundReplications(ctx, client)

	list := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		out := simplifySnapshotTask(row)
		ds, _ := row["dataset"].(string)
		pool := datasetPool(ds)
		if cov := snapshotTaskCoverage(ctx, client, row, poolNames[pool], poolErrs[pool]); cov != nil {
			out["coverage"] = cov
		}
		if boundErr == nil {
			taskID, _ := intID(row["id"])
			ids := bound[taskID]
			if ids == nil {
				ids = []int{}
			}
			out["bound_replications"] = ids
		}
		list = append(list, out)
	}

	response := map[string]interface{}{
		"snapshot_tasks": list,
		"count":          len(list),
	}
	if dataset != "" {
		response["dataset_filter"] = dataset
	}
	note := ""
	if totalKnown {
		response["total_snapshot_tasks"] = total
		if len(list) < total {
			note = fmt.Sprintf("Showing %d of %d periodic snapshot tasks (limited)", len(list), total)
		}
	} else {
		note = "Periodic snapshot task total is unknown; the count query failed"
	}
	if capped {
		note = joinNote(note, "limit was capped to 1000")
	}
	if boundErr != nil {
		note = joinNote(note, "bound replication tasks are unknown: replication.query failed")
	}
	if note != "" {
		response["note"] = note
	}
	return marshalJSON(response)
}

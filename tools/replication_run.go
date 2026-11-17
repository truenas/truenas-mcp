package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

const (
	replicationRunMethod  = "replication.run"
	snapshotTaskRunMethod = "pool.snapshottask.run"
	snapshotCreateMethod  = "pool.snapshot.create"
)

var (
	// snapshotTaskRunWait bounds how long run_snapshot_task follows the
	// task's state before reading back the snapshots. Taking a snapshot is
	// quick; vmware_sync tasks can take longer, and then the caller checks
	// query_snapshot_tasks instead.
	snapshotTaskRunWait = 2 * time.Minute
	snapshotTaskRunPoll = time.Second
	// maxEstimateSnapshots bounds the source snapshot listing used by the
	// run_replication preview.
	maxEstimateSnapshots = 5000
	// replicationTaskTTL is how long a run_replication task is tracked.
	replicationTaskTTL = 72 * time.Hour
)

// snapshotNamePattern is the snapshot name characters create_snapshot
// accepts: the ZFS component characters, without spaces.
var snapshotNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// maxZFSNameLen is ZFS_MAX_DATASET_NAME_LEN minus the terminating NUL.
const maxZFSNameLen = 255

func (r *Registry) registerReplicationRunTools() {
	taskIDSchema := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc, "minimum": 1}
	}
	dryRunSchema := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc, "default": false}
	}

	r.tools["run_snapshot_task"] = Tool{
		Definition: mcp.Tool{
			Name:        "run_snapshot_task",
			Description: "Run a periodic snapshot task now, outside its schedule. The task's own configuration decides what is snapshotted. TrueNAS queues the run and returns no job, so the tool follows the task's own state for up to about 2 minutes, then reads back and returns the snapshot names it created on each covered dataset (and any covered dataset that has none). If the run has not finished by then, it says so; check query_snapshot_tasks before running it again. Replication tasks bound to the task (auto, no own schedule) start right after it and may prune target snapshots per their retention; the preview lists them. Destructive: needs confirmation. Refused for a disabled task.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task_id": taskIDSchema("Required: Periodic snapshot task id (from query_snapshot_tasks)"),
					"dry_run": dryRunSchema("Optional: Preview without running (default: false)"),
				},
				"required": []string{"task_id"},
			},
		},
		Handler: r.handleRunSnapshotTaskWithDryRun,
		Plan:    planRunSnapshotTask,
		Execute: r.executeRunSnapshotTask,
	}

	r.tools["run_replication"] = Tool{
		Definition: mcp.Tool{
			Name:        "run_replication",
			Description: "Run a replication task now, outside its schedule. Returns a job id and a task_id to follow with tasks_get; it does not wait for the transfer. When the job finishes, the task says that nothing was transferred (no new snapshots), how many snapshots were sent across all source datasets and children, or that it could not tell. A task bound to a periodic snapshot task ships snapshots that already exist and creates none: run run_snapshot_task first to include current data. The preview shows source, target, direction, retention and an estimate of whether anything would be sent. Destructive (retention and allow_from_scratch can delete target snapshots or data): needs confirmation. Refused while the task is running or on hold, and on TrueNAS 25.10 while it is disabled.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task_id": taskIDSchema("Required: Replication task id (from query_replications)"),
					"dry_run": dryRunSchema("Optional: Preview without running (default: false)"),
				},
				"required": []string{"task_id"},
			},
		},
		Handler: r.handleRunReplicationWithDryRun,
		Plan:    planRunReplication,
		Execute: r.executeRunReplication,
	}

	r.tools["create_snapshot"] = Tool{
		Definition: mcp.Tool{
			Name:        "create_snapshot",
			Description: "Create a ZFS snapshot of a dataset now, for example before changing it. Returns the resulting snapshot name and, for a recursive snapshot, the datasets it was taken on. No retention applies unless the name matches a periodic snapshot task's naming schema; the preview warns about that, about replications that may ship it, and about replication targets. Refused if a snapshot with that name already exists on any dataset it would cover. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dataset": map[string]interface{}{
						"type":        "string",
						"description": "Required: Dataset or zvol to snapshot (e.g. 'tank/data')",
					},
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Required: Snapshot name, the part after '@' (letters, digits, '_', '-', '.', ':')",
					},
					"recursive": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Also snapshot every child dataset, atomically (default: false)",
						"default":     false,
					},
					"dry_run": dryRunSchema("Optional: Preview without creating (default: false)"),
				},
				"required": []string{"dataset", "name"},
			},
		},
		Handler: r.handleCreateSnapshotWithDryRun,
		Plan:    planCreateSnapshot,
	}
}

func requiredTaskID(args map[string]interface{}) (int, error) {
	v, ok := args["task_id"].(float64)
	if !ok || v < 1 {
		return 0, fmt.Errorf("task_id is required")
	}
	return int(v), nil
}

func queryOneTask(raw json.RawMessage, err error, what string, id int) (map[string]interface{}, error) {
	if err != nil {
		return nil, fmt.Errorf("failed to query %s: %w", what, err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected %s result: %w", what, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s %d not found", what, id)
	}
	return rows[0], nil
}

// taskDatasets returns the datasets a periodic snapshot task covers today.
func taskDatasets(ctx context.Context, client *truenas.Client, task map[string]interface{}) ([]string, error) {
	dataset, _ := task["dataset"].(string)
	recursive, _ := task["recursive"].(bool)
	filters := []interface{}{[]interface{}{"id", "=", dataset}, datasetTypeFilter}
	if recursive {
		filters = []interface{}{[]interface{}{"pool", "=", datasetPool(dataset)}, datasetTypeFilter}
	}
	names, _, err := queryDatasetNames(ctx, client, filters)
	if err != nil {
		return nil, err
	}
	if !recursive && len(names) == 0 {
		return nil, nil
	}
	return coveredDatasets(dataset, recursive, stringList(task["exclude"]), names), nil
}

// datasetsWithSnapshot returns which of datasets have a snapshot named
// snapName. It filters by dataset (not by snapshot id), because a
// snapshot id filter that names a missing snapshot empties the result.
func datasetsWithSnapshot(ctx context.Context, client *truenas.Client, datasets []string, snapName string) (map[string]bool, error) {
	have := map[string]bool{}
	if len(datasets) == 0 {
		return have, nil
	}
	raw, err := client.Call(ctx, "pool.snapshot.query", []interface{}{
		[]interface{}{"dataset", "in", datasets},
		[]interface{}{"snapshot_name", "=", snapName},
	}, map[string]interface{}{"extra": map[string]interface{}{"properties": []interface{}{"createtxg"}}})
	if err != nil {
		return nil, err
	}
	var snaps []map[string]interface{}
	if err := json.Unmarshal(raw, &snaps); err != nil {
		return nil, fmt.Errorf("unexpected pool.snapshot.query result")
	}
	for _, s := range snaps {
		if ds, ok := s["dataset"].(string); ok && s["snapshot_name"] == snapName {
			have[ds] = true
		}
	}
	return have, nil
}

func boundedErr(err error) string {
	msg, _ := tasks.BoundText(err.Error(), maxTaskStateText)
	return msg
}

// ---- run_snapshot_task ----

// replicationsAfterSnapshotTask returns the replication tasks zettarepl
// starts when snapshot task pstID finishes: bound, enabled, automatic and
// without a schedule of their own.
func replicationsAfterSnapshotTask(ctx context.Context, client *truenas.Client, pstID int) ([]map[string]interface{}, error) {
	raw, err := client.Call(ctx, "replication.query", []interface{}{}, map[string]interface{}{"order_by": []string{"id"}})
	if err != nil {
		return nil, err
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected replication.query result")
	}
	out := []map[string]interface{}{}
	for _, row := range rows {
		bound := false
		list, _ := row["periodic_snapshot_tasks"].([]interface{})
		for _, item := range list {
			m, _ := item.(map[string]interface{})
			if id, ok := intID(m["id"]); ok && id == pstID {
				bound = true
			}
		}
		auto, _ := row["auto"].(bool)
		enabled, _ := row["enabled"].(bool)
		if !bound || !auto || !enabled || row["schedule"] != nil {
			continue
		}
		id, _ := intID(row["id"])
		out = append(out, map[string]interface{}{
			"id":               id,
			"name":             row["name"],
			"target_dataset":   row["target_dataset"],
			"retention_policy": row["retention_policy"],
		})
	}
	return out, nil
}

func planRunSnapshotTask(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	id, err := requiredTaskID(args)
	if err != nil {
		return nil, err
	}
	raw, err := client.Call(ctx, "pool.snapshottask.query", []interface{}{[]interface{}{"id", "=", id}})
	task, err := queryOneTask(raw, err, "periodic snapshot task", id)
	if err != nil {
		return nil, err
	}
	dataset, _ := task["dataset"].(string)
	recursive, _ := task["recursive"].(bool)
	schema, _ := task["naming_schema"].(string)
	enabled, _ := task["enabled"].(bool)
	allowEmpty, _ := task["allow_empty"].(bool)

	covered, err := taskDatasets(ctx, client, task)
	if err != nil {
		return nil, fmt.Errorf("failed to list the task's datasets: %w", err)
	}
	after, afterErr := replicationsAfterSnapshotTask(ctx, client, id)

	loc, _, _ := nasTimezone(ctx, client)
	summary := simplifySnapshotTask(task, loc)
	current := map[string]interface{}{
		"snapshot_task":    summary,
		"datasets_covered": len(covered),
		"datasets":         capStrings(covered, maxCoverageDatasets),
	}
	if afterErr == nil {
		current["replications_started_after"] = after
	}

	var blocked []string
	if !enabled {
		blocked = append(blocked, fmt.Sprintf("periodic snapshot task %d is disabled; TrueNAS does not run a disabled task", id))
	}
	if len(covered) == 0 {
		blocked = append(blocked, fmt.Sprintf("dataset '%s' of periodic snapshot task %d does not exist", dataset, id))
	}
	if st, _ := task["state"].(map[string]interface{}); st["state"] == "RUNNING" {
		blocked = append(blocked, fmt.Sprintf("periodic snapshot task %d is already running", id))
	}

	warnings := []string{
		fmt.Sprintf("Creates one snapshot named from the schema '%s' on each of %d dataset(s) now; it fails if a snapshot with that name already exists (for example the task already ran this minute)", schema, len(covered)),
		"A real run does not create a job: the tool polls the snapshot task's state for up to about 2 minutes, then returns the result. If the run has not finished by then, it returns QUEUED; check query_snapshot_tasks",
	}
	if !allowEmpty {
		warnings = append(warnings, "allow_empty is off: a dataset with no changes since its previous snapshot of this task gets no new snapshot")
	}
	if v, ok := task["lifetime_value"].(float64); ok {
		u, _ := task["lifetime_unit"].(string)
		warnings = append(warnings, fmt.Sprintf("Retention: snapshots of this task older than %d %s(s) may be removed when the task runs", int(v), strings.ToLower(u)))
	}
	ids := make([]string, 0, len(after))
	for _, rep := range after {
		ids = append(ids, fmt.Sprint(rep["id"]))
	}
	switch {
	case afterErr != nil:
		warnings = append(warnings, "Could not check which replication tasks start after this task (replication.query failed); bound automatic replications start when it finishes")
	case len(after) > 0:
		warnings = append(warnings, fmt.Sprintf("Replication task(s) %s start when this task finishes and ship the new snapshot; they may delete target snapshots per their retention policy", strings.Join(ids, ", ")))
	}

	facts := []string{
		confirmFact("dataset", dataset),
		confirmFact("recursive", recursive),
		confirmFact("datasets covered", len(covered)),
		confirmFact("naming schema", schema),
	}
	if len(after) > 0 {
		facts = append(facts, confirmFact("replications started after", strings.Join(ids, ", ")))
	}

	return &writePlan{
		Tool:         "run_snapshot_task",
		Method:       snapshotTaskRunMethod,
		CallParams:   []interface{}{id},
		ParamNames:   []string{"id"},
		TargetName:   dataset,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Run periodic snapshot task %d on '%s' now (%d dataset(s))", id, dataset, len(covered)),
			Operation:   "snapshot",
			Target:      dataset,
			Details: map[string]interface{}{
				"naming_schema": schema,
				"recursive":     recursive,
			},
		}},
		Warnings:       warnings,
		Blocked:        blocked,
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Run periodic snapshot task %d on %s now", id, dataset), facts...),
		Result: map[string]interface{}{
			"covered":     covered,
			"after":       ids,
			"allow_empty": allowEmpty,
			"before":      readSnapshotTaskState(task),
		},
	}, nil
}

func (r *Registry) handleRunSnapshotTaskWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planRunSnapshotTask(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeRunSnapshotTask(ctx, client, args, plan)
}

// snapshotTaskState is the part of a periodic snapshot task's zettarepl
// state that tells one run from the next.
type snapshotTaskState struct {
	State        string
	DatetimeMS   float64 // state.datetime in ms since the epoch; 0 if unset
	LastSnapshot string
	Error        string
}

func readSnapshotTaskState(task map[string]interface{}) snapshotTaskState {
	st, _ := task["state"].(map[string]interface{})
	out := snapshotTaskState{}
	out.State, _ = st["state"].(string)
	out.LastSnapshot, _ = st["last_snapshot"].(string)
	out.Error, _ = st["error"].(string)
	switch d := st["datetime"].(type) {
	case map[string]interface{}:
		out.DatetimeMS, _ = d["$date"].(float64)
	case float64:
		out.DatetimeMS = d
	}
	return out
}

// finishedAfter reports whether cur is the end of a run that started after
// before was read: zettarepl sets state.datetime on every RUNNING, FINISHED
// and ERROR update, so a newer datetime with a final state is a new run.
func (cur snapshotTaskState) finishedAfter(before snapshotTaskState) bool {
	if cur.State != "FINISHED" && cur.State != "ERROR" {
		return false
	}
	return cur.DatetimeMS > before.DatetimeMS
}

// waitForSnapshotTaskRun polls pool.snapshottask.query for task id until a
// run newer than before has finished, wait passes or ctx ends. It returns
// the last state read, whether the run finished, and the last poll error.
func waitForSnapshotTaskRun(ctx context.Context, client *truenas.Client, id int, before snapshotTaskState, wait, every time.Duration) (map[string]interface{}, bool, error) {
	deadline := time.Now().Add(wait)
	var last map[string]interface{}
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return last, false, ctx.Err()
		case <-time.After(every):
		}
		raw, err := client.Call(ctx, "pool.snapshottask.query", []interface{}{[]interface{}{"id", "=", id}})
		task, err := queryOneTask(raw, err, "periodic snapshot task", id)
		if err == nil {
			last, lastErr = task, nil
			if readSnapshotTaskState(task).finishedAfter(before) {
				return task, true, nil
			}
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			return last, false, lastErr
		}
	}
}

// executeRunSnapshotTask queues the task and follows its own state.
// pool.snapshottask.run is a plain method on 25.10 and 27, not a job: it
// queues the task with zettarepl and returns null, so there is no job id.
func (r *Registry) executeRunSnapshotTask(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	id, _ := intID(plan.CallParams[0])
	before, _ := plan.Result["before"].(snapshotTaskState)
	if _, err := callPlan(ctx, client, plan); err != nil {
		return "", fmt.Errorf("failed to run periodic snapshot task %d: %w", id, err)
	}

	response := map[string]interface{}{
		"snapshot_task_id": id,
		"dataset":          plan.TargetName,
		"queued":           true,
	}
	task, finished, waitErr := waitForSnapshotTaskRun(ctx, client, id, before, snapshotTaskRunWait, snapshotTaskRunPoll)
	if !finished {
		response["state"] = "QUEUED"
		msg := fmt.Sprintf("Periodic snapshot task %d was queued, but its run was not seen to finish within %s", id, snapshotTaskRunWait)
		switch {
		case ctx.Err() != nil:
			msg = fmt.Sprintf("Periodic snapshot task %d was queued; this call ended before its run was seen to finish", id)
		case waitErr != nil:
			msg += fmt.Sprintf(" (the task could not be read: %s)", boundedErr(waitErr))
		}
		if task != nil {
			if st := readSnapshotTaskState(task).State; st != "" {
				response["task_state"] = st
			}
		}
		response["message"] = msg + fmt.Sprintf(". It may still be running: check query_snapshot_tasks task_id %d (last_run and last_snapshot) before running it again", id)
		return marshalJSON(response)
	}

	cur := readSnapshotTaskState(task)
	if cur.State == "ERROR" {
		msg, _ := tasks.BoundText(cur.Error, maxTaskStateText)
		if msg == "" {
			msg = "no error text was recorded"
		}
		return "", fmt.Errorf("periodic snapshot task %d failed: %s", id, msg)
	}

	response["state"] = "FINISHED"
	covered, _ := plan.Result["covered"].([]string)
	_, snapName, ok := lastSnapshotName(cur.LastSnapshot)
	if !ok {
		response["message"] = fmt.Sprintf("The run finished, but the task records no last snapshot; check query_snapshot_tasks task_id %d", id)
		return marshalJSON(response)
	}
	last := cur.LastSnapshot
	if last == before.LastSnapshot {
		// zettarepl treats an already-existing snapshot name as success
		// only during a DST step-back (zettarepl.py:178 on 25.10 and 27).
		// Otherwise it reports ERROR, and the tool already returns a
		// failure above. A FINISHED run whose last_snapshot did not
		// change is that DST case: the name already existed and no new
		// snapshot was created.
		response["snapshot_name"] = snapName
		response["new_snapshot"] = false
		msg := fmt.Sprintf("Periodic snapshot task %d ran, but its snapshot %s already existed (zettarepl treats an existing name as success only during a DST step-back), so this run created no new snapshot", id, last)
		if after, _ := plan.Result["after"].([]string); len(after) > 0 {
			response["replications_started_after"] = after
			msg += fmt.Sprintf(". Replication task(s) %s still start after it", strings.Join(after, ", "))
		}
		response["message"] = msg
		return marshalJSON(response)
	}
	response["new_snapshot"] = true
	response["snapshot_name"] = snapName
	have, err := datasetsWithSnapshot(ctx, client, covered, snapName)
	if err != nil {
		response["message"] = fmt.Sprintf("The run finished and recorded %s, but the snapshots could not be read back (%s)", last, boundedErr(err))
		return marshalJSON(response)
	}
	created := []string{}
	missing := []string{}
	for _, ds := range covered {
		if have[ds] {
			created = append(created, ds+"@"+snapName)
		} else {
			missing = append(missing, ds)
		}
	}
	response["snapshots_created_count"] = len(created)
	response["snapshots_created"] = capStrings(created, maxCoverageDatasets)
	if len(created) > maxCoverageDatasets {
		response["snapshots_created_truncated"] = true
	}
	msg := fmt.Sprintf("Periodic snapshot task %d created @%s on %d of %d covered dataset(s), read back from pool.snapshot.query", id, snapName, len(created), len(covered))
	if len(missing) > 0 {
		response["datasets_without_snapshot"] = capStrings(missing, maxCoverageDatasets)
		if allow, _ := plan.Result["allow_empty"].(bool); !allow {
			msg += fmt.Sprintf("; %d dataset(s) have none (allow_empty is off, so unchanged datasets get no snapshot)", len(missing))
		} else {
			msg += fmt.Sprintf("; %d dataset(s) have none", len(missing))
		}
	}
	if len(created) == 0 {
		response["warning"] = "No snapshot from this run was found on any covered dataset"
	}
	if after, _ := plan.Result["after"].([]string); len(after) > 0 {
		response["replications_started_after"] = after
		msg += fmt.Sprintf(". Replication task(s) %s now ship it; follow them with query_replications or query_jobs (method replication.run)", strings.Join(after, ", "))
	}
	response["message"] = msg
	return marshalJSON(response)
}

// ---- run_replication ----

var strftimeTokens = map[byte]string{
	'Y': `\d{4}`, 'y': `\d{2}`, 'm': `\d{2}`, 'd': `\d{2}`, 'H': `\d{2}`, 'M': `\d{2}`, 'S': `\d{2}`,
	'j': `\d{3}`, 'z': `[+-]\d{4}`, 's': `\d+`, '%': `%`,
}

// namingSchemaRegexp turns a zettarepl naming schema (strftime) into a
// regular expression for snapshot names. Unknown tokens match loosely.
func namingSchemaRegexp(schema string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(schema); i++ {
		if schema[i] == '%' && i+1 < len(schema) {
			i++
			if tok, ok := strftimeTokens[schema[i]]; ok {
				b.WriteString(tok)
			} else {
				b.WriteString(".+?")
			}
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(schema[i])))
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// replicationNameMatcher returns a matcher for the snapshot names a PUSH
// replication task sends, or nil when it has no usable rule.
func replicationNameMatcher(task map[string]interface{}) func(string) bool {
	if re, _ := task["name_regex"].(string); re != "" {
		compiled, err := regexp.Compile("^(?:" + re + ")$")
		if err != nil {
			return nil
		}
		return compiled.MatchString
	}
	var schemas []*regexp.Regexp
	for _, s := range append(stringList(task["also_include_naming_schema"]), stringList(task["naming_schema"])...) {
		schemas = append(schemas, namingSchemaRegexp(s))
	}
	list, _ := task["periodic_snapshot_tasks"].([]interface{})
	for _, item := range list {
		m, _ := item.(map[string]interface{})
		if s, _ := m["naming_schema"].(string); s != "" {
			schemas = append(schemas, namingSchemaRegexp(s))
		}
	}
	if len(schemas) == 0 {
		return nil
	}
	return func(name string) bool {
		for _, re := range schemas {
			if re.MatchString(name) {
				return true
			}
		}
		return false
	}
}

func snapshotTxg(s map[string]interface{}) (uint64, bool) {
	txg, _ := s["createtxg"].(string)
	if txg == "" {
		if props, ok := s["properties"].(map[string]interface{}); ok {
			if p, ok := props["createtxg"].(map[string]interface{}); ok {
				txg, _ = p["value"].(string)
			}
		}
	}
	n, err := strconv.ParseUint(txg, 10, 64)
	return n, err == nil
}

// replicationSendEstimate estimates, from the first source dataset only,
// whether a PUSH run would send anything: snapshots matching the task's
// naming rules that are newer than the last snapshot sent.
func replicationSendEstimate(ctx context.Context, client *truenas.Client, task map[string]interface{}) map[string]interface{} {
	est := map[string]interface{}{
		"note": "Estimate from one source dataset before the run; the run decides per dataset (children included) and may send more or less",
	}
	if dir, _ := task["direction"].(string); dir != "PUSH" {
		est["would_send"] = nil
		est["reason"] = "the source is on the remote system, so new snapshots cannot be counted here"
		return est
	}
	sources := stringList(task["source_datasets"])
	match := replicationNameMatcher(task)
	if len(sources) == 0 || match == nil {
		est["would_send"] = nil
		est["reason"] = "the task has no naming schema or name pattern to count snapshots with"
		return est
	}
	st, _ := task["state"].(map[string]interface{})
	last, _ := st["last_snapshot"].(string)
	lastDataset, lastName, hasLast := lastSnapshotName(last)
	dataset := sources[0]
	if hasLast {
		dataset = lastDataset
	}
	est["dataset"] = dataset
	raw, err := client.Call(ctx, "pool.snapshot.query", []interface{}{
		[]interface{}{"dataset", "=", dataset},
	}, map[string]interface{}{
		"extra": map[string]interface{}{"properties": []interface{}{"createtxg"}},
		"limit": maxEstimateSnapshots,
	})
	var snaps []map[string]interface{}
	if err == nil {
		err = json.Unmarshal(raw, &snaps)
	}
	if err != nil {
		est["would_send"] = nil
		est["reason"] = "source snapshots could not be listed: " + boundedErr(err)
		return est
	}
	var lastTxg uint64
	if hasLast {
		found := false
		for _, s := range snaps {
			if s["snapshot_name"] == lastName {
				lastTxg, found = snapshotTxg(s)
			}
		}
		if !found {
			est["would_send"] = nil
			est["reason"] = fmt.Sprintf("the last snapshot sent (%s) is no longer on the source; the run looks for another common snapshot", last)
			return est
		}
	}
	count := 0
	var newest string
	var newestTxg uint64
	for _, s := range snaps {
		name, _ := s["snapshot_name"].(string)
		txg, ok := snapshotTxg(s)
		if !ok || !match(name) || (hasLast && txg <= lastTxg) {
			continue
		}
		count++
		if txg > newestTxg {
			newest, newestTxg = name, txg
		}
	}
	est["new_snapshots"] = count
	est["would_send"] = count > 0
	if newest != "" {
		est["newest"] = dataset + "@" + newest
	}
	switch {
	case !hasLast && count > 0:
		est["summary"] = fmt.Sprintf("never replicated: an initial send of up to %d snapshot(s) of %s", count, dataset)
	case count > 0:
		est["summary"] = fmt.Sprintf("%d snapshot(s) of %s newer than the last one sent (%s)", count, dataset, last)
	default:
		est["summary"] = fmt.Sprintf("no new snapshots on %s; the run would most likely transfer nothing", dataset)
	}
	if len(snaps) >= maxEstimateSnapshots {
		est["note"] = est["note"].(string) + fmt.Sprintf("; only the first %d source snapshots were checked", maxEstimateSnapshots)
	}
	return est
}

func planRunReplication(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	id, err := requiredTaskID(args)
	if err != nil {
		return nil, err
	}
	raw, err := client.Call(ctx, "replication.query", []interface{}{[]interface{}{"id", "=", id}})
	task, err := queryOneTask(raw, err, "replication task", id)
	if err != nil {
		return nil, err
	}
	loc, _, _ := nasTimezone(ctx, client)
	summary := simplifyReplication(task, loc)
	name, _ := task["name"].(string)
	direction, _ := task["direction"].(string)
	sources := strings.Join(stringList(task["source_datasets"]), ", ")
	target, _ := task["target_dataset"].(string)
	retention, _ := task["retention_policy"].(string)
	allowScratch, _ := task["allow_from_scratch"].(bool)

	var blocked []string
	st, _ := task["state"].(map[string]interface{})
	switch st["state"] {
	case "RUNNING":
		blocked = append(blocked, fmt.Sprintf("replication task %d is already running", id))
	case "HOLD":
		reason, _ := st["reason"].(string)
		reason, _ = tasks.BoundText(reason, maxTaskStateText)
		blocked = append(blocked, fmt.Sprintf("replication task %d is on hold: %s", id, reason))
	}

	var warnings []string
	bound, _ := summary["bound_snapshot_tasks"].([]map[string]interface{})
	boundIDs := make([]string, 0, len(bound))
	for _, b := range bound {
		boundIDs = append(boundIDs, fmt.Sprint(b["id"]))
	}
	if len(boundIDs) > 0 {
		warnings = append(warnings, fmt.Sprintf("Bound to periodic snapshot task(s) %s: this run ships snapshots that already exist and creates none. Run run_snapshot_task first to include current data", strings.Join(boundIDs, ", ")))
	}
	if enabled, _ := task["enabled"].(bool); !enabled {
		switch disabledReplicationRuns(ctx, client) {
		case "refused":
			blocked = append(blocked, fmt.Sprintf("replication task %d is disabled; TrueNAS 25.10 does not run a disabled replication task (enable it first, or upgrade: 26 and later run it)", id))
		case "runs":
			warnings = append(warnings, "The task is disabled; on this TrueNAS version (26 or later) a manual run still runs it")
		default:
			warnings = append(warnings, "The task is disabled: TrueNAS 25.10 refuses to run it, while 26 and later still run it manually (the TrueNAS version is unknown here)")
		}
	}
	switch retention {
	case "SOURCE":
		warnings = append(warnings, "Retention SOURCE: after the transfer, target snapshots that no longer exist on the source are deleted")
	case "CUSTOM":
		warnings = append(warnings, fmt.Sprintf("Retention CUSTOM: after the transfer, target snapshots older than %v %v(s) are deleted", task["lifetime_value"], strings.ToLower(fmt.Sprint(task["lifetime_unit"]))))
	}
	if allowScratch {
		warnings = append(warnings, "allow_from_scratch is on: if source and target share no snapshot, the target dataset's data is destroyed and replicated from scratch")
	}
	warnings = append(warnings, "A replication can run for hours; follow it with tasks_get instead of waiting")

	estimate := map[string]interface{}{}
	if len(blocked) == 0 {
		estimate = replicationSendEstimate(ctx, client, task)
	}

	facts := []string{
		confirmFact("direction", direction),
		confirmFact("source", sources),
		confirmFact("target", target),
		confirmFact("transport", task["transport"]),
		confirmFact("retention", retention),
		confirmFact("allow_from_scratch", allowScratch),
	}
	if s, ok := estimate["summary"].(string); ok {
		facts = append(facts, confirmFact("estimate", s))
	}
	if len(boundIDs) > 0 {
		facts = append(facts, confirmFact("bound snapshot tasks (ships existing snapshots only)", strings.Join(boundIDs, ", ")))
	}

	return &writePlan{
		Tool:       "run_replication",
		Method:     replicationRunMethod,
		CallParams: []interface{}{id},
		ParamNames: []string{"id"},
		JobMethod:  replicationRunMethod,
		TargetName: name,
		CurrentState: map[string]interface{}{
			"replication": summary,
			"estimate":    estimate,
		},
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Run replication task %d ('%s') now: %s %s -> %s", id, name, direction, sources, target),
			Operation:   "replicate",
			Target:      target,
			Details:     map[string]interface{}{"estimate": estimate},
		}},
		Warnings:       warnings,
		Blocked:        blocked,
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Run replication task %d (%s) now", id, name), facts...),
		Result: map[string]interface{}{
			"sources":     stringList(task["source_datasets"]),
			"target":      target,
			"bound_tasks": boundIDs,
		},
	}, nil
}

func (r *Registry) handleRunReplicationWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planRunReplication(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeRunReplication(ctx, client, args, plan)
}

// replicationProgressPattern matches the job progress text middleware's
// zettarepl plugin sets when a snapshot send starts or progresses
// (_set_replication_task_progress, the same on 25.10 and 27):
//
//	Sending {sent+1} of {total}: {dataset}@{snapshot}[ ({bytes} / {bytes})][ [total {dst} of {max}]]
//
// sent and total come from zettarepl's GlobalReplicationContext, which is
// one per replication task and sums over every source dataset and child
// of the run, so they are task-wide counts, not per-dataset ones.
var replicationProgressPattern = regexp.MustCompile(`^Sending (\d+) of (\d+): (\S+)`)

// replicationSizePattern is the optional "[total {dst} of {max}]" suffix.
// dst is the used size of the target of the dataset being replicated;
// max is max(source used, target used). Middleware uses the max so the
// "of" value is never smaller than the target. This is not the amount of
// data sent.
var replicationSizePattern = regexp.MustCompile(`\[total (.+?) of (.+?)\]$`)

// replicationOutcome reads a finished replication.run job. middleware sets
// the progress text only when a snapshot send starts (and updates it while
// it runs), so a successful job with no progress text sent nothing. When
// the text has the "Sending N of M" form, N is the number of snapshot sends
// that ran, counted across all source datasets and children: the last one
// started was the N-th, and the job only succeeds once it completed. M is
// the planned total, which can be higher than N when an attempt failed and
// was retried. Any other text is reported as is, with no claim about what
// was transferred.
func replicationOutcome(sources []string) func(job map[string]interface{}) (string, interface{}) {
	where := strings.Join(sources, ", ")
	if where == "" {
		where = "the source"
	}
	return func(job map[string]interface{}) (string, interface{}) {
		progress, _ := job["progress"].(map[string]interface{})
		rawDesc, present := progress["description"]
		if !present || rawDesc == nil || rawDesc == "" {
			return fmt.Sprintf("Replication finished: no new snapshots on %s; nothing transferred", where),
				map[string]interface{}{"transferred": false, "snapshots_sent": 0}
		}
		desc, isText := rawDesc.(string)
		m := replicationProgressPattern.FindStringSubmatch(desc)
		if !isText || m == nil {
			text := desc
			if !isText {
				text = fmt.Sprint(rawDesc)
			}
			text, _ = tasks.BoundText(text, maxTaskStateText)
			return fmt.Sprintf("Replication finished; could not determine what was transferred (progress text: %q)", text),
				map[string]interface{}{"transferred": nil, "progress_description": text}
		}
		sent, err1 := strconv.Atoi(m[1])
		planned, err2 := strconv.Atoi(m[2])
		if err1 != nil || err2 != nil {
			text, _ := tasks.BoundText(desc, maxTaskStateText)
			return fmt.Sprintf("Replication finished; could not determine what was transferred (progress text: %q)", text),
				map[string]interface{}{"transferred": nil, "progress_description": text}
		}
		lastSent, _ := tasks.BoundText(m[3], maxTaskStateText)
		out := map[string]interface{}{
			"transferred":        true,
			"snapshots_sent":     sent,
			"snapshots_planned":  planned,
			"last_snapshot_sent": lastSent,
			"count_note":         "Counts are snapshot sends across all source datasets and children of the run; with a name pattern (name_regex) one incremental send can carry intermediate snapshots too",
		}
		msg := fmt.Sprintf("Replication finished: sent %d snapshot(s) across all source datasets and children, the last %s", sent, lastSent)
		if planned > sent {
			msg += fmt.Sprintf(" (%d were planned; a planned count above the sent count usually means an attempt failed and was retried)", planned)
		}
		if t := replicationSizePattern.FindStringSubmatch(desc); t != nil {
			out["last_source_size"] = map[string]interface{}{"target_used": t[1], "max_used": t[2]}
		}
		return msg, out
	}
}

func (r *Registry) executeRunReplication(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	id, _ := intID(plan.CallParams[0])
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to run replication task %d: %w", id, err)
	}
	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("replication task %d returned no job id; nothing was confirmed started", id)
	}
	sources, _ := plan.Result["sources"].([]string)
	response := map[string]interface{}{
		"replication_task_id": id,
		"name":                plan.TargetName,
		"started":             true,
		"job_id":              jobID,
	}
	msg := fmt.Sprintf("Replication task %d started (job %d). ", id, jobID)
	if r.taskManager != nil {
		task, err := r.taskManager.CreateJobTask(ctx, "run_replication", args, jobID, replicationTaskTTL,
			tasks.WithExpectedMethod(replicationRunMethod),
			tasks.WithJobOutcome(replicationOutcome(sources)))
		if err != nil {
			return "", fmt.Errorf("failed to create task: %w", err)
		}
		response["task_id"] = task.TaskID
		response["task_status"] = task.Status
		response["poll_interval"] = task.PollInterval
		msg += fmt.Sprintf("Follow it with tasks_get task_id %s: when it finishes, the task says whether anything was transferred and how many snapshots were sent. ", task.TaskID)
	} else {
		msg += fmt.Sprintf("Follow it with query_jobs (method %s) or query_replications task_id %d. ", replicationRunMethod, id)
	}
	if bound, _ := plan.Result["bound_tasks"].([]string); len(bound) > 0 {
		response["bound_snapshot_tasks"] = bound
		msg += fmt.Sprintf("The task is bound to periodic snapshot task(s) %s: it ships existing snapshots and creates none.", strings.Join(bound, ", "))
	}
	response["message"] = strings.TrimSpace(msg)
	return marshalJSON(response)
}

// disabledReplicationRuns says what replication.run does with a disabled
// task on the connected TrueNAS: 25.10 refuses it ("Task is not enabled"),
// while middleware allows running disabled replications from 26.0. It
// returns "refused", "runs", or "" when the version is not known.
func disabledReplicationRuns(ctx context.Context, client *truenas.Client) string {
	v, err, ok := client.SessionVersion(ctx)
	if !ok || err != nil || v.IsZero() {
		return ""
	}
	if v.Less(truenas.Version{Major: 26}) {
		return "refused"
	}
	return "runs"
}

// ---- create_snapshot ----

func planCreateSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	dataset, _ := args["dataset"].(string)
	dataset = strings.TrimSpace(dataset)
	name, _ := args["name"].(string)
	recursive, _ := args["recursive"].(bool)
	if dataset == "" {
		return nil, fmt.Errorf("dataset is required")
	}
	if strings.Contains(dataset, "@") {
		return nil, fmt.Errorf("dataset must be a dataset name, without '@'; put the snapshot name in 'name'")
	}
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if !snapshotNamePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid snapshot name: use letters, digits, '_', '-', '.' and ':' only")
	}
	if len(dataset)+1+len(name) > maxZFSNameLen {
		return nil, fmt.Errorf("snapshot name too long: '%s@%s' exceeds %d characters", dataset, name, maxZFSNameLen)
	}

	covered, err := taskDatasets(ctx, client, map[string]interface{}{"dataset": dataset, "recursive": recursive})
	if err != nil {
		return nil, fmt.Errorf("failed to look up dataset: %w", err)
	}
	if len(covered) == 0 {
		return nil, fmt.Errorf("dataset '%s' not found", dataset)
	}

	var blocked []string
	existing, err := datasetsWithSnapshot(ctx, client, covered, name)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing snapshots: %w", err)
	}
	if len(existing) > 0 {
		blocked = append(blocked, fmt.Sprintf("a snapshot named '%s' already exists on %d of the dataset(s) it would cover (for example %s)", name, len(existing), firstKey(existing, covered)))
	}

	full := dataset + "@" + name
	params := map[string]interface{}{"dataset": dataset, "name": name, "recursive": recursive}
	return &writePlan{
		Tool:        "create_snapshot",
		Method:      snapshotCreateMethod,
		CallParams:  []interface{}{params},
		Description: fmt.Sprintf("Create snapshot '%s' on %d dataset(s)", full, len(covered)),
		Operation:   "create",
		TargetName:  full,
		CurrentState: map[string]interface{}{
			"dataset":             dataset,
			"recursive":           recursive,
			"datasets_to_capture": len(covered),
			"datasets":            capStrings(covered, maxCoverageDatasets),
		},
		Warnings: append([]string{
			"The snapshot keeps the blocks it references: space they use is not freed while it exists",
		}, snapshotTaskInteractionWarnings(ctx, client, covered, name)...),
		Blocked: blocked,
		Result:  map[string]interface{}{"covered": covered},
	}, nil
}

// datasetUnder reports whether ds is root or, when recursive, one of its
// descendants that is not excluded (zettarepl's own rule: a child of an
// excluded dataset is excluded too).
func datasetUnder(ds, root string, recursive bool, exclude []string) bool {
	if ds == root {
		return true
	}
	if !recursive || !strings.HasPrefix(ds, root+"/") {
		return false
	}
	for _, e := range exclude {
		if ds == e || strings.HasPrefix(ds, e+"/") {
			return false
		}
	}
	return true
}

// snapshotTaskInteractionWarnings says how existing tasks may act on a
// manual snapshot named name on the covered datasets: a periodic snapshot
// task whose naming schema matches it may remove it by retention, a push
// replication whose naming rules match it may ship it, and a replication
// whose target is on this system may fail its next incremental receive
// because of it.
func snapshotTaskInteractionWarnings(ctx context.Context, client *truenas.Client, covered []string, name string) []string {
	covers := func(root string, recursive bool, exclude []string) bool {
		for _, ds := range covered {
			if datasetUnder(ds, root, recursive, exclude) {
				return true
			}
		}
		return false
	}
	var warnings []string
	if raw, err := client.Call(ctx, "pool.snapshottask.query", []interface{}{}, map[string]interface{}{"order_by": []string{"id"}}); err != nil {
		warnings = append(warnings, "Could not check periodic snapshot tasks (pool.snapshottask.query failed); if the name matches a task's naming schema, that task's retention may remove the snapshot")
	} else {
		var rows []map[string]interface{}
		if json.Unmarshal(raw, &rows) == nil {
			var ids []string
			for _, row := range rows {
				ds, _ := row["dataset"].(string)
				rec, _ := row["recursive"].(bool)
				schema, _ := row["naming_schema"].(string)
				if schema == "" || !covers(ds, rec, stringList(row["exclude"])) || !namingSchemaRegexp(schema).MatchString(name) {
					continue
				}
				ids = append(ids, fmt.Sprint(row["id"]))
			}
			if len(ids) > 0 {
				warnings = append(warnings, fmt.Sprintf("The name matches the naming schema of periodic snapshot task(s) %s on this dataset or a parent: it may be removed by that task's retention once it is older than the task's lifetime, and replications of that task may ship it", strings.Join(ids, ", ")))
			} else {
				warnings = append(warnings, "No retention applies: the snapshot stays until it is deleted")
			}
		}
	}
	raw, err := client.Call(ctx, "replication.query", []interface{}{}, map[string]interface{}{"order_by": []string{"id"}})
	var rows []map[string]interface{}
	if err != nil || json.Unmarshal(raw, &rows) != nil {
		return append(warnings, "Could not check replication tasks (replication.query failed); a manual snapshot on a replication target can make its next incremental receive fail")
	}
	var ships, targets []string
	for _, row := range rows {
		id := fmt.Sprint(row["id"])
		dir, _ := row["direction"].(string)
		transport, _ := row["transport"].(string)
		rec, _ := row["recursive"].(bool)
		exclude := stringList(row["exclude"])
		if dir == "PUSH" {
			if match := replicationNameMatcher(row); match != nil && match(name) {
				for _, src := range stringList(row["source_datasets"]) {
					if covers(src, rec, exclude) {
						ships = append(ships, id)
						break
					}
				}
			}
		}
		// The target is on this system for a PULL, or a LOCAL push.
		if target, _ := row["target_dataset"].(string); target != "" && (dir == "PULL" || transport == "LOCAL") && covers(target, rec, nil) {
			targets = append(targets, id)
		}
	}
	if len(ships) > 0 {
		warnings = append(warnings, fmt.Sprintf("The name matches the naming rules of push replication task(s) %s, whose source includes this dataset: their next run may ship it", strings.Join(ships, ", ")))
	}
	if len(targets) > 0 {
		warnings = append(warnings, fmt.Sprintf("This dataset is the target (or inside the target) of replication task(s) %s: a manual snapshot there can make the next incremental receive fail until it is removed, and the replication's retention may delete it", strings.Join(targets, ", ")))
	}
	return warnings
}

// firstKey returns the first of order that is set in m.
func firstKey(m map[string]bool, order []string) string {
	for _, k := range order {
		if m[k] {
			return k
		}
	}
	return ""
}

func (r *Registry) handleCreateSnapshotWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planCreateSnapshot(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeCreateSnapshot(ctx, client, plan)
}

func executeCreateSnapshot(ctx context.Context, client *truenas.Client, plan *writePlan) (string, error) {
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to create snapshot: %w", err)
	}
	var entry map[string]interface{}
	if err := json.Unmarshal(result, &entry); err != nil {
		return "", fmt.Errorf("snapshot create returned an unexpected result: %w", err)
	}
	params, _ := plan.CallParams[0].(map[string]interface{})
	name, _ := entry["name"].(string)
	if name == "" {
		name = plan.TargetName
	}
	snapName, _ := params["name"].(string)
	if _, n, ok := lastSnapshotName(name); ok {
		snapName = n
	}
	response := map[string]interface{}{
		"created":   true,
		"snapshot":  name,
		"dataset":   params["dataset"],
		"recursive": params["recursive"],
	}
	if txg, ok := entry["createtxg"]; ok {
		response["createtxg"] = txg
	}
	msg := fmt.Sprintf("Created snapshot %s", name)
	if rec, _ := params["recursive"].(bool); rec {
		covered, _ := plan.Result["covered"].([]string)
		have, err := datasetsWithSnapshot(ctx, client, covered, snapName)
		if err != nil {
			response["note"] = "The recursive snapshot was created, but its datasets could not be read back: " + boundedErr(err)
		} else {
			var created []string
			for _, ds := range covered {
				if have[ds] {
					created = append(created, ds+"@"+snapName)
				}
			}
			response["snapshots_created_count"] = len(created)
			response["snapshots_created"] = capStrings(created, maxCoverageDatasets)
			msg += fmt.Sprintf(" on %d dataset(s)", len(created))
		}
	}
	response["message"] = msg
	return marshalJSON(response)
}

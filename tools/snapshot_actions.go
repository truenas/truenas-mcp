package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	snapshotCloneMethod   = "pool.snapshot.clone"
	snapshotHoldMethod    = "pool.snapshot.hold"
	snapshotReleaseMethod = "pool.snapshot.release"
)

// truenasHoldTag is the only tag pool.snapshot.hold places (25.10 and 27).
const truenasHoldTag = "truenas"

func (r *Registry) registerSnapshotActionTools() {
	snapshotSchema := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	dryRunSchema := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc, "default": false}
	}

	r.tools["clone_snapshot"] = Tool{
		Definition: mcp.Tool{
			Name:        "clone_snapshot",
			Description: "Clone a ZFS snapshot to a new dataset, for example to recover files from a snapshot without rolling back. The clone is writable, starts out sharing all its blocks with the snapshot and is mounted under its parent's mountpoint. While the clone exists the snapshot cannot be deleted. The target must be a new dataset in the same pool as the snapshot, below the pool root, whose parent already exists and is mounted under /mnt/<pool>. Refused if the snapshot does not exist or the target already exists. Use dry_run to preview the source, target and mountpoint.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"snapshot": snapshotSchema("Required: Full snapshot name to clone, 'dataset@name' (e.g. 'tank/data@manual-1')"),
					"target_dataset": map[string]interface{}{
						"type":        "string",
						"description": "Required: Name of the new dataset to create, in the same pool as the snapshot (e.g. 'tank/restore/data'). A dataset name, not a /mnt path; its parent must already exist",
					},
					"dry_run": dryRunSchema("Optional: Preview without cloning (default: false)"),
				},
				"required": []string{"snapshot", "target_dataset"},
			},
		},
		Handler: handleCloneSnapshot,
		Plan:    planCloneSnapshot,
	}

	r.tools["hold_snapshot"] = Tool{
		Definition: mcp.Tool{
			Name:        "hold_snapshot",
			Description: "Place a hold on a ZFS snapshot so it cannot be deleted, manually or by periodic snapshot task or replication retention, until the hold is released with release_snapshot. TrueNAS adds the hold tag 'truenas'. Only the named snapshot is held, not snapshots of child datasets. If the snapshot already has the 'truenas' hold, nothing changes and the call succeeds. The preview shows the snapshot and its current holds. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"snapshot": snapshotSchema("Required: Full snapshot name to hold, 'dataset@name' (e.g. 'tank/data@manual-1')"),
					"dry_run":  dryRunSchema("Optional: Preview without holding (default: false)"),
				},
				"required": []string{"snapshot"},
			},
		},
		Handler: handleHoldSnapshot,
		Plan:    planHoldSnapshot,
	}

	r.tools["release_snapshot"] = Tool{
		Definition: mcp.Tool{
			Name:        "release_snapshot",
			Description: "Release the 'truenas' hold that hold_snapshot places on a ZFS snapshot. Afterwards the snapshot is deletable again: a manual delete, or periodic snapshot task or replication retention, can remove it. Only the named snapshot is released, not snapshots of child datasets. TrueNAS releases every hold on a snapshot at once, so this is refused if the snapshot has any other hold (for example one set with 'zfs hold' or by a backup tool), or if its hold count cannot be read; such holds must be managed outside this tool. Also refused if the snapshot has no holds. The preview lists the hold and any retention that schedules the snapshot's removal. Destructive: needs confirmation.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"snapshot": snapshotSchema("Required: Full snapshot name to release, 'dataset@name' (e.g. 'tank/data@manual-1')"),
					"dry_run":  dryRunSchema("Optional: Preview without releasing (default: false)"),
				},
				"required": []string{"snapshot"},
			},
		},
		Handler: handleReleaseSnapshot,
		Plan:    planReleaseSnapshot,
		Execute: executeReleaseSnapshot,
	}
}

// parseSnapshotID validates a full snapshot name (dataset@name) and returns
// its dataset and snapshot name. The dataset may be a pool root.
func parseSnapshotID(raw string) (string, string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", "", fmt.Errorf("snapshot is required")
	}
	dataset, name, ok := strings.Cut(id, "@")
	if !ok || dataset == "" || strings.Contains(name, "@") {
		return "", "", fmt.Errorf("snapshot must be a full snapshot name, 'dataset@name' (e.g. 'tank/data@manual-1')")
	}
	if datasetNameInvalidChars.MatchString(dataset) || strings.HasPrefix(dataset, "/") || strings.HasSuffix(dataset, "/") || strings.Contains(dataset, "//") {
		return "", "", fmt.Errorf("invalid dataset in snapshot name: use a dataset name such as 'tank/data' (letters, digits, '/', '_', '.', '-'), not a path")
	}
	for _, part := range strings.Split(dataset, "/") {
		if part == "." || part == ".." {
			return "", "", fmt.Errorf("invalid dataset in snapshot name: '.' and '..' are not dataset names")
		}
	}
	if !snapshotNamePattern.MatchString(name) {
		return "", "", fmt.Errorf("invalid snapshot name: use letters, digits, '_', '-', '.' and ':' only")
	}
	if len(id) > maxZFSNameLen {
		return "", "", fmt.Errorf("snapshot name too long: '%s' exceeds %d characters", id, maxZFSNameLen)
	}
	return dataset, name, nil
}

// lookupSnapshot returns the snapshot id with its holds, or nil when it does
// not exist. Holds come from extra.holds, which the public model declares
// with only the 'truenas' tag (25.10 may list others, 27 does not), so the
// native userrefs property (the snapshot's total hold count) is requested
// too; see snapshotUserrefs.
func lookupSnapshot(ctx context.Context, client *truenas.Client, id string) (map[string]interface{}, error) {
	raw, err := client.Call(ctx, "pool.snapshot.query",
		[]interface{}{[]interface{}{"id", "=", id}},
		map[string]interface{}{"extra": map[string]interface{}{"holds": true, "properties": []interface{}{"createtxg", "userrefs"}}},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to look up snapshot %s: %w", id, err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected pool.snapshot.query result")
	}
	for _, row := range rows {
		if row["id"] == id || row["name"] == id {
			return row, nil
		}
	}
	return nil, nil
}

// snapshotHoldTags returns the snapshot's hold tags, sorted.
func snapshotHoldTags(snap map[string]interface{}) []string {
	holds, _ := snap["holds"].(map[string]interface{})
	tags := make([]string, 0, len(holds))
	for tag := range holds {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

// snapshotUserrefs returns the snapshot's userrefs property: the number of
// holds ZFS has on it, whatever their tags. It is read from a top-level
// userrefs or from properties.userrefs (parsed, rawvalue or value). ok is
// false when it is missing or not a non-negative whole number.
func snapshotUserrefs(snap map[string]interface{}) (n int, ok bool) {
	v, found := snap["userrefs"]
	if !found {
		props, _ := snap["properties"].(map[string]interface{})
		p, _ := props["userrefs"].(map[string]interface{})
		for _, k := range []string{"parsed", "rawvalue", "value"} {
			if pv, has := p[k]; has && pv != nil {
				v, found = pv, true
				break
			}
		}
	}
	if !found {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		if x >= 0 && x == float64(int(x)) {
			return int(x), true
		}
	case string:
		if i, err := strconv.Atoi(x); err == nil && i >= 0 {
			return i, true
		}
	}
	return 0, false
}

// snapshotCreateTxg returns the snapshot's createtxg as a decimal string,
// or "" when it cannot be read.
func snapshotCreateTxg(snap map[string]interface{}) string {
	if n, ok := snapshotTxg(snap); ok {
		return fmt.Sprint(n)
	}
	return ""
}

func snapshotState(id string, snap map[string]interface{}, tags []string) map[string]interface{} {
	st := map[string]interface{}{
		"snapshot":    id,
		"dataset":     snap["dataset"],
		"createtxg":   snap["createtxg"],
		"holds":       tags,
		"holds_count": len(tags),
	}
	if n, ok := snapshotUserrefs(snap); ok {
		st["userrefs"] = n
	}
	return st
}

// snapshotRetention returns the middleware's retention annotation for id
// (simplified), or nil when nothing schedules its removal. extra.retention
// needs an id filter on 25.10.
func snapshotRetention(ctx context.Context, client *truenas.Client, id string) (interface{}, error) {
	raw, err := client.Call(ctx, "pool.snapshot.query",
		[]interface{}{[]interface{}{"id", "=", id}},
		map[string]interface{}{"extra": map[string]interface{}{"retention": true, "properties": []interface{}{"createtxg"}}},
	)
	if err != nil {
		return nil, err
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected pool.snapshot.query result")
	}
	for _, row := range rows {
		if row["id"] == id || row["name"] == id {
			return simplifySnapshotRetention(row["retention"]), nil
		}
	}
	return nil, nil
}

func requiredSnapshotArg(args map[string]interface{}) (id, dataset, name string, err error) {
	raw, _ := args["snapshot"].(string)
	dataset, name, err = parseSnapshotID(raw)
	if err != nil {
		return "", "", "", err
	}
	return dataset + "@" + name, dataset, name, nil
}

// ---- clone_snapshot ----

// cloneDatasetInfo is what clone_snapshot reads about a dataset.
type cloneDatasetInfo struct {
	Name       string
	Type       string
	Mountpoint string
	Locked     bool
	Encrypted  bool
	EncRoot    string
}

func lookupCloneDataset(ctx context.Context, client *truenas.Client, name string) (*cloneDatasetInfo, error) {
	raw, err := client.Call(ctx, "pool.dataset.query",
		[]interface{}{[]interface{}{"name", "=", name}},
		map[string]interface{}{
			"select": []interface{}{"name", "type", "mountpoint", "locked", "encrypted", "encryption_root"},
			"extra": map[string]interface{}{
				"retrieve_children":   false,
				"properties":          []interface{}{},
				"retrieve_user_props": false,
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to check dataset %s: %w", name, err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("failed to parse dataset query for %s: %w", name, err)
	}
	for _, row := range rows {
		if row["name"] != name {
			continue
		}
		info := &cloneDatasetInfo{Name: name}
		info.Type, _ = row["type"].(string)
		info.Mountpoint, _ = row["mountpoint"].(string)
		info.Locked, _ = row["locked"].(bool)
		info.Encrypted, _ = row["encrypted"].(bool)
		info.EncRoot, _ = row["encryption_root"].(string)
		return info, nil
	}
	return nil, nil
}

// validateCloneTarget checks the target dataset name on its own: a dataset
// name (not a path) inside the source pool, below the pool root, and not in
// a hidden system dataset such as .system.
func validateCloneTarget(target, sourcePool string) error {
	if target == "" {
		return fmt.Errorf("target_dataset is required")
	}
	if strings.HasPrefix(target, "/") {
		return fmt.Errorf("target_dataset must be a dataset name such as '%s/restore', not a path", sourcePool)
	}
	if !strings.Contains(target, "/") {
		return fmt.Errorf("target_dataset '%s' is a pool root; clone into a new dataset inside the pool, such as '%s/restore'", target, sourcePool)
	}
	if err := validateDatasetName(target); err != nil {
		return fmt.Errorf("invalid target_dataset: %w", err)
	}
	if pool := datasetPool(target); pool != sourcePool {
		return fmt.Errorf("target_dataset '%s' is in pool '%s', but the snapshot is in pool '%s'; ZFS clones stay in the snapshot's pool", target, pool, sourcePool)
	}
	for _, part := range strings.Split(target, "/")[1:] {
		if strings.HasPrefix(part, ".") {
			return fmt.Errorf("target_dataset '%s' is inside a hidden dataset ('%s'); TrueNAS manages those itself", target, part)
		}
		if part == "ix-applications" {
			return fmt.Errorf("target_dataset '%s' is inside the legacy apps dataset ('ix-applications'); TrueNAS manages it itself", target)
		}
	}
	return nil
}

// validateCloneSource refuses a snapshot of a hidden system dataset such as
// .system or .ix-virt: a clone would put a writable copy of system-managed
// data at an ordinary path.
func validateCloneSource(dataset string) error {
	for _, part := range strings.Split(dataset, "/")[1:] {
		if strings.HasPrefix(part, ".") {
			return fmt.Errorf("snapshot dataset '%s' is a hidden dataset ('%s') TrueNAS manages itself; cloning its snapshots is not supported", dataset, part)
		}
	}
	return nil
}

// mountUnderPool reports whether mp is /mnt/<pool> or a path below it.
func mountUnderPool(mp, pool string) bool {
	root := "/mnt/" + pool
	return mp != "" && path.Clean(mp) == mp && (mp == root || strings.HasPrefix(mp, root+"/"))
}

func planCloneSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	id, srcDataset, _, err := requiredSnapshotArg(args)
	if err != nil {
		return nil, err
	}
	target, _ := args["target_dataset"].(string)
	target = strings.TrimSpace(target)
	pool := datasetPool(srcDataset)
	if err := validateCloneSource(srcDataset); err != nil {
		return nil, err
	}
	if err := validateCloneTarget(target, pool); err != nil {
		return nil, err
	}

	snap, err := lookupSnapshot(ctx, client, id)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, fmt.Errorf("snapshot '%s' not found", id)
	}
	source, err := lookupCloneDataset(ctx, client, srcDataset)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("dataset '%s' of snapshot '%s' not found", srcDataset, id)
	}
	existing, err := lookupCloneDataset(ctx, client, target)
	if err != nil {
		return nil, err
	}
	parentName := target[:strings.LastIndex(target, "/")]
	parent, err := lookupCloneDataset(ctx, client, parentName)
	if err != nil {
		return nil, err
	}

	var blocked []string
	if existing != nil {
		blocked = append(blocked, fmt.Sprintf("dataset '%s' already exists; choose a new target_dataset", target))
	}
	if source.Locked {
		blocked = append(blocked, fmt.Sprintf("dataset '%s' is locked; unlock it before cloning its snapshot", srcDataset))
	}
	mountpoint := ""
	switch {
	case parent == nil:
		blocked = append(blocked, fmt.Sprintf("parent dataset '%s' does not exist; pool.snapshot.clone does not create parents, so create it first (create_dataset)", parentName))
	case parent.Type != "FILESYSTEM":
		blocked = append(blocked, fmt.Sprintf("parent '%s' is a %s, not a filesystem dataset; a clone needs a filesystem parent", parentName, strings.ToLower(parent.Type)))
	case parent.Locked:
		blocked = append(blocked, fmt.Sprintf("parent dataset '%s' is locked; unlock it first", parentName))
	case !mountUnderPool(parent.Mountpoint, pool):
		where := "has no mountpoint"
		if parent.Mountpoint != "" {
			where = fmt.Sprintf("is mounted at '%s'", parent.Mountpoint)
		}
		blocked = append(blocked, fmt.Sprintf("parent dataset '%s' %s, outside /mnt/%s; the clone would not be mounted under the pool", parentName, where, pool))
	default:
		mountpoint = parent.Mountpoint + target[strings.LastIndex(target, "/"):]
	}

	tags := snapshotHoldTags(snap)
	current := map[string]interface{}{
		"source":         snapshotState(id, snap, tags),
		"source_type":    source.Type,
		"target_dataset": target,
		"target_parent":  parentName,
	}
	if source.Type == "VOLUME" {
		current["target_device"] = "/dev/zvol/" + target
	} else if mountpoint != "" {
		current["target_mountpoint"] = mountpoint
	}

	warnings := []string{
		fmt.Sprintf("While '%s' exists, snapshot '%s' cannot be deleted (delete the clone first); retention that tries to remove the snapshot fails", target, id),
		"The clone starts out sharing all its blocks with the snapshot and uses no extra space until it is changed",
		"Space the snapshot references is not freed while the clone exists",
	}
	if source.Encrypted {
		warnings = append(warnings, fmt.Sprintf("The source is encrypted (encryption root '%s'): the clone uses the same encryption key", source.EncRoot))
	}

	facts := []string{
		confirmFact("source", id),
		confirmFact("target", target),
	}
	if mountpoint != "" {
		facts = append(facts, confirmFact("mountpoint", mountpoint))
	}

	return &writePlan{
		Tool:         "clone_snapshot",
		Method:       snapshotCloneMethod,
		CallParams:   []interface{}{map[string]interface{}{"snapshot": id, "dataset_dst": target}},
		TargetName:   target,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Clone snapshot '%s' to new dataset '%s'", id, target),
			Operation:   "clone",
			Target:      target,
			Details: map[string]interface{}{
				"source":     id,
				"target":     target,
				"mountpoint": mountpoint,
			},
		}},
		Warnings:       warnings,
		Blocked:        blocked,
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Clone %s to %s", id, target), facts...),
		Result: map[string]interface{}{
			"source":     id,
			"mountpoint": mountpoint,
		},
	}, nil
}

func handleCloneSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planCloneSnapshot(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeCloneSnapshot(ctx, client, plan)
}

func executeCloneSnapshot(ctx context.Context, client *truenas.Client, plan *writePlan) (string, error) {
	source, _ := plan.Result["source"].(string)
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to clone snapshot %s to %s: %w", source, plan.TargetName, err)
	}
	var ok bool
	if err := json.Unmarshal(result, &ok); err != nil || !ok {
		return "", fmt.Errorf("pool.snapshot.clone returned an unexpected result; check whether '%s' exists with query_datasets before retrying", plan.TargetName)
	}
	response := map[string]interface{}{
		"cloned":  true,
		"source":  source,
		"dataset": plan.TargetName,
	}
	msg := fmt.Sprintf("Cloned %s to %s", source, plan.TargetName)
	ds, err := lookupCloneDataset(ctx, client, plan.TargetName)
	switch {
	case err != nil:
		response["note"] = "The clone was created, but it could not be read back: " + boundedErr(err)
	case ds == nil:
		response["note"] = fmt.Sprintf("The clone was created, but '%s' was not found when read back", plan.TargetName)
	default:
		response["type"] = ds.Type
		if ds.Mountpoint != "" {
			response["mountpoint"] = ds.Mountpoint
			msg += fmt.Sprintf(", mounted at %s", ds.Mountpoint)
		}
	}
	response["message"] = msg + fmt.Sprintf(". The snapshot cannot be deleted while %s exists", plan.TargetName)
	return marshalJSON(response)
}

// ---- hold_snapshot ----

func planHoldSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	id, _, _, err := requiredSnapshotArg(args)
	if err != nil {
		return nil, err
	}
	snap, err := lookupSnapshot(ctx, client, id)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, fmt.Errorf("snapshot '%s' not found", id)
	}
	tags := snapshotHoldTags(snap)
	held := false
	for _, t := range tags {
		if t == truenasHoldTag {
			held = true
		}
	}

	plan := &writePlan{
		Tool:         "hold_snapshot",
		Method:       snapshotHoldMethod,
		CallParams:   []interface{}{id, map[string]interface{}{"recursive": false}},
		ParamNames:   []string{"id", "options"},
		TargetName:   id,
		CurrentState: snapshotState(id, snap, tags),
		Result:       map[string]interface{}{"already_held": held, "holds": tags},
	}
	if held {
		plan.Actions = []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("No change: snapshot '%s' already has the '%s' hold", id, truenasHoldTag),
			Operation:   "hold",
			Target:      id,
		}}
		plan.Warnings = []string{fmt.Sprintf("Snapshot '%s' is already held (tag '%s'); nothing will be changed", id, truenasHoldTag)}
		return plan, nil
	}
	plan.Actions = []PlannedAction{{
		Step:        1,
		Description: fmt.Sprintf("Hold snapshot '%s' (tag '%s'); child datasets' snapshots are not held", id, truenasHoldTag),
		Operation:   "hold",
		Target:      id,
		Details:     map[string]interface{}{"tag": truenasHoldTag, "recursive": false},
	}}
	plan.Warnings = []string{
		"While held, the snapshot cannot be deleted: a manual delete, and periodic snapshot task or replication retention, fail until the hold is released with release_snapshot",
		"Space the snapshot references is not freed while it is held",
	}
	return plan, nil
}

func handleHoldSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planHoldSnapshot(ctx, client, args)
	if err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeHoldSnapshot(ctx, client, plan)
}

func executeHoldSnapshot(ctx context.Context, client *truenas.Client, plan *writePlan) (string, error) {
	id := plan.TargetName
	if already, _ := plan.Result["already_held"].(bool); already {
		return marshalJSON(map[string]interface{}{
			"snapshot":     id,
			"held":         true,
			"already_held": true,
			"changed":      false,
			"holds":        plan.Result["holds"],
			"message":      fmt.Sprintf("Snapshot %s already has the '%s' hold; nothing was changed", id, truenasHoldTag),
		})
	}
	if _, err := callPlan(ctx, client, plan); err != nil {
		return "", fmt.Errorf("failed to hold snapshot %s: %w", id, err)
	}
	response := map[string]interface{}{
		"snapshot":     id,
		"held":         true,
		"already_held": false,
		"changed":      true,
	}
	msg := fmt.Sprintf("Snapshot %s is held (tag '%s'): it cannot be deleted until the hold is released with release_snapshot", id, truenasHoldTag)
	snap, err := lookupSnapshot(ctx, client, id)
	switch {
	case err != nil:
		response["note"] = "The hold was placed, but the snapshot could not be read back: " + boundedErr(err)
	case snap == nil:
		response["note"] = fmt.Sprintf("The hold was placed, but %s was not found when read back", id)
	default:
		response["holds"] = snapshotHoldTags(snap)
	}
	response["message"] = msg
	return marshalJSON(response)
}

// ---- release_snapshot ----

func planReleaseSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	id, _, _, err := requiredSnapshotArg(args)
	if err != nil {
		return nil, err
	}
	snap, err := lookupSnapshot(ctx, client, id)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, fmt.Errorf("snapshot '%s' not found", id)
	}
	tags := snapshotHoldTags(snap)
	current := snapshotState(id, snap, tags)
	userrefs, refsOK := snapshotUserrefs(snap)
	txg := snapshotCreateTxg(snap)
	// The token is bound to the hold state and identity the preview saw:
	// a hold added, or the snapshot recreated under the same name, after
	// the preview rejects the token.
	bind := map[string]interface{}{"holds": tags, "userrefs": userrefs, "userrefs_ok": refsOK, "createtxg": txg}
	summary := newConfirmSummary(
		fmt.Sprintf("Release the '%s' hold on %s: the snapshot becomes deletable again", truenasHoldTag, id),
		confirmFact("snapshot", id),
	)
	blockedPlan := func(reason string) *writePlan {
		return &writePlan{
			Tool:           "release_snapshot",
			Method:         snapshotReleaseMethod,
			CallParams:     []interface{}{id, map[string]interface{}{"recursive": false}},
			ParamNames:     []string{"id", "options"},
			TargetName:     id,
			CurrentState:   current,
			Blocked:        []string{reason},
			Bind:           bind,
			ConfirmSummary: summary,
		}
	}

	// pool.snapshot.release releases every hold on the snapshot, and the
	// holds list may not show tags other than 'truenas'. Release only
	// when the 'truenas' hold is provably the only one.
	var foreign []string
	for _, t := range tags {
		if t != truenasHoldTag {
			foreign = append(foreign, t)
		}
	}
	const elsewhere = "release_snapshot releases only the '" + truenasHoldTag + "' hold, and TrueNAS would release every hold at once, so other holds must be managed outside this tool where they were set; nothing was released"
	switch {
	case !refsOK:
		return blockedPlan(fmt.Sprintf("could not read the hold count (userrefs) of snapshot '%s', so holds not placed by TrueNAS cannot be ruled out; %s", id, elsewhere)), nil
	case len(foreign) > 0:
		return blockedPlan(fmt.Sprintf("snapshot '%s' has %d hold(s) not placed by TrueNAS (tag(s) %s); %s", id, len(foreign), strings.Join(foreign, ", "), elsewhere)), nil
	case userrefs > len(tags):
		return blockedPlan(fmt.Sprintf("snapshot '%s' has %d hold(s) not placed by TrueNAS and not listed (userrefs %d, listed %d); %s", id, userrefs-len(tags), userrefs, len(tags), elsewhere)), nil
	case len(tags) == 0:
		return blockedPlan(fmt.Sprintf("snapshot '%s' has no holds; there is nothing to release", id)), nil
	case txg == "":
		return blockedPlan(fmt.Sprintf("could not read the createtxg of snapshot '%s', so the confirmation cannot be bound to this snapshot; nothing was released", id)), nil
	}

	warnings := []string{
		fmt.Sprintf("After the release, snapshot '%s' is deletable again: a manual delete, or periodic snapshot task or replication retention, can remove it", id),
	}
	retentionFact := "none found"
	retention, retErr := snapshotRetention(ctx, client, id)
	switch {
	case retErr != nil:
		warnings = append(warnings, "Could not check whether retention schedules this snapshot's removal (pool.snapshot.query with retention failed); once released, retention may remove it")
		retentionFact = "unknown"
	case retention != nil:
		current["retention"] = retention
		if m, _ := retention.(map[string]interface{}); m != nil {
			when, _ := m["removal_date"].(string)
			src, _ := m["source"].(string)
			if pst, ok := m["periodic_snapshot_task_id"]; ok {
				src = fmt.Sprintf("periodic snapshot task %v", pst)
			}
			warnings = append(warnings, fmt.Sprintf("Retention (%s) schedules this snapshot's removal at %s; once released it is removed then, or at the next retention run if that time has passed", src, when))
			retentionFact = fmt.Sprintf("removal at %s (%s)", when, src)
		}
	default:
		current["retention"] = nil
	}

	return &writePlan{
		Tool:         "release_snapshot",
		Method:       snapshotReleaseMethod,
		CallParams:   []interface{}{id, map[string]interface{}{"recursive": false}},
		ParamNames:   []string{"id", "options"},
		TargetName:   id,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Release the '%s' hold on snapshot '%s', its only hold; the snapshot becomes deletable again. Child datasets' snapshots are not released", truenasHoldTag, id),
			Operation:   "release",
			Target:      id,
			Details:     map[string]interface{}{"holds": tags, "recursive": false},
		}},
		Warnings: warnings,
		Bind:     bind,
		ConfirmSummary: newConfirmSummary(
			fmt.Sprintf("Release the '%s' hold on %s: the snapshot becomes deletable again", truenasHoldTag, id),
			confirmFact("snapshot", id),
			confirmFact("hold released", strings.Join(tags, ", ")),
			confirmFact("retention", retentionFact),
		),
		Result: map[string]interface{}{"holds": tags},
	}, nil
}

func handleReleaseSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planReleaseSnapshot(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeReleaseSnapshot(ctx, client, args, plan)
}

func executeReleaseSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	id := plan.TargetName
	if _, err := callPlan(ctx, client, plan); err != nil {
		return "", fmt.Errorf("failed to release snapshot %s: %w", id, err)
	}
	response := map[string]interface{}{
		"snapshot":       id,
		"released":       true,
		"holds_released": plan.Result["holds"],
		"deletable":      true,
	}
	msg := fmt.Sprintf("Released the '%s' hold on %s: the snapshot is deletable again, manually or by retention", truenasHoldTag, id)
	snap, err := lookupSnapshot(ctx, client, id)
	switch {
	case err != nil:
		response["note"] = "The release was sent, but the snapshot could not be read back: " + boundedErr(err)
	case snap == nil:
		response["note"] = fmt.Sprintf("%s was not found when read back; it may already have been deleted", id)
	default:
		left := snapshotHoldTags(snap)
		refs, refsOK := snapshotUserrefs(snap)
		if len(left) > 0 || (refsOK && refs > 0) {
			response["deletable"] = false
			response["holds_remaining"] = left
			if refsOK {
				response["userrefs"] = refs
			}
			msg = fmt.Sprintf("Released the '%s' hold on %s, but it still has a hold (listed: %s; userrefs %d) and cannot be deleted yet", truenasHoldTag, id, strings.Join(left, ", "), refs)
		}
	}
	response["message"] = msg
	return marshalJSON(response)
}

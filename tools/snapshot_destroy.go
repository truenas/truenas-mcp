package tools

// Destructive snapshot tools (rollback_snapshot, delete_snapshot).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	snapshotRollbackMethod = "pool.snapshot.rollback"
	snapshotDeleteMethod   = "pool.snapshot.delete"
)

// destroySnapshotProps are the native properties read for every snapshot
// these tools may destroy: createtxg (its identity), userrefs (its total
// hold count, whatever the tags) and clones (its clones as ZFS lists them,
// hidden datasets included).
var destroySnapshotProps = []interface{}{"createtxg", "userrefs", "clones"}

func (r *Registry) registerSnapshotDestroyTools() {
	boolSchema := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc, "default": false}
	}

	r.tools["rollback_snapshot"] = Tool{
		Definition: mcp.Tool{
			Name:        "rollback_snapshot",
			Description: "Roll a dataset back to one of its ZFS snapshots. Everything written to the dataset since that snapshot is lost and cannot be recovered. By default the snapshot must be the dataset's newest. Rolling back to an older snapshot destroys every newer snapshot of the dataset (and any newer bookmarks) and needs allow_destroy_newer=true; it is refused if any of those snapshots is held or its hold count cannot be read, and if any has clones unless destroy_clones=true, which destroys those clones too. Clones that have their own snapshots or child datasets, or that are hidden datasets, are always refused. Only the named dataset is rolled back, not its child datasets, and clones are never force-unmounted. Refused for a pool's root dataset, for datasets TrueNAS manages itself (the boot pool, .system, ix-apps, ix-applications, .ix-virt and other hidden datasets) and for any dataset query_datasets does not list. If a bookmark newer than the newest snapshot exists, ZFS refuses the rollback and this tool does not destroy the bookmark. The preview lists every snapshot and clone that would be destroyed. A snapshot or clone created between the final check and the rollback (by a periodic snapshot task or replication, say) is destroyed with allow_destroy_newer or destroy_clones without having been previewed: TrueNAS has no compare-and-swap to prevent that. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"snapshot":            map[string]interface{}{"type": "string", "description": "Required: Full name of the snapshot to roll back to, 'dataset@name' (e.g. 'tank/data@manual-1')"},
					"allow_destroy_newer": boolSchema("Optional: Allow rolling back to a snapshot that is not the newest, destroying every newer snapshot of the dataset (default: false). Refused when the snapshot is already the newest"),
					"destroy_clones":      boolSchema("Optional: Also destroy the clones of the newer snapshots, which the preview lists (default: false). Needs allow_destroy_newer; refused when there are no such clones"),
					"dry_run":             boolSchema("Optional: Preview without rolling back (default: false)"),
				},
				"required": []string{"snapshot"},
			},
		},
		Handler: handleRollbackSnapshot,
		Plan:    planRollbackSnapshot,
		Execute: executeRollbackSnapshot,
	}

	r.tools["delete_snapshot"] = Tool{
		Definition: mcp.Tool{
			Name:        "delete_snapshot",
			Description: "Delete a ZFS snapshot permanently. Only the named snapshot is deleted unless recursive=true, which also deletes the snapshot of the same name on every child dataset. Refused if any affected snapshot is held (hold_snapshot, or a hold set outside TrueNAS) or its hold count cannot be read, for snapshots of datasets TrueNAS manages itself (the boot pool, .system, ix-apps, ix-applications, .ix-virt and other hidden datasets), and for snapshots of any dataset query_datasets does not list; with recursive, this applies to every child snapshot too. A snapshot cannot be deleted while it has clones: that is refused unless defer_if_cloned=true, which marks it for deferred deletion so that ZFS deletes it when its last clone is destroyed. The preview lists every affected snapshot, its clones, and any periodic snapshot task retention that manages it. With recursive, a child snapshot of the same name created between the final check and the delete is deleted without having been previewed: TrueNAS has no compare-and-swap to prevent that. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"snapshot":        map[string]interface{}{"type": "string", "description": "Required: Full name of the snapshot to delete, 'dataset@name' (e.g. 'tank/data@manual-1')"},
					"recursive":       boolSchema("Optional: Also delete the snapshot of the same name on every child dataset (default: false). Each one is checked and listed in the preview"),
					"defer_if_cloned": boolSchema("Optional: For snapshots that have clones, mark them for deletion when their last clone is destroyed instead of refusing (default: false). Refused when no affected snapshot has clones"),
					"dry_run":         boolSchema("Optional: Preview without deleting (default: false)"),
				},
				"required": []string{"snapshot"},
			},
		},
		Handler: handleDeleteSnapshot,
		Plan:    planDeleteSnapshot,
		Execute: executeDeleteSnapshot,
	}
}

// snapDetail is what the destructive snapshot tools read about a snapshot
// and bind their plans to.
type snapDetail struct {
	ID       string
	Dataset  string
	Name     string
	Txg      uint64
	TxgOK    bool
	Tags     []string
	Userrefs int
	RefsOK   bool
	Clones   []string
}

func snapDetailFrom(row map[string]interface{}) snapDetail {
	id, _ := row["id"].(string)
	if id == "" {
		id, _ = row["name"].(string)
	}
	s := snapDetail{ID: id, Tags: snapshotHoldTags(row)}
	s.Dataset, _ = row["dataset"].(string)
	s.Name, _ = row["snapshot_name"].(string)
	if ds, name, ok := strings.Cut(id, "@"); ok {
		if s.Dataset == "" {
			s.Dataset = ds
		}
		if s.Name == "" {
			s.Name = name
		}
	}
	s.Txg, s.TxgOK = snapshotTxg(row)
	s.Userrefs, s.RefsOK = snapshotUserrefs(row)
	s.Clones = snapshotClonesProp(row)
	return s
}

// held reports whether the snapshot has, or may have, a hold. An
// unreadable hold count counts as held.
func (s snapDetail) held() bool {
	return !s.RefsOK || s.Userrefs > 0 || len(s.Tags) > 0
}

func (s snapDetail) holdText() string {
	if !s.RefsOK {
		return fmt.Sprintf("%s (hold count unreadable)", s.ID)
	}
	if len(s.Tags) == 0 {
		return fmt.Sprintf("%s (%d unlisted hold(s))", s.ID, s.Userrefs)
	}
	return fmt.Sprintf("%s (holds: %s)", s.ID, strings.Join(s.Tags, ", "))
}

func (s snapDetail) txgText() string {
	if !s.TxgOK {
		return ""
	}
	return fmt.Sprint(s.Txg)
}

func (s snapDetail) state() map[string]interface{} {
	st := map[string]interface{}{
		"snapshot":  s.ID,
		"createtxg": s.txgText(),
		"holds":     s.Tags,
		"clones":    s.Clones,
	}
	if s.RefsOK {
		st["userrefs"] = s.Userrefs
	}
	return st
}

// bind is the snapshot's identity and the state that decides what a call
// destroys; a change to any of it after the preview rejects the token.
func (s snapDetail) bind() map[string]interface{} {
	return map[string]interface{}{
		"id":          s.ID,
		"createtxg":   s.txgText(),
		"holds":       s.Tags,
		"userrefs":    s.Userrefs,
		"userrefs_ok": s.RefsOK,
		"clones":      s.Clones,
	}
}

// snapshotClonesProp returns the snapshot's native clones property as a
// sorted list. It is a comma-separated string; a missing or empty value
// gives an empty list, so clones are also looked up by origin.
func snapshotClonesProp(row map[string]interface{}) []string {
	props, _ := row["properties"].(map[string]interface{})
	p, _ := props["clones"].(map[string]interface{})
	var parts []string
	for _, k := range []string{"parsed", "rawvalue", "value"} {
		switch v := p[k].(type) {
		case string:
			parts = strings.Split(v, ",")
		case []interface{}:
			for _, x := range v {
				if s, ok := x.(string); ok {
					parts = append(parts, s)
				}
			}
		default:
			continue
		}
		break
	}
	out := []string{}
	for _, c := range parts {
		if c = strings.TrimSpace(c); c != "" && c != "-" {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// querySnapshotDetails reads snapshots with their holds and the properties
// in destroySnapshotProps. keep, when set, filters the rows again here, so
// a middleware that ignores part of the filter cannot add snapshots.
func querySnapshotDetails(ctx context.Context, client *truenas.Client, filters []interface{}, keep func(snapDetail) bool) ([]snapDetail, error) {
	raw, err := client.Call(ctx, "pool.snapshot.query", filters,
		map[string]interface{}{"extra": map[string]interface{}{"holds": true, "properties": destroySnapshotProps}},
	)
	if err != nil {
		return nil, err
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected pool.snapshot.query result")
	}
	out := make([]snapDetail, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		s := snapDetailFrom(row)
		if s.ID == "" || seen[s.ID] || (keep != nil && !keep(s)) {
			continue
		}
		seen[s.ID] = true
		out = append(out, s)
	}
	return out, nil
}

// lookupSnapDetail returns the snapshot id, or nil when it does not exist.
func lookupSnapDetail(ctx context.Context, client *truenas.Client, id string) (*snapDetail, error) {
	snaps, err := querySnapshotDetails(ctx, client, []interface{}{[]interface{}{"id", "=", id}}, func(s snapDetail) bool { return s.ID == id })
	if err != nil {
		return nil, fmt.Errorf("failed to look up snapshot %s: %w", id, err)
	}
	if len(snaps) == 0 {
		return nil, nil
	}
	return &snaps[0], nil
}

// cloneLookup is what attachClones learned about the clones of a set of
// snapshots.
type cloneLookup struct {
	// visible are the clones pool.dataset.query lists. A clone known only
	// from the clones property is a hidden dataset.
	visible map[string]bool
}

// attachClones adds to each snapshot the datasets whose origin it is,
// from pool.dataset.query, merged with its own clones property.
func attachClones(ctx context.Context, client *truenas.Client, snaps []snapDetail) (cloneLookup, error) {
	lk := cloneLookup{visible: map[string]bool{}}
	if len(snaps) == 0 {
		return lk, nil
	}
	ids := make([]interface{}, len(snaps))
	index := make(map[string]int, len(snaps))
	for i, s := range snaps {
		ids[i] = s.ID
		index[s.ID] = i
	}
	raw, err := client.Call(ctx, "pool.dataset.query",
		[]interface{}{[]interface{}{"origin.value", "in", ids}},
		map[string]interface{}{
			"select": []interface{}{"id", "name", "origin"},
			"extra": map[string]interface{}{
				"retrieve_children":   false,
				"properties":          []interface{}{"origin"},
				"retrieve_user_props": false,
			},
		},
	)
	if err != nil {
		return lk, fmt.Errorf("failed to look up clones: %w", err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return lk, fmt.Errorf("unexpected pool.dataset.query result")
	}
	found := make([]map[string]bool, len(snaps))
	for i, s := range snaps {
		found[i] = map[string]bool{}
		for _, c := range s.Clones {
			found[i][c] = true
		}
	}
	for _, row := range rows {
		name, _ := row["name"].(string)
		origin, _ := row["origin"].(map[string]interface{})
		o, _ := origin["value"].(string)
		if o == "" {
			o, _ = origin["rawvalue"].(string)
		}
		i, ok := index[o]
		if name == "" || !ok {
			continue
		}
		found[i][name] = true
		lk.visible[name] = true
	}
	for i := range snaps {
		clones := make([]string, 0, len(found[i]))
		for c := range found[i] {
			clones = append(clones, c)
		}
		sort.Strings(clones)
		snaps[i].Clones = clones
	}
	return lk, nil
}

// bootPoolNames and internalDatasetNames copy the 25.10 middleware's
// internal-dataset rule: BOOT_POOL_NAME_VALID in
// src/middlewared/middlewared/utils/__init__.py and INTERNAL_DATASETS and
// is_internal_dataset() in
// src/middlewared/middlewared/plugins/pool_/dataset_query_utils.py
// (truenas/middleware, branch stable/goldeye). pool.dataset.query hides
// these datasets, but pool.snapshot.query still lists their snapshots.
var (
	bootPoolNames        = []string{"freenas-boot", "boot-pool"}
	internalDatasetNames = []string{".system", "ix-applications", "ix-apps", ".ix-virt"}
)

// managedDatasetPart returns what makes dataset one TrueNAS manages
// itself, or false. It applies the middleware's is_internal_dataset() rule
// exactly: every dataset of a boot pool (freenas-boot, boot-pool), and any
// name containing "/" followed by one of internalDatasetNames (.system,
// ix-applications, ix-apps, .ix-virt) as a plain substring, as the
// middleware matches it. On top of that, any component below the pool root
// that starts with "." (a hidden dataset) counts too.
func managedDatasetPart(dataset string) (string, bool) {
	for _, b := range bootPoolNames {
		if dataset == b || strings.HasPrefix(dataset, b+"/") {
			return b, true
		}
	}
	for _, i := range internalDatasetNames {
		if strings.Contains(dataset, "/"+i) {
			return i, true
		}
	}
	parts := strings.Split(dataset, "/")
	for _, part := range parts[1:] {
		if strings.HasPrefix(part, ".") {
			return part, true
		}
	}
	return "", false
}

// unlistedDatasets returns the datasets in names that pool.dataset.query
// does not list, sorted. The middleware hides the datasets it manages from
// pool.dataset.query, so this keeps following its rule if that list grows.
func unlistedDatasets(ctx context.Context, client *truenas.Client, names []string) ([]string, error) {
	want := map[string]bool{}
	list := []interface{}{}
	for _, n := range names {
		if !want[n] {
			want[n] = true
			list = append(list, n)
		}
	}
	if len(list) == 0 {
		return nil, nil
	}
	raw, err := client.Call(ctx, "pool.dataset.query",
		[]interface{}{[]interface{}{"name", "in", list}},
		map[string]interface{}{
			"select": []interface{}{"id", "name"},
			"extra": map[string]interface{}{
				"retrieve_children":   false,
				"properties":          []interface{}{},
				"retrieve_user_props": false,
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to check that query_datasets lists %s: %w", strings.Join(names, ", "), err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected pool.dataset.query result")
	}
	listed := map[string]bool{}
	for _, row := range rows {
		if name, _ := row["name"].(string); want[name] {
			listed[name] = true
		}
	}
	var out []string
	for n := range want {
		if !listed[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// snapshotNames lists snapshot ids through pool.snapshot.query's
// names-only path: select ["name"] with only name, pool and dataset
// filters. On 25.10 that path iterates only the datasets a "dataset" or
// "pool" "=" filter names (a "dataset" filter without its children) and
// reads no holds or properties, where any other query lists every snapshot
// on the system with them. minTxg, when not zero, is passed as
// extra.min_txg, so ZFS skips snapshots created before that txg. keep
// filters the ids again here, so a middleware that ignores part of the
// filter cannot add snapshots; callers read each kept snapshot's details
// with lookupSnapDetail, whose single "id =" filter 25.10 also answers
// without listing every snapshot.
func snapshotNames(ctx context.Context, client *truenas.Client, filters []interface{}, minTxg uint64, keep func(id string) bool) ([]string, error) {
	opts := map[string]interface{}{"select": []interface{}{"name"}}
	if minTxg > 0 {
		opts["extra"] = map[string]interface{}{"min_txg": minTxg}
	}
	raw, err := client.Call(ctx, "pool.snapshot.query", filters, opts)
	if err != nil {
		return nil, err
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected pool.snapshot.query result")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, row := range rows {
		id, _ := row["name"].(string)
		if id == "" {
			id, _ = row["id"].(string)
		}
		if id == "" || seen[id] || strings.Count(id, "@") != 1 || !keep(id) {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// snapDetails reads the details of each id with lookupSnapDetail. A
// snapshot that no longer exists is left out.
func snapDetails(ctx context.Context, client *truenas.Client, ids []string) ([]snapDetail, error) {
	out := make([]snapDetail, 0, len(ids))
	for _, id := range ids {
		s, err := lookupSnapDetail(ctx, client, id)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

func snapIDs(snaps []snapDetail) []string {
	out := make([]string, len(snaps))
	for i, s := range snaps {
		out[i] = s.ID
	}
	return out
}

func snapBinds(snaps []snapDetail) []interface{} {
	out := make([]interface{}, len(snaps))
	for i, s := range snaps {
		out[i] = s.bind()
	}
	return out
}

func snapStates(snaps []snapDetail) []interface{} {
	out := make([]interface{}, len(snaps))
	for i, s := range snaps {
		out[i] = s.state()
	}
	return out
}

// allClones returns every clone of snaps, sorted, each once.
func allClones(snaps []snapDetail) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range snaps {
		for _, c := range s.Clones {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

// ---- rollback_snapshot ----

// newerSnapshots returns the snapshots of target's dataset (not of its
// children) created after target, oldest first by createtxg, and the ids
// among them whose createtxg cannot be read. Candidates come from the
// names-only query of the dataset with extra.min_txg set to target's
// createtxg; each is then read by id and kept only when its createtxg is
// greater than target's.
func newerSnapshots(ctx context.Context, client *truenas.Client, target snapDetail) ([]snapDetail, []string, error) {
	dataset := target.Dataset
	ids, err := snapshotNames(ctx, client, []interface{}{[]interface{}{"dataset", "=", dataset}}, target.Txg,
		func(id string) bool { return strings.HasPrefix(id, dataset+"@") && id != target.ID })
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list the snapshots of %s: %w", dataset, err)
	}
	snaps, err := snapDetails(ctx, client, ids)
	if err != nil {
		return nil, nil, err
	}
	var newer []snapDetail
	var unreadable []string
	for _, s := range snaps {
		switch {
		case s.Dataset != dataset:
		case !s.TxgOK:
			unreadable = append(unreadable, s.ID)
		case s.Txg > target.Txg:
			newer = append(newer, s)
		}
	}
	sort.SliceStable(newer, func(i, j int) bool {
		if newer[i].Txg != newer[j].Txg {
			return newer[i].Txg < newer[j].Txg
		}
		return newer[i].ID < newer[j].ID
	})
	return newer, unreadable, nil
}

// cloneDependents returns why each clone cannot be destroyed by this tool:
// it is hidden (pool.dataset.query does not list it), or it has its own
// snapshots or child datasets, which a recursive_clones rollback would
// destroy without them being listed.
func cloneDependents(ctx context.Context, client *truenas.Client, clones []string, lk cloneLookup) ([]string, error) {
	var reasons []string
	for _, c := range clones {
		if !lk.visible[c] {
			reasons = append(reasons, fmt.Sprintf("clone '%s' is not listed by query_datasets (a hidden dataset TrueNAS manages)", c))
			continue
		}
		raw, err := client.Call(ctx, "pool.dataset.query",
			[]interface{}{[]interface{}{"name", "^", c + "/"}},
			map[string]interface{}{
				"select": []interface{}{"id", "name"},
				"extra": map[string]interface{}{
					"retrieve_children":   false,
					"properties":          []interface{}{},
					"retrieve_user_props": false,
				},
			},
		)
		if err != nil {
			return nil, fmt.Errorf("failed to check child datasets of clone %s: %w", c, err)
		}
		var rows []map[string]interface{}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("unexpected pool.dataset.query result")
		}
		children := 0
		for _, row := range rows {
			if name, _ := row["name"].(string); strings.HasPrefix(name, c+"/") {
				children++
			}
		}
		if children > 0 {
			reasons = append(reasons, fmt.Sprintf("clone '%s' has %d child dataset(s)", c, children))
		}
		snaps, err := snapshotNames(ctx, client, []interface{}{[]interface{}{"dataset", "=", c}}, 0,
			func(id string) bool { return strings.HasPrefix(id, c+"@") })
		if err != nil {
			return nil, fmt.Errorf("failed to check snapshots of clone %s: %w", c, err)
		}
		if len(snaps) > 0 {
			reasons = append(reasons, fmt.Sprintf("clone '%s' has %d snapshot(s) of its own (%s)", c, len(snaps), strings.Join(snaps, ", ")))
		}
	}
	return reasons, nil
}

func planRollbackSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	id, dataset, _, err := requiredSnapshotArg(args)
	if err != nil {
		return nil, err
	}
	allowNewer, _ := args["allow_destroy_newer"].(bool)
	destroyClones, _ := args["destroy_clones"].(bool)
	if !strings.Contains(dataset, "/") {
		return nil, fmt.Errorf("'%s' is the root dataset of pool '%s'; rolling back a pool's root dataset is not supported (it also reverts the ownership and permissions of /mnt/%s, which every dataset in the pool is reached through); nothing was changed", dataset, dataset, dataset)
	}
	if part, ok := managedDatasetPart(dataset); ok {
		return nil, fmt.Errorf("dataset '%s' is inside '%s', which TrueNAS manages itself; rolling back its snapshots is not supported; nothing was changed", dataset, part)
	}

	unlisted, err := unlistedDatasets(ctx, client, []string{dataset})
	if err != nil {
		return nil, err
	}
	if len(unlisted) > 0 {
		return nil, fmt.Errorf("dataset '%s' is not listed by query_datasets (pool.dataset.query hides the datasets TrueNAS manages itself); rolling back its snapshots is not supported; nothing was changed", dataset)
	}
	t, err := lookupSnapDetail(ctx, client, id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("snapshot '%s' not found", id)
	}
	target := *t

	options := map[string]interface{}{"recursive": false, "recursive_clones": false, "force": false, "recursive_rollback": false}
	bind := map[string]interface{}{"snapshot": target.bind()}
	current := map[string]interface{}{
		"snapshot": target.state(),
		"dataset":  dataset,
	}
	summary := newConfirmSummary(
		fmt.Sprintf("Roll back %s to %s: everything written since the snapshot is lost", dataset, id),
		confirmFact("snapshot", id),
	)
	blocked := func(reasons ...string) *writePlan {
		return &writePlan{
			Tool:           "rollback_snapshot",
			Method:         snapshotRollbackMethod,
			CallParams:     []interface{}{id, options},
			ParamNames:     []string{"id", "options"},
			TargetName:     id,
			CurrentState:   current,
			Blocked:        reasons,
			Bind:           bind,
			ConfirmSummary: summary,
		}
	}
	if !target.TxgOK {
		return blocked(fmt.Sprintf("could not read the createtxg of %s, so the snapshots newer than it cannot be determined and the confirmation cannot be bound to them; nothing was rolled back", id)), nil
	}

	newer, unreadable, err := newerSnapshots(ctx, client, target)
	if err != nil {
		return nil, err
	}
	if len(unreadable) > 0 {
		return blocked(fmt.Sprintf("could not read the createtxg of %s, so the snapshots newer than '%s' cannot be determined and the confirmation cannot be bound to them; nothing was rolled back", strings.Join(unreadable, ", "), id)), nil
	}
	current["newest"] = id
	if len(newer) > 0 {
		current["newest"] = newer[len(newer)-1].ID
	}
	lk, err := attachClones(ctx, client, newer)
	if err != nil {
		return nil, err
	}
	clones := allClones(newer)
	bind["newer"] = snapBinds(newer)
	current["newer_snapshots"] = snapStates(newer)
	current["clones_destroyed"] = clones
	current["allow_destroy_newer"] = allowNewer
	current["destroy_clones"] = destroyClones

	var reasons []string
	switch {
	case len(newer) == 0 && allowNewer:
		reasons = append(reasons, fmt.Sprintf("'%s' is already the newest snapshot of '%s'; omit allow_destroy_newer (with it, newer bookmarks would be destroyed too)", id, dataset))
	case len(newer) > 0 && !allowNewer:
		reasons = append(reasons, fmt.Sprintf("'%s' is not the newest snapshot of '%s': rolling back to it destroys %d newer snapshot(s) (%s); set allow_destroy_newer=true to allow that after reviewing them", id, dataset, len(newer), strings.Join(snapIDs(newer), ", ")))
	}
	if destroyClones && !allowNewer {
		reasons = append(reasons, "destroy_clones needs allow_destroy_newer=true")
	}
	var held []string
	for _, s := range newer {
		if s.held() {
			held = append(held, s.holdText())
		}
	}
	if len(held) > 0 {
		reasons = append(reasons, fmt.Sprintf("newer snapshot(s) that the rollback would destroy are held or may be held: %s; release the holds first (release_snapshot releases a 'truenas' hold) or roll back to a newer snapshot", strings.Join(held, "; ")))
	}
	switch {
	case len(clones) > 0 && !destroyClones:
		var withClones []string
		for _, s := range newer {
			if len(s.Clones) > 0 {
				withClones = append(withClones, fmt.Sprintf("%s (clones: %s)", s.ID, strings.Join(s.Clones, ", ")))
			}
		}
		reasons = append(reasons, fmt.Sprintf("newer snapshot(s) have dependent clones: %s; the rollback would have to destroy them too, which needs destroy_clones=true", strings.Join(withClones, "; ")))
	case len(clones) == 0 && destroyClones:
		reasons = append(reasons, "destroy_clones is set, but no newer snapshot has clones; omit it")
	case len(clones) > 0 && destroyClones:
		deps, err := cloneDependents(ctx, client, clones, lk)
		if err != nil {
			return nil, err
		}
		if len(deps) > 0 {
			reasons = append(reasons, fmt.Sprintf("%s; destroy_clones destroys only clones without dependents of their own, so remove those first", strings.Join(deps, "; ")))
		}
	}
	if len(reasons) > 0 {
		return blocked(reasons...), nil
	}

	options["recursive"] = len(newer) > 0
	options["recursive_clones"] = len(clones) > 0
	warnings := []string{
		fmt.Sprintf("Everything written to '%s' since '%s' was taken is lost and cannot be recovered", dataset, id),
		fmt.Sprintf("Only '%s' is rolled back; its child datasets are not", dataset),
		"Clients using the dataset (SMB, NFS, iSCSI, VMs or apps) see its contents change underneath them; stop them first if they keep data in it",
	}
	if len(newer) > 0 {
		warnings = append(warnings,
			fmt.Sprintf("%d newer snapshot(s) are destroyed: %s", len(newer), strings.Join(snapIDs(newer), ", ")),
			"Bookmarks newer than the snapshot are destroyed too",
			"If newer snapshots were replicated to another system, the next incremental replication from this dataset may fail until the target is rolled back as well",
		)
	}
	if len(clones) > 0 {
		warnings = append(warnings, fmt.Sprintf("These clones and all their data are destroyed: %s", strings.Join(clones, ", ")))
	}

	desc := fmt.Sprintf("Roll back dataset '%s' to snapshot '%s', its newest; data written since the snapshot is lost", dataset, id)
	if len(newer) > 0 {
		desc = fmt.Sprintf("Roll back dataset '%s' to snapshot '%s', destroying %d newer snapshot(s)", dataset, id, len(newer))
		if len(clones) > 0 {
			desc += fmt.Sprintf(" and %d clone(s)", len(clones))
		}
		desc += "; data written since the snapshot is lost"
	}
	return &writePlan{
		Tool:         "rollback_snapshot",
		Method:       snapshotRollbackMethod,
		CallParams:   []interface{}{id, options},
		ParamNames:   []string{"id", "options"},
		TargetName:   id,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: desc,
			Operation:   "rollback",
			Target:      id,
			Details: map[string]interface{}{
				"dataset":                    dataset,
				"snapshots_destroyed":        snapIDs(newer),
				"clones_destroyed":           clones,
				"child_datasets_rolled_back": false,
				"force":                      false,
			},
		}},
		Warnings: warnings,
		Bind:     bind,
		ConfirmSummary: newConfirmSummary(
			fmt.Sprintf("Roll back %s to %s: everything written since the snapshot is lost", dataset, id),
			confirmFact("snapshot", id),
			confirmFact("snapshots destroyed", listOrNone(snapIDs(newer))),
			confirmFact("clones destroyed", listOrNone(clones)),
		),
		Result: map[string]interface{}{
			"dataset":   dataset,
			"destroyed": snapIDs(newer),
			"clones":    clones,
		},
	}, nil
}

func handleRollbackSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planRollbackSnapshot(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeRollbackSnapshot(ctx, client, args, plan)
}

func executeRollbackSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	id := plan.TargetName
	dataset, _ := plan.Result["dataset"].(string)
	if _, err := callPlan(ctx, client, plan); err != nil {
		destroyed, _ := plan.Result["destroyed"].([]string)
		if len(destroyed) == 0 && strings.Contains(err.Error(), "more recent snapshots or bookmarks exist") {
			return "", fmt.Errorf("failed to roll back %s to %s: %w; no snapshot newer than %s was listed at the preview, so a bookmark newer than it (or a snapshot taken since the preview) is blocking the rollback. ZFS can roll back past a newer bookmark only by destroying it, which this tool does not do for the newest snapshot; remove the bookmark outside this tool, or clone the snapshot with clone_snapshot to recover files instead; check query_snapshots before retrying", dataset, id, err, id)
		}
		return "", fmt.Errorf("failed to roll back %s to %s: %w; check query_snapshots before retrying", dataset, id, err)
	}
	response := map[string]interface{}{
		"rolled_back":         true,
		"snapshot":            id,
		"dataset":             dataset,
		"snapshots_destroyed": plan.Result["destroyed"],
		"clones_destroyed":    plan.Result["clones"],
	}
	msg := fmt.Sprintf("Rolled back %s to %s", dataset, id)
	target, err := lookupSnapDetail(ctx, client, id)
	var newer []snapDetail
	if err == nil && target != nil && target.TxgOK {
		newer, _, err = newerSnapshots(ctx, client, *target)
	}
	switch {
	case err != nil:
		response["note"] = "The rollback finished, but the snapshots could not be read back: " + boundedErr(err)
	case target == nil:
		response["note"] = fmt.Sprintf("The rollback finished, but %s does not exist when read back; check query_snapshots", id)
	case !target.TxgOK:
		response["note"] = fmt.Sprintf("The rollback finished, but the createtxg of %s could not be read back, so whether it is the newest snapshot is unknown; check query_snapshots", id)
	case len(newer) > 0:
		response["note"] = fmt.Sprintf("The rollback finished, but %s is not the newest snapshot when read back (newest: %s); check query_snapshots", id, newer[len(newer)-1].ID)
	default:
		response["newest_snapshot"] = id
	}
	response["message"] = msg + "; data written since the snapshot is gone"
	return marshalJSON(response)
}

// ---- delete_snapshot ----

// snapshotRetentions returns the simplified retention annotation of each
// id that has one. extra.retention needs an id filter on 25.10, and only a
// single "id =" filter keeps pool.snapshot.query from listing every
// snapshot on the system, so each id is read on its own.
func snapshotRetentions(ctx context.Context, client *truenas.Client, ids []string) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	for _, id := range ids {
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
			if rid, _ := row["id"].(string); rid != id {
				continue
			}
			if r := simplifySnapshotRetention(row["retention"]); r != nil {
				out[id] = r
			}
		}
	}
	return out, nil
}

func retentionText(r interface{}) string {
	m, _ := r.(map[string]interface{})
	when, _ := m["removal_date"].(string)
	if when == "" {
		when = "an unknown time"
	}
	src, _ := m["source"].(string)
	if pst, ok := m["periodic_snapshot_task_id"]; ok {
		src = fmt.Sprintf("periodic snapshot task %v", pst)
	} else if src == "property" {
		src = "a retention property"
	}
	return fmt.Sprintf("%s, removal at %s", src, when)
}

func planDeleteSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	id, dataset, name, err := requiredSnapshotArg(args)
	if err != nil {
		return nil, err
	}
	recursive, _ := args["recursive"].(bool)
	deferCloned, _ := args["defer_if_cloned"].(bool)
	if part, ok := managedDatasetPart(dataset); ok {
		return nil, fmt.Errorf("dataset '%s' is inside '%s', which TrueNAS manages itself; deleting its snapshots is not supported; nothing was changed", dataset, part)
	}

	target, err := lookupSnapDetail(ctx, client, id)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("snapshot '%s' not found", id)
	}
	affected := []snapDetail{*target}
	if recursive {
		pool, _, _ := strings.Cut(dataset, "/")
		childIDs, err := snapshotNames(ctx, client,
			[]interface{}{[]interface{}{"pool", "=", pool}, []interface{}{"name", "^", dataset + "/"}}, 0,
			func(id string) bool {
				ds, n, _ := strings.Cut(id, "@")
				return n == name && strings.HasPrefix(ds, dataset+"/")
			})
		if err != nil {
			return nil, fmt.Errorf("failed to list the child snapshots of %s: %w", id, err)
		}
		children, err := snapDetails(ctx, client, childIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to read the child snapshots of %s: %w", id, err)
		}
		for _, c := range children {
			if c.Name == name && strings.HasPrefix(c.Dataset, dataset+"/") && c.ID == c.Dataset+"@"+name {
				affected = append(affected, c)
			}
		}
		sort.Slice(affected[1:], func(i, j int) bool { return affected[1+i].ID < affected[1+j].ID })
	}
	affectedDatasets := make([]string, len(affected))
	for i, s := range affected {
		affectedDatasets[i] = s.Dataset
	}
	unlisted, err := unlistedDatasets(ctx, client, affectedDatasets)
	if err != nil {
		return nil, err
	}
	if _, err := attachClones(ctx, client, affected); err != nil {
		return nil, err
	}
	ids := snapIDs(affected)
	clones := allClones(affected)
	sendRecursive := recursive && len(affected) > 1
	sendDefer := deferCloned && len(clones) > 0
	options := map[string]interface{}{"defer": sendDefer, "recursive": sendRecursive}
	bind := map[string]interface{}{"snapshots": snapBinds(affected)}
	current := map[string]interface{}{
		"snapshot":        target.state(),
		"snapshots":       snapStates(affected),
		"recursive":       recursive,
		"defer_if_cloned": deferCloned,
	}
	headline := fmt.Sprintf("Delete snapshot %s permanently", id)
	if sendRecursive {
		headline = fmt.Sprintf("Delete snapshot %s and %d child snapshot(s) permanently", id, len(affected)-1)
	}
	blocked := func(reasons ...string) *writePlan {
		return &writePlan{
			Tool:           "delete_snapshot",
			Method:         snapshotDeleteMethod,
			CallParams:     []interface{}{id, options},
			ParamNames:     []string{"id", "options"},
			TargetName:     id,
			CurrentState:   current,
			Blocked:        reasons,
			Bind:           bind,
			ConfirmSummary: newConfirmSummary(headline, confirmFact("snapshots", strings.Join(ids, ", "))),
		}
	}

	var reasons, unreadable, held, managed []string
	for _, s := range affected {
		if !s.TxgOK {
			unreadable = append(unreadable, s.ID)
		}
		if s.held() {
			held = append(held, s.holdText())
		}
		if part, ok := managedDatasetPart(s.Dataset); ok {
			managed = append(managed, fmt.Sprintf("%s (in '%s')", s.ID, part))
		}
	}
	if len(unreadable) > 0 {
		reasons = append(reasons, fmt.Sprintf("could not read the createtxg of %s, so the confirmation cannot be bound to the snapshot(s); nothing was deleted", strings.Join(unreadable, ", ")))
	}
	if len(managed) > 0 {
		reasons = append(reasons, fmt.Sprintf("the recursive delete would include snapshots of datasets TrueNAS manages itself: %s; delete the other snapshots without recursive instead", strings.Join(managed, ", ")))
	}
	if len(unlisted) > 0 {
		reasons = append(reasons, fmt.Sprintf("dataset(s) %s are not listed by query_datasets (pool.dataset.query hides the datasets TrueNAS manages itself); deleting their snapshots is not supported", strings.Join(unlisted, ", ")))
	}
	if len(held) > 0 {
		reasons = append(reasons, fmt.Sprintf("snapshot(s) are held or may be held: %s; a held snapshot cannot be deleted until every hold is released (release_snapshot releases a 'truenas' hold; other holds are managed where they were set)", strings.Join(held, "; ")))
	}
	switch {
	case len(clones) > 0 && !deferCloned:
		var withClones []string
		for _, s := range affected {
			if len(s.Clones) > 0 {
				withClones = append(withClones, fmt.Sprintf("%s (clones: %s)", s.ID, strings.Join(s.Clones, ", ")))
			}
		}
		reasons = append(reasons, fmt.Sprintf("snapshot(s) have dependent clones: %s; a snapshot cannot be deleted while its clones exist. Delete or promote the clones first, or set defer_if_cloned=true to have ZFS delete the snapshot when its last clone is destroyed", strings.Join(withClones, "; ")))
	case len(clones) == 0 && deferCloned:
		reasons = append(reasons, "defer_if_cloned is set, but no affected snapshot has clones; omit it")
	}
	if len(reasons) > 0 {
		return blocked(reasons...), nil
	}

	warnings := []string{"Deleted snapshots cannot be recovered; files that exist only in them are lost"}
	if recursive && !sendRecursive {
		warnings = append(warnings, fmt.Sprintf("No child dataset of '%s' has a snapshot named '%s'; only %s is deleted", dataset, name, id))
	}
	if sendRecursive {
		warnings = append(warnings, fmt.Sprintf("recursive: %d snapshot(s) are deleted: %s", len(affected), strings.Join(ids, ", ")))
	}
	if sendDefer {
		warnings = append(warnings, fmt.Sprintf("Snapshots with clones (%s) are not deleted now: they are marked for deferred deletion and ZFS deletes each one when its last clone is destroyed; the clones are not changed", strings.Join(clones, ", ")))
	}
	warnings = append(warnings, "If a deleted snapshot is the newest one a replication target has in common with this system, the next incremental replication may need a full resend")

	retentionFact := "none found"
	retention, retErr := snapshotRetentions(ctx, client, ids)
	switch {
	case retErr != nil:
		warnings = append(warnings, "Could not check whether a periodic snapshot task's retention manages these snapshots (pool.snapshot.query with retention failed)")
		retentionFact = "unknown"
	case len(retention) > 0:
		current["retention"] = retention
		var facts []string
		for _, sid := range ids {
			if r, ok := retention[sid]; ok {
				t := retentionText(r)
				warnings = append(warnings, fmt.Sprintf("%s is managed by retention (%s); deleting it removes it before then", sid, t))
				facts = append(facts, fmt.Sprintf("%s: %s", sid, t))
			}
		}
		retentionFact = strings.Join(facts, "; ")
	default:
		current["retention"] = nil
	}

	desc := fmt.Sprintf("Delete snapshot '%s'; child datasets' snapshots are not deleted", id)
	if sendRecursive {
		desc = fmt.Sprintf("Delete snapshot '%s' and the snapshot '%s' on %d child dataset(s)", id, name, len(affected)-1)
	}
	if sendDefer {
		desc += "; snapshots with clones are marked for deferred deletion"
	}
	facts := []string{
		confirmFact("snapshots", strings.Join(ids, ", ")),
		confirmFact("clones", listOrNone(clones)),
		confirmFact("retention", retentionFact),
	}
	if sendDefer {
		facts = append(facts, confirmFact("deferred until clones are destroyed", strings.Join(clones, ", ")))
	}
	return &writePlan{
		Tool:         "delete_snapshot",
		Method:       snapshotDeleteMethod,
		CallParams:   []interface{}{id, options},
		ParamNames:   []string{"id", "options"},
		TargetName:   id,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: desc,
			Operation:   "delete",
			Target:      id,
			Details: map[string]interface{}{
				"snapshots": ids,
				"recursive": sendRecursive,
				"defer":     sendDefer,
				"clones":    clones,
			},
		}},
		Warnings:       warnings,
		Bind:           bind,
		ConfirmSummary: newConfirmSummary(headline, facts...),
		Result: map[string]interface{}{
			"snapshots": ids,
			"deferred":  sendDefer,
		},
	}, nil
}

func handleDeleteSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planDeleteSnapshot(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeDeleteSnapshot(ctx, client, args, plan)
}

func executeDeleteSnapshot(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	id := plan.TargetName
	ids, _ := plan.Result["snapshots"].([]string)
	deferred, _ := plan.Result["deferred"].(bool)
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to delete snapshot %s: %w; check query_snapshots before retrying", id, err)
	}
	var ok bool
	if err := json.Unmarshal(result, &ok); err != nil || !ok {
		return "", fmt.Errorf("pool.snapshot.delete returned an unexpected result; check whether %s still exists with query_snapshots before retrying", id)
	}
	response := map[string]interface{}{
		"snapshot":  id,
		"snapshots": ids,
		"deleted":   true,
		"deferred":  deferred,
	}
	var remaining []snapDetail
	var readErr error
	for _, sid := range ids {
		s, err := lookupSnapDetail(ctx, client, sid)
		if err != nil {
			readErr = err
			break
		}
		if s != nil {
			remaining = append(remaining, *s)
		}
	}
	if readErr == nil && deferred && len(remaining) > 0 {
		_, readErr = attachClones(ctx, client, remaining)
	}
	// With defer, only a remaining snapshot that still has clones is
	// pending deletion because of them. One without clones should be gone;
	// if it is held (a hold added after the preview), ZFS deletes it only
	// when its last hold is released, so it is reported as remaining.
	var pending, left, leftHeld []string
	for _, s := range remaining {
		switch {
		case deferred && len(s.Clones) > 0:
			pending = append(pending, s.ID)
		case deferred && s.held():
			leftHeld = append(leftHeld, s.ID)
			left = append(left, s.ID)
		default:
			left = append(left, s.ID)
		}
	}
	msg := fmt.Sprintf("Deleted %s", strings.Join(ids, ", "))
	var notes []string
	switch {
	case readErr != nil:
		notes = append(notes, "The delete finished, but the snapshots could not be read back: "+boundedErr(readErr))
	default:
		if len(pending) > 0 {
			response["pending_deletion"] = pending
			msg = fmt.Sprintf("Deleted the other snapshots; %s are marked for deletion and are removed when their last clone is destroyed", strings.Join(pending, ", "))
			if len(pending) == len(ids) {
				msg = fmt.Sprintf("%s are marked for deletion and are removed when their last clone is destroyed", strings.Join(pending, ", "))
			}
		}
		if len(left) > 0 {
			response["remaining"] = left
			if len(pending) == 0 {
				msg = fmt.Sprintf("TrueNAS reported success, but %s still exist", strings.Join(left, ", "))
			}
			why := ""
			if deferred {
				why = " and have no clones, so they are not pending deletion because of clones"
			}
			notes = append(notes, fmt.Sprintf("TrueNAS reported success, but %s still exist when read back%s; check query_snapshots", strings.Join(left, ", "), why))
		}
		if len(leftHeld) > 0 {
			notes = append(notes, fmt.Sprintf("%s are held (a hold may have been added after the preview); with defer, ZFS deletes a held snapshot only when its last hold is released", strings.Join(leftHeld, ", ")))
		}
	}
	if len(notes) > 0 {
		response["note"] = strings.Join(notes, "; ")
	}
	response["message"] = msg
	return marshalJSON(response)
}

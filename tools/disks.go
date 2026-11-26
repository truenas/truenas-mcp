package tools

// Disk inventory and health tools (query_disks, get_disk_health). Both
// project middleware records onto fixed field lists: disk.query can carry
// a SED password (passwd) and a KMIP id, and neither is requested or kept.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerDiskTools() {
	r.tools["query_disks"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_disks",
			Description: "List physical disks: name, serial, model, size, type (HDD, SSD or NVMe), bus, the pool each belongs to, and whether it is in use. 'usage' is USED when the disk is part of an imported pool (including the boot pool) or of an exported pool that could be imported, UNUSED otherwise, and UNKNOWN when TrueNAS did not report it. Use 'name' for one disk, 'pool' for the disks of one pool, 'unused_only' for disks free to add to a pool, and 'limit' to bound the list (default: 50, maximum: 1000).",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Disk name (exact match, e.g. 'sda' or 'nvme0n1') to return only that disk",
						"minLength":   1,
					},
					"pool": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Return only disks of this pool (exact name; an exported pool also matches)",
						"minLength":   1,
					},
					"unused_only": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Return only disks that are not part of any imported or exported pool (default: false). Cannot be combined with 'pool'",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of disks to return (default: 50, maximum: 1000)",
						"minimum":     1,
					},
				},
			},
		},
		Handler: handleQueryDisks,
	}

	r.tools["get_disk_health"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_disk_health",
			Description: "Report the health of every disk in one call, to find a disk that is failing: its ZFS device state and read/write/checksum error counts from the pool topology (data pools and the boot pool), its temperature and critical threshold, and any current disk temperature alert (an alert dismissed in the UI still counts while TrueNAS keeps reporting it, and is marked dismissed). Each disk has 'healthy' and a list of 'problems'; unhealthy disks are listed first and named in 'unhealthy'. Disks not in a pool are included with temperature only. A removed or unavailable pool device whose disk is not present has 'name' null and its serial and model from the TrueNAS disk database; 'last_known_name' is the name it had, which Linux may since have given to another disk. TrueNAS 25.10 has no public SMART test API, so SMART self-test results and attributes are not included. Temperatures are cached by TrueNAS for up to 5 minutes. Use 'name' for one disk or 'pool' for the disks of one pool.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Disk name (exact match, e.g. 'sda' or 'nvme0n1') to return only that disk; a removed pool device whose last known name matches is also returned",
						"minLength":   1,
					},
					"pool": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Return only disks of this pool (exact name, e.g. the boot pool 'boot-pool')",
						"minLength":   1,
					},
				},
			},
		},
		Handler: handleGetDiskHealth,
	}
}

// diskSmartNote is returned by get_disk_health.
const diskSmartNote = "TrueNAS 25.10 has no public SMART test API: SMART self-test results and attributes are not included. ZFS error counts, device state and temperature are."

// zfsLeafHealthy are the leaf device states that are not a problem.
// Hot spares report AVAIL, or INUSE while they stand in for another disk.
var zfsLeafHealthy = map[string]bool{"ONLINE": true, "AVAIL": true, "INUSE": true}

// topologyKinds are the pool topology groups, in output order.
var topologyKinds = []string{"data", "log", "cache", "spare", "special", "dedup"}

// queryDiskRows returns disk.query rows by name, with pool names joined.
// Expired (removed) disks are not included.
func queryDiskRows(ctx context.Context, client *truenas.Client) (map[string]map[string]interface{}, error) {
	raw, err := client.Call(ctx, "disk.query", []interface{}{}, map[string]interface{}{
		"extra": map[string]interface{}{"pools": true},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query disks: %w", err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("failed to parse disks: unexpected result")
	}
	out := make(map[string]map[string]interface{}, len(rows))
	for _, row := range rows {
		if name, ok := row["name"].(string); ok && name != "" {
			out[name] = row
		}
	}
	return out, nil
}

// diskDetailRows returns the disk.details used and unused lists by disk
// name. A disk the kernel reports without a size is in neither.
func diskDetailRows(ctx context.Context, client *truenas.Client) (used, unused map[string]map[string]interface{}, err error) {
	return diskDetailRowsWith(ctx, client, false)
}

// diskDetailRowsWith is diskDetailRows with join_partitions set as given:
// when true, each row's partitions list holds the partitions on the disk
// (25.10 reads them with disk.list_partitions, the same check pool.replace
// uses to decide that a disk is not clean).
func diskDetailRowsWith(ctx context.Context, client *truenas.Client, joinPartitions bool) (used, unused map[string]map[string]interface{}, err error) {
	raw, err := client.Call(ctx, "disk.details", map[string]interface{}{"join_partitions": joinPartitions, "type": "BOTH"})
	if err != nil {
		return nil, nil, err
	}
	var res struct {
		Used   []map[string]interface{} `json:"used"`
		Unused []map[string]interface{} `json:"unused"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, nil, fmt.Errorf("unexpected result")
	}
	index := func(rows []map[string]interface{}) map[string]map[string]interface{} {
		m := make(map[string]map[string]interface{}, len(rows))
		for _, row := range rows {
			if name, ok := row["name"].(string); ok && name != "" {
				m[name] = row
			}
		}
		return m
	}
	return index(res.Used), index(res.Unused), nil
}

// diskType returns HDD, SSD or NVMe. The middleware types every
// non-rotational disk as SSD, so NVMe is recognised by its name, bus,
// driver (disk.details) or subsystem (disk.query).
func diskType(name string, rows ...map[string]interface{}) string {
	if strings.HasPrefix(name, "nvme") {
		return "NVMe"
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		bus, _ := row["bus"].(string)
		driver, _ := row["driver"].(string)
		subsystem, _ := row["subsystem"].(string)
		if strings.EqualFold(bus, "nvme") || driver == "nvme" || strings.EqualFold(subsystem, "nvme") {
			return "NVMe"
		}
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		if t, _ := row["type"].(string); t == "HDD" || t == "SSD" {
			return t
		}
	}
	return "UNKNOWN"
}

// firstString returns the first non-empty string under key in rows.
func firstString(key string, rows ...map[string]interface{}) (string, bool) {
	for _, row := range rows {
		if s, ok := row[key].(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

// firstNumber returns the first positive number under key in rows.
func firstNumber(key string, rows ...map[string]interface{}) (float64, bool) {
	for _, row := range rows {
		if n, ok := row[key].(float64); ok && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// simplifyDisk merges a disk's disk.query row (q) and disk.details row (d),
// either of which may be nil. used reports which disk.details list the
// disk was in (nil when it was in neither or disk.details failed).
func simplifyDisk(name string, q, d map[string]interface{}, used *bool) map[string]interface{} {
	out := map[string]interface{}{"name": name, "type": diskType(name, d, q)}
	for _, key := range []string{"serial", "model", "bus"} {
		if s, ok := firstString(key, q, d); ok {
			out[key] = s
		} else {
			out[key] = nil
		}
	}
	if size, ok := firstNumber("size", q, d); ok {
		out["size_bytes"] = int64(size)
		out["size"] = formatBytes(int64(size))
	} else {
		out["size_bytes"] = nil
	}
	if rpm, ok := firstNumber("rotationrate", q, d); ok {
		out["rotation_rate_rpm"] = int(rpm)
	}
	pool, _ := firstString("pool", q)
	if pool == "" {
		pool, _ = firstString("imported_zpool", d)
	}
	if pool != "" {
		out["pool"] = pool
	} else {
		out["pool"] = nil
	}
	if exported, ok := firstString("exported_zpool", d); ok {
		out["exported_pool"] = exported
	}
	if dup := stringList(d["duplicate_serial"]); len(dup) > 0 {
		out["duplicate_serial"] = dup
	}
	switch {
	case used != nil && *used:
		out["usage"], out["in_use"] = "USED", true
	case used != nil:
		out["usage"], out["in_use"] = "UNUSED", false
	default:
		out["usage"], out["in_use"] = "UNKNOWN", pool != ""
	}
	if q == nil {
		out["note"] = "Not yet in the TrueNAS disk database"
	} else if d == nil && used == nil {
		out["note"] = "Not reported by disk.details (TrueNAS skips disks that report no size, for example a disk that is spun down or failing); in_use is from pool membership only"
	}
	return out
}

// diskNameLess orders disk names so that sdb comes before sdaa.
func diskNameLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func handleQueryDisks(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	name, _ := args["name"].(string)
	pool, _ := args["pool"].(string)
	unusedOnly, _ := args["unused_only"].(bool)
	if unusedOnly && pool != "" {
		return "", fmt.Errorf("invalid arguments for tool query_disks: unused_only=true lists disks that are in no pool and cannot be combined with pool; nothing was executed")
	}
	limit, capped := queryLimit(args)

	rows, err := queryDiskRows(ctx, client)
	if err != nil {
		return "", err
	}
	used, unused, detailsErr := diskDetailRows(ctx, client)
	if unusedOnly && detailsErr != nil {
		return "", fmt.Errorf("failed to classify disks as used or unused: disk.details: %w", detailsErr)
	}

	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	if detailsErr == nil {
		for _, m := range []map[string]map[string]interface{}{used, unused} {
			for n := range m {
				if _, ok := rows[n]; !ok {
					names = append(names, n)
				}
			}
		}
	}
	sort.Slice(names, func(i, j int) bool { return diskNameLess(names[i], names[j]) })

	list := []map[string]interface{}{}
	for _, n := range names {
		if name != "" && n != name {
			continue
		}
		var detail map[string]interface{}
		var inUse *bool
		if detailsErr == nil {
			if d, ok := used[n]; ok {
				detail, inUse = d, boolPtr(true)
			} else if d, ok := unused[n]; ok {
				detail, inUse = d, boolPtr(false)
			}
		}
		disk := simplifyDisk(n, rows[n], detail, inUse)
		if detailsErr != nil {
			delete(disk, "note")
		}
		if pool != "" && disk["pool"] != pool && disk["exported_pool"] != pool {
			continue
		}
		if unusedOnly && disk["usage"] != "UNUSED" {
			continue
		}
		list = append(list, disk)
	}
	if name != "" && len(list) == 0 {
		if pool != "" {
			return "", fmt.Errorf("disk %q not found in pool %q", name, pool)
		}
		return "", fmt.Errorf("disk %q not found", name)
	}

	total := len(list)
	if len(list) > limit {
		list = list[:limit]
	}
	response := map[string]interface{}{
		"disks":       list,
		"count":       len(list),
		"total_disks": total,
	}
	if pool != "" {
		response["pool_filter"] = pool
	}
	note := ""
	if len(list) < total {
		note = fmt.Sprintf("Showing %d of %d disks (limited)", len(list), total)
	}
	if capped {
		note = joinNote(note, "limit was capped to 1000")
	}
	if detailsErr != nil {
		note = joinNote(note, "disk.details failed, so usage is UNKNOWN and in_use is from pool membership only (exported pools are not checked)")
	}
	if note != "" {
		response["note"] = note
	}
	return marshalJSON(response)
}

func boolPtr(b bool) *bool { return &b }

// healthPool is a pool record (pool.query or boot.get_state) for
// get_disk_health.
type healthPool struct {
	name, status string
	healthy      interface{}
	boot         bool
	topology     map[string]interface{}
}

// diskLeaf is a leaf device of a pool topology.
type diskLeaf struct {
	pool, kind, vdev, vdevStatus string
	leaf                         map[string]interface{}
}

// topologyLeaves returns the leaf devices of a pool topology, in
// topologyKinds order. vdev is the leaf's parent vdev (mirror-0,
// raidz1-0, spare-0), empty for a single-disk vdev.
func topologyLeaves(pool string, topology map[string]interface{}) []diskLeaf {
	var out []diskLeaf
	var walk func(kind, parent, parentStatus string, v interface{})
	walk = func(kind, parent, parentStatus string, v interface{}) {
		list, _ := v.([]interface{})
		for _, item := range list {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			kids, _ := m["children"].([]interface{})
			if len(kids) == 0 {
				out = append(out, diskLeaf{pool: pool, kind: kind, vdev: parent, vdevStatus: parentStatus, leaf: m})
				continue
			}
			name, _ := m["name"].(string)
			status, _ := m["status"].(string)
			walk(kind, name, status, kids)
		}
	}
	for _, kind := range topologyKinds {
		walk(kind, "", "", topology[kind])
	}
	return out
}

// diskTemperature is one entry of disk.temperatures with thresholds.
type diskTemperature struct {
	temp, crit float64
	hasTemp    bool
	hasCrit    bool
}

// parseDiskTemperatures reads disk.temperatures(names, true): each value
// is [temperature, critical] or null, and either number may be null.
func parseDiskTemperatures(raw json.RawMessage) (map[string]diskTemperature, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("unexpected result")
	}
	out := make(map[string]diskTemperature, len(m))
	for name, v := range m {
		var t diskTemperature
		switch val := v.(type) {
		case []interface{}:
			if len(val) > 0 {
				t.temp, t.hasTemp = val[0].(float64)
			}
			if len(val) > 1 {
				t.crit, t.hasCrit = val[1].(float64)
				t.hasCrit = t.hasCrit && t.crit > 0
			}
		case float64:
			t.temp, t.hasTemp = val, true
		}
		out[name] = t
	}
	return out, nil
}

// leafErrors returns a leaf's read, write and checksum error counts.
func leafErrors(leaf map[string]interface{}) (counts [3]int64, known bool) {
	stats, ok := leaf["stats"].(map[string]interface{})
	if !ok {
		return counts, false
	}
	for i, key := range []string{"read_errors", "write_errors", "checksum_errors"} {
		if n, ok := stats[key].(float64); ok {
			counts[i] = int64(n)
			known = true
		}
	}
	return counts, known
}

func handleGetDiskHealth(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	name, _ := args["name"].(string)
	poolFilter, _ := args["pool"].(string)

	filters := []interface{}{}
	if poolFilter != "" {
		filters = append(filters, []interface{}{"name", "=", poolFilter})
	}
	raw, err := client.Call(ctx, "pool.query", filters, map[string]interface{}{
		"select": []interface{}{"name", "status", "healthy", "topology"},
	})
	if err != nil {
		return "", fmt.Errorf("failed to query pools: %w", err)
	}
	var poolRows []map[string]interface{}
	if err := json.Unmarshal(raw, &poolRows); err != nil {
		return "", fmt.Errorf("failed to parse pools: unexpected result")
	}
	pools := make([]healthPool, 0, len(poolRows)+1)
	toHealthPool := func(row map[string]interface{}, boot bool) healthPool {
		p := healthPool{healthy: row["healthy"], boot: boot}
		p.name, _ = row["name"].(string)
		p.status, _ = row["status"].(string)
		p.topology, _ = row["topology"].(map[string]interface{})
		return p
	}
	for _, row := range poolRows {
		pools = append(pools, toHealthPool(row, false))
	}

	var notes []string
	if raw, err := client.Call(ctx, "boot.get_state"); err != nil {
		notes = append(notes, "boot pool state unavailable: boot.get_state failed")
	} else {
		var row map[string]interface{}
		if json.Unmarshal(raw, &row) == nil && row != nil {
			if p := toHealthPool(row, true); poolFilter == "" || p.name == poolFilter {
				pools = append(pools, p)
			}
		}
	}
	if poolFilter != "" && len(pools) == 0 {
		return "", fmt.Errorf("pool %q not found", poolFilter)
	}

	inventory, err := queryDiskRows(ctx, client)
	if err != nil {
		return "", err
	}

	// One entry per pool leaf device, then the disks in no pool topology.
	var entries []map[string]interface{}
	inTopology := map[string]bool{}
	poolSummaries := make([]map[string]interface{}, 0, len(pools))
	for _, p := range pools {
		summary := map[string]interface{}{"name": p.name, "status": p.status, "healthy": p.healthy}
		if p.boot {
			summary["boot"] = true
		}
		poolSummaries = append(poolSummaries, summary)
		for _, l := range topologyLeaves(p.name, p.topology) {
			diskName, _ := l.leaf["disk"].(string)
			leafGUID, _ := l.leaf["guid"].(string)
			q := inventory[diskName]
			// A leaf without a disk is removed or unavailable. Its
			// unavail_disk is the expired disk record, named as it was
			// when last seen; Linux may have given that name to another
			// disk since. Only a present disk with the same ZFS guid is
			// the same disk; otherwise keep the record's own identity
			// and do not join temperatures or the no-pool list by name.
			var lastKnown string
			if diskName == "" {
				unavail, _ := l.leaf["unavail_disk"].(map[string]interface{})
				lastKnown, _ = firstString("name", unavail)
				q = unavail
				if row := inventory[lastKnown]; row != nil && leafGUID != "" && zfsGUIDString(row["zfs_guid"]) == leafGUID {
					diskName, q, lastKnown = lastKnown, row, ""
				}
			}
			entry := map[string]interface{}{"pool": p.name, "vdev_kind": l.kind}
			if diskName != "" {
				entry["name"] = diskName
				inTopology[diskName] = true
			} else {
				entry["name"] = nil
				if lastKnown != "" {
					entry["last_known_name"] = lastKnown
				}
			}
			if l.vdev != "" {
				entry["vdev"] = l.vdev
				entry["vdev_status"] = l.vdevStatus
			}
			for _, key := range []string{"device", "guid"} {
				if s, ok := l.leaf[key].(string); ok && s != "" {
					entry[key] = s
				}
			}
			status, _ := l.leaf["status"].(string)
			entry["zfs_status"] = status
			if counts, ok := leafErrors(l.leaf); ok {
				entry["read_errors"], entry["write_errors"], entry["checksum_errors"] = counts[0], counts[1], counts[2]
			}
			typeName := diskName
			if typeName == "" {
				typeName = lastKnown
			}
			addDiskIdentity(entry, typeName, q)
			entries = append(entries, entry)
		}
	}
	if poolFilter == "" {
		for n, q := range inventory {
			if inTopology[n] {
				continue
			}
			entry := map[string]interface{}{"name": n, "pool": nil}
			addDiskIdentity(entry, n, q)
			entries = append(entries, entry)
		}
	}
	if name != "" {
		kept := entries[:0]
		for _, e := range entries {
			if e["name"] == name || e["last_known_name"] == name {
				kept = append(kept, e)
			}
		}
		entries = kept
		if len(entries) == 0 {
			if poolFilter != "" {
				return "", fmt.Errorf("disk %q not found in pool %q", name, poolFilter)
			}
			return "", fmt.Errorf("disk %q not found", name)
		}
	}

	diskNames := []string{}
	seen := map[string]bool{}
	for _, e := range entries {
		if n, ok := e["name"].(string); ok && !seen[n] {
			seen[n] = true
			diskNames = append(diskNames, n)
		}
	}
	sort.Strings(diskNames)

	temps := map[string]diskTemperature{}
	tempsKnown := false
	if len(diskNames) > 0 {
		if raw, err := client.Call(ctx, "disk.temperatures", diskNames, true); err != nil {
			notes = append(notes, "temperatures unavailable: disk.temperatures failed")
		} else if t, err := parseDiskTemperatures(raw); err != nil {
			notes = append(notes, "temperatures unavailable: disk.temperatures returned an unexpected result")
		} else {
			temps, tempsKnown = t, true
		}
	}

	alertsByDisk := map[string][]string{}
	alerts := []map[string]interface{}{}
	if len(diskNames) > 0 {
		if raw, err := client.Call(ctx, "disk.temperature_alerts", diskNames); err != nil {
			notes = append(notes, "temperature alerts unavailable: disk.temperature_alerts failed")
		} else {
			var rows []map[string]interface{}
			if err := json.Unmarshal(raw, &rows); err != nil {
				notes = append(notes, "temperature alerts unavailable: disk.temperature_alerts returned an unexpected result")
			}
			for _, row := range rows {
				a := simplifyAlert(row)
				alerts = append(alerts, a)
				argMap, _ := row["args"].(map[string]interface{})
				dev, _ := argMap["device"].(string)
				text, _ := firstString("formatted", a)
				if text == "" {
					text, _ = firstString("text", a)
				}
				if dismissed, _ := row["dismissed"].(bool); dismissed {
					text = "(dismissed) " + text
				}
				disk := strings.TrimPrefix(dev, "/dev/")
				alertsByDisk[disk] = append(alertsByDisk[disk], text)
			}
		}
	}

	unhealthy := []string{}
	for _, e := range entries {
		n, _ := e["name"].(string)
		var problems []string
		if status, ok := e["zfs_status"].(string); ok && !zfsLeafHealthy[status] {
			if status == "" {
				status = "no state"
			}
			problems = append(problems, fmt.Sprintf("ZFS reports the device as %s", status))
		}
		if _, inPool := e["zfs_status"]; inPool && n == "" {
			problems = append(problems, "the pool device is not matched to any disk on the system (removed or not detected)")
		}
		r, _ := e["read_errors"].(int64)
		w, _ := e["write_errors"].(int64)
		c, _ := e["checksum_errors"].(int64)
		if r+w+c > 0 {
			problems = append(problems, fmt.Sprintf("ZFS counted %d read, %d write and %d checksum errors", r, w, c))
		}
		if n != "" {
			t, ok := temps[n]
			switch {
			case ok && t.hasTemp:
				e["temperature_c"] = t.temp
			case tempsKnown:
				e["temperature_c"] = nil
			}
			if ok && t.hasCrit {
				e["critical_temperature_c"] = t.crit
			}
			if ok && t.hasTemp && t.hasCrit && t.temp >= t.crit {
				problems = append(problems, fmt.Sprintf("temperature %.0f C is at or above the critical %.0f C", t.temp, t.crit))
			}
			for _, text := range alertsByDisk[n] {
				problems = append(problems, "temperature alert: "+text)
			}
		}
		if problems == nil {
			problems = []string{}
		}
		e["problems"] = problems
		e["healthy"] = len(problems) == 0
		if len(problems) > 0 {
			unhealthy = append(unhealthy, diskLabel(e))
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		hi, _ := entries[i]["healthy"].(bool)
		hj, _ := entries[j]["healthy"].(bool)
		if hi != hj {
			return !hi
		}
		ni, nj := diskLabel(entries[i]), diskLabel(entries[j])
		if ni != nj {
			return diskNameLess(ni, nj)
		}
		pi, _ := entries[i]["pool"].(string)
		pj, _ := entries[j]["pool"].(string)
		return pi < pj
	})
	sort.Strings(unhealthy)

	response := map[string]interface{}{
		"pools":              poolSummaries,
		"disks":              entries,
		"count":              len(entries),
		"unhealthy":          unhealthy,
		"unhealthy_count":    len(unhealthy),
		"temperature_alerts": alerts,
		"smart_note":         diskSmartNote,
	}
	if entries == nil {
		response["disks"] = []map[string]interface{}{}
	}
	if len(notes) > 0 {
		response["note"] = strings.Join(notes, "; ")
	}
	return marshalJSON(response)
}

// addDiskIdentity adds the serial, model and type of a disk from its
// disk.query row (or unavail_disk entry), which may be nil.
func addDiskIdentity(entry map[string]interface{}, name string, q map[string]interface{}) {
	for _, key := range []string{"serial", "model"} {
		if s, ok := firstString(key, q); ok {
			entry[key] = s
		}
	}
	if name != "" {
		entry["type"] = diskType(name, q)
	}
}

// zfsGUIDString returns a disk.query zfs_guid as a string, or "". 25.10
// sends guids as strings; a JSON number cannot hold every 64-bit guid
// exactly, so any other type never matches.
func zfsGUIDString(v interface{}) string {
	g, _ := v.(string)
	return g
}

// diskLabel names a get_disk_health entry: the disk name, else the pool
// device guid (with the last known name of a removed disk).
func diskLabel(e map[string]interface{}) string {
	if n, ok := e["name"].(string); ok && n != "" {
		return n
	}
	last, _ := e["last_known_name"].(string)
	if g, ok := e["guid"].(string); ok && g != "" {
		if last != "" {
			return "guid " + g + " (last seen as " + last + ")"
		}
		return "guid " + g
	}
	if last != "" {
		return "last seen as " + last
	}
	return "unknown device"
}

package tools

// Pool disk tools (offline_disk, online_disk, replace_disk).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	poolOfflineMethod = "pool.offline"
	poolOnlineMethod  = "pool.online"
	poolReplaceMethod = "pool.replace"
)

// replaceDiskTaskTTL is how long a pool.replace job is tracked as a task.
const replaceDiskTaskTTL = 24 * time.Hour

func (r *Registry) registerDiskActionTools() {
	poolSchema := map[string]interface{}{"type": "string", "description": "Required: Name of the pool the disk belongs to, as query_pools lists it (e.g. 'tank'). The boot pool is refused", "minLength": 1}
	diskSchema := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc, "minLength": 1}
	}
	dryRunSchema := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc, "default": false}
	}

	r.tools["offline_disk"] = Tool{
		Definition: mcp.Tool{
			Name:        "offline_disk",
			Description: "Take one ONLINE disk of a pool offline with pool.offline, for example before pulling it. ZFS stops using the disk and the pool runs with less redundancy until online_disk brings it back. Allowed only when the pool is ONLINE, reported healthy and has no resilver running, the disk and every other disk of its vdev are ONLINE, and the vdev keeps at least one disk's worth of redundancy afterwards: a 3-way mirror or RAIDZ2 vdev or wider qualifies, a 2-way mirror or RAIDZ1 vdev does not. Refused for single-disk (stripe) vdevs, dRAID and other vdev types, cache and spare devices, a vdev with a replacement or hot spare in progress, and the boot pool. Special, dedup and log vdevs follow the same redundancy rule as data vdevs. There is no force option. The preview shows the vdev layout and the redundancy left after the disk goes offline. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pool":    poolSchema,
					"disk":    diskSchema("Required: The disk to take offline: its name as get_disk_health or query_disks shows it (e.g. 'sdb'), or its ZFS device guid from get_disk_health"),
					"dry_run": dryRunSchema("Optional: Preview without taking the disk offline (default: false)"),
				},
				"required": []string{"pool", "disk"},
			},
		},
		Handler: handleOfflineDisk,
		Plan:    planOfflineDisk,
		Execute: executeOfflineDisk,
	}

	r.tools["online_disk"] = Tool{
		Definition: mcp.Tool{
			Name:        "online_disk",
			Description: "Bring a disk that is OFFLINE in a pool back online with pool.online. ZFS then resilvers the data written to its vdev while the disk was offline. Allowed only for a disk whose ZFS state is OFFLINE in the named pool and that is present on the system; disks in any other state (FAULTED, REMOVED, UNAVAIL, DEGRADED) are refused, and so are a vdev with a replacement or hot spare in progress and the boot pool. There is no force option. The preview shows the vdev layout and the expected resilver. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pool":    poolSchema,
					"disk":    diskSchema("Required: The OFFLINE disk to bring online: its name as get_disk_health or query_disks shows it (e.g. 'sdb'), or its ZFS device guid from get_disk_health"),
					"dry_run": dryRunSchema("Optional: Preview without bringing the disk online (default: false)"),
				},
				"required": []string{"pool", "disk"},
			},
		},
		Handler: handleOnlineDisk,
		Plan:    planOnlineDisk,
		Execute: executeOnlineDisk,
	}

	r.tools["replace_disk"] = Tool{
		Definition: mcp.Tool{
			Name:        "replace_disk",
			Description: "Replace a disk of a pool with an unused disk using pool.replace, which runs as a job tracked with tasks_get. TrueNAS repartitions the replacement disk, so everything on it is destroyed. ZFS then resilvers onto it and detaches the old disk when the resilver finishes. The replacement must be reported unused by TrueNAS (query_disks unused_only), be in no imported or exported pool, not be a boot disk, have no duplicate serial, have no partitions and be at least as large as the disk it replaces; swap, self-encrypting drive locking and other open use of it are not checked. The disk being replaced must be in a mirror or RAIDZ vdev of data, special, dedup or log devices; single-disk (stripe) vdevs, dRAID, cache and spare devices are refused. If its vdev already has a failed or offline disk, only that disk can be replaced; replacing a healthy disk is refused while any device of the pool is not ONLINE. Also refused while a resilver or another pool.replace job is running, for a vdev with a replacement or hot spare in progress, and for the boot pool. There is no force option. The preview shows both disks with their sizes and serials, the vdev layout and that the replacement disk's contents will be destroyed. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pool":             poolSchema,
					"disk":             diskSchema("Required: The disk to replace: its name as get_disk_health shows it (e.g. 'sdb'), or its ZFS device guid from get_disk_health, which is the only way to name a disk that is no longer present"),
					"replacement_disk": diskSchema("Required: Name of the unused disk to put in its place, as query_disks with unused_only lists it (e.g. 'sdk'). Everything on it is destroyed"),
					"dry_run":          dryRunSchema("Optional: Preview without replacing the disk (default: false)"),
				},
				"required": []string{"pool", "disk", "replacement_disk"},
			},
		},
		Handler: r.handleReplaceDisk,
		Plan:    planReplaceDisk,
		Execute: r.executeReplaceDisk,
	}
}

// poolMember is a leaf device of a pool topology.
type poolMember struct {
	GUID   string
	Disk   string // empty when ZFS does not match the device to a present disk
	Status string
	// LastKnown is the name an absent device's disk record had.
	LastKnown string
	Serial    string
	Size      int64 // 0 when unknown
	// Within names the nested vdev (replacing-0, spare-0) holding the
	// device, empty for a direct child of the top-level vdev.
	Within string
}

func (m poolMember) label() string {
	switch {
	case m.Disk != "":
		return m.Disk
	case m.LastKnown != "":
		return fmt.Sprintf("guid %s (last seen as %s)", m.GUID, m.LastKnown)
	}
	return "guid " + m.GUID
}

func (m poolMember) view() map[string]interface{} {
	v := map[string]interface{}{"guid": m.GUID, "status": m.Status, "serial": nilIfEmpty(m.Serial)}
	if m.Disk != "" {
		v["disk"] = m.Disk
	} else {
		v["disk"] = nil
		if m.LastKnown != "" {
			v["last_known_name"] = m.LastKnown
		}
	}
	if m.Size > 0 {
		v["size_bytes"] = m.Size
		v["size"] = formatBytes(m.Size)
	} else {
		v["size_bytes"] = nil
	}
	if m.Within != "" {
		v["within"] = m.Within
	}
	return v
}

func (m poolMember) bind() map[string]interface{} {
	return map[string]interface{}{
		"guid": m.GUID, "disk": m.Disk, "status": m.Status, "last_known": m.LastKnown,
		"serial": m.Serial, "size": m.Size, "within": m.Within,
	}
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// poolVdev is a top-level vdev of a pool topology.
type poolVdev struct {
	Kind   string // data, log, cache, spare, special or dedup
	Type   string // MIRROR, RAIDZ1, DISK, ...
	Name   string
	GUID   string
	Status string
	// Stripe is a single-disk vdev: the top-level entry is the disk.
	Stripe bool
	// Width is the number of direct children (1 for a stripe).
	Width   int
	Members []poolMember
	// Nested names the child vdevs (replacing-N, spare-N) of a vdev with
	// a replacement or hot spare in progress.
	Nested []string
}

// parity is how many failed disks the vdev tolerates when every disk is
// healthy. ok is false for vdev types this server does not reason about.
func (v poolVdev) parity() (int, bool) {
	if v.Stripe {
		return 0, true
	}
	switch v.Type {
	case "MIRROR":
		return v.Width - 1, true
	case "RAIDZ1":
		return 1, true
	case "RAIDZ2":
		return 2, true
	case "RAIDZ3":
		return 3, true
	}
	return 0, false
}

// notOnline returns the direct members of the vdev that are not ONLINE,
// skipping skip (a guid).
func (v poolVdev) notOnline(skip string) []poolMember {
	var out []poolMember
	for _, m := range v.Members {
		if m.Within == "" && m.Status != "ONLINE" && m.GUID != skip {
			out = append(out, m)
		}
	}
	return out
}

func (v poolVdev) typeText() string {
	if v.Stripe {
		return "single disk (stripe)"
	}
	return v.Type
}

func (v poolVdev) label() string {
	if v.Stripe {
		return fmt.Sprintf("single-disk %s vdev %s", v.Kind, v.Members[0].label())
	}
	return fmt.Sprintf("%s vdev %s", v.Kind, v.Name)
}

// layoutText is one line per vdev: its type, state and each disk's state,
// with the states in override (by guid) shown instead.
func (v poolVdev) layoutText(override map[string]string) string {
	parts := make([]string, 0, len(v.Members))
	for _, m := range v.Members {
		st := m.Status
		if o, ok := override[m.GUID]; ok {
			st = o
		}
		parts = append(parts, fmt.Sprintf("%s %s", m.label(), st))
	}
	return fmt.Sprintf("%s (%s, %s): %s", v.label(), v.typeText(), v.Status, strings.Join(parts, ", "))
}

func (v poolVdev) view() map[string]interface{} {
	members := make([]interface{}, len(v.Members))
	for i, m := range v.Members {
		members[i] = m.view()
	}
	out := map[string]interface{}{
		"kind": v.Kind, "type": v.typeText(), "status": v.Status, "width": v.Width, "disks": members,
	}
	if !v.Stripe {
		out["name"] = v.Name
	}
	if p, ok := v.parity(); ok {
		out["tolerates_failed_disks"] = p
	}
	if len(v.Nested) > 0 {
		out["in_progress"] = v.Nested
	}
	return out
}

func (v poolVdev) bind() map[string]interface{} {
	members := make([]interface{}, len(v.Members))
	for i, m := range v.Members {
		members[i] = map[string]interface{}{"guid": m.GUID, "disk": m.Disk, "status": m.Status, "within": m.Within}
	}
	return map[string]interface{}{
		"kind": v.Kind, "type": v.Type, "name": v.Name, "guid": v.GUID, "status": v.Status,
		"stripe": v.Stripe, "width": v.Width, "members": members, "nested": v.Nested,
	}
}

// poolDiskState is what the pool disk tools read about a pool.
type poolDiskState struct {
	ID          int
	Name        string
	Status      string
	Healthy     bool
	Resilvering bool
	Allocated   int64
	Vdevs       []poolVdev
}

func (p poolDiskState) view() map[string]interface{} {
	return map[string]interface{}{
		"name": p.Name, "id": p.ID, "status": p.Status, "healthy": p.Healthy, "resilver_running": p.Resilvering,
	}
}

func (p poolDiskState) bind() map[string]interface{} {
	vdevs := make([]interface{}, len(p.Vdevs))
	for i, v := range p.Vdevs {
		vdevs[i] = v.bind()
	}
	return map[string]interface{}{
		"id": p.ID, "name": p.Name, "status": p.Status, "healthy": p.Healthy,
		"resilvering": p.Resilvering, "vdevs": vdevs,
	}
}

// find returns the vdev and member index of the device named disk, by
// disk name or ZFS guid.
func (p poolDiskState) find(disk string) (*poolVdev, int) {
	for vi := range p.Vdevs {
		for mi, m := range p.Vdevs[vi].Members {
			if m.GUID == disk || (m.Disk != "" && m.Disk == disk) {
				return &p.Vdevs[vi], mi
			}
		}
	}
	return nil, -1
}

// lastSeenAs returns the guids of absent devices last seen as name.
func (p poolDiskState) lastSeenAs(name string) []string {
	var out []string
	for _, v := range p.Vdevs {
		for _, m := range v.Members {
			if m.Disk == "" && m.LastKnown == name {
				out = append(out, m.GUID)
			}
		}
	}
	return out
}

func poolMemberFrom(leaf map[string]interface{}, disks map[string]map[string]interface{}, within string) poolMember {
	m := poolMember{GUID: zfsGUIDString(leaf["guid"]), Within: within}
	m.Disk, _ = leaf["disk"].(string)
	m.Status, _ = leaf["status"].(string)
	var row map[string]interface{}
	if m.Disk != "" {
		row = disks[m.Disk]
	} else {
		row, _ = leaf["unavail_disk"].(map[string]interface{})
		m.LastKnown, _ = firstString("name", row)
	}
	m.Serial, _ = firstString("serial", row)
	if n, ok := firstNumber("size", row); ok {
		m.Size = int64(n)
	}
	return m
}

func parsePoolDiskState(row map[string]interface{}, disks map[string]map[string]interface{}) (poolDiskState, error) {
	p := poolDiskState{}
	p.Name, _ = row["name"].(string)
	p.Status, _ = row["status"].(string)
	p.Healthy, _ = row["healthy"].(bool)
	id, ok := row["id"].(float64)
	if !ok {
		return p, fmt.Errorf("pool.query did not report the id of pool '%s'", p.Name)
	}
	p.ID, ok = wholePositiveInt(id)
	if !ok {
		return p, fmt.Errorf("pool.query reported an invalid id for pool '%s'", p.Name)
	}
	if n, ok := row["allocated"].(float64); ok && n > 0 {
		p.Allocated = int64(n)
	}
	if scan, ok := row["scan"].(map[string]interface{}); ok {
		fn, _ := scan["function"].(string)
		state, _ := scan["state"].(string)
		p.Resilvering = fn == "RESILVER" && state == "SCANNING"
	}
	topology, ok := row["topology"].(map[string]interface{})
	if !ok {
		return p, fmt.Errorf("pool.query did not report the topology of pool '%s'", p.Name)
	}
	for _, kind := range topologyKinds {
		items, _ := topology[kind].([]interface{})
		for _, item := range items {
			top, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			v := poolVdev{Kind: kind, GUID: zfsGUIDString(top["guid"])}
			v.Type, _ = top["type"].(string)
			v.Name, _ = top["name"].(string)
			v.Status, _ = top["status"].(string)
			kids, _ := top["children"].([]interface{})
			if len(kids) == 0 {
				v.Stripe, v.Width = true, 1
				v.Members = []poolMember{poolMemberFrom(top, disks, "")}
				p.Vdevs = append(p.Vdevs, v)
				continue
			}
			v.Width = len(kids)
			for _, k := range kids {
				child, ok := k.(map[string]interface{})
				if !ok {
					continue
				}
				grand, _ := child["children"].([]interface{})
				if len(grand) == 0 {
					v.Members = append(v.Members, poolMemberFrom(child, disks, ""))
					continue
				}
				name, _ := child["name"].(string)
				if name == "" {
					name, _ = child["type"].(string)
				}
				v.Nested = append(v.Nested, name)
				for _, g := range grand {
					if leaf, ok := g.(map[string]interface{}); ok {
						v.Members = append(v.Members, poolMemberFrom(leaf, disks, name))
					}
				}
			}
			p.Vdevs = append(p.Vdevs, v)
		}
	}
	return p, nil
}

// bootPoolDisks returns the boot pool's name and the disks it uses, from
// boot.get_state. An unreadable boot pool fails the plan: without it the
// tools cannot tell a boot disk from any other.
func bootPoolDisks(ctx context.Context, client *truenas.Client) (string, map[string]bool, error) {
	raw, err := client.Call(ctx, "boot.get_state")
	if err != nil {
		return "", nil, fmt.Errorf("could not read the boot pool (boot.get_state failed: %w), so boot disks cannot be told apart; nothing was changed", err)
	}
	var row map[string]interface{}
	if err := json.Unmarshal(raw, &row); err != nil || row == nil {
		return "", nil, fmt.Errorf("could not read the boot pool (boot.get_state returned an unexpected result), so boot disks cannot be told apart; nothing was changed")
	}
	name, _ := row["name"].(string)
	topology, _ := row["topology"].(map[string]interface{})
	if name == "" || topology == nil {
		return "", nil, fmt.Errorf("could not read the boot pool (boot.get_state reported no name or topology), so boot disks cannot be told apart; nothing was changed")
	}
	disks := map[string]bool{}
	for _, l := range topologyLeaves(name, topology) {
		if d, _ := l.leaf["disk"].(string); d != "" {
			disks[d] = true
		}
	}
	return name, disks, nil
}

// poolDiskTarget is the pool and device a disk tool acts on.
type poolDiskTarget struct {
	pool      poolDiskState
	vdev      poolVdev
	member    poolMember
	bootName  string
	bootDisks map[string]bool
	disks     map[string]map[string]interface{}
}

// loadPoolDiskTarget reads the pool, the boot pool and the disk inventory
// and finds the device named by args["disk"]. Problems with the names
// themselves are errors; the state checks are left to each tool.
func loadPoolDiskTarget(ctx context.Context, client *truenas.Client, tool string, args map[string]interface{}) (*poolDiskTarget, error) {
	poolName, _ := args["pool"].(string)
	disk, _ := args["disk"].(string)
	if strings.TrimSpace(poolName) == "" {
		return nil, fmt.Errorf("pool is required")
	}
	if strings.TrimSpace(disk) == "" {
		return nil, fmt.Errorf("disk is required")
	}
	for _, b := range bootPoolNames {
		if poolName == b {
			return nil, fmt.Errorf("'%s' is the boot pool; %s does not change boot pool disks; nothing was changed", poolName, tool)
		}
	}
	bootName, bootDisks, err := bootPoolDisks(ctx, client)
	if err != nil {
		return nil, err
	}
	if poolName == bootName {
		return nil, fmt.Errorf("'%s' is the boot pool; %s does not change boot pool disks; nothing was changed", poolName, tool)
	}
	raw, err := client.Call(ctx, "pool.query",
		[]interface{}{[]interface{}{"name", "=", poolName}},
		map[string]interface{}{"select": []interface{}{"id", "name", "status", "healthy", "allocated", "scan", "topology"}},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query pool '%s': %w", poolName, err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected pool.query result")
	}
	var row map[string]interface{}
	for _, r := range rows {
		if n, _ := r["name"].(string); n == poolName {
			row = r
			break
		}
	}
	if row == nil {
		return nil, fmt.Errorf("pool '%s' not found (query_pools lists the pools)", poolName)
	}
	disks, err := queryDiskRows(ctx, client)
	if err != nil {
		return nil, err
	}
	pool, err := parsePoolDiskState(row, disks)
	if err != nil {
		return nil, err
	}
	v, i := pool.find(disk)
	if v == nil {
		if guids := pool.lastSeenAs(disk); len(guids) > 0 {
			return nil, fmt.Errorf("no disk named '%s' is present in pool '%s'; a device that is no longer present was last seen as '%s' (guid %s); name it by its guid, which get_disk_health shows", disk, poolName, disk, strings.Join(guids, ", "))
		}
		if bootDisks[disk] {
			return nil, fmt.Errorf("disk '%s' is a boot pool disk, not a disk of pool '%s'; %s does not change boot pool disks; nothing was changed", disk, poolName, tool)
		}
		return nil, fmt.Errorf("disk '%s' is not a device of pool '%s' (get_disk_health with pool '%s' lists its disks and their guids)", disk, poolName, poolName)
	}
	if v.Members[i].GUID == "" {
		return nil, fmt.Errorf("pool.query reported no ZFS guid (as a string) for disk '%s' of pool '%s', and %s names the device to TrueNAS only by its guid; nothing was changed", disk, poolName, tool)
	}
	return &poolDiskTarget{pool: pool, vdev: *v, member: v.Members[i], bootName: bootName, bootDisks: bootDisks, disks: disks}, nil
}

func (t *poolDiskTarget) bind() map[string]interface{} {
	return map[string]interface{}{
		"pool":   t.pool.bind(),
		"vdev":   t.vdev.GUID,
		"target": t.member.bind(),
	}
}

func (t *poolDiskTarget) current() map[string]interface{} {
	return map[string]interface{}{
		"pool": t.pool.view(),
		"disk": t.member.view(),
		"vdev": t.vdev.view(),
	}
}

func (t *poolDiskTarget) diskFact() string {
	m := t.member
	parts := []string{m.label()}
	if m.Serial != "" {
		parts = append(parts, "serial "+m.Serial)
	}
	if m.Size > 0 {
		parts = append(parts, formatBytes(m.Size))
	}
	parts = append(parts, "guid "+m.GUID, m.Status)
	return strings.Join(parts, ", ")
}

// staleNameNote says that the name and serial shown for the target come
// from the pool's device path, which can be stale on older pools, while
// the call names the device by its guid.
func (t *poolDiskTarget) staleNameNote() string {
	return fmt.Sprintf("The disk name and serial shown for %s are matched from the pool's device path; on older pools that path (/dev/sdX) can be stale, so the name or serial shown may not be the physical disk's. The call names the device by its ZFS guid %s, which is not affected; check the guid with get_disk_health", t.member.label(), t.member.GUID)
}

// commonDiskBlocks are the refusals every disk tool shares: devices this
// server does not change and vdevs with a replacement in progress.
func (t *poolDiskTarget) commonDiskBlocks(action string) []string {
	var reasons []string
	switch t.vdev.Kind {
	case "cache":
		reasons = append(reasons, fmt.Sprintf("%s is a cache (L2ARC) device; %s does not change cache devices", t.member.label(), action))
	case "spare":
		reasons = append(reasons, fmt.Sprintf("%s is a hot spare; %s does not change spare devices", t.member.label(), action))
	}
	if len(t.vdev.Nested) > 0 {
		reasons = append(reasons, fmt.Sprintf("%s has a replacement or hot spare in progress (%s); wait for it to finish (get_disk_health shows the vdev) before changing its disks", t.vdev.label(), strings.Join(t.vdev.Nested, ", ")))
	}
	return reasons
}

func diskPlanBlocked(plan *writePlan, reasons []string) *writePlan {
	plan.Blocked = reasons
	plan.Actions = nil
	plan.Warnings = nil
	plan.Result = nil
	return plan
}

func failedLabels(ms []poolMember) string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = fmt.Sprintf("%s (%s)", m.label(), m.Status)
	}
	return strings.Join(out, ", ")
}

// ---- offline_disk ----

func planOfflineDisk(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	t, err := loadPoolDiskTarget(ctx, client, "offline_disk", args)
	if err != nil {
		return nil, err
	}
	m, v := t.member, t.vdev
	parity, parityOK := v.parity()
	remaining := parity - len(v.notOnline("")) - 1
	current := t.current()
	after := v.layoutText(map[string]string{m.GUID: "OFFLINE"})
	current["vdev_layout"] = v.layoutText(nil)
	headline := fmt.Sprintf("Take disk %s of pool %s offline", m.label(), t.pool.Name)
	plan := &writePlan{
		Tool:         "offline_disk",
		Method:       poolOfflineMethod,
		CallParams:   []interface{}{t.pool.ID, map[string]interface{}{"label": m.GUID}},
		ParamNames:   []string{"id", "options"},
		TargetName:   m.label(),
		CurrentState: current,
		Bind:         t.bind(),
		ConfirmSummary: newConfirmSummary(headline,
			confirmFact("pool", t.pool.Name),
			confirmFact("disk", t.diskFact()),
			confirmFact("vdev now", v.layoutText(nil)),
			confirmFact("vdev after", after),
			confirmFact("redundancy left in the vdev", redundancyText(remaining)),
		),
	}

	reasons := t.commonDiskBlocks("offline_disk")
	if t.pool.Status != "ONLINE" {
		reasons = append(reasons, fmt.Sprintf("pool '%s' is %s, not ONLINE; taking another disk offline while a device is already failed or offline could leave the pool without redundancy, so resolve that first (get_disk_health shows which device)", t.pool.Name, orUnknown(t.pool.Status)))
	} else if !t.pool.Healthy {
		reasons = append(reasons, fmt.Sprintf("pool '%s' is ONLINE but TrueNAS does not report it healthy (for example a disk with read, write or checksum errors); taking a disk offline is refused until it is, so check get_disk_health first", t.pool.Name))
	}
	if t.pool.Resilvering {
		reasons = append(reasons, fmt.Sprintf("a resilver is running on pool '%s'; wait for it to finish before taking a disk offline", t.pool.Name))
	}
	if m.Status != "ONLINE" {
		reasons = append(reasons, fmt.Sprintf("disk %s is %s, not ONLINE; offline_disk only takes healthy disks offline (use replace_disk to replace a failed disk)", m.label(), orUnknown(m.Status)))
	}
	switch {
	case v.Kind == "cache" || v.Kind == "spare":
	case v.Stripe:
		reasons = append(reasons, fmt.Sprintf("%s has no redundancy: taking its only disk offline would take the whole vdev offline", v.label()))
	case !parityOK:
		reasons = append(reasons, fmt.Sprintf("%s is a %s vdev; offline_disk only reasons about mirror and RAIDZ vdevs", v.label(), orUnknown(v.Type)))
	default:
		if others := v.notOnline(m.GUID); len(others) > 0 {
			reasons = append(reasons, fmt.Sprintf("%s is already missing redundancy: %s; taking another disk offline is refused", v.label(), failedLabels(others)))
		} else if remaining < 1 {
			reasons = append(reasons, fmt.Sprintf("taking %s offline would leave %s (%s of %d disks, tolerates %d failed) with no redundancy: one more disk failure would fail the vdev; offline_disk needs at least one disk's worth of redundancy left (a 3-way mirror or RAIDZ2 vdev or wider)", m.label(), v.label(), v.Type, v.Width, parity))
		}
	}
	if len(reasons) > 0 {
		return diskPlanBlocked(plan, reasons), nil
	}

	plan.Actions = []PlannedAction{{
		Step:        1,
		Description: fmt.Sprintf("Take disk %s offline in %s of pool '%s'; %s", m.label(), v.label(), t.pool.Name, redundancyText(remaining)),
		Operation:   "offline",
		Target:      m.label(),
		Details: map[string]interface{}{
			"pool":                       t.pool.Name,
			"disk":                       m.label(),
			"guid":                       m.GUID,
			"vdev":                       v.Name,
			"vdev_layout_after":          after,
			"redundancy_left_after":      remaining,
			"tolerates_failed_disks_now": parity,
			"force":                      false,
		},
	}}
	plan.Warnings = []string{
		fmt.Sprintf("Until %s is back online, %s tolerates %d more failed disk(s) instead of %d", m.label(), v.label(), remaining, parity),
		"ZFS stops writing to the disk; the pool shows as DEGRADED while it is offline",
		"Bringing it back with online_disk resilvers the data written while it was offline; if the disk is replaced instead, use replace_disk",
		"The offline state persists across reboots until the disk is brought online",
		t.staleNameNote(),
	}
	plan.Result = map[string]interface{}{"pool": t.pool.Name, "guid": m.GUID, "vdev": v.Name}
	return plan, nil
}

func redundancyText(n int) string {
	switch {
	case n < 0:
		return "the vdev would fail"
	case n == 0:
		return "no redundancy left"
	case n == 1:
		return "1 more disk failure tolerated"
	}
	return fmt.Sprintf("%d more disk failures tolerated", n)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "in an unknown state"
	}
	return s
}

func handleOfflineDisk(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planOfflineDisk(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeOfflineDisk(ctx, client, args, plan)
}

func executeOfflineDisk(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	return executeOnOffDisk(ctx, client, plan, "OFFLINE", "take", "offline")
}

// executeOnOffDisk sends pool.offline or pool.online and reads the
// device back.
func executeOnOffDisk(ctx context.Context, client *truenas.Client, plan *writePlan, want, verb, word string) (string, error) {
	poolName, _ := plan.Result["pool"].(string)
	guid, _ := plan.Result["guid"].(string)
	disk := plan.TargetName
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to %s disk %s %s in pool '%s': %w; check get_disk_health before retrying", verb, disk, word, poolName, err)
	}
	var ok bool
	if err := json.Unmarshal(result, &ok); err != nil || !ok {
		return "", fmt.Errorf("%s returned an unexpected result; check the state of disk %s with get_disk_health before retrying", plan.Method, disk)
	}
	response := map[string]interface{}{
		"pool": poolName,
		"disk": disk,
		"guid": guid,
		word:   true,
	}
	msg := fmt.Sprintf("Disk %s of pool '%s' is %s", disk, poolName, word)
	t, readErr := loadPoolDiskTarget(ctx, client, plan.Tool, map[string]interface{}{"pool": poolName, "disk": guid})
	switch {
	case readErr != nil:
		response["note"] = "TrueNAS reported success, but the pool could not be read back: " + boundedErr(readErr)
	default:
		response["status_after"] = t.member.Status
		response["pool_status"] = t.pool.Status
		response["vdev_layout"] = t.vdev.layoutText(nil)
		if t.member.Status != want {
			response["note"] = fmt.Sprintf("TrueNAS reported success, but the disk is %s when read back, not %s; check get_disk_health", orUnknown(t.member.Status), want)
		}
		if want == "ONLINE" && t.pool.Resilvering {
			msg += "; a resilver is running (get_disk_health and query_pools show its progress)"
		}
	}
	if want == "OFFLINE" {
		msg += "; bring it back with online_disk"
	}
	response["message"] = msg
	return marshalJSON(response)
}

// ---- online_disk ----

func planOnlineDisk(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	t, err := loadPoolDiskTarget(ctx, client, "online_disk", args)
	if err != nil {
		return nil, err
	}
	m, v := t.member, t.vdev
	current := t.current()
	current["vdev_layout"] = v.layoutText(nil)
	after := v.layoutText(map[string]string{m.GUID: "ONLINE (resilvering)"})
	resilver := fmt.Sprintf("ZFS resilvers onto %s the data written to %s while the disk was offline (only the changed blocks, not a full rebuild); the vdev regains its redundancy when that finishes", m.label(), v.label())
	plan := &writePlan{
		Tool:         "online_disk",
		Method:       poolOnlineMethod,
		CallParams:   []interface{}{t.pool.ID, map[string]interface{}{"label": m.GUID}},
		ParamNames:   []string{"id", "options"},
		TargetName:   m.label(),
		CurrentState: current,
		Bind:         t.bind(),
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Bring disk %s of pool %s back online", m.label(), t.pool.Name),
			confirmFact("pool", t.pool.Name),
			confirmFact("disk", t.diskFact()),
			confirmFact("vdev now", v.layoutText(nil)),
			confirmFact("expected resilver", resilver),
		),
	}
	reasons := t.commonDiskBlocks("online_disk")
	if m.Status != "OFFLINE" {
		reasons = append(reasons, fmt.Sprintf("disk %s is %s, not OFFLINE; online_disk only brings back a disk that was taken offline (use replace_disk for a failed or missing disk)", m.label(), orUnknown(m.Status)))
	} else if m.Disk == "" {
		reasons = append(reasons, fmt.Sprintf("the OFFLINE device %s is not matched to any disk present on the system; reconnect the disk (or replace it with replace_disk) first", m.label()))
	}
	if len(reasons) > 0 {
		return diskPlanBlocked(plan, reasons), nil
	}
	warnings := []string{
		resilver,
		"Pool I/O may be slower while the resilver runs",
		t.staleNameNote(),
	}
	if t.pool.Resilvering {
		warnings = append(warnings, fmt.Sprintf("A resilver is already running on pool '%s'; ZFS may restart it to include this disk", t.pool.Name))
	}
	if others := v.notOnline(m.GUID); len(others) > 0 {
		warnings = append(warnings, fmt.Sprintf("Other devices of %s are not ONLINE: %s", v.label(), failedLabels(others)))
	}
	plan.Actions = []PlannedAction{{
		Step:        1,
		Description: fmt.Sprintf("Bring disk %s back online in %s of pool '%s'", m.label(), v.label(), t.pool.Name),
		Operation:   "online",
		Target:      m.label(),
		Details: map[string]interface{}{
			"pool":              t.pool.Name,
			"disk":              m.label(),
			"guid":              m.GUID,
			"vdev":              v.Name,
			"vdev_layout_after": after,
			"expected_resilver": "partial: only data written while the disk was offline",
			"force":             false,
		},
	}}
	plan.Warnings = warnings
	plan.Result = map[string]interface{}{"pool": t.pool.Name, "guid": m.GUID, "vdev": v.Name}
	return plan, nil
}

func handleOnlineDisk(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planOnlineDisk(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeOnlineDisk(ctx, client, args, plan)
}

func executeOnlineDisk(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	return executeOnOffDisk(ctx, client, plan, "ONLINE", "bring", "online")
}

// ---- replace_disk ----

// replacementDisk is what replace_disk reads about the new disk.
type replacementDisk struct {
	Name       string
	Serial     string
	Model      string
	Identifier string // from disk.query, bound in the token
	// DetailIdentifier is the identifier disk.details reports, which
	// pool.replace looks the disk up by.
	DetailIdentifier string
	// Partitions names the partitions disk.details (join_partitions)
	// reports on the disk; PartitionsKnown is false when it reported none
	// as a list.
	Partitions      []string
	PartitionsKnown bool
	Size            int64
	Unused          bool
	Used            bool
	Pool            string
	Exported        string
	Boot            bool
	DupSerial       []string
	InDiskQuery     bool
}

func (d replacementDisk) bind() map[string]interface{} {
	return map[string]interface{}{
		"name": d.Name, "serial": d.Serial, "identifier": d.Identifier, "details_identifier": d.DetailIdentifier,
		"partitions": d.Partitions, "partitions_known": d.PartitionsKnown, "size": d.Size,
		"unused": d.Unused, "used": d.Used, "pool": d.Pool, "exported_pool": d.Exported,
		"boot": d.Boot, "duplicate_serial": d.DupSerial, "known": d.InDiskQuery,
	}
}

func (d replacementDisk) view() map[string]interface{} {
	v := map[string]interface{}{
		"name": d.Name, "serial": nilIfEmpty(d.Serial), "model": nilIfEmpty(d.Model),
		"unused": d.Unused, "pool": nilIfEmpty(d.Pool), "boot_disk": d.Boot,
	}
	if d.Size > 0 {
		v["size_bytes"] = d.Size
		v["size"] = formatBytes(d.Size)
	} else {
		v["size_bytes"] = nil
	}
	if d.Exported != "" {
		v["exported_pool"] = d.Exported
	}
	if len(d.DupSerial) > 0 {
		v["duplicate_serial"] = d.DupSerial
	}
	if len(d.Partitions) > 0 {
		v["partitions"] = d.Partitions
	}
	return v
}

func (d replacementDisk) fact() string {
	parts := []string{d.Name}
	if d.Serial != "" {
		parts = append(parts, "serial "+d.Serial)
	}
	if d.Size > 0 {
		parts = append(parts, formatBytes(d.Size))
	}
	return strings.Join(parts, ", ")
}

func readReplacementDisk(ctx context.Context, client *truenas.Client, name string, t *poolDiskTarget) (replacementDisk, error) {
	d := replacementDisk{Name: name, Boot: t.bootDisks[name]}
	used, unused, err := diskDetailRowsWith(ctx, client, true)
	if err != nil {
		return d, fmt.Errorf("could not check that disk '%s' is unused (disk.details failed: %w); nothing was changed", name, err)
	}
	var detail map[string]interface{}
	if row, ok := unused[name]; ok {
		d.Unused, detail = true, row
	}
	if row, ok := used[name]; ok {
		d.Used, detail = true, row
	}
	q, inQuery := t.disks[name]
	d.InDiskQuery = inQuery
	d.Serial, _ = firstString("serial", q, detail)
	d.Model, _ = firstString("model", q, detail)
	d.Identifier, _ = firstString("identifier", q)
	d.DetailIdentifier, _ = firstString("identifier", detail)
	if parts, ok := detail["partitions"].([]interface{}); ok {
		d.PartitionsKnown = true
		for _, p := range parts {
			name := "unnamed partition"
			if row, ok := p.(map[string]interface{}); ok {
				if n, ok := row["name"].(string); ok && n != "" {
					name = n
				}
			}
			d.Partitions = append(d.Partitions, name)
		}
	}
	if n, ok := firstNumber("size", q, detail); ok {
		d.Size = int64(n)
	}
	d.Pool, _ = firstString("pool", q)
	if d.Pool == "" {
		d.Pool, _ = firstString("imported_zpool", detail)
	}
	d.Exported, _ = firstString("exported_zpool", detail)
	d.DupSerial = stringList(detail["duplicate_serial"])
	return d, nil
}

// runningReplaceJobs returns the ids of pool.replace jobs that are waiting
// or running.
func runningReplaceJobs(ctx context.Context, client *truenas.Client) ([]string, error) {
	raw, err := client.Call(ctx, "core.get_jobs", []interface{}{
		[]interface{}{"method", "=", poolReplaceMethod},
		[]interface{}{"state", "in", []interface{}{"WAITING", "RUNNING"}},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to check for running disk replacements (core.get_jobs): %w", err)
	}
	var jobs []map[string]interface{}
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return nil, fmt.Errorf("unexpected core.get_jobs result")
	}
	var out []string
	for _, j := range jobs {
		method, _ := j["method"].(string)
		state, _ := j["state"].(string)
		if method == poolReplaceMethod && (state == "WAITING" || state == "RUNNING") {
			out = append(out, fmt.Sprint(j["id"]))
		}
	}
	return out, nil
}

func planReplaceDisk(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	newName, _ := args["replacement_disk"].(string)
	if strings.TrimSpace(newName) == "" {
		return nil, fmt.Errorf("replacement_disk is required")
	}
	t, err := loadPoolDiskTarget(ctx, client, "replace_disk", args)
	if err != nil {
		return nil, err
	}
	m, v := t.member, t.vdev
	if newName == m.Disk || newName == m.GUID {
		return nil, fmt.Errorf("replacement_disk '%s' is the disk being replaced; name an unused disk (query_disks with unused_only lists them)", newName)
	}
	nd, err := readReplacementDisk(ctx, client, newName, t)
	if err != nil {
		return nil, err
	}
	jobs, err := runningReplaceJobs(ctx, client)
	if err != nil {
		return nil, err
	}

	parity, parityOK := v.parity()
	failed := v.notOnline("")
	others := v.notOnline(m.GUID)
	current := t.current()
	current["vdev_layout"] = v.layoutText(nil)
	current["replacement_disk"] = nd.view()
	bind := t.bind()
	bind["replacement"] = nd.bind()
	bind["running_replace_jobs"] = jobs
	sizeFact := "unknown"
	if m.Size > 0 && nd.Size > 0 {
		sizeFact = fmt.Sprintf("%s replaced by %s", formatBytes(m.Size), formatBytes(nd.Size))
	}
	// 25.10 pool.replace finds the new disk by its disk.details
	// identifier (find_disk_from_identifier), not by its name.
	options := map[string]interface{}{"label": m.GUID, "disk": nd.DetailIdentifier, "force": false}
	plan := &writePlan{
		Tool:         "replace_disk",
		Method:       poolReplaceMethod,
		CallParams:   []interface{}{t.pool.ID, options},
		ParamNames:   []string{"id", "options"},
		JobMethod:    poolReplaceMethod,
		TargetName:   m.label(),
		CurrentState: current,
		Bind:         bind,
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Replace disk %s of pool %s with %s; everything on %s is destroyed", m.label(), t.pool.Name, newName, newName),
			confirmFact("pool", t.pool.Name),
			confirmFact("disk replaced", t.diskFact()),
			confirmFact("replacement disk (contents destroyed)", nd.fact()),
			confirmFact("sizes", sizeFact),
			confirmFact("vdev", v.layoutText(nil)),
		),
	}

	reasons := t.commonDiskBlocks("replace_disk")
	if t.pool.Resilvering {
		reasons = append(reasons, fmt.Sprintf("a resilver is running on pool '%s'; wait for it to finish before replacing a disk", t.pool.Name))
	}
	if len(jobs) > 0 {
		reasons = append(reasons, fmt.Sprintf("a disk replacement is already running (pool.replace job %s); wait for it to finish", strings.Join(jobs, ", ")))
	}
	switch {
	case v.Kind == "cache" || v.Kind == "spare":
	case v.Stripe:
		reasons = append(reasons, fmt.Sprintf("%s has no redundancy; replace_disk only replaces disks in mirror and RAIDZ vdevs", v.label()))
	case !parityOK:
		reasons = append(reasons, fmt.Sprintf("%s is a %s vdev; replace_disk only reasons about mirror and RAIDZ vdevs", v.label(), orUnknown(v.Type)))
	case len(failed) > parity || v.Status == "FAULTED" || v.Status == "UNAVAIL":
		reasons = append(reasons, fmt.Sprintf("%s is %s with %d failed or offline disk(s) (%s), more than it tolerates; replacing a disk cannot restore it", v.label(), orUnknown(v.Status), len(failed), failedLabels(failed)))
	case m.Status == "ONLINE" && len(others) > 0:
		reasons = append(reasons, fmt.Sprintf("%s is already degraded: %s; replacing the healthy disk %s now would risk its last redundancy, so replace the failed or offline disk first", v.label(), failedLabels(others), m.label()))
	case m.Status == "ONLINE" && t.pool.Status != "ONLINE":
		reasons = append(reasons, fmt.Sprintf("pool '%s' is %s; replace the failed or offline device first (get_disk_health shows it) before replacing the healthy disk %s", t.pool.Name, orUnknown(t.pool.Status), m.label()))
	}
	if m.Size <= 0 {
		reasons = append(reasons, fmt.Sprintf("the size of %s is not known, so the replacement cannot be checked to be at least as large", m.label()))
	}
	switch {
	case nd.Boot:
		reasons = append(reasons, fmt.Sprintf("disk '%s' is a boot disk of '%s'", newName, t.bootName))
	case nd.Pool != "":
		reasons = append(reasons, fmt.Sprintf("disk '%s' is part of pool '%s'", newName, nd.Pool))
	case nd.Exported != "":
		reasons = append(reasons, fmt.Sprintf("disk '%s' holds exported pool '%s', which could still be imported", newName, nd.Exported))
	case nd.Used:
		reasons = append(reasons, fmt.Sprintf("disk '%s' is in use according to TrueNAS (disk.details)", newName))
	case !nd.Unused:
		reasons = append(reasons, fmt.Sprintf("disk '%s' is not reported as an unused disk by TrueNAS (query_disks with unused_only lists the unused disks)", newName))
	}
	if !nd.InDiskQuery {
		reasons = append(reasons, fmt.Sprintf("disk '%s' is not in the TrueNAS disk database yet, so its identity cannot be bound to the confirmation", newName))
	}
	if nd.Unused || nd.Used {
		switch {
		case nd.DetailIdentifier == "":
			reasons = append(reasons, fmt.Sprintf("TrueNAS (disk.details) reports no identifier for disk '%s', which pool.replace needs to find the disk", newName))
		case nd.DetailIdentifier != nd.Identifier:
			reasons = append(reasons, fmt.Sprintf("disk '%s' has identifier %s in disk.details but %s in the TrueNAS disk database (disk.query), so which disk pool.replace would use cannot be confirmed", newName, nd.DetailIdentifier, orNone(nd.Identifier)))
		}
		switch {
		case !nd.PartitionsKnown:
			reasons = append(reasons, fmt.Sprintf("TrueNAS (disk.details) did not report the partitions on disk '%s', so it cannot be checked to be clean", newName))
		case len(nd.Partitions) > 0:
			reasons = append(reasons, fmt.Sprintf("disk '%s' has %d partition(s) (%s); TrueNAS refuses to replace with a disk that is not clean unless force is sent, which this tool never does. Wipe it first (for example from the TrueNAS web UI) if nothing on it is needed", newName, len(nd.Partitions), strings.Join(nd.Partitions, ", ")))
		}
	}
	if len(nd.DupSerial) > 0 {
		reasons = append(reasons, fmt.Sprintf("disk '%s' shares its serial with %s; TrueNAS refuses such disks without force, which this tool never sends", newName, strings.Join(nd.DupSerial, ", ")))
	}
	switch {
	case nd.Size <= 0:
		reasons = append(reasons, fmt.Sprintf("the size of disk '%s' is not known, so it cannot be checked to be at least as large as %s", newName, m.label()))
	case m.Size > 0 && nd.Size < m.Size:
		reasons = append(reasons, fmt.Sprintf("disk '%s' (%s) is smaller than %s (%s)", newName, formatBytes(nd.Size), m.label(), formatBytes(m.Size)))
	}
	if len(reasons) > 0 {
		return diskPlanBlocked(plan, reasons), nil
	}

	resilver := fmt.Sprintf("ZFS resilvers %s's share of %s onto %s (a full rebuild of that disk); it can take hours or days on large disks, and the pool stays usable meanwhile", m.label(), v.label(), newName)
	if t.pool.Allocated > 0 {
		resilver += fmt.Sprintf(" (pool data allocated: %s)", formatBytes(t.pool.Allocated))
	}
	warnings := []string{
		fmt.Sprintf("Everything on disk '%s' (%s) is destroyed: TrueNAS repartitions it", newName, nd.fact()),
		resilver,
		fmt.Sprintf("The job finishes once the replacement has started; follow the resilver with get_disk_health or query_pools. %s stays in the pool until the resilver completes, then ZFS detaches it", m.label()),
		"Pool I/O may be slower while the resilver runs",
		fmt.Sprintf("Not checked on '%s': swap, self-encrypting drive (SED) locking and whether anything has the disk open; make sure nothing on the system uses it", newName),
		t.staleNameNote(),
	}
	if m.Status == "ONLINE" {
		warnings = append(warnings, fmt.Sprintf("%s is healthy; it keeps serving data until the resilver completes", m.label()))
	}
	if nd.Size > m.Size {
		warnings = append(warnings, fmt.Sprintf("'%s' is larger than %s; the extra space is used only once every disk of the vdev is at least that large (and the pool is expanded)", newName, m.label()))
	}
	plan.Actions = []PlannedAction{{
		Step:        1,
		Description: fmt.Sprintf("Replace disk %s in %s of pool '%s' with '%s', destroying everything on '%s'", m.label(), v.label(), t.pool.Name, newName, newName),
		Operation:   "replace",
		Target:      m.label(),
		Details: map[string]interface{}{
			"pool":                    t.pool.Name,
			"disk":                    m.label(),
			"guid":                    m.GUID,
			"disk_status":             m.Status,
			"disk_size":               sizeOrUnknown(m.Size),
			"replacement_disk":        newName,
			"replacement_identifier":  nd.DetailIdentifier,
			"replacement_serial":      nilIfEmpty(nd.Serial),
			"replacement_size":        sizeOrUnknown(nd.Size),
			"replacement_destroyed":   true,
			"vdev":                    v.Name,
			"vdev_layout":             v.layoutText(nil),
			"tolerates_failed_disks":  parity,
			"failed_disks_in_vdev":    len(failed),
			"expected_resilver":       "full rebuild of the replaced disk's data",
			"force":                   false,
			"old_disk_detached_after": "the resilver completes",
		},
	}}
	plan.Warnings = warnings
	plan.Result = map[string]interface{}{"pool": t.pool.Name, "guid": m.GUID, "replacement": newName}
	return plan, nil
}

func sizeOrUnknown(n int64) string {
	if n <= 0 {
		return "unknown"
	}
	return formatBytes(n)
}

func (r *Registry) handleReplaceDisk(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planReplaceDisk(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeReplaceDisk(ctx, client, args, plan)
}

func (r *Registry) executeReplaceDisk(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	poolName, _ := plan.Result["pool"].(string)
	newName, _ := plan.Result["replacement"].(string)
	disk := plan.TargetName
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to replace disk %s in pool '%s' with '%s': %w; check get_disk_health and query_jobs before retrying", disk, poolName, newName, err)
	}
	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("pool.replace returned no job id for replacing %s with '%s'; check get_disk_health and query_jobs before retrying", disk, newName)
	}
	response := map[string]interface{}{
		"pool":             poolName,
		"disk":             disk,
		"replacement_disk": newName,
		"job_id":           jobID,
	}
	msg := fmt.Sprintf("Replacement of disk %s with '%s' in pool '%s' started (job %d)", disk, newName, poolName, jobID)
	if r.taskManager != nil {
		opts := []tasks.TaskOption{}
		if plan.JobMethod != "" {
			opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
		}
		task, err := r.taskManager.CreateJobTask(ctx, "replace_disk", args, jobID, replaceDiskTaskTTL, opts...)
		if err != nil {
			response["note"] = "The job started, but it could not be tracked as a task: " + boundedErr(err) + "; follow it with query_jobs"
		} else {
			response["task_id"] = task.TaskID
			response["task_status"] = task.Status
			response["poll_interval"] = task.PollInterval
			msg += fmt.Sprintf(". Track the job with tasks_get task_id %s", task.TaskID)
		}
	}
	response["message"] = msg + ". The resilver continues after the job finishes; follow it with get_disk_health or query_pools"
	return marshalJSON(response)
}

package tools

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

const (
	tb8 = 8e12
	tb4 = 4e12
	tb1 = 1e12
)

// diskVdev is a top-level vdev of a fixture pool.
func diskVdev(typ, name, guid string, kids ...map[string]interface{}) map[string]interface{} {
	children := make([]interface{}, len(kids))
	for i, k := range kids {
		children[i] = k
	}
	return map[string]interface{}{"type": typ, "name": name, "guid": guid, "status": "ONLINE", "children": children}
}

// diskActionTankPool is pool tank: a 3-way mirror, a RAIDZ1, a RAIDZ2, a
// 2-way special mirror, a single log disk, a cache disk and a spare.
func diskActionTankPool() map[string]interface{} {
	spare := leaf("sdo", "160", "AVAIL", 0, 0, 0)
	return map[string]interface{}{
		"id": 1.0, "name": "tank", "status": "ONLINE", "healthy": true, "allocated": 4e12,
		"scan": map[string]interface{}{"function": "SCRUB", "state": "FINISHED"},
		"topology": map[string]interface{}{
			"data": []interface{}{
				diskVdev("MIRROR", "mirror-0", "100", leaf("sda", "101", "ONLINE", 0, 0, 0), leaf("sdb", "102", "ONLINE", 0, 0, 0), leaf("sdc", "103", "ONLINE", 0, 0, 0)),
				diskVdev("RAIDZ1", "raidz1-1", "110", leaf("sdd", "111", "ONLINE", 0, 0, 0), leaf("sde", "112", "ONLINE", 0, 0, 0), leaf("sdf", "113", "ONLINE", 0, 0, 0)),
				diskVdev("RAIDZ2", "raidz2-2", "120", leaf("sdg", "121", "ONLINE", 0, 0, 0), leaf("sdh", "122", "ONLINE", 0, 0, 0), leaf("sdi", "123", "ONLINE", 0, 0, 0), leaf("sdj", "124", "ONLINE", 0, 0, 0)),
			},
			"special": []interface{}{
				diskVdev("MIRROR", "mirror-3", "130", leaf("sdm", "131", "ONLINE", 0, 0, 0), leaf("sdn", "132", "ONLINE", 0, 0, 0)),
			},
			"log":   []interface{}{leaf("nvme1n1", "140", "ONLINE", 0, 0, 0)},
			"cache": []interface{}{leaf("nvme2n1", "150", "ONLINE", 0, 0, 0)},
			"spare": []interface{}{spare},
			"dedup": []interface{}{},
		},
	}
}

// diskActionScratchPool is a pool with one single-disk data vdev.
func diskActionScratchPool() map[string]interface{} {
	return map[string]interface{}{
		"id": 2.0, "name": "scratch", "status": "ONLINE", "healthy": true,
		"topology": map[string]interface{}{"data": []interface{}{leaf("sdq", "201", "ONLINE", 0, 0, 0)}},
	}
}

func diskActionBootState() map[string]interface{} {
	return map[string]interface{}{
		"name": "boot-pool", "status": "ONLINE", "healthy": true,
		"topology": map[string]interface{}{"data": []interface{}{leaf("nvme0n1", "901", "ONLINE", 0, 0, 0)}},
	}
}

func diskRow(name, serial string, size float64, pool interface{}) map[string]interface{} {
	return map[string]interface{}{
		"name": name, "devname": name, "serial": serial, "size": size, "pool": pool,
		"identifier": "{serial_lunid}" + serial, "model": "FAKE DISK", "type": "HDD",
	}
}

// diskActionDiskRows are the disk.query rows: tank's disks, unused disks
// sdk (8 TB) and sdl (4 TB), sdp of pool backup, sdr (in neither
// disk.details list), sds (duplicate serial) and the boot disk.
func diskActionDiskRows() map[string]map[string]interface{} {
	rows := map[string]map[string]interface{}{}
	for _, d := range []string{"sda", "sdb", "sdc", "sdd", "sde", "sdf", "sdg", "sdh", "sdi", "sdj", "sdo"} {
		rows[d] = diskRow(d, "FAKE-SERIAL-"+strings.ToUpper(d), tb8, "tank")
	}
	rows["sdm"] = diskRow("sdm", "FAKE-SERIAL-SDM", tb1, "tank")
	rows["sdn"] = diskRow("sdn", "FAKE-SERIAL-SDN", tb1, "tank")
	rows["nvme1n1"] = diskRow("nvme1n1", "FAKE-SERIAL-NV1", 5e11, "tank")
	rows["nvme2n1"] = diskRow("nvme2n1", "FAKE-SERIAL-NV2", 5e11, "tank")
	rows["sdq"] = diskRow("sdq", "FAKE-SERIAL-SDQ", tb8, "scratch")
	rows["sdk"] = diskRow("sdk", "FAKE-SERIAL-SDK", tb8, nil)
	rows["sdl"] = diskRow("sdl", "FAKE-SERIAL-SDL", tb4, nil)
	rows["sdp"] = diskRow("sdp", "FAKE-SERIAL-SDP", tb8, "backup")
	rows["sdr"] = diskRow("sdr", "FAKE-SERIAL-SDR", tb8, nil)
	rows["sds"] = diskRow("sds", "FAKE-SERIAL-DUP", tb8, nil)
	rows["sdt"] = diskRow("sdt", "FAKE-SERIAL-SDT", 0, nil)
	rows["nvme0n1"] = diskRow("nvme0n1", "FAKE-SERIAL-BOOT", 2.5e11, nil)
	return rows
}

type diskActionFake struct {
	mu      sync.Mutex
	pools   map[string]map[string]interface{}
	boot    map[string]interface{}
	disks   map[string]map[string]interface{}
	unused  map[string]map[string]interface{}
	used    map[string]map[string]interface{}
	jobs    []map[string]interface{}
	failGet map[string]bool
}

func newDiskActionFake() *diskActionFake {
	f := &diskActionFake{
		pools: map[string]map[string]interface{}{"tank": diskActionTankPool(), "scratch": diskActionScratchPool()},
		boot:  diskActionBootState(),
		disks: diskActionDiskRows(),
		used:  map[string]map[string]interface{}{},
		unused: map[string]map[string]interface{}{
			"sdk": {"name": "sdk", "size": tb8, "serial": "FAKE-SERIAL-SDK"},
			"sdl": {"name": "sdl", "size": tb4, "serial": "FAKE-SERIAL-SDL"},
			"sds": {"name": "sds", "size": tb8, "serial": "FAKE-SERIAL-DUP", "duplicate_serial": []interface{}{"sdu"}},
			"sdt": {"name": "sdt", "serial": "FAKE-SERIAL-SDT"},
			"sdv": {"name": "sdv", "size": tb8, "serial": "FAKE-SERIAL-SDV"},
		},
		failGet: map[string]bool{},
	}
	for _, d := range []string{"sda", "sdb", "sdc", "sdd", "sde", "sdf", "sdg", "sdh", "sdi", "sdj", "sdm", "sdn", "sdo", "nvme1n1", "nvme2n1", "sdq", "nvme0n1"} {
		f.used[d] = map[string]interface{}{"name": d, "size": f.disks[d]["size"], "imported_zpool": f.disks[d]["pool"]}
	}
	f.used["sdp"] = map[string]interface{}{"name": "sdp", "size": tb8, "imported_zpool": "backup"}
	return f
}

func (f *diskActionFake) handle(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet[method] {
		return nil, map[string]interface{}{"code": -32001, "message": "fake failure"}, true
	}
	var p []interface{}
	_ = json.Unmarshal(params, &p)
	switch method {
	case "boot.get_state":
		return f.boot, nil, true
	case "pool.query":
		filters, _ := p[0].([]interface{})
		out := []interface{}{}
		for _, row := range f.pools {
			if jobMatchesFilters(row, filters) {
				out = append(out, row)
			}
		}
		return out, nil, true
	case "disk.query":
		out := []interface{}{}
		for _, row := range f.disks {
			out = append(out, row)
		}
		return out, nil, true
	case "disk.details":
		used, unused := []interface{}{}, []interface{}{}
		for _, row := range f.used {
			used = append(used, row)
		}
		for _, row := range f.unused {
			unused = append(unused, row)
		}
		return map[string]interface{}{"used": used, "unused": unused}, nil, true
	case "core.get_jobs":
		filters, _ := p[0].([]interface{})
		out := []interface{}{}
		for _, j := range f.jobs {
			if jobMatchesFilters(j, filters) {
				out = append(out, j)
			}
		}
		return out, nil, true
	case "pool.offline", "pool.online":
		opts, _ := p[1].(map[string]interface{})
		status := "OFFLINE"
		if method == "pool.online" {
			status = "ONLINE"
		}
		f.setLeaf(poolNameByID(f.pools, p[0]), opts["label"].(string), "status", status)
		return true, nil, true
	case "pool.replace":
		f.jobs = append(f.jobs, map[string]interface{}{
			"id": 55.0, "method": "pool.replace", "state": "RUNNING", "arguments": p,
			"time_started": map[string]interface{}{"$date": 1767225600000.0},
			"progress":     map[string]interface{}{"percent": 0.0, "description": "", "extra": nil},
		})
		return 55, nil, true
	}
	return nil, nil, false
}

func poolNameByID(pools map[string]map[string]interface{}, id interface{}) string {
	for name, row := range pools {
		if row["id"] == id {
			return name
		}
	}
	return ""
}

// leafOf returns the topology entry (leaf or vdev) of pool with the guid.
func (f *diskActionFake) leafOf(pool, guid string) map[string]interface{} {
	var found map[string]interface{}
	var walk func(v interface{})
	walk = func(v interface{}) {
		list, _ := v.([]interface{})
		for _, item := range list {
			m, _ := item.(map[string]interface{})
			if m["guid"] == guid {
				found = m
			}
			walk(m["children"])
		}
	}
	top, _ := f.pools[pool]["topology"].(map[string]interface{})
	for _, kind := range topologyKinds {
		walk(top[kind])
	}
	return found
}

func (f *diskActionFake) setLeaf(pool, guid, key string, value interface{}) {
	if l := f.leafOf(pool, guid); l != nil {
		l[key] = value
	}
}

func (f *diskActionFake) mutate(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *diskActionFake) registry(t *testing.T) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{handle: f.handle}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

func (f *diskActionFake) registryWithTasks(t *testing.T) (*Registry, *fakeMiddleware, *tasks.Manager) {
	t.Helper()
	fake := &fakeMiddleware{handle: f.handle}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: 5 * time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake, mgr
}

var diskActionTools = []string{"offline_disk", "online_disk", "replace_disk"}

var diskWriteMethods = []string{"pool.offline", "pool.online", "pool.replace"}

func assertNoDiskWrites(t *testing.T, fake *fakeMiddleware) {
	t.Helper()
	for _, m := range diskWriteMethods {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("%s sent %d time(s)", m, n)
		}
	}
}

// degrade marks guid in tank with status and the pool DEGRADED.
func (f *diskActionFake) degrade(guid, status string) {
	f.setLeaf("tank", guid, "status", status)
	f.pools["tank"]["status"] = "DEGRADED"
	f.pools["tank"]["healthy"] = false
}

// ---- classification, read-only ----

func TestDiskActionToolsSafety(t *testing.T) {
	r := NewRegistry(nil, nil)
	for _, name := range diskActionTools {
		if got := safetyOf(name); got != (Safety{Access: Destructive}) {
			t.Errorf("%s safety = %+v, want destructive, not idempotent", name, got)
		}
		tool := r.tools[name]
		if tool.Plan == nil || tool.Execute == nil {
			t.Errorf("%s has no Plan or Execute", name)
		}
		if !hasParam(tool, "confirmation_token") || !hasParam(tool, "dry_run") {
			t.Errorf("%s lacks confirmation_token or dry_run", name)
		}
		if a := tool.Definition.Annotations; a == nil || !a.DestructiveHint || a.IdempotentHint || a.ReadOnlyHint {
			t.Errorf("%s annotations = %+v", name, a)
		}
		if readOnlyDryRunTools[name] {
			t.Errorf("%s is listed for read-only dry runs", name)
		}
		for _, p := range []string{"force", "label", "preserve_settings", "id"} {
			if hasParam(tool, p) {
				t.Errorf("%s declares %s", name, p)
			}
		}
	}
	if _, err := NewRegistry(nil, nil).CallTool(t.Context(), "replace_disk", map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": "sdk", "force": true}); err == nil || !strings.Contains(err.Error(), "force") {
		t.Errorf("force accepted: err = %v", err)
	}
}

func TestDiskActionToolsRefusedInReadOnlyMode(t *testing.T) {
	f := newDiskActionFake()
	r, fake := f.registry(t)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	args := map[string]map[string]interface{}{
		"offline_disk": {"pool": "tank", "disk": "sdb"},
		"online_disk":  {"pool": "tank", "disk": "sdb"},
		"replace_disk": {"pool": "tank", "disk": "sdb", "replacement_disk": "sdk"},
	}
	for _, name := range diskActionTools {
		if names[name] {
			t.Errorf("read-only lists %s", name)
		}
		for _, extra := range []map[string]interface{}{{"dry_run": true}, {"dry_run": false}, {}, {"confirmation_token": "x"}} {
			if _, err := r.CallTool(t.Context(), name, mergeArgs(args[name], extra)); err == nil || !strings.Contains(err.Error(), "read-only mode") {
				t.Errorf("read-only %s %v: err = %v", name, extra, err)
			}
		}
	}
	for _, m := range append([]string{"pool.query", "disk.query", "disk.details", "boot.get_state"}, diskWriteMethods...) {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("read-only mode sent %s %d time(s)", m, n)
		}
	}
	if err := r.SetPolicy(Policy{ReadOnly: true, Allow: []string{"replace_disk"}}); err == nil {
		t.Error("--allow-tools with a disk write tool accepted in read-only mode")
	}
}

// ---- offline_disk ----

func TestOfflineDiskConfirmFlow(t *testing.T) {
	f := newDiskActionFake()
	r, fake := f.registry(t)
	preview, out := confirmAndRun(t, r, fake, "offline_disk", "pool.offline", map[string]interface{}{"pool": "tank", "disk": "sdb"})
	assertContainsAll(t, "preview", previewBody(t, preview),
		"data vdev mirror-0 (MIRROR, ONLINE): sda ONLINE, sdb ONLINE, sdc ONLINE",
		"vdev after: data vdev mirror-0 (MIRROR, ONLINE): sda ONLINE, sdb OFFLINE, sdc ONLINE",
		"redundancy left in the vdev: 1 more disk failure tolerated",
		`"redundancy_left_after":1`, `"tolerates_failed_disks_now":2`, `"force":false`,
		"FAKE-SERIAL-SDB", "tolerates 1 more failed disk(s) instead of 2")
	p := snapActionParams(t, fake, "pool.offline")
	if previewText(t, p) != `[1,{"label":"102"}]` {
		t.Errorf("pool.offline params = %s", previewText(t, p))
	}
	resp := decodeObj(t, out)
	if resp["offline"] != true || resp["status_after"] != "OFFLINE" || resp["note"] != nil || resp["guid"] != "102" {
		t.Errorf("response = %v", resp)
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "online_disk") {
		t.Errorf("message = %q", msg)
	}
}

func TestOfflineDiskByGUIDAndRAIDZ2(t *testing.T) {
	f := newDiskActionFake()
	r, fake := f.registry(t)
	dry := dryRunOf(t, r, "offline_disk", map[string]interface{}{"pool": "tank", "disk": "122"})
	d := plannedDetails(t, dry)
	if d["disk"] != "sdh" || d["redundancy_left_after"] != 1.0 || d["vdev"] != "raidz2-2" {
		t.Errorf("details = %v", d)
	}
	cur := dry["current_state"].(map[string]interface{})
	if v := cur["vdev"].(map[string]interface{}); v["tolerates_failed_disks"] != 2.0 || v["width"] != 4.0 {
		t.Errorf("vdev = %v", v)
	}
	assertNoDiskWrites(t, fake)
}

func TestOfflineDiskRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   map[string]interface{}
		change func(f *diskActionFake)
		want   string
	}{
		{"2-way special mirror", map[string]interface{}{"pool": "tank", "disk": "sdm"}, nil, "with no redundancy"},
		{"raidz1", map[string]interface{}{"pool": "tank", "disk": "sde"}, nil, "with no redundancy"},
		{"stripe data vdev", map[string]interface{}{"pool": "scratch", "disk": "sdq"}, nil, "has no redundancy: taking its only disk offline would take the whole vdev offline"},
		{"single log disk", map[string]interface{}{"pool": "tank", "disk": "nvme1n1"}, nil, "single-disk log vdev nvme1n1 has no redundancy"},
		{"cache", map[string]interface{}{"pool": "tank", "disk": "nvme2n1"}, nil, "cache (L2ARC) device"},
		{"spare", map[string]interface{}{"pool": "tank", "disk": "sdo"}, nil, "is a hot spare"},
		{"pool degraded elsewhere", map[string]interface{}{"pool": "tank", "disk": "sdb"},
			func(f *diskActionFake) { f.degrade("123", "FAULTED") }, "pool 'tank' is DEGRADED, not ONLINE"},
		{"vdev already degraded", map[string]interface{}{"pool": "tank", "disk": "sdg"},
			func(f *diskActionFake) { f.degrade("123", "OFFLINE") }, "raidz2-2 is already missing redundancy: sdi (OFFLINE)"},
		{"3-way mirror with a faulted disk", map[string]interface{}{"pool": "tank", "disk": "sda"},
			func(f *diskActionFake) { f.degrade("103", "FAULTED") }, "already missing redundancy"},
		{"disk not online", map[string]interface{}{"pool": "tank", "disk": "sdb"},
			func(f *diskActionFake) { f.degrade("102", "DEGRADED") }, "disk sdb is DEGRADED, not ONLINE"},
		{"already offline", map[string]interface{}{"pool": "tank", "disk": "sdb"},
			func(f *diskActionFake) { f.degrade("102", "OFFLINE") }, "disk sdb is OFFLINE, not ONLINE"},
		{"resilver running", map[string]interface{}{"pool": "tank", "disk": "sdb"},
			func(f *diskActionFake) {
				f.pools["tank"]["scan"] = map[string]interface{}{"function": "RESILVER", "state": "SCANNING"}
			}, "a resilver is running"},
		{"replacement in progress", map[string]interface{}{"pool": "tank", "disk": "sdg"},
			func(f *diskActionFake) {
				v := f.leafOf("tank", "120")
				kids := v["children"].([]interface{})
				kids[3] = map[string]interface{}{"type": "REPLACING", "name": "replacing-3", "guid": "125", "status": "ONLINE",
					"children": []interface{}{leaf("sdj", "124", "ONLINE", 0, 0, 0), leaf("sdv", "126", "ONLINE", 0, 0, 0)}}
			}, "replacement or hot spare in progress (replacing-3)"},
		{"draid", map[string]interface{}{"pool": "tank", "disk": "sdb"},
			func(f *diskActionFake) { f.leafOf("tank", "100")["type"] = "DRAID2" }, "is a DRAID2 vdev"},
		{"boot pool by name", map[string]interface{}{"pool": "boot-pool", "disk": "nvme0n1"}, nil, "is the boot pool"},
		{"freenas-boot", map[string]interface{}{"pool": "freenas-boot", "disk": "ada0"}, nil, "is the boot pool"},
		{"renamed boot pool", map[string]interface{}{"pool": "sysboot", "disk": "nvme0n1"},
			func(f *diskActionFake) { f.boot["name"] = "sysboot" }, "is the boot pool"},
		{"boot disk named in a data pool", map[string]interface{}{"pool": "tank", "disk": "nvme0n1"}, nil, "is a boot pool disk"},
		{"boot state unreadable", map[string]interface{}{"pool": "tank", "disk": "sdb"},
			func(f *diskActionFake) { f.failGet["boot.get_state"] = true }, "could not read the boot pool"},
		{"unknown pool", map[string]interface{}{"pool": "nopool", "disk": "sdb"}, nil, "pool 'nopool' not found"},
		{"disk of another pool", map[string]interface{}{"pool": "tank", "disk": "sdq"}, nil, "is not a device of pool 'tank'"},
		{"empty disk", map[string]interface{}{"pool": "tank", "disk": " "}, nil, "disk is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiskActionFake()
			if tc.change != nil {
				f.mutate(func() { tc.change(f) })
			}
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "offline_disk", "pool.offline", tc.args, tc.want)
			assertNoDiskWrites(t, fake)
		})
	}
}

// A pool or disk that changes between the preview and the confirmed call
// is refused at confirm, and nothing is written.
func TestOfflineDiskTokenRejectedAfterStateChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(f *diskActionFake)
		blocked string
	}{
		{"sibling disk degraded", func(f *diskActionFake) { f.degrade("103", "DEGRADED") }, "already missing redundancy"},
		{"disk elsewhere in the pool faulted", func(f *diskActionFake) { f.degrade("113", "FAULTED") }, "is DEGRADED, not ONLINE"},
		{"target disk went offline", func(f *diskActionFake) { f.degrade("102", "OFFLINE") }, "not ONLINE"},
		{"resilver started", func(f *diskActionFake) {
			f.pools["tank"]["scan"] = map[string]interface{}{"function": "RESILVER", "state": "SCANNING"}
		}, "resilver is running"},
		{"disk swapped under the same name", func(f *diskActionFake) { f.disks["sdb"]["serial"] = "FAKE-SERIAL-OTHER" }, ""},
		{"disk renamed (named by guid)", func(f *diskActionFake) { f.setLeaf("tank", "102", "disk", "sdz") }, ""},
		{"mirror widened", func(f *diskActionFake) {
			v := f.leafOf("tank", "100")
			v["children"] = append(v["children"].([]interface{}), leaf("sdv", "104", "ONLINE", 0, 0, 0))
		}, ""},
		{"another vdev added", func(f *diskActionFake) {
			top := f.pools["tank"]["topology"].(map[string]interface{})
			top["log"] = append(top["log"].([]interface{}), leaf("sdv", "141", "ONLINE", 0, 0, 0))
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiskActionFake()
			r, fake := f.registry(t)
			args := map[string]interface{}{"pool": "tank", "disk": "sdb"}
			if strings.Contains(tc.name, "by guid") {
				args["disk"] = "102"
			}
			tok := destroyPreviewToken(t, r, "offline_disk", args)
			f.mutate(func() { tc.change(f) })
			err := assertTokenRejected(t, r, fake, "offline_disk", "pool.offline", args, tok)
			if tc.blocked != "" && (err == nil || !strings.Contains(err.Error(), tc.blocked)) {
				t.Errorf("not refused for the change: err = %v", err)
			}
			assertNoDiskWrites(t, fake)
		})
	}
}

func TestOfflineDiskReadBackMismatch(t *testing.T) {
	f := newDiskActionFake()
	r, fake := f.registry(t)
	inner := fake.handle
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "pool.offline" {
			return true, nil, true
		}
		return inner(method, params)
	}
	_, out := confirmAndRun(t, r, fake, "offline_disk", "pool.offline", map[string]interface{}{"pool": "tank", "disk": "sdb"})
	resp := decodeObj(t, out)
	if note, _ := resp["note"].(string); !strings.Contains(note, "is ONLINE when read back, not OFFLINE") {
		t.Errorf("response = %v", resp)
	}
}

func TestOfflineDiskUnexpectedResult(t *testing.T) {
	f := newDiskActionFake()
	r, fake := f.registry(t)
	inner := fake.handle
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "pool.offline" {
			return false, nil, true
		}
		return inner(method, params)
	}
	args := map[string]interface{}{"pool": "tank", "disk": "sdb"}
	tok := destroyPreviewToken(t, r, "offline_disk", args)
	if _, err := r.CallTool(t.Context(), "offline_disk", mergeArgs(args, map[string]interface{}{"confirmation_token": tok})); err == nil || !strings.Contains(err.Error(), "unexpected result") {
		t.Errorf("err = %v", err)
	}
}

// ---- online_disk ----

func TestOnlineDiskConfirmFlow(t *testing.T) {
	f := newDiskActionFake()
	f.degrade("102", "OFFLINE")
	r, fake := f.registry(t)
	preview, out := confirmAndRun(t, r, fake, "online_disk", "pool.online", map[string]interface{}{"pool": "tank", "disk": "sdb"})
	assertContainsAll(t, "preview", previewBody(t, preview),
		"vdev now: data vdev mirror-0 (MIRROR, ONLINE): sda ONLINE, sdb OFFLINE, sdc ONLINE",
		"expected resilver: ZFS resilvers onto sdb the data written to data vdev mirror-0 while the disk was offline",
		`"expected_resilver":"partial: only data written while the disk was offline"`,
		"sdb ONLINE (resilvering)")
	p := snapActionParams(t, fake, "pool.online")
	if previewText(t, p) != `[1,{"label":"102"}]` {
		t.Errorf("pool.online params = %s", previewText(t, p))
	}
	resp := decodeObj(t, out)
	if resp["online"] != true || resp["status_after"] != "ONLINE" || resp["note"] != nil {
		t.Errorf("response = %v", resp)
	}
}

func TestOnlineDiskRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		disk   string
		change func(f *diskActionFake)
		want   string
	}{
		{"online disk", "sdb", nil, "disk sdb is ONLINE, not OFFLINE"},
		{"faulted disk", "sdb", func(f *diskActionFake) { f.degrade("102", "FAULTED") }, "disk sdb is FAULTED, not OFFLINE"},
		{"removed disk", "112", func(f *diskActionFake) {
			l := f.leafOf("tank", "112")
			l["disk"] = nil
			l["unavail_disk"] = map[string]interface{}{"name": "sde", "serial": "FAKE-SERIAL-SDE", "size": tb8}
			f.degrade("112", "REMOVED")
		}, "is REMOVED, not OFFLINE"},
		{"offline but absent", "112", func(f *diskActionFake) {
			f.leafOf("tank", "112")["disk"] = nil
			f.degrade("112", "OFFLINE")
		}, "not matched to any disk present"},
		{"cache", "nvme2n1", nil, "cache (L2ARC) device"},
		{"replacement in progress", "sdg", func(f *diskActionFake) {
			v := f.leafOf("tank", "120")
			kids := v["children"].([]interface{})
			kids[3] = map[string]interface{}{"type": "SPARE", "name": "spare-3", "guid": "125", "status": "DEGRADED",
				"children": []interface{}{leaf("sdj", "124", "FAULTED", 0, 0, 0), leaf("sdo", "160", "INUSE", 0, 0, 0)}}
			f.degrade("121", "OFFLINE")
		}, "replacement or hot spare in progress (spare-3)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiskActionFake()
			if tc.change != nil {
				f.mutate(func() { tc.change(f) })
			}
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "online_disk", "pool.online", map[string]interface{}{"pool": "tank", "disk": tc.disk}, tc.want)
			assertNoDiskWrites(t, fake)
		})
	}
	f := newDiskActionFake()
	r, fake := f.registry(t)
	assertRefused(t, r, fake, "online_disk", "pool.online", map[string]interface{}{"pool": "boot-pool", "disk": "nvme0n1"}, "is the boot pool")
}

func TestOnlineDiskTokenRejectedAfterStateChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(f *diskActionFake)
		blocked string
	}{
		{"disk brought online elsewhere", func(f *diskActionFake) { f.setLeaf("tank", "102", "status", "ONLINE") }, "not OFFLINE"},
		{"disk faulted", func(f *diskActionFake) { f.setLeaf("tank", "102", "status", "FAULTED") }, "not OFFLINE"},
		{"different disk in the slot", func(f *diskActionFake) { f.disks["sdb"]["serial"] = "FAKE-SERIAL-OTHER" }, ""},
		{"sibling faulted", func(f *diskActionFake) { f.setLeaf("tank", "101", "status", "FAULTED") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiskActionFake()
			f.degrade("102", "OFFLINE")
			r, fake := f.registry(t)
			args := map[string]interface{}{"pool": "tank", "disk": "sdb"}
			tok := destroyPreviewToken(t, r, "online_disk", args)
			f.mutate(func() { tc.change(f) })
			err := assertTokenRejected(t, r, fake, "online_disk", "pool.online", args, tok)
			if tc.blocked != "" && (err == nil || !strings.Contains(err.Error(), tc.blocked)) {
				t.Errorf("not refused for the change: err = %v", err)
			}
			assertNoDiskWrites(t, fake)
		})
	}
}

// ---- replace_disk ----

func TestReplaceDiskConfirmFlowTracksJob(t *testing.T) {
	f := newDiskActionFake()
	f.degrade("112", "FAULTED")
	r, fake, mgr := f.registryWithTasks(t)
	args := map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": "sdk"}
	preview, out := confirmAndRun(t, r, fake, "replace_disk", "pool.replace", args)
	assertContainsAll(t, "preview", previewBody(t, preview),
		"Replace disk sde of pool tank with sdk; everything on sdk is destroyed",
		"replacement disk (contents destroyed): sdk, serial FAKE-SERIAL-SDK, 7.28 TiB",
		"disk replaced: sde, serial FAKE-SERIAL-SDE, 7.28 TiB, guid 112, FAULTED",
		"sizes: 7.28 TiB replaced by 7.28 TiB",
		"vdev: data vdev raidz1-1 (RAIDZ1, ONLINE): sdd ONLINE, sde FAULTED, sdf ONLINE",
		"Everything on disk 'sdk' (sdk, serial FAKE-SERIAL-SDK, 7.28 TiB) is destroyed",
		`"replacement_destroyed":true`, `"force":false`, "full rebuild")
	p := snapActionParams(t, fake, "pool.replace")
	if previewText(t, p) != `[1,{"disk":"sdk","force":false,"label":"112"}]` {
		t.Errorf("pool.replace params = %s", previewText(t, p))
	}
	resp := decodeObj(t, out)
	if resp["job_id"] != 55.0 || resp["replacement_disk"] != "sdk" || resp["disk"] != "sde" || resp["task_id"] == nil {
		t.Fatalf("response = %v", resp)
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "tasks_get") || !strings.Contains(msg, "resilver continues") {
		t.Errorf("message = %q", msg)
	}
	taskID := resp["task_id"].(string)
	if task := getTask(t, r, mgr, taskID); task["status"] != "working" {
		t.Errorf("task while the job runs = %v", task)
	}
	f.mutate(func() {
		f.jobs[0]["state"] = "SUCCESS"
		f.jobs[0]["result"] = true
		f.jobs[0]["progress"] = map[string]interface{}{"percent": 100.0, "description": "", "extra": nil}
	})
	if task := getTask(t, r, mgr, taskID); task["status"] != "completed" {
		t.Errorf("task after the job = %v", task)
	}
}

func TestReplaceDiskFailedJobFailsTask(t *testing.T) {
	f := newDiskActionFake()
	f.degrade("112", "FAULTED")
	r, fake, mgr := f.registryWithTasks(t)
	_, out := confirmAndRun(t, r, fake, "replace_disk", "pool.replace", map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": "sdk"})
	resp := decodeObj(t, out)
	f.mutate(func() {
		f.jobs[0]["state"] = "FAILED"
		f.jobs[0]["error"] = "[EFAULT] fake replace failure"
	})
	if task := getTask(t, r, mgr, resp["task_id"].(string)); task["status"] != "failed" {
		t.Errorf("task = %v", task)
	}
}

func TestReplaceDiskMissingDeviceByGUID(t *testing.T) {
	f := newDiskActionFake()
	l := f.leafOf("tank", "112")
	l["disk"] = nil
	l["unavail_disk"] = map[string]interface{}{"name": "sde", "serial": "FAKE-SERIAL-SDE", "size": tb8}
	f.degrade("112", "UNAVAIL")
	delete(f.disks, "sde")
	r, fake := f.registry(t)
	assertRefused(t, r, fake, "replace_disk", "pool.replace", map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": "sdk"}, "name it by its guid")
	_, out := confirmAndRun(t, r, fake, "replace_disk", "pool.replace", map[string]interface{}{"pool": "tank", "disk": "112", "replacement_disk": "sdk"})
	if resp := decodeObj(t, out); resp["disk"] != "guid 112 (last seen as sde)" {
		t.Errorf("response = %v", resp)
	}
	if p := snapActionParams(t, fake, "pool.replace"); previewText(t, p[1]) != `{"disk":"sdk","force":false,"label":"112"}` {
		t.Errorf("params = %s", previewText(t, p))
	}
}

func TestReplaceDiskHealthyDiskInHealthyVdev(t *testing.T) {
	f := newDiskActionFake()
	r, fake := f.registry(t)
	dry := dryRunOf(t, r, "replace_disk", map[string]interface{}{"pool": "tank", "disk": "sdb", "replacement_disk": "sdk"})
	assertContainsAll(t, "warnings", previewText(t, dry["warnings"]), "sdb is healthy; it keeps serving data until the resilver completes")
	assertNoDiskWrites(t, fake)
}

func TestReplaceDiskRefusals(t *testing.T) {
	faultSDE := func(f *diskActionFake) { f.degrade("112", "FAULTED") }
	for _, tc := range []struct {
		name   string
		disk   string
		repl   string
		change func(f *diskActionFake)
		want   string
	}{
		{"smaller replacement", "sde", "sdl", faultSDE, "disk 'sdl' (3.64 TiB) is smaller than sde (7.28 TiB)"},
		{"replacement in another pool", "sde", "sdp", faultSDE, "disk 'sdp' is part of pool 'backup'"},
		{"replacement in this pool", "sde", "sdb", faultSDE, "disk 'sdb' is part of pool 'tank'"},
		{"replacement is a boot disk", "sde", "nvme0n1", faultSDE, "disk 'nvme0n1' is a boot disk of 'boot-pool'"},
		{"replacement holds an exported pool", "sde", "sdk", func(f *diskActionFake) {
			faultSDE(f)
			f.unused["sdk"]["exported_zpool"] = "oldpool"
		}, "holds exported pool 'oldpool'"},
		{"replacement used per disk.details", "sde", "sdk", func(f *diskActionFake) {
			faultSDE(f)
			delete(f.unused, "sdk")
			f.used["sdk"] = map[string]interface{}{"name": "sdk", "size": tb8}
		}, "is in use according to TrueNAS"},
		{"replacement not reported unused", "sde", "sdr", faultSDE, "is not reported as an unused disk"},
		{"replacement unknown", "sde", "sdzz", faultSDE, "is not reported as an unused disk"},
		{"replacement not in the disk database", "sde", "sdv", faultSDE, "not in the TrueNAS disk database"},
		{"replacement duplicate serial", "sde", "sds", faultSDE, "shares its serial with sdu"},
		{"replacement size unknown", "sde", "sdt", faultSDE, "the size of disk 'sdt' is not known"},
		{"target size unknown", "112", "sdk", func(f *diskActionFake) {
			l := f.leafOf("tank", "112")
			l["disk"] = nil
			l["unavail_disk"] = map[string]interface{}{"name": "sde"}
			f.degrade("112", "REMOVED")
		}, "the size of guid 112 (last seen as sde) is not known"},
		{"healthy disk in a degraded vdev", "sdd", "sdk", faultSDE, "raidz1-1 is already degraded: sde (FAULTED); replacing the healthy disk sdd now would risk its last redundancy"},
		{"healthy disk while the pool is degraded", "sdb", "sdk", faultSDE, "pool 'tank' is DEGRADED; replace the failed or offline device first"},
		{"vdev faulted", "sde", "sdk", func(f *diskActionFake) {
			faultSDE(f)
			f.setLeaf("tank", "113", "status", "FAULTED")
			f.setLeaf("tank", "110", "status", "FAULTED")
		}, "more than it tolerates"},
		{"stripe", "sdq", "sdk", nil, "has no redundancy; replace_disk only replaces disks in mirror and RAIDZ vdevs"},
		{"single log disk", "nvme1n1", "sdk", nil, "has no redundancy"},
		{"cache", "nvme2n1", "sdk", nil, "cache (L2ARC) device"},
		{"spare", "sdo", "sdk", nil, "is a hot spare"},
		{"draid", "sde", "sdk", func(f *diskActionFake) {
			faultSDE(f)
			f.leafOf("tank", "110")["type"] = "DRAID1"
		}, "is a DRAID1 vdev"},
		{"resilver running", "sde", "sdk", func(f *diskActionFake) {
			faultSDE(f)
			f.pools["tank"]["scan"] = map[string]interface{}{"function": "RESILVER", "state": "SCANNING"}
		}, "a resilver is running on pool 'tank'"},
		{"replace job running", "sde", "sdk", func(f *diskActionFake) {
			faultSDE(f)
			f.jobs = append(f.jobs, map[string]interface{}{"id": 40.0, "method": "pool.replace", "state": "RUNNING"})
		}, "pool.replace job 40"},
		{"replacement in progress", "sdj", "sdk", func(f *diskActionFake) {
			v := f.leafOf("tank", "120")
			kids := v["children"].([]interface{})
			kids[3] = map[string]interface{}{"type": "REPLACING", "name": "replacing-3", "guid": "125", "status": "DEGRADED",
				"children": []interface{}{leaf("sdj", "124", "FAULTED", 0, 0, 0), leaf("sdv", "126", "ONLINE", 0, 0, 0)}}
			f.pools["tank"]["status"] = "DEGRADED"
		}, "replacement or hot spare in progress (replacing-3)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiskActionFake()
			if tc.change != nil {
				f.mutate(func() { tc.change(f) })
			}
			r, fake := f.registry(t)
			pool := "tank"
			if tc.disk == "sdq" {
				pool = "scratch"
			}
			assertRefused(t, r, fake, "replace_disk", "pool.replace", map[string]interface{}{"pool": pool, "disk": tc.disk, "replacement_disk": tc.repl}, tc.want)
			assertNoDiskWrites(t, fake)
		})
	}
}

func TestReplaceDiskArgumentErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   map[string]interface{}
		change func(f *diskActionFake)
		want   string
	}{
		{"same disk", map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": "sde"}, nil, "is the disk being replaced"},
		{"boot pool", map[string]interface{}{"pool": "boot-pool", "disk": "nvme0n1", "replacement_disk": "sdk"}, nil, "is the boot pool"},
		{"boot disk as target", map[string]interface{}{"pool": "tank", "disk": "nvme0n1", "replacement_disk": "sdk"}, nil, "is a boot pool disk"},
		{"disk.details fails", map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": "sdk"},
			func(f *diskActionFake) { f.failGet["disk.details"] = true }, "could not check that disk 'sdk' is unused"},
		{"core.get_jobs fails", map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": "sdk"},
			func(f *diskActionFake) { f.failGet["core.get_jobs"] = true }, "failed to check for running disk replacements"},
		{"blank replacement", map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": " "}, nil, "replacement_disk is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiskActionFake()
			f.degrade("112", "FAULTED")
			if tc.change != nil {
				tc.change(f)
			}
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "replace_disk", "pool.replace", tc.args, tc.want)
		})
	}
}

func TestReplaceDiskTokenRejectedAfterStateChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(f *diskActionFake)
		blocked string
	}{
		{"replacement disk swapped", func(f *diskActionFake) {
			f.disks["sdk"]["serial"] = "FAKE-SERIAL-OTHER"
			f.disks["sdk"]["identifier"] = "{serial_lunid}FAKE-SERIAL-OTHER"
		}, ""},
		{"replacement disk shrank", func(f *diskActionFake) {
			f.disks["sdk"]["size"] = tb4
			f.unused["sdk"]["size"] = tb4
		}, "is smaller than"},
		{"replacement disk grew", func(f *diskActionFake) {
			f.disks["sdk"]["size"] = 1e13
			f.unused["sdk"]["size"] = 1e13
		}, ""},
		{"replacement disk added to a pool", func(f *diskActionFake) {
			delete(f.unused, "sdk")
			f.used["sdk"] = map[string]interface{}{"name": "sdk", "size": tb8, "imported_zpool": "backup"}
			f.disks["sdk"]["pool"] = "backup"
		}, "part of pool 'backup'"},
		{"target disk came back", func(f *diskActionFake) { f.setLeaf("tank", "112", "status", "ONLINE") }, ""},
		{"sibling disk failed", func(f *diskActionFake) { f.setLeaf("tank", "111", "status", "FAULTED") }, ""},
		{"disk elsewhere failed", func(f *diskActionFake) { f.setLeaf("tank", "101", "status", "FAULTED") }, ""},
		{"replacement job started elsewhere", func(f *diskActionFake) {
			f.jobs = append(f.jobs, map[string]interface{}{"id": 41.0, "method": "pool.replace", "state": "RUNNING"})
		}, "already running"},
		{"resilver started", func(f *diskActionFake) {
			f.pools["tank"]["scan"] = map[string]interface{}{"function": "RESILVER", "state": "SCANNING"}
		}, "resilver is running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiskActionFake()
			f.degrade("112", "FAULTED")
			r, fake := f.registry(t)
			args := map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": "sdk"}
			tok := destroyPreviewToken(t, r, "replace_disk", args)
			f.mutate(func() { tc.change(f) })
			err := assertTokenRejected(t, r, fake, "replace_disk", "pool.replace", args, tok)
			if tc.blocked != "" && (err == nil || !strings.Contains(err.Error(), tc.blocked)) {
				t.Errorf("not refused for the change: err = %v", err)
			}
			assertNoDiskWrites(t, fake)
		})
	}
}

func TestReplaceDiskMiddlewareErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result interface{}
		rpcErr map[string]interface{}
		want   string
	}{
		{"rpc error", nil, map[string]interface{}{"code": -32001, "message": "fake replace refusal"}, "failed to replace disk sde"},
		{"no job id", nil, nil, "returned no job id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiskActionFake()
			f.degrade("112", "FAULTED")
			r, fake := f.registry(t)
			inner := fake.handle
			fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
				if method == "pool.replace" {
					return tc.result, tc.rpcErr, true
				}
				return inner(method, params)
			}
			args := map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": "sdk"}
			tok := destroyPreviewToken(t, r, "replace_disk", args)
			_, err := r.CallTool(t.Context(), "replace_disk", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if n := len(fake.called("pool.replace")); n != 1 {
				t.Errorf("pool.replace calls = %d", n)
			}
		})
	}
}

// Without a task manager the job id is still returned.
func TestReplaceDiskWithoutTaskManager(t *testing.T) {
	f := newDiskActionFake()
	f.degrade("112", "FAULTED")
	r, fake := f.registry(t)
	_, out := confirmAndRun(t, r, fake, "replace_disk", "pool.replace", map[string]interface{}{"pool": "tank", "disk": "sde", "replacement_disk": "sdk"})
	if resp := decodeObj(t, out); resp["job_id"] != 55.0 || resp["task_id"] != nil {
		t.Errorf("response = %v", resp)
	}
}

// withDiskActionResults adds canned answers to results so the disk tools
// can plan against pool tank in tests that use a results table: a
// topology for its pool.query rows, the boot pool, disk.query and
// disk.details.
func withDiskActionResults(results map[string]interface{}) map[string]interface{} {
	f := newDiskActionFake()
	if pools, ok := results["pool.query"].([]interface{}); ok {
		for _, p := range pools {
			if m, ok := p.(map[string]interface{}); ok && m["name"] == "tank" {
				m["topology"] = diskActionTankPool()["topology"]
			}
		}
	}
	disks, used, unused := []interface{}{}, []interface{}{}, []interface{}{}
	for _, row := range f.disks {
		disks = append(disks, row)
	}
	for _, row := range f.used {
		used = append(used, row)
	}
	for _, row := range f.unused {
		unused = append(unused, row)
	}
	results["boot.get_state"] = f.boot
	results["disk.query"] = disks
	results["disk.details"] = map[string]interface{}{"used": used, "unused": unused}
	return results
}

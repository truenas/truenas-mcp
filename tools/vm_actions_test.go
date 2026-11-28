package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// withVMActionResults adds VM preflight and status fixtures to a fake
// middleware results map used by shared registry invariant tests.
func withVMActionResults(results map[string]interface{}) map[string]interface{} {
	const gib = float64(1 << 30)
	if sysInfo, ok := results["system.info"].(map[string]interface{}); ok {
		if _, hasCores := sysInfo["cores"]; !hasCores {
			sysInfo["cores"] = 8.0
		}
		if _, hasMem := sysInfo["physmem"]; !hasMem {
			sysInfo["physmem"] = 32 * gib
		}
	} else {
		results["system.info"] = map[string]interface{}{
			"hostname": "nas", "version": "25.10.0", "uptime_seconds": 100.0,
			"cores": 8.0, "physmem": 32 * gib,
		}
	}
	results["vm.virtualization_details"] = map[string]interface{}{"supported": true, "error": nil}
	results["auth.me"] = map[string]interface{}{
		"pw_name": "truenas_admin",
		"privilege": map[string]interface{}{
			"web_shell": true,
			"roles":     map[string]interface{}{"$set": []interface{}{"VM_WRITE", "FULL_ADMIN"}},
		},
	}
	results["vm.maximum_supported_vcpus"] = 255.0
	results["vm.get_available_memory"] = 16 * gib
	results["vm.device.disk_choices"] = map[string]interface{}{
		"/dev/zvol/tank/vms/free0": "tank/vms/free0",
	}
	results["vm.device.nic_attach_choices"] = map[string]interface{}{
		"br0": "br0", "eno1": "eno1",
	}
	results["vm.device.bind_choices"] = map[string]interface{}{
		"127.0.0.1": "127.0.0.1", "0.0.0.0": "0.0.0.0", "192.0.2.10": "192.0.2.10",
	}
	stoppedDev := map[string]interface{}{
		"id": 10.0, "vm": 1.0, "order": 1001.0,
		"attributes": map[string]interface{}{
			"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso",
		},
	}
	results["vm.device.query"] = []interface{}{stoppedDev}
	results["vm.query"] = []interface{}{
		map[string]interface{}{
			"id": 1.0, "name": "stoppedvm", "uuid": "11111111-1111-1111-1111-111111111111",
			"vcpus": 1.0, "cores": 2.0, "threads": 1.0, "memory": 2048.0,
			"bootloader": "UEFI", "autostart": false,
			"status":  map[string]interface{}{"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"},
			"devices": []interface{}{stoppedDev},
		},
		map[string]interface{}{
			"id": 2.0, "name": "runningvm", "uuid": "22222222-2222-2222-2222-222222222222",
			"vcpus": 1.0, "cores": 2.0, "threads": 1.0, "memory": 2048.0,
			"bootloader": "UEFI", "autostart": false,
			"status":  map[string]interface{}{"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"},
			"devices": []interface{}{},
		},
	}
	results["vm.status"] = map[string]interface{}{"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"}
	return results
}

type vmFakeEnv struct {
	version          string
	cores            float64
	physmem          float64
	virtSupported    bool
	virtErr          interface{}
	maxVCPUs         float64
	availMem         float64
	availMemOC       float64
	vms              []map[string]interface{}
	statuses         map[int]map[string]interface{}
	diskChoices      map[string]string
	nicChoices       interface{}
	bindChoices      map[string]string
	datasets         map[string]map[string]interface{}
	stats            map[string]map[string]interface{}
	statfs           map[string]map[string]interface{}
	failDeviceIdx    int // >=0 fails vm.device.create on this 0-based call
	failUpdate       bool
	failDelete       bool
	failDeviceUpdate bool
	failDeviceDelete bool
	displayDevs      []map[string]interface{} // extra vm.device.query DISPLAY rows
	nextVMID         int
	nextDevID        int
	nextJobID        int
	devCreateCount   int
	jobs             map[int]map[string]interface{}

	// Backing-storage consumers and snapshots for the zvol destroy guards.
	snapshots       []map[string]interface{} // pool.snapshot.query rows
	iscsiExtents    []map[string]interface{}
	nvmetNamespaces []map[string]interface{}
	virtInstances   []map[string]interface{}
	virtDevices     map[string][]map[string]interface{}
	failConsumers   string // method name of a consumer query to fail
	// keepStorage simulates force=true swallowing a failed zvol destroy or
	// unlink: the record is removed but the storage stays.
	keepStorage bool
	// keepVM simulates vm.delete returning while the VM row stays.
	keepVM bool
	// keepDevice simulates vm.device.delete returning while the device row stays.
	keepDevice bool
	// onDeviceCreateFail runs when vm.device.create fails (failDeviceIdx).
	onDeviceCreateFail func()
	// snapshotsIgnoreFilter makes pool.snapshot.query return every row.
	snapshotsIgnoreFilter bool
	// onCall runs before each request is answered.
	onCall func(method string)
}

func newVMFakeEnv(version string) *vmFakeEnv {
	const gib = float64(1 << 30)
	return &vmFakeEnv{
		version:       version,
		cores:         8,
		physmem:       32 * gib,
		virtSupported: true,
		virtErr:       nil,
		maxVCPUs:      255,
		availMem:      16 * gib,
		availMemOC:    24 * gib,
		vms: []map[string]interface{}{
			{
				"id": 1.0, "name": "existing_stopped", "uuid": "aaaa0000-0000-0000-0000-000000000001",
				"vcpus": 1.0, "cores": 2.0, "threads": 1.0, "memory": 2048.0,
				"bootloader": "UEFI", "autostart": false,
				"status": map[string]interface{}{"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"},
				"devices": []interface{}{
					map[string]interface{}{
						"id": 10.0,
						"attributes": map[string]interface{}{
							"dtype": "DISK", "type": "VIRTIO", "path": "/dev/zvol/tank/vms/inuse0",
						},
					},
				},
			},
			{
				"id": 2.0, "name": "existing_running", "uuid": "aaaa0000-0000-0000-0000-000000000002",
				"vcpus": 1.0, "cores": 2.0, "threads": 1.0, "memory": 4096.0,
				"bootloader": "UEFI", "autostart": false,
				"status":  map[string]interface{}{"state": "RUNNING", "pid": 9001.0, "domain_state": "RUNNING"},
				"devices": []interface{}{},
			},
			{
				"id": 3.0, "name": "existing_suspended", "uuid": "aaaa0000-0000-0000-0000-000000000003",
				"vcpus": 1.0, "cores": 2.0, "threads": 1.0, "memory": 2048.0,
				"bootloader": "UEFI", "autostart": false,
				"status":  map[string]interface{}{"state": "SUSPENDED", "pid": 9002.0, "domain_state": "PAUSED"},
				"devices": []interface{}{},
			},
		},
		statuses: map[int]map[string]interface{}{
			1: {"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"},
			2: {"state": "RUNNING", "pid": 9001.0, "domain_state": "RUNNING"},
			3: {"state": "SUSPENDED", "pid": 9002.0, "domain_state": "PAUSED"},
		},
		diskChoices: map[string]string{
			"/dev/zvol/tank/vms/free0":  "tank/vms/free0",
			"/dev/zvol/tank/vms/inuse0": "tank/vms/inuse0",
		},
		nicChoices: map[string]string{
			"br0":  "br0",
			"eno1": "eno1",
		},
		bindChoices: map[string]string{
			"127.0.0.1":  "127.0.0.1",
			"0.0.0.0":    "0.0.0.0",
			"::":         "::",
			"192.0.2.10": "192.0.2.10",
		},
		datasets: map[string]map[string]interface{}{
			"tank": {
				"id": "tank", "name": "tank", "type": "FILESYSTEM", "mountpoint": "/mnt/tank",
				"locked": false, "readonly": map[string]interface{}{"parsed": false},
				"available": map[string]interface{}{"parsed": 500 * gib},
			},
			"tank/vms": {
				"id": "tank/vms", "name": "tank/vms", "type": "FILESYSTEM", "mountpoint": "/mnt/tank/vms",
				"locked": false, "readonly": map[string]interface{}{"parsed": false},
				"available": map[string]interface{}{"parsed": 200 * gib},
			},
			"tank/vms/free0": {
				"id": "tank/vms/free0", "name": "tank/vms/free0", "type": "VOLUME",
				"locked": false, "readonly": map[string]interface{}{"parsed": false},
				"volsize": map[string]interface{}{"parsed": 20 * gib},
			},
			"tank/vms/inuse0": {
				"id": "tank/vms/inuse0", "name": "tank/vms/inuse0", "type": "VOLUME",
				"locked": false, "readonly": map[string]interface{}{"parsed": false},
				"volsize": map[string]interface{}{"parsed": 20 * gib},
			},
			"tank/isos": {
				"id": "tank/isos", "name": "tank/isos", "type": "FILESYSTEM", "mountpoint": "/mnt/tank/isos",
				"locked": false, "readonly": map[string]interface{}{"parsed": false},
				"available": map[string]interface{}{"parsed": 100 * gib},
			},
		},
		stats: map[string]map[string]interface{}{
			"/mnt": {
				"realpath": "/mnt", "type": "DIRECTORY", "inode": 1.0, "dev": 1.0, "mount_id": 1.0, "is_mountpoint": false,
			},
			"/mnt/tank": {
				"realpath": "/mnt/tank", "type": "DIRECTORY", "inode": 2.0, "dev": 2.0, "mount_id": 2.0, "is_mountpoint": true,
			},
			"/mnt/tank/isos": {
				"realpath": "/mnt/tank/isos", "type": "DIRECTORY", "inode": 3.0, "dev": 3.0, "mount_id": 3.0, "is_mountpoint": true,
			},
			"/mnt/tank/isos/installer.iso": {
				"realpath": "/mnt/tank/isos/installer.iso", "type": "FILE",
				"inode": 100.0, "dev": 3.0, "mount_id": 3.0,
				"size": 1.5 * gib, "mtime": 1700000000.0, "uid": 0.0, "gid": 0.0, "mode": 0o100644,
			},
		},
		statfs: map[string]map[string]interface{}{
			"/mnt/tank/isos": {
				"source": "tank/isos", "fstype": "zfs", "dest": "/mnt/tank/isos",
				"fsid": "fsid-isos", "flags": []interface{}{"RW"}, "avail_bytes": 100 * gib,
			},
		},
		failDeviceIdx: -1,
		nextVMID:      10,
		nextDevID:     100,
		nextJobID:     500,
		jobs:          map[int]map[string]interface{}{},
	}
}

func (e *vmFakeEnv) buildRegistry(t *testing.T, readOnly bool) (*Registry, *fakeMiddleware, *tasks.Manager) {
	t.Helper()
	fake := &fakeMiddleware{
		results: map[string]interface{}{},
	}
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if e.onCall != nil {
			e.onCall(method)
		}
		if method == e.failConsumers {
			return nil, map[string]interface{}{"code": -32000, "message": "simulated " + method + " error"}, true
		}
		switch method {
		case "pool.snapshot.query":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			var filters []interface{}
			if len(p) > 0 {
				filters, _ = p[0].([]interface{})
			}
			out := []interface{}{}
			for _, row := range e.snapshots {
				if e.snapshotsIgnoreFilter || datasetRowMatchesFilters(row, filters) {
					out = append(out, row)
				}
			}
			return out, nil, true
		case "iscsi.extent.query":
			return nonNilRows(e.iscsiExtents), nil, true
		case "nvmet.namespace.query":
			return nonNilRows(e.nvmetNamespaces), nil, true
		case "virt.instance.query":
			return nonNilRows(e.virtInstances), nil, true
		case "virt.instance.device_list":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			id, _ := p[0].(string)
			return nonNilRows(e.virtDevices[id]), nil, true
		case "system.info":
			info := map[string]interface{}{"hostname": "nas", "version": e.version, "uptime_seconds": 100.0}
			if e.cores >= 0 {
				info["cores"] = e.cores
			}
			if e.physmem >= 0 {
				info["physmem"] = e.physmem
			}
			return info, nil, true
		case "vm.virtualization_details":
			return map[string]interface{}{"supported": e.virtSupported, "error": e.virtErr}, nil, true
		case "vm.maximum_supported_vcpus":
			return e.maxVCPUs, nil, true
		case "vm.get_available_memory":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			if len(p) > 0 {
				if oc, ok := p[0].(bool); ok && oc {
					return e.availMemOC, nil, true
				}
			}
			return e.availMem, nil, true
		case "vm.device.disk_choices":
			return e.diskChoices, nil, true
		case "vm.device.nic_attach_choices":
			return e.nicChoices, nil, true
		case "vm.device.bind_choices":
			return e.bindChoices, nil, true
		case "vm.query":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			var filters []interface{}
			if len(p) > 0 {
				filters, _ = p[0].([]interface{})
			}
			out := make([]interface{}, 0, len(e.vms))
			for _, vm := range e.vms {
				if vmMatchesFilters(vm, filters) {
					out = append(out, vm)
				}
			}
			return out, nil, true
		case "vm.status":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			if len(p) > 0 {
				if id, ok := p[0].(float64); ok {
					if st, exists := e.statuses[int(id)]; exists {
						return st, nil, true
					}
				}
			}
			return nil, map[string]interface{}{"code": -32000, "message": "VM not found"}, true
		case "pool.dataset.query":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			var filters []interface{}
			if len(p) > 0 {
				filters, _ = p[0].([]interface{})
			}
			out := []interface{}{}
			for _, row := range e.datasets {
				if datasetRowMatchesFilters(row, filters) {
					out = append(out, row)
				}
			}
			return out, nil, true
		case "filesystem.stat":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			if len(p) > 0 {
				if pathStr, ok := p[0].(string); ok {
					if st, exists := e.stats[pathStr]; exists {
						return st, nil, true
					}
					return nil, map[string]interface{}{"code": -32000, "message": fmt.Sprintf("[ENOENT] Path %s not found", pathStr)}, true
				}
			}
			return nil, map[string]interface{}{"code": -32000, "message": "[ENOENT] not found"}, true
		case "filesystem.statfs":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			if len(p) > 0 {
				if pathStr, ok := p[0].(string); ok {
					if st, exists := e.statfs[pathStr]; exists {
						return st, nil, true
					}
				}
			}
			return nil, map[string]interface{}{"code": -32000, "message": "[ENOENT] statfs failed"}, true
		case "vm.create":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			payload, _ := p[0].(map[string]interface{})
			id := e.nextVMID
			e.nextVMID++
			created := copyMapShallow(payload)
			created["id"] = float64(id)
			created["uuid"] = fmt.Sprintf("bbbb0000-0000-0000-0000-%012d", id)
			created["status"] = map[string]interface{}{"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"}
			created["devices"] = []interface{}{}
			e.vms = append(e.vms, created)
			e.statuses[id] = map[string]interface{}{"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"}
			return created, nil, true
		case "vm.device.create":
			idx := e.devCreateCount
			e.devCreateCount++
			if e.failDeviceIdx >= 0 && idx == e.failDeviceIdx {
				if e.onDeviceCreateFail != nil {
					e.onDeviceCreateFail()
				}
				return nil, map[string]interface{}{"code": -32000, "message": "simulated device creation error"}, true
			}
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			payload, _ := p[0].(map[string]interface{})
			id := e.nextDevID
			e.nextDevID++
			created := copyMapShallow(payload)
			created["id"] = float64(id)
			if _, hasOrder := created["order"]; !hasOrder {
				created["order"] = 1002.0
			}
			return created, nil, true
		case "vm.device.query":
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			var filters []interface{}
			if len(p) > 0 {
				filters, _ = p[0].([]interface{})
			}
			seen := map[int]bool{}
			out := []interface{}{}
			addIfMatch := func(d map[string]interface{}) {
				if id, ok := extractZFSInt64(d["id"]); ok && id > 0 {
					if seen[int(id)] {
						return
					}
					seen[int(id)] = true
				}
				if vmDeviceMatchesFilters(d, filters) {
					out = append(out, d)
				}
			}
			for _, vm := range e.vms {
				devs, _ := vm["devices"].([]interface{})
				for _, item := range devs {
					if dm, ok := item.(map[string]interface{}); ok {
						addIfMatch(dm)
					}
				}
			}
			for _, d := range e.displayDevs {
				addIfMatch(d)
			}
			return out, nil, true
		case "vm.device.update":
			if e.failDeviceUpdate {
				return nil, map[string]interface{}{"code": -32000, "message": "simulated device update error"}, true
			}
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			if len(p) < 2 {
				return nil, map[string]interface{}{"code": -32000, "message": "missing params"}, true
			}
			devID := int(p[0].(float64))
			upd, _ := p[1].(map[string]interface{})
			for _, vm := range e.vms {
				devs, _ := vm["devices"].([]interface{})
				for _, item := range devs {
					dm, ok := item.(map[string]interface{})
					if !ok {
						continue
					}
					if id, ok := extractZFSInt64(dm["id"]); ok && int(id) == devID {
						if ord, ok := upd["order"]; ok {
							dm["order"] = ord
						}
						if newAttrs, ok := upd["attributes"].(map[string]interface{}); ok {
							curAttrs, _ := dm["attributes"].(map[string]interface{})
							if curAttrs == nil {
								curAttrs = map[string]interface{}{}
								dm["attributes"] = curAttrs
							}
							for k, v := range newAttrs {
								curAttrs[k] = v
							}
						}
						return copyMapShallow(dm), nil, true
					}
				}
			}
			return nil, map[string]interface{}{"code": -32000, "message": fmt.Sprintf("device %d not found", devID)}, true
		case "vm.device.delete":
			if e.failDeviceDelete {
				return nil, map[string]interface{}{"code": -32000, "message": "simulated device delete error"}, true
			}
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			if len(p) < 1 {
				return nil, map[string]interface{}{"code": -32000, "message": "missing device id"}, true
			}
			devID := int(p[0].(float64))
			var opts map[string]interface{}
			if len(p) > 1 {
				opts, _ = p[1].(map[string]interface{})
			}
			for _, vm := range e.vms {
				devs, _ := vm["devices"].([]interface{})
				for i, item := range devs {
					dm, ok := item.(map[string]interface{})
					if !ok {
						continue
					}
					if id, ok := extractZFSInt64(dm["id"]); ok && int(id) == devID {
						attrs, _ := dm["attributes"].(map[string]interface{})
						pathStr, _ := attrs["path"].(string)
						if delZvol, _ := opts["zvol"].(bool); delZvol && !e.keepStorage {
							if zn, ok := zvolPathToName(pathStr); ok {
								delete(e.datasets, zn)
							}
						}
						if delRaw, _ := opts["raw_file"].(bool); delRaw && !e.keepStorage {
							delete(e.stats, pathStr)
						}
						if !e.keepDevice {
							vm["devices"] = append(devs[:i], devs[i+1:]...)
						}
						return true, nil, true
					}
				}
			}
			return nil, map[string]interface{}{"code": -32000, "message": fmt.Sprintf("device %d not found", devID)}, true
		case "vm.update":
			if e.failUpdate {
				return nil, map[string]interface{}{"code": -32000, "message": "simulated update error"}, true
			}
			return map[string]interface{}{"id": 10.0, "autostart": true}, nil, true
		case "vm.delete":
			if e.failDelete {
				return nil, map[string]interface{}{"code": -32000, "message": "simulated delete error"}, true
			}
			var p []interface{}
			_ = json.Unmarshal(params, &p)
			if len(p) > 0 {
				if vmID, ok := p[0].(float64); ok {
					var opts map[string]interface{}
					if len(p) > 1 {
						opts, _ = p[1].(map[string]interface{})
					}
					delZvols, _ := opts["zvols"].(bool)
					for i, vm := range e.vms {
						if id, ok := extractZFSInt64(vm["id"]); ok && int(id) == int(vmID) {
							if delZvols && !e.keepStorage {
								devs, _ := vm["devices"].([]interface{})
								for _, item := range devs {
									dm, _ := item.(map[string]interface{})
									attrs, _ := dm["attributes"].(map[string]interface{})
									if dtype, _ := attrs["dtype"].(string); dtype == "DISK" {
										if pathStr, _ := attrs["path"].(string); pathStr != "" {
											if zn, ok := zvolPathToName(pathStr); ok {
												delete(e.datasets, zn)
											}
										}
									}
								}
							}
							if !e.keepVM {
								e.vms = append(e.vms[:i], e.vms[i+1:]...)
								delete(e.statuses, int(vmID))
							}
							break
						}
					}
				}
			}
			if v, err := truenas.ParseVersion(e.version); err == nil && !v.Less(truenas.MustParseVersion("28.0")) {
				return nil, nil, true
			}
			return true, nil, true
		case "vm.start", "vm.poweroff", "vm.reset":
			return nil, nil, true
		case "vm.stop", "vm.restart":
			jobID := e.nextJobID
			e.nextJobID++
			e.jobs[jobID] = map[string]interface{}{
				"id":       float64(jobID),
				"method":   method,
				"state":    "RUNNING",
				"progress": map[string]interface{}{"percent": 50.0, "description": "in progress"},
			}
			return float64(jobID), nil, true
		case "core.get_jobs":
			out := make([]interface{}, 0, len(e.jobs))
			for _, j := range e.jobs {
				out = append(out, j)
			}
			return out, nil, true
		default:
			return nil, nil, false
		}
	}
	client, _ := newFakeClientFrom(t, fake)
	if e.version != "" {
		if err := client.RequireSupportedVersion(context.Background()); err != nil {
			t.Fatalf("RequireSupportedVersion(%s): %v", e.version, err)
		}
	}
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: 50 * time.Millisecond, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	r := NewRegistry(client, mgr)
	if readOnly {
		r.SetPolicy(Policy{ReadOnly: true})
	}
	return r, fake, mgr
}

func nonNilRows(rows []map[string]interface{}) []map[string]interface{} {
	if rows == nil {
		return []map[string]interface{}{}
	}
	return rows
}

func vmDeviceMatchesFilters(dev map[string]interface{}, filters []interface{}) bool {
	for _, f := range filters {
		cond, ok := f.([]interface{})
		if !ok || len(cond) != 3 {
			continue
		}
		field, _ := cond[0].(string)
		op, _ := cond[1].(string)
		val := cond[2]
		if op != "=" {
			continue
		}
		var actual interface{}
		if strings.HasPrefix(field, "attributes.") {
			attrs, _ := dev["attributes"].(map[string]interface{})
			actual = attrs[strings.TrimPrefix(field, "attributes.")]
		} else {
			actual = dev[field]
		}
		if fmt.Sprintf("%v", actual) != fmt.Sprintf("%v", val) {
			return false
		}
	}
	return true
}

func vmMatchesFilters(vm map[string]interface{}, filters []interface{}) bool {
	for _, f := range filters {
		cond, ok := f.([]interface{})
		if !ok || len(cond) != 3 {
			continue
		}
		field, _ := cond[0].(string)
		op, _ := cond[1].(string)
		val := cond[2]
		if op == "=" {
			if fmt.Sprintf("%v", vm[field]) != fmt.Sprintf("%v", val) {
				return false
			}
		}
	}
	return true
}

func datasetRowMatchesFilters(row map[string]interface{}, filters []interface{}) bool {
	for _, f := range filters {
		cond, ok := f.([]interface{})
		if !ok || len(cond) != 3 {
			continue
		}
		field, _ := cond[0].(string)
		op, _ := cond[1].(string)
		val, _ := cond[2].(string)
		got, _ := row[field].(string)
		switch op {
		case "=":
			if got != val {
				return false
			}
		case "^":
			if !strings.HasPrefix(got, val) {
				return false
			}
		}
	}
	return true
}

func confirmTokenFromPreview(t *testing.T, previewJSON string) string {
	t.Helper()
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(previewJSON), &obj); err != nil {
		t.Fatalf("parse preview JSON: %v\n%s", err, previewJSON)
	}
	tok, _ := obj["confirmation_token"].(string)
	if tok == "" {
		t.Fatalf("preview has no confirmation_token:\n%s", previewJSON)
	}
	return tok
}

func TestCreateVMFullHappyPathAndPasswordRedaction(t *testing.T) {
	const dispSecret = "vM-pAsS!"
	env := newVMFakeEnv("25.10.0")
	r, fake, _ := env.buildRegistry(t, false)

	args := map[string]interface{}{
		"name":        "lab_truenas_01",
		"description": "Test VM for serial install",
		"vcpus":       1.0,
		"cores":       2.0,
		"threads":     2.0,
		"memory":      4096.0,
		"min_memory":  2048.0,
		"bootloader":  "UEFI",
		"devices": []interface{}{
			map[string]interface{}{
				"dtype": "DISK", "type": "VIRTIO",
				"path": "/dev/zvol/tank/vms/free0",
			},
			map[string]interface{}{
				"dtype": "DISK", "type": "AHCI",
				"create_zvol": true, "zvol_name": "tank/vms/newdisk0",
				"zvol_volsize": float64(10 << 30),
			},
			map[string]interface{}{
				"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso", "order": 1000.0,
			},
			map[string]interface{}{
				"dtype": "NIC", "type": "VIRTIO", "nic_attach": "br0", "mac": "52:54:00:12:34:56",
			},
			map[string]interface{}{
				"dtype": "NIC", "type": "E1000", "nic_attach": "eno1",
			},
			map[string]interface{}{
				"dtype": "DISPLAY", "type": "SPICE", "password": dispSecret,
			},
		},
	}

	preview, err := r.CallTool(t.Context(), "create_vm", args)
	if err != nil {
		t.Fatalf("create_vm preview: %v", err)
	}
	if strings.Contains(preview, dispSecret) {
		t.Fatal("display password leaked in dry-run preview")
	}
	if !strings.Contains(preview, `"password_set": true`) {
		t.Errorf("preview should report password_set=true:\n%s", preview)
	}
	if !strings.Contains(preview, "macvtap") {
		t.Errorf("preview should warn about macvtap on non-bridge eno1:\n%s", preview)
	}
	if len(fake.called("vm.create")) != 0 || len(fake.called("vm.device.create")) != 0 {
		t.Fatal("preview sent write calls to middleware")
	}

	tok := confirmTokenFromPreview(t, preview)
	confirmArgs := copyMapShallow(args)
	confirmArgs["confirmation_token"] = tok

	out, err := r.CallTool(t.Context(), "create_vm", confirmArgs)
	if err != nil {
		t.Fatalf("create_vm confirm: %v", err)
	}
	if strings.Contains(out, dispSecret) {
		t.Fatal("display password leaked in create_vm execute response")
	}

	vmCalls := fake.called("vm.create")
	if len(vmCalls) != 1 {
		t.Fatalf("vm.create calls = %d, want 1", len(vmCalls))
	}
	var vmParams []map[string]interface{}
	if err := json.Unmarshal(vmCalls[0].Params, &vmParams); err != nil || len(vmParams) != 1 {
		t.Fatalf("unmarshal vm.create params: %v", err)
	}
	if vmParams[0]["autostart"] != false {
		t.Errorf("autostart = %v, want false", vmParams[0]["autostart"])
	}
	if vmParams[0]["ensure_display_device"] != false {
		t.Errorf("ensure_display_device = %v, want false", vmParams[0]["ensure_display_device"])
	}

	devCalls := fake.called("vm.device.create")
	if len(devCalls) != 6 {
		t.Fatalf("vm.device.create calls = %d, want 6", len(devCalls))
	}
	// Verify DISPLAY device received bind=127.0.0.1 and the password.
	var dispParams []map[string]interface{}
	if err := json.Unmarshal(devCalls[5].Params, &dispParams); err != nil {
		t.Fatalf("unmarshal display params: %v", err)
	}
	dispAttrs := dispParams[0]["attributes"].(map[string]interface{})
	if dispAttrs["bind"] != "127.0.0.1" {
		t.Errorf("display bind = %v, want 127.0.0.1", dispAttrs["bind"])
	}
	if dispAttrs["password"] != dispSecret {
		t.Error("display password was not forwarded to vm.device.create")
	}
}

func TestCreateVMNicAttachChoicesMasterShape(t *testing.T) {
	env := newVMFakeEnv("28.0.0-MASTER")
	env.nicChoices = map[string]interface{}{
		"BRIDGE":  []interface{}{"br0"},
		"MACVLAN": []interface{}{"enp1s0"},
	}
	r, _, _ := env.buildRegistry(t, false)

	args := map[string]interface{}{
		"name":   "master_nic_vm",
		"memory": 2048.0,
		"devices": []interface{}{
			map[string]interface{}{"dtype": "NIC", "type": "VIRTIO", "nic_attach": "br0"},
			map[string]interface{}{"dtype": "NIC", "type": "VIRTIO", "nic_attach": "enp1s0", "trust_guest_rx_filters": true},
		},
	}
	preview, err := r.CallTool(t.Context(), "create_vm", args)
	if err != nil {
		t.Fatalf("create_vm master nic_attach_choices: %v", err)
	}
	if !strings.Contains(preview, `"attach_mode": "bridge"`) {
		t.Errorf("expected bridge mode for br0:\n%s", preview)
	}
	if !strings.Contains(preview, `"attach_mode": "macvtap"`) {
		t.Errorf("expected macvtap mode for enp1s0:\n%s", preview)
	}

	// trust_guest_rx_filters on bridge must be refused even on master shape.
	badArgs := map[string]interface{}{
		"name":   "master_nic_bad",
		"memory": 2048.0,
		"devices": []interface{}{
			map[string]interface{}{"dtype": "NIC", "type": "VIRTIO", "nic_attach": "br0", "trust_guest_rx_filters": true},
		},
	}
	if _, err := r.CallTool(t.Context(), "create_vm", badArgs); err == nil || !strings.Contains(err.Error(), "trust_guest_rx_filters") {
		t.Fatalf("expected trust_guest_rx_filters bridge refusal, got %v", err)
	}
}

func TestCreateVMRefusalGuards(t *testing.T) {
	const secretVNC = "ToOLongVncPass99"
	const gib = float64(1 << 30)

	cases := []struct {
		name    string
		setup   func(e *vmFakeEnv)
		args    map[string]interface{}
		wantSub string
	}{
		{
			name:    "virtualization unsupported",
			setup:   func(e *vmFakeEnv) { e.virtSupported = false; e.virtErr = "KVM not enabled" },
			args:    map[string]interface{}{"name": "vm1", "memory": 1024.0},
			wantSub: "KVM not enabled",
		},
		{
			name:    "existing VM name",
			args:    map[string]interface{}{"name": "existing_stopped", "memory": 1024.0},
			wantSub: "already exists",
		},
		{
			name:    "invalid VM name chars",
			args:    map[string]interface{}{"name": "bad-name!", "memory": 1024.0},
			wantSub: "alphanumeric",
		},
		{
			name:    "multiline description",
			args:    map[string]interface{}{"name": "vm1", "memory": 1024.0, "description": "line1\nline2"},
			wantSub: "single line",
		},
		{
			name:    "min_memory exceeds memory",
			args:    map[string]interface{}{"name": "vm1", "memory": 1024.0, "min_memory": 2048.0},
			wantSub: "min_memory",
		},
		{
			name:    "cpu_model with HOST-PASSTHROUGH",
			args:    map[string]interface{}{"name": "vm1", "memory": 1024.0, "cpu_mode": "HOST-PASSTHROUGH", "cpu_model": "EPYC"},
			wantSub: "cpu_model",
		},
		{
			name:    "vcpus exceed maximum_supported_vcpus",
			setup:   func(e *vmFakeEnv) { e.maxVCPUs = 4; e.cores = 16 },
			args:    map[string]interface{}{"name": "vm1", "memory": 1024.0, "vcpus": 2.0, "cores": 3.0},
			wantSub: "maximum supported vCPUs",
		},
		{
			name:    "vcpus exceed host cores",
			setup:   func(e *vmFakeEnv) { e.cores = 4 },
			args:    map[string]interface{}{"name": "vm1", "memory": 1024.0, "vcpus": 2.0, "cores": 2.0, "threads": 2.0},
			wantSub: "host CPU count",
		},
		{
			name:    "missing host cores fails closed",
			setup:   func(e *vmFakeEnv) { e.cores = -1 },
			args:    map[string]interface{}{"name": "vm1", "memory": 1024.0},
			wantSub: "host CPU count could not be read",
		},
		{
			name:    "memory exceeds host physmem",
			setup:   func(e *vmFakeEnv) { e.physmem = 2 * gib; e.availMem = 16 * gib },
			args:    map[string]interface{}{"name": "vm1", "memory": 4096.0},
			wantSub: "host physical memory",
		},
		{
			name:    "missing host physmem fails closed",
			setup:   func(e *vmFakeEnv) { e.physmem = -1 },
			args:    map[string]interface{}{"name": "vm1", "memory": 1024.0},
			wantSub: "host physical memory could not be read",
		},
		{
			name: "wrong field for device dtype",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0", "nic_attach": "br0"},
				},
			},
			wantSub: "does not accept field",
		},
		{
			name: "DISK non-zvol path",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/sda"},
				},
			},
			wantSub: "/dev/zvol/",
		},
		{
			name: "DISK managed dataset boot-pool",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/boot-pool/vol1"},
				},
			},
			wantSub: "TrueNAS manages itself",
		},
		{
			name: "DISK managed dataset .system",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/.system/vol1"},
				},
			},
			wantSub: "TrueNAS manages itself",
		},
		{
			name: "DISK managed dataset ix-apps on create_zvol",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "create_zvol": true, "zvol_name": "tank/ix-apps/vol1", "zvol_volsize": float64(1 << 30)},
				},
			},
			wantSub: "TrueNAS manages itself",
		},
		{
			name: "DISK duplicate zvol in same request",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0"},
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0"},
				},
			},
			wantSub: "duplicates zvol",
		},
		{
			name: "DISK zvol already attached to another VM",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/inuse0"},
				},
			},
			wantSub: "already attached to VM",
		},
		{
			name: "DISK zvol not in disk_choices",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/unlisted0"},
				},
			},
			wantSub: "vm.device.disk_choices",
		},
		{
			name: "DISK zvol in disk_choices but missing dataset row refused",
			setup: func(e *vmFakeEnv) {
				e.diskChoices["/dev/zvol/tank/vms/ghost0"] = "tank/vms/ghost0"
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/ghost0"},
				},
			},
			wantSub: "was not found with pool.dataset.query",
		},
		{
			name: "DISK dataset not VOLUME refused",
			setup: func(e *vmFakeEnv) {
				e.datasets["tank/vms/free0"]["type"] = "FILESYSTEM"
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0"},
				},
			},
			wantSub: "not a ZFS VOLUME",
		},
		{
			name: "DISK locked zvol refused",
			setup: func(e *vmFakeEnv) {
				e.datasets["tank/vms/free0"]["locked"] = true
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0"},
				},
			},
			wantSub: "locked",
		},
		{
			name: "DISK readonly zvol refused",
			setup: func(e *vmFakeEnv) {
				e.datasets["tank/vms/free0"]["readonly"] = map[string]interface{}{"parsed": true}
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0"},
				},
			},
			wantSub: "read-only",
		},
		{
			name: "DISK create_zvol and path together refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{
						"dtype": "DISK", "create_zvol": true,
						"path": "/dev/zvol/tank/vms/new0", "zvol_name": "tank/vms/new0", "zvol_volsize": float64(1 << 30),
					},
				},
			},
			wantSub: "cannot set both path and create_zvol",
		},
		{
			name: "DISK create_zvol existing dataset row only refused",
			setup: func(e *vmFakeEnv) {
				e.datasets["tank/vms/onlyds"] = map[string]interface{}{
					"id": "tank/vms/onlyds", "name": "tank/vms/onlyds", "type": "VOLUME", "locked": false,
				}
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{
						"dtype": "DISK", "create_zvol": true,
						"zvol_name": "tank/vms/onlyds", "zvol_volsize": float64(1 << 30),
					},
				},
			},
			wantSub: "refuse to create an existing zvol",
		},
		{
			name: "DISK create_zvol existing in disk_choices only refused",
			setup: func(e *vmFakeEnv) {
				e.diskChoices["/dev/zvol/tank/vms/onlychoice"] = "tank/vms/onlychoice"
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{
						"dtype": "DISK", "create_zvol": true,
						"zvol_name": "tank/vms/onlychoice", "zvol_volsize": float64(1 << 30),
					},
				},
			},
			wantSub: "already exists in vm.device.disk_choices",
		},
		{
			name: "DISK create_zvol missing parent dataset refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{
						"dtype": "DISK", "create_zvol": true,
						"zvol_name": "tank/missingparent/disk0", "zvol_volsize": float64(1 << 30),
					},
				},
			},
			wantSub: "parent dataset tank/missingparent does not exist",
		},
		{
			name: "DISK create_zvol parent not FILESYSTEM refused",
			setup: func(e *vmFakeEnv) {
				e.datasets["tank/vms"]["type"] = "VOLUME"
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{
						"dtype": "DISK", "create_zvol": true,
						"zvol_name": "tank/vms/new0", "zvol_volsize": float64(1 << 30),
					},
				},
			},
			wantSub: "not a FILESYSTEM dataset",
		},
		{
			name: "DISK create_zvol parent locked refused",
			setup: func(e *vmFakeEnv) {
				e.datasets["tank/vms"]["locked"] = true
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{
						"dtype": "DISK", "create_zvol": true,
						"zvol_name": "tank/vms/new0", "zvol_volsize": float64(1 << 30),
					},
				},
			},
			wantSub: "parent dataset tank/vms is locked",
		},
		{
			name: "DISK create_zvol parent readonly refused",
			setup: func(e *vmFakeEnv) {
				e.datasets["tank/vms"]["readonly"] = map[string]interface{}{"parsed": true}
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{
						"dtype": "DISK", "create_zvol": true,
						"zvol_name": "tank/vms/new0", "zvol_volsize": float64(1 << 30),
					},
				},
			},
			wantSub: "parent dataset tank/vms is read-only",
		},
		{
			name: "DISK create_zvol exceeds parent available space",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{
						"dtype": "DISK", "create_zvol": true,
						"zvol_name": "tank/vms/huge0", "zvol_volsize": float64(500 * gib),
					},
				},
			},
			wantSub: "exceeds available space",
		},
		{
			name: "CDROM path on pool root refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/installer.iso"},
				},
			},
			wantSub: "root dataset of pool",
		},
		{
			name: "CDROM path in managed dataset refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/.system/installer.iso"},
				},
			},
			wantSub: "TrueNAS manages itself",
		},
		{
			name: "CDROM dot-prefixed filename refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/.hidden.iso"},
				},
			},
			wantSub: "TrueNAS manages itself",
		},
		{
			name: "CDROM path with braces refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/a{1}.iso"},
				},
			},
			wantSub: "'{' or '}'",
		},
		{
			name: "CDROM missing ISO file refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/missing.iso"},
				},
			},
			wantSub: "does not exist",
		},
		{
			name: "CDROM non-regular file (directory) refused",
			setup: func(e *vmFakeEnv) {
				e.stats["/mnt/tank/isos/dir.iso"] = map[string]interface{}{
					"realpath": "/mnt/tank/isos/dir.iso", "type": "DIRECTORY", "inode": 201.0, "dev": 3.0, "mount_id": 3.0,
				}
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/dir.iso"},
				},
			},
			wantSub: "not a regular file",
		},
		{
			name: "CDROM symlink ISO refused",
			setup: func(e *vmFakeEnv) {
				e.stats["/mnt/tank/isos/link.iso"] = map[string]interface{}{
					"realpath": "/etc/passwd", "type": "SYMLINK", "inode": 200.0, "dev": 3.0, "mount_id": 3.0,
				}
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/link.iso"},
				},
			},
			wantSub: "symlink",
		},
		{
			name: "CDROM file realpath mismatch refused",
			setup: func(e *vmFakeEnv) {
				e.stats["/mnt/tank/isos/installer.iso"]["realpath"] = "/mnt/tank/other/installer.iso"
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
				},
			},
			wantSub: "resolves to",
		},
		{
			name: "CDROM parent symlink refused",
			setup: func(e *vmFakeEnv) {
				e.stats["/mnt/tank/isos"] = map[string]interface{}{
					"realpath": "/mnt/tank/realisos", "type": "SYMLINK", "inode": 3.0, "dev": 3.0, "mount_id": 3.0,
				}
				e.stats["/mnt/tank/realisos"] = map[string]interface{}{
					"realpath": "/mnt/tank/realisos", "type": "DIRECTORY", "inode": 4.0, "dev": 3.0, "mount_id": 3.0, "is_mountpoint": true,
				}
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
				},
			},
			wantSub: "symlink",
		},
		{
			name: "CDROM non-ZFS filesystem refused",
			setup: func(e *vmFakeEnv) {
				e.statfs["/mnt/tank/isos"]["fstype"] = "ext4"
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
				},
			},
			wantSub: "not on a ZFS dataset",
		},
		{
			name: "CDROM different pool in statfs refused",
			setup: func(e *vmFakeEnv) {
				e.statfs["/mnt/tank/isos"]["source"] = "otherpool/isos"
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
				},
			},
			wantSub: "not on a dataset of pool",
		},
		{
			name: "CDROM managed dataset via statfs refused",
			setup: func(e *vmFakeEnv) {
				e.statfs["/mnt/tank/isos"]["source"] = "tank/.system"
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
				},
			},
			wantSub: "TrueNAS manages itself",
		},
		{
			name: "CDROM on pool root dataset via statfs refused",
			setup: func(e *vmFakeEnv) {
				e.statfs["/mnt/tank/isos"]["source"] = "tank"
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
				},
			},
			wantSub: "root dataset of pool",
		},
		{
			name: "CDROM missing dataset row refused",
			setup: func(e *vmFakeEnv) {
				delete(e.datasets, "tank/isos")
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
				},
			},
			wantSub: "was not found with pool.dataset.query",
		},
		{
			name: "CDROM on locked dataset refused",
			setup: func(e *vmFakeEnv) {
				e.datasets["tank/isos"]["locked"] = true
			},
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
				},
			},
			wantSub: "locked",
		},
		{
			name: "NIC unknown nic_attach refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "NIC", "nic_attach": "eth99"},
				},
			},
			wantSub: "vm.device.nic_attach_choices",
		},
		{
			name: "NIC multicast MAC refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "NIC", "nic_attach": "br0", "mac": "ff:00:11:22:33:44"},
				},
			},
			wantSub: "multicast or broadcast",
		},
		{
			name: "NIC trust_guest_rx_filters on E1000 refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "NIC", "type": "E1000", "nic_attach": "eno1", "trust_guest_rx_filters": true},
				},
			},
			wantSub: "E1000",
		},
		{
			name: "NIC trust_guest_rx_filters on bridge refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "NIC", "type": "VIRTIO", "nic_attach": "br0", "trust_guest_rx_filters": true},
				},
			},
			wantSub: "bridge interface",
		},
		{
			name: "DISPLAY bind 0.0.0.0 refused without flag",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISPLAY", "bind": "0.0.0.0"},
				},
			},
			wantSub: "allow_wildcard_display_bind",
		},
		{
			name: "DISPLAY bind :: refused without flag",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISPLAY", "bind": "::"},
				},
			},
			wantSub: "allow_wildcard_display_bind",
		},
		{
			name: "DISPLAY bind not in bind_choices refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISPLAY", "bind": "192.0.2.99"},
				},
			},
			wantSub: "vm.device.bind_choices",
		},
		{
			name: "DISPLAY duplicate SPICE refused",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE"},
					map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE"},
				},
			},
			wantSub: "duplicates SPICE display",
		},
		{
			name: "DISPLAY VNC password > 8 chars refused without echoing password",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISPLAY", "type": "VNC", "password": secretVNC},
				},
			},
			wantSub: "at most 8 characters",
		},
		{
			name: "DISPLAY VNC web=true refused on 25.10",
			args: map[string]interface{}{
				"name": "vm1", "memory": 1024.0,
				"devices": []interface{}{
					map[string]interface{}{"dtype": "DISPLAY", "type": "VNC", "web": true},
				},
			},
			wantSub: "VNC does not support web=true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMFakeEnv("25.10.0")
			if tc.setup != nil {
				tc.setup(env)
			}
			r, fake, _ := env.buildRegistry(t, false)
			_, err := r.CallTool(t.Context(), "create_vm", tc.args)
			if err == nil {
				t.Fatalf("expected refusal containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
			if strings.Contains(err.Error(), secretVNC) {
				t.Errorf("error leaked display password: %v", err)
			}
			assertNoWriteCalls(t, fake)
		})
	}
}

func TestCreateVMAllowWildcardDisplayBind(t *testing.T) {
	env := newVMFakeEnv("25.10.0")
	r, _, _ := env.buildRegistry(t, false)

	args := map[string]interface{}{
		"name":                        "wildcard_vm",
		"memory":                      1024.0,
		"allow_wildcard_display_bind": true,
		"devices": []interface{}{
			map[string]interface{}{"dtype": "DISPLAY", "bind": "0.0.0.0", "password": "pw1"},
		},
	}
	preview, err := r.CallTool(t.Context(), "create_vm", args)
	if err != nil {
		t.Fatalf("wildcard bind with explicit flag failed: %v", err)
	}
	if !strings.Contains(preview, "exposing the VM console on all host interfaces") {
		t.Errorf("expected wildcard bind warning in preview:\n%s", preview)
	}
}

func TestCreateVMPartialDeviceFailureRollsBack(t *testing.T) {
	const pw = "Sup3rSec"
	args := func() map[string]interface{} {
		return map[string]interface{}{
			"name":      "partial_vm",
			"memory":    1024.0,
			"autostart": true,
			"devices": []interface{}{
				map[string]interface{}{"dtype": "DISK", "create_zvol": true, "zvol_name": "tank/vms/new1", "zvol_volsize": float64(1 << 30)},
				map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0"},
				map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE", "password": pw},
			},
		}
	}
	run := func(t *testing.T, env *vmFakeEnv) (*fakeMiddleware, error) {
		t.Helper()
		r, fake, _ := env.buildRegistry(t, false)
		preview, err := r.CallTool(t.Context(), "create_vm", args())
		if err != nil {
			t.Fatal(err)
		}
		confirmArgs := args()
		confirmArgs["confirmation_token"] = confirmTokenFromPreview(t, preview)
		_, err = r.CallTool(t.Context(), "create_vm", confirmArgs)
		if err == nil {
			t.Fatal("expected partial device creation error")
		}
		if strings.Contains(err.Error(), pw) {
			t.Errorf("partial failure error leaked password: %v", err)
		}
		if n := len(fake.called("vm.update")); n != 0 {
			t.Errorf("vm.update calls = %d, want 0 (autostart must not be enabled on a partial VM)", n)
		}
		return fake, err
	}

	t.Run("rollback deletes the VM and keeps zvols", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		env.failDeviceIdx = 2
		fake, err := run(t, env)
		for _, want := range []string{"attached 2 of 3 device(s)", "devices[2] (DISPLAY)", "Rolled back: deleted VM \"partial_vm\" (id 10)", "kept: tank/vms/new1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err.Error(), want)
			}
		}
		del := fake.called("vm.delete")
		if len(del) != 1 {
			t.Fatalf("vm.delete calls = %d, want 1", len(del))
		}
		var p []interface{}
		if err := json.Unmarshal(del[0].Params, &p); err != nil || len(p) != 2 {
			t.Fatalf("vm.delete params: %s", del[0].Params)
		}
		opts, _ := p[1].(map[string]interface{})
		if p[0] != 10.0 || opts["zvols"] != false || opts["force"] != false {
			t.Errorf("vm.delete params = %s, want [10, {zvols:false, force:false}]", del[0].Params)
		}
	})

	t.Run("failed zvol device is reported as possibly created", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		env.failDeviceIdx = 0
		_, err := run(t, env)
		for _, want := range []string{"kept: none", "zvol tank/vms/new1 of the failed device may also have been created"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err.Error(), want)
			}
		}
	})

	t.Run("rollback failure says the VM still exists", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		env.failDeviceIdx = 2
		env.failDelete = true
		_, err := run(t, env)
		for _, want := range []string{"Rollback with vm.delete also failed", "VM \"partial_vm\" (id 10) still exists with autostart off and 2 device(s) attached", "Zvols created by this call: tank/vms/new1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err.Error(), want)
			}
		}
	})
}

func TestCreateVMAutostartEnabledAfterDevices(t *testing.T) {
	args := func() map[string]interface{} {
		return map[string]interface{}{
			"name":      "auto_vm",
			"memory":    1024.0,
			"autostart": true,
			"devices": []interface{}{
				map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0"},
				map[string]interface{}{"dtype": "NIC", "nic_attach": "br0"},
			},
		}
	}
	confirm := func(t *testing.T, env *vmFakeEnv) (*fakeMiddleware, string, error) {
		t.Helper()
		r, fake, _ := env.buildRegistry(t, false)
		preview, err := r.CallTool(t.Context(), "create_vm", args())
		if err != nil {
			t.Fatal(err)
		}
		confirmArgs := args()
		confirmArgs["confirmation_token"] = confirmTokenFromPreview(t, preview)
		out, err := r.CallTool(t.Context(), "create_vm", confirmArgs)
		return fake, out, err
	}

	t.Run("vm.create gets autostart=false and vm.update follows the last device", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		fake, out, err := confirm(t, env)
		if err != nil {
			t.Fatal(err)
		}
		var vmParams []map[string]interface{}
		if err := json.Unmarshal(fake.called("vm.create")[0].Params, &vmParams); err != nil {
			t.Fatal(err)
		}
		if vmParams[0]["autostart"] != false {
			t.Errorf("vm.create autostart = %v, want false", vmParams[0]["autostart"])
		}
		var order []string
		fake.mu.Lock()
		for _, c := range fake.calls {
			if c.Method == "vm.create" || c.Method == "vm.device.create" || c.Method == "vm.update" {
				order = append(order, c.Method)
			}
		}
		fake.mu.Unlock()
		want := "vm.create,vm.device.create,vm.device.create,vm.update"
		if got := strings.Join(order, ","); got != want {
			t.Errorf("call order = %s, want %s", got, want)
		}
		upd := fake.called("vm.update")
		if !strings.Contains(string(upd[0].Params), `"autostart":true`) || !strings.HasPrefix(string(upd[0].Params), "[10,") {
			t.Errorf("vm.update params = %s", upd[0].Params)
		}
		if !strings.Contains(out, "enabled autostart") {
			t.Errorf("unexpected output: %s", out)
		}
	})

	t.Run("vm.update failure reports autostart still off without rollback", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		env.failUpdate = true
		fake, _, err := confirm(t, env)
		if err == nil || !strings.Contains(err.Error(), "autostart is still off") {
			t.Fatalf("err = %v, want autostart-still-off error", err)
		}
		if n := len(fake.called("vm.delete")); n != 0 {
			t.Errorf("vm.delete calls = %d, want 0", n)
		}
	})

	t.Run("autostart false sends no vm.update", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		a := args()
		a["autostart"] = false
		preview, err := r.CallTool(t.Context(), "create_vm", a)
		if err != nil {
			t.Fatal(err)
		}
		a["confirmation_token"] = confirmTokenFromPreview(t, preview)
		if _, err := r.CallTool(t.Context(), "create_vm", a); err != nil {
			t.Fatal(err)
		}
		if n := len(fake.called("vm.update")); n != 0 {
			t.Errorf("vm.update calls = %d, want 0", n)
		}
	})
}

func TestCreateVMMiddlewareDeviceChecksUpFront(t *testing.T) {
	disp := func(extra map[string]interface{}) map[string]interface{} {
		d := map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE", "password": "pw1"}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}
	otherDisplay := map[string]interface{}{
		"id": 60.0, "vm": 1.0, "attributes": map[string]interface{}{"dtype": "DISPLAY", "port": 5900.0, "web_port": 5901.0},
	}
	cases := []struct {
		name    string
		version string
		setup   func(e *vmFakeEnv)
		devices []interface{}
		wantSub string // "" = must pass preview
	}{
		{name: "25.10 missing display password", version: "25.10.0",
			devices: []interface{}{map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE"}},
			wantSub: "requires a display password"},
		{name: "25.10 empty display password", version: "25.10.0",
			devices: []interface{}{map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE", "password": ""}},
			wantSub: "requires a display password"},
		{name: "blank display password", version: "28.0.0-MASTER",
			devices: []interface{}{map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE", "password": "   "}},
			wantSub: "must not be blank"},
		{name: "master allows no display password", version: "28.0.0-MASTER",
			devices: []interface{}{map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE"}}},
		{name: "display port used by another VM", version: "25.10.0",
			setup:   func(e *vmFakeEnv) { e.displayDevs = []map[string]interface{}{otherDisplay} },
			devices: []interface{}{disp(map[string]interface{}{"port": 5900.0})},
			wantSub: "port 5900 is already used by a display device of VM id 1"},
		{name: "web_port used by another VM", version: "28.0.0-MASTER",
			setup:   func(e *vmFakeEnv) { e.displayDevs = []map[string]interface{}{otherDisplay} },
			devices: []interface{}{disp(map[string]interface{}{"web_port": 5901.0})},
			wantSub: "web_port 5901 is already used by a display device of VM id 1"},
		{name: "reserved port 6000", version: "25.10.0",
			devices: []interface{}{disp(map[string]interface{}{"port": 6000.0})},
			wantSub: "reserved by TrueNAS"},
		{name: "free display ports pass", version: "25.10.0",
			setup:   func(e *vmFakeEnv) { e.displayDevs = []map[string]interface{}{otherDisplay} },
			devices: []interface{}{disp(map[string]interface{}{"port": 5902.0, "web_port": 5903.0})}},
		{name: "port equal to web_port in one request", version: "25.10.0",
			devices: []interface{}{disp(map[string]interface{}{"port": 5950.0, "web_port": 5950.0})},
			wantSub: "already used by devices[0].port in this request"},
		{name: "logical sector size above physical", version: "28.0.0-MASTER",
			devices: []interface{}{map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0", "logical_sectorsize": 4096.0, "physical_sectorsize": 512.0}},
			wantSub: "cannot be greater than physical_sectorsize"},
		{name: "25.10 physical without logical", version: "25.10.0",
			devices: []interface{}{map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0", "physical_sectorsize": 4096.0}},
			wantSub: "physical_sectorsize requires logical_sectorsize"},
		{name: "master physical without logical passes", version: "28.0.0-MASTER",
			devices: []interface{}{map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0", "physical_sectorsize": 4096.0}}},
		{name: "valid sector sizes pass", version: "25.10.0",
			devices: []interface{}{map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0", "logical_sectorsize": 512.0, "physical_sectorsize": 4096.0}}},
		// An unknown version gets the stricter pre-27.0 rules.
		{name: "unknown version missing display password", version: "",
			devices: []interface{}{map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE"}},
			wantSub: "requires a display password"},
		{name: "unknown version physical without logical", version: "",
			devices: []interface{}{map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0", "physical_sectorsize": 4096.0}},
			wantSub: "physical_sectorsize requires logical_sectorsize"},
		{name: "unknown version VNC web", version: "",
			devices: []interface{}{disp(map[string]interface{}{"type": "VNC", "web": true})},
			wantSub: "VNC does not support web=true"},
		{name: "unknown version valid devices pass", version: "",
			devices: []interface{}{disp(nil), map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0", "logical_sectorsize": 512.0, "physical_sectorsize": 4096.0}}},
		{name: "disk serial refused", version: "25.10.0",
			devices: []interface{}{map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0", "serial": "ABC123"}},
			wantSub: "serial"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMFakeEnv(tc.version)
			if tc.setup != nil {
				tc.setup(env)
			}
			r, fake, _ := env.buildRegistry(t, false)
			args := map[string]interface{}{"name": "vm1", "memory": 1024.0, "devices": tc.devices}
			_, err := r.CallTool(t.Context(), "create_vm", args)
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("preview should pass, got %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
			assertNoWriteCalls(t, fake)
		})
	}
}

func TestCreateVMAvailableMemoryOnlyWarns(t *testing.T) {
	const gib = float64(1 << 30)
	env := newVMFakeEnv("25.10.0")
	env.availMem = 1 * gib
	r, fake, _ := env.buildRegistry(t, false)
	args := map[string]interface{}{"name": "bigvm", "memory": 2048.0}
	preview, err := r.CallTool(t.Context(), "create_vm", args)
	if err != nil {
		t.Fatalf("available memory should only warn, got %v", err)
	}
	if !strings.Contains(preview, "exceeds currently available VM memory") {
		t.Errorf("missing available-memory warning:\n%s", preview)
	}
	args["confirmation_token"] = confirmTokenFromPreview(t, preview)
	if _, err := r.CallTool(t.Context(), "create_vm", args); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if n := len(fake.called("vm.create")); n != 1 {
		t.Fatalf("vm.create calls = %d, want 1", n)
	}
}

func TestCreateVMCDROMGuardsWithoutOtherCoverage(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(e *vmFakeEnv)
		path    string
		wantSub string
	}{
		{name: "path outside /mnt", path: "/etc/installer.iso", wantSub: "outside the storage pools"},
		{name: "parent is not a directory",
			setup: func(e *vmFakeEnv) {
				e.stats["/mnt/tank/isos"] = map[string]interface{}{
					"realpath": "/mnt/tank/isos", "type": "FILE", "inode": 3.0, "dev": 3.0, "mount_id": 3.0,
				}
			},
			path: "/mnt/tank/isos/installer.iso", wantSub: "/mnt/tank/isos is a FILE, not a directory"},
		{name: "locked child dataset under the ISO parent",
			setup: func(e *vmFakeEnv) {
				e.datasets["tank/isos/sub"] = map[string]interface{}{
					"id": "tank/isos/sub", "name": "tank/isos/sub", "type": "FILESYSTEM", "mountpoint": "/mnt/tank/isos/sub", "locked": true,
				}
				e.stats["/mnt/tank/isos/sub"] = map[string]interface{}{
					"realpath": "/mnt/tank/isos/sub", "type": "DIRECTORY", "inode": 5.0, "dev": 3.0, "mount_id": 3.0,
				}
				e.stats["/mnt/tank/isos/sub/installer.iso"] = map[string]interface{}{
					"realpath": "/mnt/tank/isos/sub/installer.iso", "type": "FILE", "inode": 101.0, "dev": 3.0, "mount_id": 3.0,
					"size": 1024.0, "mtime": 1700000000.0, "uid": 0.0, "gid": 0.0, "mode": 0o100644,
				}
				e.statfs["/mnt/tank/isos/sub"] = e.statfs["/mnt/tank/isos"]
			},
			path: "/mnt/tank/isos/sub/installer.iso", wantSub: "is inside dataset tank/isos/sub, which is locked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMFakeEnv("25.10.0")
			if tc.setup != nil {
				tc.setup(env)
			}
			r, fake, _ := env.buildRegistry(t, false)
			args := map[string]interface{}{"name": "vm1", "memory": 1024.0, "devices": []interface{}{
				map[string]interface{}{"dtype": "CDROM", "path": tc.path},
			}}
			_, err := r.CallTool(t.Context(), "create_vm", args)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
			assertNoWriteCalls(t, fake)
		})
	}
}

func TestCreateVMTokenBindingAndStaleness(t *testing.T) {
	const gib = float64(1 << 30)

	baseArgs := func() map[string]interface{} {
		return map[string]interface{}{
			"name":   "bound_vm",
			"memory": 2048.0,
			"devices": []interface{}{
				map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/free0"},
				map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
				map[string]interface{}{"dtype": "NIC", "nic_attach": "br0"},
				map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE", "password": "pw1"},
			},
		}
	}

	mutations := []struct {
		name   string
		mutate func(e *vmFakeEnv, confirmArgs map[string]interface{})
	}{
		{
			name: "disk_choices changed",
			mutate: func(e *vmFakeEnv, _ map[string]interface{}) {
				e.diskChoices["/dev/zvol/tank/vms/extra"] = "tank/vms/extra"
			},
		},
		{
			name: "zvol resized",
			mutate: func(e *vmFakeEnv, _ map[string]interface{}) {
				e.datasets["tank/vms/free0"]["volsize"] = map[string]interface{}{"parsed": 40 * gib}
			},
		},
		{
			name: "ISO file mtime changed",
			mutate: func(e *vmFakeEnv, _ map[string]interface{}) {
				e.stats["/mnt/tank/isos/installer.iso"]["mtime"] = 1700009999.0
			},
		},
		{
			name: "nic_attach_choices changed",
			mutate: func(e *vmFakeEnv, _ map[string]interface{}) {
				e.nicChoices = map[string]string{"br0": "br0", "eno1": "eno1", "eno2": "eno2"}
			},
		},
		{
			name:   "bind_choices changed",
			mutate: func(e *vmFakeEnv, _ map[string]interface{}) { e.bindChoices["192.0.2.20"] = "192.0.2.20" },
		},
		{
			name: "display password changed",
			mutate: func(_ *vmFakeEnv, confirmArgs map[string]interface{}) {
				devs := confirmArgs["devices"].([]interface{})
				devs[3].(map[string]interface{})["password"] = "pw2"
			},
		},
		{
			name: "another VM added a display device",
			mutate: func(e *vmFakeEnv, _ map[string]interface{}) {
				e.displayDevs = append(e.displayDevs, map[string]interface{}{
					"id": 70.0, "vm": 2.0, "attributes": map[string]interface{}{"dtype": "DISPLAY", "port": 5910.0, "web_port": 5911.0},
				})
			},
		},
	}

	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMFakeEnv("25.10.0")
			r, fake, _ := env.buildRegistry(t, false)

			args := baseArgs()
			preview, err := r.CallTool(t.Context(), "create_vm", args)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			tok := confirmTokenFromPreview(t, preview)

			confirmArgs := baseArgs()
			confirmArgs["confirmation_token"] = tok
			tc.mutate(env, confirmArgs)

			if _, err := r.CallTool(t.Context(), "create_vm", confirmArgs); err == nil {
				t.Fatalf("mutation %q should have rejected or blocked confirm", tc.name)
			}
			assertNoWriteCalls(t, fake)
		})
	}

	// Small available_memory fluctuation (still enough) must NOT invalidate token.
	t.Run("available memory jitter within budget keeps token valid", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		args := baseArgs()
		preview, err := r.CallTool(t.Context(), "create_vm", args)
		if err != nil {
			t.Fatal(err)
		}
		env.availMem -= 64 * 1024 * 1024 // 64 MiB ARC drift
		confirmArgs := baseArgs()
		confirmArgs["confirmation_token"] = confirmTokenFromPreview(t, preview)
		if _, err := r.CallTool(t.Context(), "create_vm", confirmArgs); err != nil {
			t.Fatalf("confirm failed despite sufficient available memory: %v", err)
		}
		if len(fake.called("vm.create")) != 1 {
			t.Fatalf("vm.create calls = %d, want 1", len(fake.called("vm.create")))
		}
	})
}

func TestVMPowerToolsAndJobTracking(t *testing.T) {
	t.Run("start_vm happy path with overcommit and refusal when running", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)

		// Refused when RUNNING.
		if _, err := r.CallTool(t.Context(), "start_vm", map[string]interface{}{"name": "existing_running"}); err == nil || !strings.Contains(err.Error(), "already RUNNING") {
			t.Fatalf("expected RUNNING refusal, got %v", err)
		}
		// Refused when SUSPENDED.
		if _, err := r.CallTool(t.Context(), "start_vm", map[string]interface{}{"id": 3.0}); err == nil || !strings.Contains(err.Error(), "already SUSPENDED") {
			t.Fatalf("expected SUSPENDED refusal, got %v", err)
		}
		// Refused when id and name mismatch.
		if _, err := r.CallTool(t.Context(), "start_vm", map[string]interface{}{"id": 1.0, "name": "existing_running"}); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("expected id/name mismatch refusal, got %v", err)
		}
		// Refused when neither id nor name provided.
		if _, err := r.CallTool(t.Context(), "start_vm", map[string]interface{}{}); err == nil || !strings.Contains(err.Error(), "id or name") {
			t.Fatalf("expected selector refusal, got %v", err)
		}
		// Refused when virtualization unsupported.
		env.virtSupported = false
		if _, err := r.CallTool(t.Context(), "start_vm", map[string]interface{}{"id": 1.0}); err == nil || !strings.Contains(err.Error(), "virtualization") {
			t.Fatalf("expected virtualization refusal, got %v", err)
		}
		env.virtSupported = true
		// Refused when insufficient memory without overcommit, allowed with overcommit=true.
		env.availMem = 1 << 30 // 1 GiB < 2048 MiB
		if _, err := r.CallTool(t.Context(), "start_vm", map[string]interface{}{"id": 1.0}); err == nil || !strings.Contains(err.Error(), "available") {
			t.Fatalf("expected memory refusal, got %v", err)
		}
		assertNoWriteCalls(t, fake)

		preview, err := r.CallTool(t.Context(), "start_vm", map[string]interface{}{"name": "existing_stopped", "overcommit": true})
		if err != nil {
			t.Fatalf("start_vm preview: %v", err)
		}
		tok := confirmTokenFromPreview(t, preview)
		out, err := r.CallTool(t.Context(), "start_vm", map[string]interface{}{
			"name": "existing_stopped", "overcommit": true, "confirmation_token": tok,
		})
		if err != nil {
			t.Fatalf("start_vm confirm: %v", err)
		}
		if !strings.Contains(out, `"status": "started"`) {
			t.Errorf("unexpected start_vm output: %s", out)
		}
		calls := fake.called("vm.start")
		if len(calls) != 1 {
			t.Fatalf("vm.start calls = %d, want 1", len(calls))
		}
		if !strings.Contains(string(calls[0].Params), `"overcommit":true`) {
			t.Errorf("vm.start params = %s, want overcommit:true", calls[0].Params)
		}
	})

	t.Run("stop_vm graceful default, force options, refusal when stopped, and job tracking", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		r, fake, mgr := env.buildRegistry(t, false)

		if _, err := r.CallTool(t.Context(), "stop_vm", map[string]interface{}{"id": 1.0}); err == nil || !strings.Contains(err.Error(), "already STOPPED") {
			t.Fatalf("expected STOPPED refusal, got %v", err)
		}
		assertNoWriteCalls(t, fake)

		preview, err := r.CallTool(t.Context(), "stop_vm", map[string]interface{}{"id": 2.0})
		if err != nil {
			t.Fatalf("stop_vm preview: %v", err)
		}
		tok := confirmTokenFromPreview(t, preview)
		out, err := r.CallTool(t.Context(), "stop_vm", map[string]interface{}{"id": 2.0, "confirmation_token": tok})
		if err != nil {
			t.Fatalf("stop_vm confirm: %v", err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		taskID, _ := resp["task_id"].(string)
		if taskID == "" {
			t.Fatalf("stop_vm did not return task_id: %s", out)
		}
		task, err := mgr.Get(taskID)
		if err != nil || task.ToolName != "stop_vm" {
			t.Fatalf("task lookup failed: task=%+v err=%v", task, err)
		}
		calls := fake.called("vm.stop")
		if len(calls) != 1 || !strings.Contains(string(calls[0].Params), `"force":false`) || !strings.Contains(string(calls[0].Params), `"force_after_timeout":false`) {
			t.Errorf("vm.stop params = %v, want graceful defaults", calls)
		}
	})

	t.Run("poweroff_vm refusal when stopped and token bound to status", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)

		if _, err := r.CallTool(t.Context(), "poweroff_vm", map[string]interface{}{"id": 1.0}); err == nil || !strings.Contains(err.Error(), "already STOPPED") {
			t.Fatalf("expected STOPPED refusal, got %v", err)
		}

		preview, err := r.CallTool(t.Context(), "poweroff_vm", map[string]interface{}{"id": 2.0})
		if err != nil {
			t.Fatal(err)
		}
		tok := confirmTokenFromPreview(t, preview)

		// Change VM PID (status changed) -> token must be rejected!
		env.statuses[2]["pid"] = 9999.0
		if _, err := r.CallTool(t.Context(), "poweroff_vm", map[string]interface{}{"id": 2.0, "confirmation_token": tok}); err == nil {
			t.Fatal("poweroff_vm accepted stale token after VM pid changed")
		}
		assertNoWriteCalls(t, fake)

		// Fresh preview + confirm succeeds.
		preview2, err := r.CallTool(t.Context(), "poweroff_vm", map[string]interface{}{"id": 2.0})
		if err != nil {
			t.Fatal(err)
		}
		tok2 := confirmTokenFromPreview(t, preview2)
		out, err := r.CallTool(t.Context(), "poweroff_vm", map[string]interface{}{"id": 2.0, "confirmation_token": tok2})
		if err != nil {
			t.Fatalf("poweroff_vm confirm: %v", err)
		}
		if !strings.Contains(out, `"status": "powered_off"`) || len(fake.called("vm.poweroff")) != 1 {
			t.Errorf("unexpected poweroff result: %s", out)
		}
	})

	t.Run("restart_vm refusal when stopped and job tracking", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		r, fake, mgr := env.buildRegistry(t, false)

		if _, err := r.CallTool(t.Context(), "restart_vm", map[string]interface{}{"id": 1.0}); err == nil || !strings.Contains(err.Error(), "STOPPED") {
			t.Fatalf("expected STOPPED refusal, got %v", err)
		}
		assertNoWriteCalls(t, fake)

		preview, err := r.CallTool(t.Context(), "restart_vm", map[string]interface{}{"id": 2.0})
		if err != nil {
			t.Fatal(err)
		}
		tok := confirmTokenFromPreview(t, preview)
		out, err := r.CallTool(t.Context(), "restart_vm", map[string]interface{}{"id": 2.0, "confirmation_token": tok})
		if err != nil {
			t.Fatalf("restart_vm confirm: %v", err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		taskID, _ := resp["task_id"].(string)
		if taskID == "" {
			t.Fatalf("restart_vm did not return task_id: %s", out)
		}
		task, err := mgr.Get(taskID)
		if err != nil || task.ToolName != "restart_vm" {
			t.Fatalf("restart_vm task lookup failed: task=%+v err=%v", task, err)
		}
		if len(fake.called("vm.restart")) != 1 {
			t.Fatalf("vm.restart calls = %d, want 1", len(fake.called("vm.restart")))
		}
	})
}

func TestResetVMVersionGatingAndTokenBinding(t *testing.T) {
	// reset_vm must be hidden and refused on non-MASTER releases before 28.0 (vm.reset exists in v28_0_0 / master only).
	for _, ver := range []string{"25.10.0", "25.10.8", "27.0.0", "27.1.0"} {
		t.Run("refused on "+ver, func(t *testing.T) {
			env := newVMFakeEnv(ver)
			r, fake, _ := env.buildRegistry(t, false)
			if listed(r)["reset_vm"] {
				t.Errorf("reset_vm should be hidden on TrueNAS %s", ver)
			}
			_, err := r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 2.0})
			if err == nil || !strings.Contains(err.Error(), "28.0") || !strings.Contains(err.Error(), "nothing was executed") {
				t.Fatalf("reset_vm on %s: err = %v, want 28.0 version gate error", ver, err)
			}
			if len(fake.called("vm.query")) != 0 || len(fake.called("vm.reset")) != 0 {
				t.Errorf("reset_vm made middleware calls despite version gate")
			}
		})
	}

	for _, ver := range []string{"27.1.0-MASTER", "28.0.0"} {
		t.Run("allowed on "+ver, func(t *testing.T) {
			env := newVMFakeEnv(ver)
			r, fake, _ := env.buildRegistry(t, false)
			if !listed(r)["reset_vm"] {
				t.Fatalf("reset_vm should be listed on TrueNAS %s", ver)
			}
			preview, err := r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 2.0})
			if err != nil {
				t.Fatalf("reset_vm preview on %s: %v", ver, err)
			}
			tok := confirmTokenFromPreview(t, preview)
			out, err := r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 2.0, "confirmation_token": tok})
			if err != nil {
				t.Fatalf("reset_vm confirm on %s: %v", ver, err)
			}
			if !strings.Contains(out, `"status": "reset"`) || len(fake.called("vm.reset")) != 1 {
				t.Errorf("unexpected reset_vm output on %s: %s", ver, out)
			}
		})
	}

	t.Run("allowed on 28.0.0-MASTER with guards and status binding", func(t *testing.T) {
		env := newVMFakeEnv("28.0.0-MASTER")
		r, fake, _ := env.buildRegistry(t, false)
		if !listed(r)["reset_vm"] {
			t.Fatal("reset_vm should be listed on TrueNAS 28.0.0-MASTER")
		}
		// Refuses STOPPED and SUSPENDED.
		if _, err := r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 1.0}); err == nil || !strings.Contains(err.Error(), "STOPPED") {
			t.Fatalf("expected STOPPED refusal, got %v", err)
		}
		if _, err := r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 3.0}); err == nil || !strings.Contains(err.Error(), "SUSPENDED") {
			t.Fatalf("expected SUSPENDED refusal, got %v", err)
		}

		preview, err := r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 2.0})
		if err != nil {
			t.Fatal(err)
		}
		tok := confirmTokenFromPreview(t, preview)

		// Status change invalidates token.
		env.statuses[2]["pid"] = 9998.0
		if _, err := r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 2.0, "confirmation_token": tok}); err == nil {
			t.Fatal("reset_vm accepted stale token after pid changed")
		}
		assertNoWriteCalls(t, fake)

		// Fresh preview + confirm calls vm.reset.
		preview2, err := r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 2.0})
		if err != nil {
			t.Fatal(err)
		}
		tok2 := confirmTokenFromPreview(t, preview2)
		out, err := r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 2.0, "confirmation_token": tok2})
		if err != nil {
			t.Fatalf("reset_vm confirm: %v", err)
		}
		if !strings.Contains(out, `"status": "reset"`) || len(fake.called("vm.reset")) != 1 {
			t.Errorf("unexpected reset_vm output: %s", out)
		}
	})
}

func TestVMToolsHiddenAndRefusedUnderReadOnly(t *testing.T) {
	env := newVMFakeEnv("28.0.0-MASTER")
	r, fake, _ := env.buildRegistry(t, true)
	names := listed(r)

	vmTools := []struct {
		name string
		args map[string]interface{}
	}{
		{"create_vm", map[string]interface{}{"name": "rovm", "memory": 1024.0, "dry_run": true}},
		{"start_vm", map[string]interface{}{"id": 1.0, "dry_run": true}},
		{"stop_vm", map[string]interface{}{"id": 2.0, "dry_run": true}},
		{"poweroff_vm", map[string]interface{}{"id": 2.0, "dry_run": true}},
		{"restart_vm", map[string]interface{}{"id": 2.0, "dry_run": true}},
		{"reset_vm", map[string]interface{}{"id": 2.0, "dry_run": true}},
	}
	for _, tc := range vmTools {
		if names[tc.name] {
			t.Errorf("%s should be hidden under --read-only", tc.name)
		}
		if _, err := r.CallTool(t.Context(), tc.name, tc.args); err == nil {
			t.Errorf("%s should be refused under --read-only", tc.name)
		}
	}
	assertNoWriteCalls(t, fake)
	if len(fake.called("vm.query")) != 0 {
		t.Errorf("read-only refusal should occur before any vm.query call")
	}
}

// TestResetVMMethodNotFoundIsFriendly checks that a MASTER build that does
// not serve vm.reset gets the same "needs a newer TrueNAS" message as
// get_vm_guest_ips, while other vm.reset errors are passed through.
func TestResetVMMethodNotFoundIsFriendly(t *testing.T) {
	const want = "reset_vm requires TrueNAS 28.0 or later; vm.reset is not available on this TrueNAS release, so nothing was executed"
	for _, tc := range []struct {
		name   string
		rpcErr map[string]interface{}
		want   string // exact error; "" means wantSub applies
		sub    string
	}{
		{"jsonrpc -32601", map[string]interface{}{"code": -32601, "message": "Method not found"}, want, ""},
		{"-32601 code only", map[string]interface{}{"code": -32601, "message": "no such call"}, want, ""},
		{"method does not exist", map[string]interface{}{"code": -32001, "message": "Method does not exist"}, want, ""},
		{"other error", map[string]interface{}{"code": -32001, "message": "libvirt connection lost"}, "", `failed to reset VM "existing_running": `},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMFakeEnv("27.1.0-MASTER")
			r, fake, _ := env.buildRegistry(t, false)
			next := fake.handle
			fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
				if method == "vm.reset" {
					return nil, tc.rpcErr, true
				}
				return next(method, params)
			}
			preview, err := r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 2.0})
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			_, err = r.CallTool(t.Context(), "reset_vm", map[string]interface{}{"id": 2.0, "confirmation_token": confirmTokenFromPreview(t, preview)})
			switch {
			case err == nil:
				t.Fatal("expected an error")
			case tc.want != "" && err.Error() != tc.want:
				t.Errorf("err = %v\nwant %s", err, tc.want)
			case tc.want == "" && (!strings.Contains(err.Error(), tc.sub) || !strings.Contains(err.Error(), "libvirt connection lost")):
				t.Errorf("err = %v, want the wrapped middleware error", err)
			}
			if n := len(fake.called("vm.reset")); n != 1 {
				t.Errorf("vm.reset calls = %d, want 1", n)
			}
		})
	}
}

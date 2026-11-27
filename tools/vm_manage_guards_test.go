package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the backing-storage guards of delete_vm and delete_vm_device,
// the read-back after a delete, and the update_vm_device validators that
// the nested attributes schema does not enforce at runtime.

func callCount(fake *fakeMiddleware, method string) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	n := 0
	for _, c := range fake.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func snapRow(id string, userrefs int, clones string, holds ...string) map[string]interface{} {
	ds, name, _ := strings.Cut(id, "@")
	h := map[string]interface{}{}
	for _, tag := range holds {
		h[tag] = 1700000000.0
	}
	return map[string]interface{}{
		"id": id, "name": id, "dataset": ds, "snapshot_name": name,
		"holds": h,
		"properties": map[string]interface{}{
			"createtxg": map[string]interface{}{"value": "100", "rawvalue": "100", "parsed": "100"},
			"userrefs":  map[string]interface{}{"value": itoa(userrefs), "rawvalue": itoa(userrefs), "parsed": float64(userrefs)},
			"clones":    map[string]interface{}{"value": clones, "rawvalue": clones, "parsed": clones},
		},
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func confirmTool(t *testing.T, r interface {
	CallTool(context.Context, string, map[string]interface{}) (string, error)
}, tool string, args map[string]interface{}, tok string) (map[string]interface{}, error) {
	t.Helper()
	out, err := r.CallTool(context.Background(), tool, tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err != nil {
		return nil, err
	}
	var res map[string]interface{}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%s result parse: %v\n%s", tool, err, out)
	}
	return res, nil
}

func mustToken(t *testing.T, tok string, res map[string]interface{}) {
	t.Helper()
	if tok == "" {
		raw, _ := json.Marshal(res)
		t.Fatalf("expected a confirmation token, got: %s", raw)
	}
}

// --- delete_vm: snapshots, clones, holds and other consumers ---

func TestDeleteVMZvolSnapshotGuards(t *testing.T) {
	for _, ver := range []string{"25.10.0", "27.1.0-MASTER", "28.0.0-MASTER"} {
		t.Run(ver+" clone via clones property", func(t *testing.T) {
			env := newVMManageEnv(ver)
			env.snapshots = []map[string]interface{}{snapRow("tank/vms/app-disk0@base", 0, "tank/vms/clonevm-disk0")}
			r, fake, _ := env.buildRegistry(t, false)
			assertPreviewBlocked(t, r, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true}, "tank/vms/app-disk0@base (clones: tank/vms/clonevm-disk0)")
			assertNoWriteCalls(t, fake)
		})
		t.Run(ver+" clone via origin", func(t *testing.T) {
			env := newVMManageEnv(ver)
			env.snapshots = []map[string]interface{}{snapRow("tank/vms/app-disk0@base", 0, "")}
			env.datasets["tank/vms/clonevm-disk0"] = map[string]interface{}{
				"id": "tank/vms/clonevm-disk0", "name": "tank/vms/clonevm-disk0", "type": "VOLUME", "locked": false,
				"origin": map[string]interface{}{"value": "tank/vms/app-disk0@base"},
			}
			r, _, _ := env.buildRegistry(t, false)
			assertPreviewBlocked(t, r, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true}, "its snapshots have clones")
		})
		t.Run(ver+" held snapshot", func(t *testing.T) {
			env := newVMManageEnv(ver)
			env.snapshots = []map[string]interface{}{snapRow("tank/vms/app-disk0@repl", 1, "", "replication")}
			r, _, _ := env.buildRegistry(t, false)
			assertPreviewBlocked(t, r, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true}, "tank/vms/app-disk0@repl (holds: replication)")
		})
		t.Run(ver+" hold without tag", func(t *testing.T) {
			env := newVMManageEnv(ver)
			env.snapshots = []map[string]interface{}{snapRow("tank/vms/app-disk0@repl", 2, "")}
			r, _, _ := env.buildRegistry(t, false)
			assertPreviewBlocked(t, r, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true}, "its snapshots are held")
		})
	}

	t.Run("plain snapshots are counted and allowed", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		env.snapshots = []map[string]interface{}{
			snapRow("tank/vms/app-disk0@a", 0, ""),
			snapRow("tank/vms/app-disk0@b", 0, ""),
			snapRow("tank/vms/other-disk0@a", 0, "tank/elsewhere"),
		}
		r, _, _ := env.buildRegistry(t, false)
		res, tok := previewAndGetToken(t, r, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true})
		mustToken(t, tok, res)
		raw, _ := json.Marshal(res)
		if !strings.Contains(string(raw), "tank/vms/app-disk0 (20.00 GiB, 2 snapshot(s))") || !strings.Contains(string(raw), `"snapshot_count":2`) {
			t.Fatalf("expected the snapshot count in the preview: %s", raw)
		}
	})

	t.Run("snapshots of other datasets are ignored even if the filter is", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		env.snapshotsIgnoreFilter = true
		env.snapshots = []map[string]interface{}{snapRow("tank/vms/other-disk0@base", 1, "tank/vms/x", "hold")}
		r, _, _ := env.buildRegistry(t, false)
		res, tok := previewAndGetToken(t, r, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true})
		mustToken(t, tok, res)
	})

	t.Run("zvols=false does not look at snapshots", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		env.snapshots = []map[string]interface{}{snapRow("tank/vms/app-disk0@base", 1, "tank/vms/clonevm-disk0", "x")}
		r, fake, _ := env.buildRegistry(t, false)
		res, tok := previewAndGetToken(t, r, "delete_vm", map[string]interface{}{"id": 1.0})
		mustToken(t, tok, res)
		if n := callCount(fake, "pool.snapshot.query"); n != 0 {
			t.Fatalf("pool.snapshot.query called %d times with zvols=false", n)
		}
	})
}

func TestDeleteVMZvolConsumerGuards(t *testing.T) {
	cases := []struct {
		name  string
		ver   string
		setup func(e *vmFakeEnv)
		want  string
	}{
		{"iscsi path", "25.10.0", func(e *vmFakeEnv) {
			e.iscsiExtents = []map[string]interface{}{{"id": 3.0, "name": "lun0", "type": "DISK", "path": "zvol/tank/vms/app-disk0"}}
		}, `iSCSI extent "lun0" (id 3)`},
		{"iscsi disk field", "28.0.0-MASTER", func(e *vmFakeEnv) {
			e.iscsiExtents = []map[string]interface{}{{"id": 4.0, "name": "lun1", "type": "DISK", "disk": "zvol/tank/vms/app-disk0"}}
		}, `iSCSI extent "lun1" (id 4)`},
		{"nvmet", "27.1.0-MASTER", func(e *vmFakeEnv) {
			e.nvmetNamespaces = []map[string]interface{}{{"id": 7.0, "device_type": "ZVOL", "device_path": "zvol/tank/vms/app-disk0"}}
		}, "NVMe-oF namespace id 7"},
		{"incus 25.10", "25.10.0", func(e *vmFakeEnv) {
			e.virtInstances = []map[string]interface{}{{"id": "web", "name": "web"}}
			e.virtDevices = map[string][]map[string]interface{}{"web": {{"dev_type": "DISK", "source": "/dev/zvol/tank/vms/app-disk0"}}}
		}, `Incus instance "web"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMManageEnv(tc.ver)
			tc.setup(env)
			r, fake, _ := env.buildRegistry(t, false)
			assertPreviewBlocked(t, r, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true}, tc.want)
			assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 10.0, "zvol": true}, tc.want)
			assertNoWriteCalls(t, fake)
		})
	}

	t.Run("incus is not queried from 27.0", func(t *testing.T) {
		env := newVMManageEnv("27.1.0-MASTER")
		r, fake, _ := env.buildRegistry(t, false)
		res, tok := previewAndGetToken(t, r, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true})
		mustToken(t, tok, res)
		if n := callCount(fake, "virt.instance.query"); n != 0 {
			t.Fatalf("virt.instance.query called %d times on 27.1", n)
		}
		if callCount(fake, "iscsi.extent.query") == 0 || callCount(fake, "nvmet.namespace.query") == 0 {
			t.Fatal("expected iSCSI and NVMe-oF checks")
		}
	})

	for _, m := range []string{"iscsi.extent.query", "nvmet.namespace.query", "virt.instance.query", "pool.snapshot.query"} {
		t.Run("fails closed when "+m+" fails", func(t *testing.T) {
			env := newVMManageEnv("25.10.0")
			env.failConsumers = m
			r, fake, _ := env.buildRegistry(t, false)
			for _, call := range []struct {
				tool string
				args map[string]interface{}
			}{
				{"delete_vm", map[string]interface{}{"id": 1.0, "zvols": true}},
				{"delete_vm_device", map[string]interface{}{"device_id": 10.0, "zvol": true}},
			} {
				if _, err := r.CallTool(context.Background(), call.tool, call.args); err == nil || !strings.Contains(err.Error(), "simulated") {
					t.Fatalf("%s: expected the %s failure to refuse, got %v", call.tool, m, err)
				}
			}
			assertNoWriteCalls(t, fake)
		})
	}
}

func TestDeleteVMTokenBindsStaleness(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(e *vmFakeEnv)
	}{
		{"uuid", func(e *vmFakeEnv) { e.vms[0]["uuid"] = "aaaa0000-0000-0000-0000-00000000ffff" }},
		{"status pid", func(e *vmFakeEnv) {
			e.statuses[1] = map[string]interface{}{"state": "STOPPED", "pid": 77.0, "domain_state": "SHUTOFF"}
		}},
		{"zvol row", func(e *vmFakeEnv) {
			e.datasets["tank/vms/app-disk0"]["volsize"] = map[string]interface{}{"parsed": float64(21 << 30)}
		}},
		{"new snapshot", func(e *vmFakeEnv) {
			e.snapshots = append(e.snapshots, snapRow("tank/vms/app-disk0@late", 0, ""))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMManageEnv("25.10.0")
			env.snapshots = []map[string]interface{}{snapRow("tank/vms/app-disk0@a", 0, "")}
			r, fake, _ := env.buildRegistry(t, false)
			args := map[string]interface{}{"id": 1.0, "zvols": true}
			res, tok := previewAndGetToken(t, r, "delete_vm", args)
			mustToken(t, tok, res)
			tc.mutate(env)
			if _, err := confirmTool(t, r, "delete_vm", args, tok); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("expected a stale-token refusal after the %s changed, got %v", tc.name, err)
			}
			assertNoWriteCalls(t, fake)
		})
	}
}

func TestDeleteVMReportsWhatRemains(t *testing.T) {
	t.Run("force keeps a zvol it failed to destroy", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		env.keepStorage = true
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"id": 1.0, "zvols": true, "force": true}
		res, tok := previewAndGetToken(t, r, "delete_vm", args)
		mustToken(t, tok, res)
		out, err := confirmTool(t, r, "delete_vm", args, tok)
		if err != nil {
			t.Fatal(err)
		}
		msg, _ := out["message"].(string)
		left, _ := out["zvols_still_present"].([]interface{})
		if len(left) != 1 || left[0] != "tank/vms/app-disk0" || !strings.Contains(msg, "destroyed 0 of 1") || !strings.Contains(msg, "Still present (force=true ignored the destroy failure): tank/vms/app-disk0") {
			t.Fatalf("expected the kept zvol to be reported: %+v", out)
		}
		if out["deleted"] != true {
			t.Fatalf("VM row is gone, deleted should be true: %+v", out)
		}
	})

	t.Run("zvols destroyed are confirmed", func(t *testing.T) {
		env := newVMManageEnv("28.0.0-MASTER")
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"id": 1.0, "zvols": true}
		_, tok := previewAndGetToken(t, r, "delete_vm", args)
		out, err := confirmTool(t, r, "delete_vm", args, tok)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := out["zvols_destroyed"].([]interface{})
		if len(got) != 1 || got[0] != "tank/vms/app-disk0" || out["zvols_still_present"] != nil {
			t.Fatalf("unexpected result: %+v", out)
		}
	})

	t.Run("VM still listed after vm.delete", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		env.keepVM = true
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"id": 1.0, "force": true}
		_, tok := previewAndGetToken(t, r, "delete_vm", args)
		out, err := confirmTool(t, r, "delete_vm", args, tok)
		if err != nil {
			t.Fatal(err)
		}
		if out["deleted"] != false || out["vm_still_exists"] != true || !strings.Contains(out["message"].(string), "still exists") {
			t.Fatalf("expected the surviving VM to be reported: %+v", out)
		}
	})

	t.Run("read-back runs when the request is cancelled", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"id": 1.0, "zvols": true, "force": true}
		_, tok := previewAndGetToken(t, r, "delete_vm", args)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var once sync.Once
		deleted := false
		env.onCall = func(method string) {
			if method == "vm.delete" {
				deleted = true
				return
			}
			if deleted {
				once.Do(func() { cancel(); time.Sleep(150 * time.Millisecond) })
			}
		}
		outStr, err := r.CallTool(ctx, "delete_vm", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]interface{}
		_ = json.Unmarshal([]byte(outStr), &out)
		if out["deleted"] != true || out["vm_status_unverified"] != nil || out["zvols_unverified"] != nil {
			t.Fatalf("read-back should not depend on the request context: %+v", out)
		}
	})
}

// --- delete_vm_device: zvol ---

func TestDeleteVMDeviceZvolGuards(t *testing.T) {
	t.Run("zvol with snapshots is refused", func(t *testing.T) {
		env := newVMManageEnv("28.0.0-MASTER")
		env.snapshots = []map[string]interface{}{snapRow("tank/vms/app-disk0@a", 0, "")}
		r, fake, _ := env.buildRegistry(t, false)
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 10.0, "zvol": true}, "it has 1 snapshot(s) (tank/vms/app-disk0@a)")
		assertNoWriteCalls(t, fake)
	})

	t.Run("warning no longer says snapshots are destroyed", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, _, _ := env.buildRegistry(t, false)
		res, tok := previewAndGetToken(t, r, "delete_vm_device", map[string]interface{}{"device_id": 10.0, "zvol": true})
		mustToken(t, tok, res)
		raw, _ := json.Marshal(res)
		if strings.Contains(string(raw), "snapshots on it will be destroyed") || !strings.Contains(string(raw), "PERMANENT DATA LOSS: zvol tank/vms/app-disk0 (20.00 GiB) will be destroyed") {
			t.Fatalf("unexpected warning: %s", raw)
		}
	})

	t.Run("force reports a zvol that is still present", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		env.keepStorage = true
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"device_id": 10.0, "zvol": true, "force": true}
		_, tok := previewAndGetToken(t, r, "delete_vm_device", args)
		out, err := confirmTool(t, r, "delete_vm_device", args, tok)
		if err != nil {
			t.Fatal(err)
		}
		bs, _ := out["backing_storage"].(map[string]interface{})
		if out["zvol_deleted"] != false || bs["outcome"] != "still_present" || !strings.Contains(out["message"].(string), "is still present (force=true ignored the failure)") {
			t.Fatalf("expected the kept zvol to be reported: %+v", out)
		}
	})

	t.Run("destroyed zvol is confirmed", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"device_id": 10.0, "zvol": true}
		_, tok := previewAndGetToken(t, r, "delete_vm_device", args)
		out, err := confirmTool(t, r, "delete_vm_device", args, tok)
		if err != nil {
			t.Fatal(err)
		}
		if out["zvol_deleted"] != true || out["deleted"] != true {
			t.Fatalf("unexpected result: %+v", out)
		}
	})
}

func TestDeleteVMDeviceTokenBindsStaleness(t *testing.T) {
	cases := []struct {
		name   string
		args   map[string]interface{}
		mutate func(e *vmFakeEnv)
	}{
		{"vm uuid", map[string]interface{}{"device_id": 11.0}, func(e *vmFakeEnv) {
			e.vms[0]["uuid"] = "aaaa0000-0000-0000-0000-00000000eeee"
		}},
		{"vm status", map[string]interface{}{"device_id": 11.0}, func(e *vmFakeEnv) {
			e.statuses[1] = map[string]interface{}{"state": "STOPPED", "pid": 55.0, "domain_state": "SHUTOFF"}
		}},
		{"zvol dataset", map[string]interface{}{"device_id": 10.0, "zvol": true}, func(e *vmFakeEnv) {
			e.datasets["tank/vms/app-disk0"]["volsize"] = map[string]interface{}{"parsed": float64(40 << 30)}
		}},
		{"raw file inode", map[string]interface{}{"device_id": 14.0, "raw_file": true}, func(e *vmFakeEnv) {
			e.stats["/mnt/tank/vms/raw0.img"]["inode"] = 999.0
		}},
		{"raw file size", map[string]interface{}{"device_id": 14.0, "raw_file": true}, func(e *vmFakeEnv) {
			e.stats["/mnt/tank/vms/raw0.img"]["size"] = 1.0
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMManageEnv("25.10.0")
			r, fake, _ := env.buildRegistry(t, false)
			res, tok := previewAndGetToken(t, r, "delete_vm_device", tc.args)
			mustToken(t, tok, res)
			tc.mutate(env)
			if _, err := confirmTool(t, r, "delete_vm_device", tc.args, tok); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("expected a stale-token refusal after the %s changed, got %v", tc.name, err)
			}
			assertNoWriteCalls(t, fake)
		})
	}
}

// --- delete_vm_device: raw_file guards, one case per guard ---

func TestDeleteVMDeviceRawFileGuards(t *testing.T) {
	setPath := func(e *vmFakeEnv, p string) {
		dev := e.vms[0]["devices"].([]interface{})[4].(map[string]interface{})
		dev["attributes"].(map[string]interface{})["path"] = p
	}
	cases := []struct {
		name  string
		setup func(e *vmFakeEnv)
		want  string
	}{
		{"path outside /mnt", func(e *vmFakeEnv) { setPath(e, "/srv/raw0.img") }, "only paths under /mnt/<pool> are allowed"},
		{"file in pool root dataset", func(e *vmFakeEnv) { setPath(e, "/mnt/tank/raw0.img") }, "directly in the root dataset of pool 'tank'"},
		{"parent through symlink", func(e *vmFakeEnv) { e.stats["/mnt/tank/vms"]["realpath"] = "/mnt/tank/elsewhere" }, "goes through a symlink"},
		{"parent not a directory", func(e *vmFakeEnv) { e.stats["/mnt/tank/vms"]["type"] = "FILE" }, "is a FILE, not a directory"},
		{"not ZFS", func(e *vmFakeEnv) { e.statfs["/mnt/tank/vms"]["fstype"] = "ext4" }, "is not on a ZFS dataset"},
		{"dataset of another pool", func(e *vmFakeEnv) {
			e.statfs["/mnt/tank/vms"]["source"] = "other/vms"
			e.datasets["other/vms"] = map[string]interface{}{"id": "other/vms", "name": "other/vms", "type": "FILESYSTEM", "locked": false}
		}, "not on a dataset of pool 'tank'"},
		{"pool root dataset", func(e *vmFakeEnv) { e.statfs["/mnt/tank/vms"]["source"] = "tank" }, "is on the root dataset of pool 'tank'"},
		{"managed dataset", func(e *vmFakeEnv) {
			e.statfs["/mnt/tank/vms"]["source"] = "tank/ix-apps"
			e.datasets["tank/ix-apps"] = map[string]interface{}{"id": "tank/ix-apps", "name": "tank/ix-apps", "type": "FILESYSTEM", "locked": false}
		}, "which TrueNAS manages itself"},
		{"dataset not found", func(e *vmFakeEnv) { e.statfs["/mnt/tank/vms"]["source"] = "tank/gone" }, "was not found with pool.dataset.query"},
		{"dataset locked", func(e *vmFakeEnv) { e.datasets["tank/vms"]["locked"] = true }, "dataset tank/vms is locked"},
		{"lock state unreadable", func(e *vmFakeEnv) { delete(e.datasets["tank/vms"], "locked") }, "dataset tank/vms is locked, or its lock state could not be read"},
		{"not a regular file", func(e *vmFakeEnv) { e.stats["/mnt/tank/vms/raw0.img"]["type"] = "DIRECTORY" }, "is a DIRECTORY, not a regular file"},
		{"symlink", func(e *vmFakeEnv) { e.stats["/mnt/tank/vms/raw0.img"]["type"] = "SYMLINK" }, "is a symlink; refusing to delete"},
		{"realpath mismatch", func(e *vmFakeEnv) { e.stats["/mnt/tank/vms/raw0.img"]["realpath"] = "/mnt/tank/vms/raw1.img" }, "resolves to /mnt/tank/vms/raw1.img"},
		{"shared with another VM", func(e *vmFakeEnv) {
			e.vms[1]["devices"] = append(e.vms[1]["devices"].([]interface{}), map[string]interface{}{
				"id": 23.0, "vm": 2.0, "order": 1004.0,
				"attributes": map[string]interface{}{"dtype": "RAW", "path": "/mnt/tank/vms/raw0.img"},
			})
		}, "it is also used by"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMManageEnv("25.10.0")
			tc.setup(env)
			r, fake, _ := env.buildRegistry(t, false)
			assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 14.0, "raw_file": true}, tc.want)
			assertNoWriteCalls(t, fake)
		})
	}

	t.Run("baseline passes", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"device_id": 14.0, "raw_file": true}
		res, tok := previewAndGetToken(t, r, "delete_vm_device", args)
		mustToken(t, tok, res)
		out, err := confirmTool(t, r, "delete_vm_device", args, tok)
		if err != nil {
			t.Fatal(err)
		}
		if out["raw_file_deleted"] != true {
			t.Fatalf("expected the raw file read back as deleted: %+v", out)
		}
	})

	t.Run("force reports a raw file that is still present", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		env.keepStorage = true
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"device_id": 14.0, "raw_file": true, "force": true}
		_, tok := previewAndGetToken(t, r, "delete_vm_device", args)
		out, err := confirmTool(t, r, "delete_vm_device", args, tok)
		if err != nil {
			t.Fatal(err)
		}
		if out["raw_file_deleted"] != false || !strings.Contains(out["message"].(string), "raw file /mnt/tank/vms/raw0.img is still present") {
			t.Fatalf("expected the kept raw file to be reported: %+v", out)
		}
	})
}

// --- update_vm_device ---

func TestUpdateVMDeviceDiskPathNeedsStoppedVM(t *testing.T) {
	env := newVMManageEnv("25.10.0")
	env.statuses[1] = map[string]interface{}{"state": "RUNNING", "pid": 99.0, "domain_state": "RUNNING"}
	env.diskChoices["/dev/zvol/tank/vms/free0"] = "tank/vms/free0"
	r, fake, _ := env.buildRegistry(t, false)
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id": 10.0, "force": true, "attributes": map[string]interface{}{"path": "/dev/zvol/tank/vms/free0"},
	}, "DISK path changes need VM \"appvm\" (id 1) to be STOPPED")
	// Other DISK settings still update with force on a running VM.
	res, tok := previewAndGetToken(t, r, "update_vm_device", map[string]interface{}{
		"device_id": 10.0, "force": true, "attributes": map[string]interface{}{"iotype": "NATIVE"},
	})
	mustToken(t, tok, res)
	// The same path is not a change.
	res, tok = previewAndGetToken(t, r, "update_vm_device", map[string]interface{}{
		"device_id": 10.0, "force": true, "attributes": map[string]interface{}{"path": "/dev/zvol/tank/vms/app-disk0", "iotype": "NATIVE"},
	})
	mustToken(t, tok, res)
	assertNoWriteCalls(t, fake)
}

func TestUpdateVMDeviceDiskPathDatasetMissing(t *testing.T) {
	env := newVMManageEnv("25.10.0")
	env.diskChoices["/dev/zvol/tank/vms/ghost0"] = "tank/vms/ghost0"
	r, fake, _ := env.buildRegistry(t, false)
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id": 10.0, "attributes": map[string]interface{}{"path": "/dev/zvol/tank/vms/ghost0"},
	}, "DISK zvol tank/vms/ghost0 was not found with pool.dataset.query")
	assertNoWriteCalls(t, fake)
}

func TestUpdateVMDeviceValidatorGaps(t *testing.T) {
	cases := []struct {
		name  string
		dev   float64
		attrs map[string]interface{}
		want  string
	}{
		{"disk path managed dataset", 10, map[string]interface{}{"path": "/dev/zvol/tank/ix-apps/vol0"}, "which TrueNAS manages itself"},
		{"disk path not a string", 10, map[string]interface{}{"path": 5.0}, "DISK attributes.path must be a non-empty"},
		{"disk path empty", 10, map[string]interface{}{"path": ""}, "DISK attributes.path must be a non-empty"},
		{"cdrom in pool root dataset", 11, map[string]interface{}{"path": "/mnt/tank/second.iso"}, "directly in the root dataset of pool 'tank'"},
		{"cdrom in managed dataset", 11, map[string]interface{}{"path": "/mnt/tank/ix-apps/x.iso"}, "inside 'ix-apps'"},
		{"cdrom braces", 11, map[string]interface{}{"path": "/mnt/tank/isos/{x}.iso"}, "must not contain '{' or '}'"},
		{"nic type", 12, map[string]interface{}{"type": "RTL8139"}, "NIC attributes.type must be E1000 or VIRTIO"},
		{"nic type not a string", 12, map[string]interface{}{"type": 1.0}, "NIC attributes.type must be E1000 or VIRTIO"},
		{"display type", 13, map[string]interface{}{"type": "RDP"}, "DISPLAY attributes.type must be SPICE or VNC"},
		{"display resolution", 13, map[string]interface{}{"resolution": "1x1"}, "is not a supported resolution"},
		{"display resolution not a string", 13, map[string]interface{}{"resolution": 1024.0}, "is not a supported resolution"},
		{"display bind not a string", 13, map[string]interface{}{"bind": 1.0}, "DISPLAY attributes.bind must be a non-empty"},
		{"display bind empty", 13, map[string]interface{}{"bind": ""}, "DISPLAY attributes.bind must be a non-empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMManageEnv("25.10.0")
			r, fake, _ := env.buildRegistry(t, false)
			_, err := r.CallTool(context.Background(), "update_vm_device", map[string]interface{}{"device_id": tc.dev, "attributes": tc.attrs})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			assertNoWriteCalls(t, fake)
		})
	}
}

// --- libvirt access from mode bits ---

func TestCheckLibvirtStatAccess(t *testing.T) {
	cases := []struct {
		name string
		st   map[string]interface{}
		bit  int64
		want bool
	}{
		{"other read", map[string]interface{}{"mode": float64(0o100644), "user": "root", "group": "root", "gid": 0.0}, 0o004, true},
		{"owner libvirt-qemu", map[string]interface{}{"mode": float64(0o100600), "user": "libvirt-qemu", "group": "root", "gid": 0.0}, 0o004, true},
		{"owner bit for another user", map[string]interface{}{"mode": float64(0o100600), "user": "alice", "group": "root", "gid": 0.0}, 0o004, false},
		{"owner libvirt-qemu without the bit", map[string]interface{}{"mode": float64(0o100060), "user": "libvirt-qemu", "group": "staff", "gid": 50.0}, 0o004, false},
		{"group kvm", map[string]interface{}{"mode": float64(0o100640), "user": "root", "group": "kvm", "gid": 78.0}, 0o004, true},
		{"group libvirt-qemu", map[string]interface{}{"mode": float64(0o100640), "user": "root", "group": "libvirt-qemu", "gid": 986.0}, 0o004, true},
		{"unnamed non-root group", map[string]interface{}{"mode": float64(0o100640), "user": "root", "group": "", "gid": 1000.0}, 0o004, false},
		{"unknown group", map[string]interface{}{"mode": float64(0o100640), "user": "root", "gid": 1000.0}, 0o004, false},
		{"other group", map[string]interface{}{"mode": float64(0o100640), "user": "root", "group": "staff", "gid": 50.0}, 0o004, false},
		{"directory traverse by other", map[string]interface{}{"mode": float64(0o040711), "user": "root", "group": "root"}, 0o001, true},
		{"directory without traverse", map[string]interface{}{"mode": float64(0o040750), "user": "root", "group": "staff"}, 0o001, false},
		{"acl defers to the middleware", map[string]interface{}{"mode": float64(0o100600), "user": "root", "acl": true}, 0o004, true},
		{"unreadable mode defers to the middleware", map[string]interface{}{"user": "root"}, 0o004, true},
	}
	for _, tc := range cases {
		if got, _ := checkLibvirtStatAccess(tc.st, tc.bit); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- create_vm cleanup survives a cancelled request ---

func TestCreateVMCleanupIgnoresCancellation(t *testing.T) {
	baseArgs := func() map[string]interface{} {
		return map[string]interface{}{
			"name": "newvm", "memory": 1024.0,
			"devices": []interface{}{
				map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
			},
		}
	}

	t.Run("rollback", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		env.failDeviceIdx = 0
		r, fake, _ := env.buildRegistry(t, false)
		args := baseArgs()
		res, tok := previewAndGetToken(t, r, "create_vm", args)
		mustToken(t, tok, res)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env.onDeviceCreateFail = cancel
		_, err := r.CallTool(ctx, "create_vm", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err == nil || !strings.Contains(err.Error(), "Rolled back") {
			t.Fatalf("expected the rollback to complete after cancellation, got %v", err)
		}
		if callCount(fake, "vm.delete") != 1 {
			t.Fatal("vm.delete was not sent")
		}
	})

	t.Run("autostart", func(t *testing.T) {
		env := newVMFakeEnv("25.10.0")
		r, _, _ := env.buildRegistry(t, false)
		args := baseArgs()
		args["autostart"] = true
		res, tok := previewAndGetToken(t, r, "create_vm", args)
		mustToken(t, tok, res)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env.onCall = func(method string) {
			if method == "vm.update" {
				cancel()
				time.Sleep(150 * time.Millisecond)
			}
		}
		out, err := r.CallTool(ctx, "create_vm", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err != nil || !strings.Contains(out, "enabled autostart") {
			t.Fatalf("expected autostart to be enabled despite cancellation, got %v %s", err, out)
		}
	})

	t.Run("cleanup context", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		cctx, ccancel := vmCleanupContext(parent)
		defer ccancel()
		cancel()
		if cctx.Err() != nil {
			t.Fatal("cleanup context was cancelled with its parent")
		}
		if dl, ok := cctx.Deadline(); !ok || time.Until(dl) > vmCleanupTimeout || time.Until(dl) < vmCleanupTimeout-time.Minute {
			t.Fatalf("unexpected cleanup deadline: %v %v", dl, ok)
		}
	})
}

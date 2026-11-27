package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// newVMManageEnv sets up a realistic fake TrueNAS environment with two VMs:
//   - VM 1 ("appvm", STOPPED): DISK 10 (/dev/zvol/tank/vms/app-disk0, 20 GiB),
//     CDROM 11 (/mnt/tank/isos/installer.iso), NIC 12 (VIRTIO on br0),
//     DISPLAY 13 (SPICE on 127.0.0.1:5900/5901 with password), RAW 14 (/mnt/tank/vms/raw0.img)
//   - VM 2 ("othervm", RUNNING): DISK 20 (/dev/zvol/tank/vms/other-disk0, 30 GiB),
//     DISPLAY 21 (VNC on 127.0.0.1:5910/5911)
func newVMManageEnv(version string) *vmFakeEnv {
	const gib = float64(1 << 30)
	env := newVMFakeEnv(version)
	env.vms = []map[string]interface{}{
		{
			"id": 1.0, "name": "appvm", "uuid": "aaaa0000-0000-0000-0000-000000000001",
			"vcpus": 1.0, "cores": 2.0, "threads": 1.0, "memory": 2048.0,
			"bootloader": "UEFI", "autostart": false,
			"status": map[string]interface{}{"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"},
			"devices": []interface{}{
				map[string]interface{}{
					"id": 10.0, "vm": 1.0, "order": 1001.0,
					"attributes": map[string]interface{}{
						"dtype": "DISK", "path": "/dev/zvol/tank/vms/app-disk0",
						"type": "VIRTIO", "iotype": "THREADS", "serial": "DISK0001",
					},
				},
				map[string]interface{}{
					"id": 11.0, "vm": 1.0, "order": 1002.0,
					"attributes": map[string]interface{}{
						"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso",
					},
				},
				map[string]interface{}{
					"id": 12.0, "vm": 1.0, "order": 1003.0,
					"attributes": map[string]interface{}{
						"dtype": "NIC", "type": "VIRTIO", "nic_attach": "br0",
						"mac": "02:00:00:00:00:12", "trust_guest_rx_filters": false,
					},
				},
				map[string]interface{}{
					"id": 13.0, "vm": 1.0, "order": 1004.0,
					"attributes": map[string]interface{}{
						"dtype": "DISPLAY", "type": "SPICE", "bind": "127.0.0.1",
						"port": 5900.0, "web_port": 5901.0, "web": true,
						"resolution": "1024x768", "password": "********", "wait": false,
					},
				},
				map[string]interface{}{
					"id": 14.0, "vm": 1.0, "order": 1005.0,
					"attributes": map[string]interface{}{
						"dtype": "RAW", "path": "/mnt/tank/vms/raw0.img", "type": "VIRTIO",
					},
				},
			},
		},
		{
			"id": 2.0, "name": "othervm", "uuid": "aaaa0000-0000-0000-0000-000000000002",
			"vcpus": 1.0, "cores": 2.0, "threads": 1.0, "memory": 2048.0,
			"bootloader": "UEFI", "autostart": false,
			"status": map[string]interface{}{"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"},
			"devices": []interface{}{
				map[string]interface{}{
					"id": 20.0, "vm": 2.0, "order": 1001.0,
					"attributes": map[string]interface{}{
						"dtype": "DISK", "path": "/dev/zvol/tank/vms/other-disk0",
						"type": "VIRTIO",
					},
				},
				map[string]interface{}{
					"id": 21.0, "vm": 2.0, "order": 1002.0,
					"attributes": map[string]interface{}{
						"dtype": "DISPLAY", "type": "VNC", "bind": "127.0.0.1",
						"port": 5910.0, "web_port": 5911.0, "web": false,
						"password": "********",
					},
				},
			},
		},
	}
	env.statuses = map[int]map[string]interface{}{
		1: {"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"},
		2: {"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"},
	}
	env.datasets["tank/vms/app-disk0"] = map[string]interface{}{
		"id": "tank/vms/app-disk0", "name": "tank/vms/app-disk0", "type": "VOLUME",
		"locked": false, "readonly": map[string]interface{}{"parsed": false},
		"volsize": map[string]interface{}{"parsed": 20 * gib},
	}
	env.datasets["tank/vms/other-disk0"] = map[string]interface{}{
		"id": "tank/vms/other-disk0", "name": "tank/vms/other-disk0", "type": "VOLUME",
		"locked": false, "readonly": map[string]interface{}{"parsed": false},
		"volsize": map[string]interface{}{"parsed": 30 * gib},
	}
	env.stats["/mnt/tank/isos/second.iso"] = map[string]interface{}{
		"realpath": "/mnt/tank/isos/second.iso", "type": "FILE",
		"inode": 101.0, "dev": 3.0, "mount_id": 3.0,
		"size": 2.0 * gib, "mtime": 1700000100.0, "uid": 0.0, "gid": 0.0, "mode": 0o100644,
	}
	env.stats["/mnt/tank/vms"] = map[string]interface{}{
		"realpath": "/mnt/tank/vms", "type": "DIRECTORY",
		"inode": 4.0, "dev": 4.0, "mount_id": 4.0, "is_mountpoint": true,
	}
	env.stats["/mnt/tank/vms/raw0.img"] = map[string]interface{}{
		"realpath": "/mnt/tank/vms/raw0.img", "type": "FILE",
		"inode": 200.0, "dev": 4.0, "mount_id": 4.0,
		"size": 4.0 * gib, "mtime": 1700000200.0, "uid": 0.0, "gid": 0.0, "mode": 0o100644,
	}
	env.statfs["/mnt/tank/vms"] = map[string]interface{}{
		"source": "tank/vms", "fstype": "zfs", "dest": "/mnt/tank/vms",
		"fsid": "fsid-vms", "flags": []interface{}{"RW"}, "avail_bytes": 100 * gib,
	}
	return env
}

func previewAndGetToken(t *testing.T, r *Registry, tool string, args map[string]interface{}) (map[string]interface{}, string) {
	t.Helper()
	out, err := r.CallTool(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("%s preview failed: %v", tool, err)
	}
	var res map[string]interface{}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%s preview JSON parse failed: %v\n%s", tool, err, out)
	}
	tok, _ := res["confirmation_token"].(string)
	return res, tok
}

func assertPreviewBlocked(t *testing.T, r *Registry, tool string, args map[string]interface{}, wantSub string) map[string]interface{} {
	t.Helper()
	out, err := r.CallTool(context.Background(), tool, args)
	if err != nil {
		if strings.Contains(err.Error(), wantSub) {
			return nil
		}
		t.Fatalf("%s returned error %v, want blocked/error containing %q", tool, err, wantSub)
	}
	var res map[string]interface{}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%s output parse failed: %v\n%s", tool, err, out)
	}
	if _, hasTok := res["confirmation_token"]; hasTok {
		t.Fatalf("%s issued a confirmation_token despite expecting block %q:\n%s", tool, wantSub, out)
	}
	preview, _ := res["preview"].(map[string]interface{})
	if preview == nil {
		preview = res
	}
	if status, _ := preview["status"].(string); status != "blocked" {
		t.Fatalf("%s preview status = %q, want blocked (wantSub=%q):\n%s", tool, status, wantSub, out)
	}
	if !strings.Contains(out, wantSub) {
		t.Fatalf("%s blocked output does not contain %q:\n%s", tool, wantSub, out)
	}
	return res
}

func TestSimplifyVMDevicesIDOrderCDROMAndOther(t *testing.T) {
	const secret = "NeverShowThisPass!"
	devices := []interface{}{
		map[string]interface{}{
			"id": 10.0, "order": 1001.0,
			"attributes": map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/d0", "type": "VIRTIO", "serial": "SER1"},
		},
		map[string]interface{}{
			"id": 11.0, "order": 1002.0,
			"attributes": map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/installer.iso"},
		},
		map[string]interface{}{
			"id": 12.0, "order": 1003.0,
			"attributes": map[string]interface{}{"dtype": "NIC", "type": "VIRTIO", "nic_attach": "br0", "mac": "02:00:00:00:00:12"},
		},
		map[string]interface{}{
			"id": 13.0, "order": 1004.0,
			"attributes": map[string]interface{}{"dtype": "DISPLAY", "type": "SPICE", "port": 5900.0, "web_port": 5901.0, "bind": "127.0.0.1", "password": secret},
		},
		map[string]interface{}{
			"id": 14.0, "order": 1005.0,
			"attributes": map[string]interface{}{"dtype": "RAW", "path": "/mnt/tank/vms/raw0.img"},
		},
	}
	got := simplifyVMDevices(devices)
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), `"password"`) {
		t.Fatalf("simplifyVMDevices leaked display password: %s", raw)
	}
	if got["cdrom_count"] != 1 {
		t.Fatalf("cdrom_count = %v, want 1", got["cdrom_count"])
	}
	disks := got["disks"].([]map[string]interface{})
	if disks[0]["id"] != 10 || disks[0]["order"] != 1001 {
		t.Fatalf("disk id/order = %v/%v, want 10/1001", disks[0]["id"], disks[0]["order"])
	}
	cdroms := got["cdroms"].([]map[string]interface{})
	if cdroms[0]["id"] != 11 || cdroms[0]["order"] != 1002 || cdroms[0]["path"] != "/mnt/tank/isos/installer.iso" {
		t.Fatalf("unexpected cdrom summary: %+v", cdroms[0])
	}
	other := got["other_devices"].([]map[string]interface{})
	if len(other) != 1 || other[0]["id"] != 14 || other[0]["dtype"] != "RAW" {
		t.Fatalf("unexpected other_devices: %+v", other)
	}
}

func TestVMManageReadOnlyHiddenAndRefused(t *testing.T) {
	env := newVMManageEnv("25.10.0")
	r, fake, _ := env.buildRegistry(t, true)
	listed := map[string]bool{}
	for _, tool := range r.ListTools() {
		listed[tool.Name] = true
	}
	for _, name := range []string{"update_vm_device", "delete_vm_device", "delete_vm"} {
		if listed[name] {
			t.Errorf("%s listed under --read-only", name)
		}
		_, err := r.CallTool(context.Background(), name, map[string]interface{}{"device_id": 10.0, "id": 1.0})
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s under --read-only: err = %v, want read-only refusal", name, err)
		}
	}
	assertNoWriteCalls(t, fake)
}

func TestUpdateVMDeviceHappyPaths(t *testing.T) {
	t.Run("order-only update with sibling collision warning", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"device_id": 10.0, "order": 1002.0}
		res, tok := previewAndGetToken(t, r, "update_vm_device", args)
		if tok == "" {
			t.Fatalf("expected confirmation token, got: %+v", res)
		}
		rawPrev, _ := json.Marshal(res)
		if !strings.Contains(string(rawPrev), "Sibling device 11 already has order 1002") {
			t.Errorf("expected sibling order warning, got: %s", rawPrev)
		}
		assertNoWriteCalls(t, fake)

		out, err := r.CallTool(context.Background(), "update_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err != nil {
			t.Fatalf("confirm failed: %v", err)
		}
		if !strings.Contains(out, "Updated DISK device 10") {
			t.Errorf("unexpected execute output: %s", out)
		}
		call := lastCall(t, fake, "vm.device.update")
		var params []interface{}
		_ = json.Unmarshal(call.Params, &params)
		if int(params[0].(float64)) != 10 {
			t.Errorf("device id = %v, want 10", params[0])
		}
		upd := params[1].(map[string]interface{})
		if int(upd["order"].(float64)) != 1002 {
			t.Errorf("order = %v, want 1002", upd["order"])
		}
		if _, hasAttrs := upd["attributes"]; hasAttrs {
			t.Errorf("order-only update should not send attributes: %+v", upd)
		}
	})

	t.Run("order-only update works on RAW device", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"device_id": 14.0, "order": 1009.0}
		_, tok := previewAndGetToken(t, r, "update_vm_device", args)
		if tok == "" {
			t.Fatal("expected confirmation token for RAW order update")
		}
		if _, err := r.CallTool(context.Background(), "update_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok})); err != nil {
			t.Fatalf("confirm failed: %v", err)
		}
	})

	t.Run("CDROM ISO swap", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{
			"device_id":  11.0,
			"attributes": map[string]interface{}{"path": "/mnt/tank/isos/second.iso"},
		}
		res, tok := previewAndGetToken(t, r, "update_vm_device", args)
		if tok == "" {
			t.Fatalf("expected token, got %+v", res)
		}
		out, err := r.CallTool(context.Background(), "update_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err != nil {
			t.Fatalf("confirm failed: %v", err)
		}
		if !strings.Contains(out, "/mnt/tank/isos/second.iso") {
			t.Errorf("expected swapped path in response: %s", out)
		}
		call := lastCall(t, fake, "vm.device.update")
		if !strings.Contains(string(call.Params), "/mnt/tank/isos/second.iso") {
			t.Errorf("unexpected vm.device.update params: %s", call.Params)
		}
	})

	t.Run("DISK swap zvol path and sector sizes", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{
			"device_id": 10.0,
			"attributes": map[string]interface{}{
				"path":                "/dev/zvol/tank/vms/free0",
				"type":                "AHCI",
				"iotype":              "NATIVE",
				"logical_sectorsize":  512.0,
				"physical_sectorsize": 4096.0,
			},
		}
		_, tok := previewAndGetToken(t, r, "update_vm_device", args)
		if tok == "" {
			t.Fatal("expected confirmation token")
		}
		if _, err := r.CallTool(context.Background(), "update_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok})); err != nil {
			t.Fatalf("confirm failed: %v", err)
		}
	})

	t.Run("DISK type update keeping current path not in disk_choices", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, _, _ := env.buildRegistry(t, false)
		// /dev/zvol/tank/vms/app-disk0 is already attached so it is NOT in disk_choices.
		args := map[string]interface{}{
			"device_id": 10.0,
			"attributes": map[string]interface{}{
				"path": "/dev/zvol/tank/vms/app-disk0",
				"type": "AHCI",
			},
		}
		_, tok := previewAndGetToken(t, r, "update_vm_device", args)
		if tok == "" {
			t.Fatal("expected token when keeping existing disk path")
		}
	})

	t.Run("NIC update on 28.0 nested BRIDGE/MACVLAN choices with trust_guest_rx_filters on macvtap", func(t *testing.T) {
		env := newVMManageEnv("28.0.0-MASTER")
		env.nicChoices = map[string]interface{}{
			"BRIDGE":  []interface{}{"br0"},
			"MACVLAN": []interface{}{"eno1"},
		}
		r, _, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{
			"device_id": 12.0,
			"attributes": map[string]interface{}{
				"nic_attach":             "eno1",
				"mac":                    "02:11:22:33:44:55",
				"trust_guest_rx_filters": true,
			},
		}
		res, tok := previewAndGetToken(t, r, "update_vm_device", args)
		if tok == "" {
			t.Fatalf("expected token, got %+v", res)
		}
		rawPrev, _ := json.Marshal(res)
		if !strings.Contains(string(rawPrev), "macvtap") {
			t.Errorf("expected macvtap warning: %s", rawPrev)
		}
		if _, err := r.CallTool(context.Background(), "update_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok})); err != nil {
			t.Fatalf("confirm failed: %v", err)
		}
	})

	t.Run("DISPLAY password update never echoes secret", func(t *testing.T) {
		const secret = "TopSecret#2026"
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{
			"device_id": 13.0,
			"attributes": map[string]interface{}{
				"password":   secret,
				"resolution": "1920x1080",
				"port":       5920.0,
				"web_port":   5921.0,
			},
		}
		res, tok := previewAndGetToken(t, r, "update_vm_device", args)
		rawPrev, _ := json.Marshal(res)
		if strings.Contains(string(rawPrev), secret) {
			t.Fatalf("preview leaked display password: %s", rawPrev)
		}
		if tok == "" {
			t.Fatalf("expected token, got %s", rawPrev)
		}
		out, err := r.CallTool(context.Background(), "update_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err != nil {
			t.Fatalf("confirm failed: %v", err)
		}
		if strings.Contains(out, secret) {
			t.Fatalf("execute response leaked display password: %s", out)
		}
		call := lastCall(t, fake, "vm.device.update")
		if !strings.Contains(string(call.Params), secret) {
			t.Errorf("middleware vm.device.update did not receive password")
		}
	})
}

func TestUpdateVMDeviceAttributeAllowlists(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]interface{}
		wantSub string
	}{
		{
			name:    "neither order nor attributes",
			args:    map[string]interface{}{"device_id": 10.0},
			wantSub: "provide order and/or non-empty attributes",
		},
		{
			name:    "empty attributes object",
			args:    map[string]interface{}{"device_id": 10.0, "attributes": map[string]interface{}{}},
			wantSub: "provide order and/or non-empty attributes",
		},
		{
			name:    "no-op update rejected",
			args:    map[string]interface{}{"device_id": 10.0, "order": 1001.0},
			wantSub: "no changes requested",
		},
		{
			name:    "dtype change rejected",
			args:    map[string]interface{}{"device_id": 10.0, "attributes": map[string]interface{}{"dtype": "CDROM"}},
			wantSub: "dtype cannot be changed",
		},
		{
			name:    "DISK serial rejected as read-only",
			args:    map[string]interface{}{"device_id": 10.0, "attributes": map[string]interface{}{"serial": "NEW001"}},
			wantSub: "read-only",
		},
		{
			name:    "DISK create_zvol rejected on update",
			args:    map[string]interface{}{"device_id": 10.0, "attributes": map[string]interface{}{"create_zvol": true}},
			wantSub: "only valid when creating",
		},
		{
			name:    "DISK zvol_name rejected on update",
			args:    map[string]interface{}{"device_id": 10.0, "attributes": map[string]interface{}{"zvol_name": "tank/vms/x"}},
			wantSub: "only valid when creating",
		},
		{
			name:    "DISK zvol_volsize rejected on update",
			args:    map[string]interface{}{"device_id": 10.0, "attributes": map[string]interface{}{"zvol_volsize": 1073741824.0}},
			wantSub: "only valid when creating",
		},
		{
			name:    "DISK cross-dtype nic_attach rejected",
			args:    map[string]interface{}{"device_id": 10.0, "attributes": map[string]interface{}{"nic_attach": "br0"}},
			wantSub: "not allowed when updating a DISK device",
		},
		{
			name:    "DISK unknown field rejected",
			args:    map[string]interface{}{"device_id": 10.0, "attributes": map[string]interface{}{"bogus": 1}},
			wantSub: "not allowed when updating a DISK device",
		},
		{
			name:    "CDROM cross-dtype type rejected",
			args:    map[string]interface{}{"device_id": 11.0, "attributes": map[string]interface{}{"type": "VIRTIO"}},
			wantSub: "not allowed when updating a CDROM device",
		},
		{
			name:    "CDROM unknown field rejected",
			args:    map[string]interface{}{"device_id": 11.0, "attributes": map[string]interface{}{"bogus": "x"}},
			wantSub: "not allowed when updating a CDROM device",
		},
		{
			name:    "NIC cross-dtype path rejected",
			args:    map[string]interface{}{"device_id": 12.0, "attributes": map[string]interface{}{"path": "/dev/zvol/tank/vms/free0"}},
			wantSub: "not allowed when updating a NIC device",
		},
		{
			name:    "NIC unknown field rejected",
			args:    map[string]interface{}{"device_id": 12.0, "attributes": map[string]interface{}{"bogus": true}},
			wantSub: "not allowed when updating a NIC device",
		},
		{
			name:    "DISPLAY cross-dtype mac rejected",
			args:    map[string]interface{}{"device_id": 13.0, "attributes": map[string]interface{}{"mac": "02:00:00:00:00:99"}},
			wantSub: "not allowed when updating a DISPLAY device",
		},
		{
			name:    "DISPLAY unknown field rejected",
			args:    map[string]interface{}{"device_id": 13.0, "attributes": map[string]interface{}{"bogus": "x"}},
			wantSub: "not allowed when updating a DISPLAY device",
		},
		{
			name:    "RAW attributes rejected (non-editable dtype)",
			args:    map[string]interface{}{"device_id": 14.0, "attributes": map[string]interface{}{"path": "/mnt/tank/vms/raw1.img"}},
			wantSub: "only supported for DISK, CDROM, NIC, and DISPLAY",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newVMManageEnv("25.10.0")
			r, fake, _ := env.buildRegistry(t, false)
			_, err := r.CallTool(context.Background(), "update_vm_device", tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
			assertNoWriteCalls(t, fake)
		})
	}
}

func TestUpdateVMDeviceRunningAndForce(t *testing.T) {
	env := newVMManageEnv("25.10.0")
	r, fake, _ := env.buildRegistry(t, false)

	// Device 20 belongs to running VM 2.
	args := map[string]interface{}{"device_id": 20.0, "order": 1005.0}
	assertPreviewBlocked(t, r, "update_vm_device", args, "force=true")
	assertNoWriteCalls(t, fake)

	// SUSPENDED VM also refused without force.
	env.statuses[2] = map[string]interface{}{"state": "SUSPENDED", "pid": 4321.0, "domain_state": "PAUSED"}
	assertPreviewBlocked(t, r, "update_vm_device", args, "SUSPENDED")

	// With force=true, preview warns and confirm succeeds WITHOUT sending force to middleware.
	env.statuses[2] = map[string]interface{}{"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"}
	forceArgs := map[string]interface{}{"device_id": 20.0, "order": 1005.0, "force": true}
	res, tok := previewAndGetToken(t, r, "update_vm_device", forceArgs)
	if tok == "" {
		t.Fatalf("expected token with force=true, got %+v", res)
	}
	rawPrev, _ := json.Marshal(res)
	if !strings.Contains(string(rawPrev), "stored configuration only") {
		t.Errorf("expected stored-configuration warning, got: %s", rawPrev)
	}
	if _, err := r.CallTool(context.Background(), "update_vm_device", tokenArgs(forceArgs, map[string]interface{}{"confirmation_token": tok})); err != nil {
		t.Fatalf("confirm with force=true failed: %v", err)
	}
	call := lastCall(t, fake, "vm.device.update")
	if strings.Contains(string(call.Params), `"force"`) {
		t.Fatalf("vm.device.update payload must not include force: %s", call.Params)
	}
}

func TestUpdateVMDeviceDiskValidatorsAndVersionGates(t *testing.T) {
	const gib = float64(1 << 30)

	t.Run("logical > physical refused everywhere", func(t *testing.T) {
		for _, ver := range []string{"25.10.0", "27.0.0", "28.0.0-MASTER", ""} {
			env := newVMManageEnv(ver)
			r, _, _ := env.buildRegistry(t, false)
			assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
				"device_id": 10.0,
				"attributes": map[string]interface{}{
					"logical_sectorsize": 4096.0, "physical_sectorsize": 512.0,
				},
			}, "cannot exceed physical_sectorsize")
		}
	})

	t.Run("logical > existing physical refused", func(t *testing.T) {
		env := newVMManageEnv("28.0.0-MASTER")
		dev10 := env.vms[0]["devices"].([]interface{})[0].(map[string]interface{})
		dev10["attributes"].(map[string]interface{})["physical_sectorsize"] = 512.0
		r, _, _ := env.buildRegistry(t, false)
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  10.0,
			"attributes": map[string]interface{}{"logical_sectorsize": 4096.0},
		}, "cannot exceed physical_sectorsize")
	})

	t.Run("physical without logical refused on 25.10 and unknown version, allowed on 27.0 and 28.0", func(t *testing.T) {
		for _, ver := range []string{"25.10.0", ""} {
			env := newVMManageEnv(ver)
			r, _, _ := env.buildRegistry(t, false)
			assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
				"device_id":  10.0,
				"attributes": map[string]interface{}{"physical_sectorsize": 4096.0},
			}, "physical_sectorsize requires logical_sectorsize")
		}
		for _, ver := range []string{"27.0.0", "28.0.0-MASTER"} {
			env := newVMManageEnv(ver)
			r, _, _ := env.buildRegistry(t, false)
			_, tok := previewAndGetToken(t, r, "update_vm_device", map[string]interface{}{
				"device_id":  10.0,
				"attributes": map[string]interface{}{"physical_sectorsize": 4096.0},
			})
			if tok == "" {
				t.Fatalf("version %s: expected token for physical_sectorsize without logical_sectorsize", ver)
			}
		}
	})

	t.Run("disk path guards: managed dataset, shared zvol, not in disk_choices, locked, readonly", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, _, _ := env.buildRegistry(t, false)

		// Managed dataset path refused
		_, err := r.CallTool(context.Background(), "update_vm_device", map[string]interface{}{
			"device_id":  10.0,
			"attributes": map[string]interface{}{"path": "/dev/zvol/tank/ix-apps/bad"},
		})
		if err == nil || !strings.Contains(err.Error(), "ix-apps") {
			t.Fatalf("expected managed dataset refusal, got: %v", err)
		}

		// Non-/dev/zvol/ path refused
		_, err = r.CallTool(context.Background(), "update_vm_device", map[string]interface{}{
			"device_id":  10.0,
			"attributes": map[string]interface{}{"path": "/dev/sdb"},
		})
		if err == nil || !strings.Contains(err.Error(), "/dev/zvol/") {
			t.Fatalf("expected /dev/zvol/ refusal, got: %v", err)
		}

		// Zvol already attached to another VM refused
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  10.0,
			"attributes": map[string]interface{}{"path": "/dev/zvol/tank/vms/other-disk0"},
		}, "already attached to")

		// Zvol not in disk_choices refused
		env.datasets["tank/vms/unlisted"] = map[string]interface{}{
			"id": "tank/vms/unlisted", "name": "tank/vms/unlisted", "type": "VOLUME",
			"locked": false, "readonly": map[string]interface{}{"parsed": false},
			"volsize": map[string]interface{}{"parsed": 10 * gib},
		}
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  10.0,
			"attributes": map[string]interface{}{"path": "/dev/zvol/tank/vms/unlisted"},
		}, "not in vm.device.disk_choices")

		// Locked zvol refused
		env.datasets["tank/vms/free0"]["locked"] = true
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  10.0,
			"attributes": map[string]interface{}{"path": "/dev/zvol/tank/vms/free0"},
		}, "is locked")

		// Read-only zvol refused
		env.datasets["tank/vms/free0"]["locked"] = false
		env.datasets["tank/vms/free0"]["readonly"] = map[string]interface{}{"parsed": true}
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  10.0,
			"attributes": map[string]interface{}{"path": "/dev/zvol/tank/vms/free0"},
		}, "is read-only")

		// Non-VOLUME dataset refused
		env.datasets["tank/vms/free0"]["readonly"] = map[string]interface{}{"parsed": false}
		env.datasets["tank/vms/free0"]["type"] = "FILESYSTEM"
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  10.0,
			"attributes": map[string]interface{}{"path": "/dev/zvol/tank/vms/free0"},
		}, "not a VOLUME")
	})
}

func TestUpdateVMDeviceCDROMValidators(t *testing.T) {
	const gib = float64(1 << 30)
	env := newVMManageEnv("25.10.0")
	r, _, _ := env.buildRegistry(t, false)

	// Empty path refused with eject hint
	_, err := r.CallTool(context.Background(), "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": ""},
	})
	if err == nil || !strings.Contains(err.Error(), "delete_vm_device") {
		t.Fatalf("empty CDROM path: err = %v, want hint to use delete_vm_device", err)
	}

	// Braces in path refused
	_, err = r.CallTool(context.Background(), "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": "/mnt/tank/isos/{bad}.iso"},
	})
	if err == nil || !strings.Contains(err.Error(), "{") {
		t.Fatalf("braces in CDROM path: err = %v", err)
	}

	// Path outside /mnt/<pool> refused
	_, err = r.CallTool(context.Background(), "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": "/etc/passwd"},
	})
	if err == nil || !strings.Contains(err.Error(), "/mnt/") {
		t.Fatalf("non-/mnt CDROM path: err = %v", err)
	}

	// Duplicate CDROM path on sibling CDROM refused
	env.vms[0]["devices"] = append(env.vms[0]["devices"].([]interface{}), map[string]interface{}{
		"id": 15.0, "vm": 1.0, "order": 1006.0,
		"attributes": map[string]interface{}{"dtype": "CDROM", "path": "/mnt/tank/isos/second.iso"},
	})
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": "/mnt/tank/isos/second.iso"},
	}, "already attached to sibling CDROM")
	env.vms[0]["devices"] = env.vms[0]["devices"].([]interface{})[:5]

	// Locked dataset refused
	env.datasets["tank/isos"]["locked"] = true
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": "/mnt/tank/isos/second.iso"},
	}, "is locked")
	env.datasets["tank/isos"]["locked"] = false

	// Missing ISO refused
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": "/mnt/tank/isos/missing.iso"},
	}, "does not exist")

	// Symlink ISO refused
	env.stats["/mnt/tank/isos/link.iso"] = map[string]interface{}{
		"realpath": "/mnt/tank/isos/second.iso", "type": "SYMLINK", "mode": 0o120777,
	}
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": "/mnt/tank/isos/link.iso"},
	}, "is a symlink")

	// Managed dataset refused
	env.stats["/mnt/tank/ix-apps"] = map[string]interface{}{
		"realpath": "/mnt/tank/ix-apps", "type": "DIRECTORY", "inode": 5.0, "dev": 5.0, "mount_id": 5.0,
	}
	env.stats["/mnt/tank/ix-apps/app.iso"] = map[string]interface{}{
		"realpath": "/mnt/tank/ix-apps/app.iso", "type": "FILE", "size": gib, "mode": 0o100644,
	}
	env.statfs["/mnt/tank/ix-apps"] = map[string]interface{}{
		"source": "tank/ix-apps", "fstype": "zfs", "dest": "/mnt/tank/ix-apps", "fsid": "fsid-ix",
	}
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": "/mnt/tank/ix-apps/app.iso"},
	}, "ix-apps")

	// File not readable by libvirt-qemu (0600 root:root, no ACL) refused
	env.stats["/mnt/tank/isos/private.iso"] = map[string]interface{}{
		"realpath": "/mnt/tank/isos/private.iso", "type": "FILE",
		"size": gib, "uid": 0.0, "gid": 0.0, "user": "root", "group": "root",
		"mode": 0o100600, "acl": false,
	}
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": "/mnt/tank/isos/private.iso"},
	}, "not readable by libvirt-qemu")

	// Parent dir not traversable by libvirt-qemu (0700 root:root, no ACL) refused
	env.stats["/mnt/tank/isos"]["mode"] = 0o40700
	env.stats["/mnt/tank/isos"]["uid"] = 0.0
	env.stats["/mnt/tank/isos"]["gid"] = 0.0
	env.stats["/mnt/tank/isos"]["acl"] = false
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": "/mnt/tank/isos/second.iso"},
	}, "not traversable by libvirt-qemu")

	// Restore parent dir and verify ACL-enabled 0600 file is allowed (cannot inspect ACE without private RPC)
	delete(env.stats["/mnt/tank/isos"], "mode")
	env.stats["/mnt/tank/isos/acl.iso"] = map[string]interface{}{
		"realpath": "/mnt/tank/isos/acl.iso", "type": "FILE",
		"size": gib, "uid": 0.0, "gid": 0.0, "mode": 0o100600, "acl": true,
	}
	_, tok := previewAndGetToken(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  11.0,
		"attributes": map[string]interface{}{"path": "/mnt/tank/isos/acl.iso"},
	})
	if tok == "" {
		t.Fatal("expected token for ACL-enabled ISO")
	}
}

func TestUpdateVMDeviceNICValidators(t *testing.T) {
	env := newVMManageEnv("25.10.0")
	r, _, _ := env.buildRegistry(t, false)

	// Unknown nic_attach refused
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  12.0,
		"attributes": map[string]interface{}{"nic_attach": "eth99"},
	}, "not in vm.device.nic_attach_choices")

	// trust_guest_rx_filters=true refused when attached to existing bridge br0
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  12.0,
		"attributes": map[string]interface{}{"trust_guest_rx_filters": true},
	}, "not supported when attached to bridge")

	// trust_guest_rx_filters=true refused with E1000 even on macvtap
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id": 12.0,
		"attributes": map[string]interface{}{
			"type": "E1000", "nic_attach": "eno1", "trust_guest_rx_filters": true,
		},
	}, "requires NIC type VIRTIO")

	// Multicast MAC refused
	_, err := r.CallTool(context.Background(), "update_vm_device", map[string]interface{}{
		"device_id":  12.0,
		"attributes": map[string]interface{}{"mac": "01:00:5e:00:00:01"},
	})
	if err == nil || !strings.Contains(err.Error(), "multicast") {
		t.Fatalf("expected multicast MAC refusal, got: %v", err)
	}

	// Duplicate MAC on sibling NIC refused
	env.vms[0]["devices"] = append(env.vms[0]["devices"].([]interface{}), map[string]interface{}{
		"id": 15.0, "vm": 1.0, "order": 1006.0,
		"attributes": map[string]interface{}{"dtype": "NIC", "type": "VIRTIO", "nic_attach": "eno1", "mac": "02:AA:BB:CC:DD:EE"},
	})
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  12.0,
		"attributes": map[string]interface{}{"mac": "02:aa:bb:cc:dd:ee"},
	}, "already used by sibling NIC")

	// Changing NIC type to E1000 when existing trust_guest_rx_filters is true refused
	dev12 := env.vms[0]["devices"].([]interface{})[2].(map[string]interface{})
	dev12["attributes"].(map[string]interface{})["nic_attach"] = "eno1"
	dev12["attributes"].(map[string]interface{})["trust_guest_rx_filters"] = true
	assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
		"device_id":  12.0,
		"attributes": map[string]interface{}{"type": "E1000"},
	}, "requires NIC type VIRTIO")
}

func TestUpdateVMDeviceDisplayValidatorsAndVersionGates(t *testing.T) {
	t.Run("port clashes, reserved 6000, self-clash, and non-self-collision", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, _, _ := env.buildRegistry(t, false)

		// Port 5910 is used by VM 2's DISPLAY 21.
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"port": 5910.0},
		}, "already in use by VM id 2")

		// Web port 5911 is used by VM 2's DISPLAY 21.
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"web_port": 5911.0},
		}, "already in use by VM id 2")

		// Reserved port 6000 refused on both port and web_port.
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"port": 6000.0},
		}, "reserved by TrueNAS")
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"web_port": 6000.0},
		}, "reserved by TrueNAS")

		// Setting port equal to existing web_port (5901) refused.
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"port": 5901.0},
		}, "cannot both be 5901")

		// Updating resolution alone does NOT collide with device 13's own ports 5900/5901.
		_, tok := previewAndGetToken(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"resolution": "1280x1024"},
		})
		if tok == "" {
			t.Fatal("expected token when updating resolution without port conflict")
		}
	})

	t.Run("password rules by version and VNC max length without echoing secret", func(t *testing.T) {
		const longSecret = "TooLongVncSecret99"
		// 25.10 and unknown version refuse clearing password.
		for _, ver := range []string{"25.10.0", ""} {
			env := newVMManageEnv(ver)
			r, _, _ := env.buildRegistry(t, false)
			assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
				"device_id":  13.0,
				"attributes": map[string]interface{}{"password": ""},
			}, "requires a non-empty display password")

			// Updating another field when existing password is empty also refused on 25.10 / unknown.
			dev13 := env.vms[0]["devices"].([]interface{})[3].(map[string]interface{})
			dev13["attributes"].(map[string]interface{})["password"] = ""
			assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
				"device_id":  13.0,
				"attributes": map[string]interface{}{"resolution": "1920x1080"},
			}, "requires a display password")
		}

		// 28.0 allows clearing password with a warning.
		env28 := newVMManageEnv("28.0.0-MASTER")
		r28, _, _ := env28.buildRegistry(t, false)
		_, tok := previewAndGetToken(t, r28, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"password": ""},
		})
		if tok == "" {
			t.Fatal("expected 28.0 to allow clearing display password")
		}

		// Blank whitespace-only password refused everywhere.
		_, err := r28.CallTool(context.Background(), "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"password": "   "},
		})
		if err == nil || !strings.Contains(err.Error(), "must not be blank") {
			t.Fatalf("expected blank password refusal, got: %v", err)
		}

		// VNC password > 8 chars refused without echoing secret.
		_, err = r28.CallTool(context.Background(), "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"type": "VNC", "password": longSecret},
		})
		if err == nil || !strings.Contains(err.Error(), "at most 8 characters") {
			t.Fatalf("expected VNC password length refusal, got: %v", err)
		}
		if strings.Contains(err.Error(), longSecret) {
			t.Fatalf("error leaked VNC password: %v", err)
		}
	})

	t.Run("VNC web=true version gate (25.10 & unknown refused, 28.0 allowed)", func(t *testing.T) {
		for _, ver := range []string{"25.10.0", ""} {
			env := newVMManageEnv(ver)
			r, _, _ := env.buildRegistry(t, false)
			// Switching SPICE (web=true) to VNC without setting web=false is refused!
			assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
				"device_id":  13.0,
				"attributes": map[string]interface{}{"type": "VNC", "password": "abcd"},
			}, "VNC does not support web=true")
		}
		env28 := newVMManageEnv("28.0.0-MASTER")
		r28, _, _ := env28.buildRegistry(t, false)
		_, tok := previewAndGetToken(t, r28, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"type": "VNC", "web": true, "password": "abcd"},
		})
		if tok == "" {
			t.Fatal("expected 28.0 to allow VNC web=true")
		}
	})

	t.Run("wildcard bind guard, bind_choices, duplicate display type, port bounds, and newline password", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		env.bindChoices["::"] = "::"
		r, _, _ := env.buildRegistry(t, false)
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"bind": "0.0.0.0"},
		}, "allow_wildcard_display_bind=true")
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"bind": "::"},
		}, "allow_wildcard_display_bind=true")

		_, tok := previewAndGetToken(t, r, "update_vm_device", map[string]interface{}{
			"device_id":                   13.0,
			"attributes":                  map[string]interface{}{"bind": "0.0.0.0"},
			"allow_wildcard_display_bind": true,
		})
		if tok == "" {
			t.Fatal("expected token when allow_wildcard_display_bind=true")
		}

		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"bind": "192.0.2.99"},
		}, "not in vm.device.bind_choices")

		// Port below 5900 refused
		_, err := r.CallTool(context.Background(), "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"port": 5899.0},
		})
		if err == nil || !strings.Contains(err.Error(), "between 5900 and 65535") {
			t.Fatalf("expected port range error, got: %v", err)
		}

		// Newline in password refused
		_, err = r.CallTool(context.Background(), "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"password": "bad\npass"},
		})
		if err == nil || !strings.Contains(err.Error(), "single-line") {
			t.Fatalf("expected newline password refusal, got: %v", err)
		}

		// Switching SPICE -> VNC when VM already has a VNC display refused
		env.vms[0]["devices"] = append(env.vms[0]["devices"].([]interface{}), map[string]interface{}{
			"id": 16.0, "vm": 1.0, "order": 1007.0,
			"attributes": map[string]interface{}{"dtype": "DISPLAY", "type": "VNC", "bind": "127.0.0.1", "web": false, "password": "********"},
		})
		assertPreviewBlocked(t, r, "update_vm_device", map[string]interface{}{
			"device_id":  13.0,
			"attributes": map[string]interface{}{"type": "VNC", "web": false, "password": "abcd"},
		}, "already has a VNC DISPLAY")
	})
}

func TestUpdateVMDeviceTokenStaleness(t *testing.T) {
	env := newVMManageEnv("25.10.0")
	r, fake, _ := env.buildRegistry(t, false)
	args := map[string]interface{}{"device_id": 10.0, "order": 1009.0}
	_, tok := previewAndGetToken(t, r, "update_vm_device", args)

	// VM starts between preview and confirm -> token must be rejected as stale!
	env.statuses[1] = map[string]interface{}{"state": "RUNNING", "pid": 999.0, "domain_state": "RUNNING"}
	_, err := r.CallTool(context.Background(), "update_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil {
		t.Fatal("expected stale/blocked error after VM started between preview and confirm")
	}
	assertNoWriteCalls(t, fake)

	// Reset VM to STOPPED, get fresh token, then mutate device's existing order.
	env.statuses[1] = map[string]interface{}{"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"}
	_, tok2 := previewAndGetToken(t, r, "update_vm_device", args)
	dev10 := env.vms[0]["devices"].([]interface{})[0].(map[string]interface{})
	dev10["order"] = 1007.0
	_, err = r.CallTool(context.Background(), "update_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok2}))
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("expected stale token error after device row changed, got: %v", err)
	}
	assertNoWriteCalls(t, fake)
}

func TestDeleteVMDeviceHappyPathsAndGuards(t *testing.T) {
	t.Run("eject CDROM keeps ISO on disk", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"device_id": 11.0}
		res, tok := previewAndGetToken(t, r, "delete_vm_device", args)
		rawPrev, _ := json.Marshal(res)
		if !strings.Contains(string(rawPrev), "ISO file on disk is kept") {
			t.Errorf("expected ISO kept message in preview: %s", rawPrev)
		}
		out, err := r.CallTool(context.Background(), "delete_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err != nil {
			t.Fatalf("confirm failed: %v", err)
		}
		if !strings.Contains(out, "Ejected and removed CDROM device 11") {
			t.Errorf("unexpected execute output: %s", out)
		}
		call := lastCall(t, fake, "vm.device.delete")
		var params []interface{}
		_ = json.Unmarshal(call.Params, &params)
		opts := params[1].(map[string]interface{})
		if opts["zvol"] != false || opts["raw_file"] != false || opts["force"] != false {
			t.Errorf("unexpected options: %+v", opts)
		}
	})

	t.Run("delete DISK with zvol=true shows size and destroys zvol", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"device_id": 10.0, "zvol": true}
		res, tok := previewAndGetToken(t, r, "delete_vm_device", args)
		rawPrev, _ := json.Marshal(res)
		if !strings.Contains(string(rawPrev), "20.00 GiB") || !strings.Contains(string(rawPrev), "tank/vms/app-disk0") {
			t.Errorf("expected zvol name and size in preview: %s", rawPrev)
		}
		if _, err := r.CallTool(context.Background(), "delete_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok})); err != nil {
			t.Fatalf("confirm failed: %v", err)
		}
		call := lastCall(t, fake, "vm.device.delete")
		var params []interface{}
		_ = json.Unmarshal(call.Params, &params)
		opts := params[1].(map[string]interface{})
		if opts["zvol"] != true {
			t.Errorf("expected zvol=true in vm.device.delete options: %+v", opts)
		}
	})

	t.Run("delete RAW with raw_file=true shows size", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"device_id": 14.0, "raw_file": true}
		res, tok := previewAndGetToken(t, r, "delete_vm_device", args)
		rawPrev, _ := json.Marshal(res)
		if !strings.Contains(string(rawPrev), "4.00 GiB") {
			t.Errorf("expected raw file size in preview: %s", rawPrev)
		}
		if _, err := r.CallTool(context.Background(), "delete_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok})); err != nil {
			t.Fatalf("confirm failed: %v", err)
		}
		call := lastCall(t, fake, "vm.device.delete")
		var params []interface{}
		_ = json.Unmarshal(call.Params, &params)
		if params[1].(map[string]interface{})["raw_file"] != true {
			t.Errorf("expected raw_file=true: %s", call.Params)
		}
	})

	t.Run("refusals: running/suspended even with force, ERROR needs force, shared zvol, managed zvol, wrong dtype", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)

		// RUNNING VM refused EVEN with force=true (middleware refuses unconditionally).
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 20.0, "force": true}, "even with force=true")

		// SUSPENDED VM refused EVEN with force=true.
		env.statuses[2] = map[string]interface{}{"state": "SUSPENDED", "pid": 4321.0, "domain_state": "PAUSED"}
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 20.0, "force": true}, "even with force=true")

		// ERROR state refused without force, allowed with force=true.
		env.statuses[1] = map[string]interface{}{"state": "ERROR", "pid": nil, "domain_state": "ERROR"}
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 11.0}, "force=true")
		_, tok := previewAndGetToken(t, r, "delete_vm_device", map[string]interface{}{"device_id": 11.0, "force": true})
		if tok == "" {
			t.Fatal("expected token for ERROR state with force=true")
		}
		env.statuses[1] = map[string]interface{}{"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"}

		// zvol + raw_file both true refused
		_, err := r.CallTool(context.Background(), "delete_vm_device", map[string]interface{}{"device_id": 10.0, "zvol": true, "raw_file": true})
		if err == nil || !strings.Contains(err.Error(), "cannot both be true") {
			t.Fatalf("expected zvol+raw_file refusal, got: %v", err)
		}

		// zvol=true on CDROM refused
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 11.0, "zvol": true}, "only valid for DISK")

		// raw_file=true on DISK refused
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 10.0, "raw_file": true}, "only valid for RAW")

		// zvol=true when attached to another VM refused!
		env.vms[1]["devices"] = append(env.vms[1]["devices"].([]interface{}), map[string]interface{}{
			"id": 22.0, "vm": 2.0, "order": 1003.0,
			"attributes": map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/app-disk0", "type": "VIRTIO"},
		})
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 10.0, "zvol": true}, "also attached to")

		// zvol=true on managed dataset path refused!
		dev10 := env.vms[0]["devices"].([]interface{})[0].(map[string]interface{})
		dev10["attributes"].(map[string]interface{})["path"] = "/dev/zvol/tank/ix-apps/secret-zvol"
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 10.0, "zvol": true}, "ix-apps")

		// zvol=true on non-/dev/zvol/ path refused!
		dev10["attributes"].(map[string]interface{})["path"] = "/dev/sdb"
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 10.0, "zvol": true}, "not under /dev/zvol/")

		// zvol=true on non-VOLUME dataset refused!
		dev10["attributes"].(map[string]interface{})["path"] = "/dev/zvol/tank/vms"
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 10.0, "zvol": true}, "not a VOLUME")

		// raw_file=true when shared with another VM refused!
		env.vms[1]["devices"] = append(env.vms[1]["devices"].([]interface{}), map[string]interface{}{
			"id": 23.0, "vm": 2.0, "order": 1004.0,
			"attributes": map[string]interface{}{"dtype": "RAW", "path": "/mnt/tank/vms/raw0.img"},
		})
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 14.0, "raw_file": true}, "also used by")
		env.vms[1]["devices"] = env.vms[1]["devices"].([]interface{})[:2]

		// raw_file=true when file is a symlink refused!
		env.stats["/mnt/tank/vms/raw0.img"]["type"] = "SYMLINK"
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 14.0, "raw_file": true}, "is a symlink")
		env.stats["/mnt/tank/vms/raw0.img"]["type"] = "FILE"

		// raw_file=true on managed dataset refused!
		env.statfs["/mnt/tank/vms"]["source"] = "tank/ix-apps"
		assertPreviewBlocked(t, r, "delete_vm_device", map[string]interface{}{"device_id": 14.0, "raw_file": true}, "ix-apps")

		assertNoWriteCalls(t, fake)
	})

	t.Run("token staleness when another VM attaches the zvol before confirm", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"device_id": 10.0, "zvol": true}
		_, tok := previewAndGetToken(t, r, "delete_vm_device", args)

		// Another VM attaches app-disk0 before confirm!
		env.vms[1]["devices"] = append(env.vms[1]["devices"].([]interface{}), map[string]interface{}{
			"id": 22.0, "vm": 2.0, "order": 1003.0,
			"attributes": map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/app-disk0", "type": "VIRTIO"},
		})
		_, err := r.CallTool(context.Background(), "delete_vm_device", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err == nil {
			t.Fatal("expected refusal after another VM attached the zvol")
		}
		assertNoWriteCalls(t, fake)
	})
}

func TestDeleteVMHappyPathsAndGuards(t *testing.T) {
	t.Run("delete STOPPED VM keeping zvols on both 25.10 (bool) and 28.0 (null)", func(t *testing.T) {
		for _, ver := range []string{"25.10.0", "28.0.0-MASTER"} {
			env := newVMManageEnv(ver)
			// Put a plaintext password in the fake DISPLAY device to verify it is never leaked.
			dev13 := env.vms[0]["devices"].([]interface{})[3].(map[string]interface{})
			dev13["attributes"].(map[string]interface{})["password"] = "SecretVMDisp99"
			r, fake, _ := env.buildRegistry(t, false)

			args := map[string]interface{}{"name": "appvm"}
			res, tok := previewAndGetToken(t, r, "delete_vm", args)
			rawPrev, _ := json.Marshal(res)
			if strings.Contains(string(rawPrev), "SecretVMDisp99") {
				t.Fatalf("%s: delete_vm preview leaked display password: %s", ver, rawPrev)
			}
			if !strings.Contains(string(rawPrev), "20.00 GiB") {
				t.Errorf("%s: expected kept zvol size in preview: %s", ver, rawPrev)
			}
			out, err := r.CallTool(context.Background(), "delete_vm", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
			if err != nil {
				t.Fatalf("%s: confirm failed: %v", ver, err)
			}
			var execRes map[string]interface{}
			if err := json.Unmarshal([]byte(out), &execRes); err != nil {
				t.Fatalf("%s: parse output: %v", ver, err)
			}
			msg, _ := execRes["message"].(string)
			if !strings.Contains(msg, `Deleted VM "appvm" (id 1)`) || !strings.Contains(msg, "kept") {
				t.Errorf("%s: unexpected message: %s", ver, msg)
			}
			call := lastCall(t, fake, "vm.delete")
			var params []interface{}
			_ = json.Unmarshal(call.Params, &params)
			opts := params[1].(map[string]interface{})
			if opts["zvols"] != false || opts["force"] != false {
				t.Errorf("%s: expected zvols=false, force=false, got %+v", ver, opts)
			}
		}
	})

	t.Run("delete STOPPED VM with zvols=true lists zvols and sizes", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"id": 1.0, "zvols": true}
		res, tok := previewAndGetToken(t, r, "delete_vm", args)
		rawPrev, _ := json.Marshal(res)
		if !strings.Contains(string(rawPrev), "tank/vms/app-disk0 (20.00 GiB, 0 snapshot(s))") {
			t.Errorf("expected zvol name and size in preview/summary: %s", rawPrev)
		}
		if _, err := r.CallTool(context.Background(), "delete_vm", tokenArgs(args, map[string]interface{}{"confirmation_token": tok})); err != nil {
			t.Fatalf("confirm failed: %v", err)
		}
		call := lastCall(t, fake, "vm.delete")
		var params []interface{}
		_ = json.Unmarshal(call.Params, &params)
		if params[1].(map[string]interface{})["zvols"] != true {
			t.Errorf("expected zvols=true in vm.delete call: %s", call.Params)
		}
	})

	t.Run("running, suspended, and error VMs refused without force; running allowed with force=true", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		assertPreviewBlocked(t, r, "delete_vm", map[string]interface{}{"id": 2.0}, "force=true")
		env.statuses[2] = map[string]interface{}{"state": "SUSPENDED", "pid": 4321.0, "domain_state": "PAUSED"}
		assertPreviewBlocked(t, r, "delete_vm", map[string]interface{}{"id": 2.0}, "SUSPENDED")
		env.statuses[2] = map[string]interface{}{"state": "ERROR", "pid": nil, "domain_state": "ERROR"}
		assertPreviewBlocked(t, r, "delete_vm", map[string]interface{}{"id": 2.0}, "ERROR")
		env.statuses[2] = map[string]interface{}{"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"}
		assertNoWriteCalls(t, fake)

		// Mismatched id and name refused
		_, err := r.CallTool(context.Background(), "delete_vm", map[string]interface{}{"id": 1.0, "name": "othervm"})
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("expected id/name mismatch refusal, got: %v", err)
		}

		forceArgs := map[string]interface{}{"id": 2.0, "force": true}
		res, tok := previewAndGetToken(t, r, "delete_vm", forceArgs)
		rawPrev, _ := json.Marshal(res)
		if !strings.Contains(string(rawPrev), "forcibly power it off") {
			t.Errorf("expected forced poweroff warning: %s", rawPrev)
		}
		if _, err := r.CallTool(context.Background(), "delete_vm", tokenArgs(forceArgs, map[string]interface{}{"confirmation_token": tok})); err != nil {
			t.Fatalf("confirm with force=true failed: %v", err)
		}
		call := lastCall(t, fake, "vm.delete")
		var params []interface{}
		_ = json.Unmarshal(call.Params, &params)
		if params[1].(map[string]interface{})["force"] != true {
			t.Errorf("expected force=true in vm.delete call: %s", call.Params)
		}
	})

	t.Run("zvols=true refused if shared with another VM or managed dataset or non-VOLUME", func(t *testing.T) {
		// Shared with another VM
		env := newVMManageEnv("25.10.0")
		env.vms[1]["devices"] = append(env.vms[1]["devices"].([]interface{}), map[string]interface{}{
			"id": 22.0, "vm": 2.0, "order": 1003.0,
			"attributes": map[string]interface{}{"dtype": "DISK", "path": "/dev/zvol/tank/vms/app-disk0", "type": "VIRTIO"},
		})
		r, fake, _ := env.buildRegistry(t, false)
		assertPreviewBlocked(t, r, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true}, "also attached to")

		// Managed dataset path
		env2 := newVMManageEnv("25.10.0")
		dev10 := env2.vms[0]["devices"].([]interface{})[0].(map[string]interface{})
		dev10["attributes"].(map[string]interface{})["path"] = "/dev/zvol/boot-pool/disk0"
		r2, fake2, _ := env2.buildRegistry(t, false)
		assertPreviewBlocked(t, r2, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true}, "boot-pool")

		// Non-VOLUME dataset
		env3 := newVMManageEnv("25.10.0")
		dev10_3 := env3.vms[0]["devices"].([]interface{})[0].(map[string]interface{})
		dev10_3["attributes"].(map[string]interface{})["path"] = "/dev/zvol/tank/vms"
		r3, fake3, _ := env3.buildRegistry(t, false)
		assertPreviewBlocked(t, r3, "delete_vm", map[string]interface{}{"id": 1.0, "zvols": true}, "not a VOLUME")

		assertNoWriteCalls(t, fake)
		assertNoWriteCalls(t, fake2)
		assertNoWriteCalls(t, fake3)
	})

	t.Run("token staleness when device list or VM status changes before confirm", func(t *testing.T) {
		env := newVMManageEnv("25.10.0")
		r, fake, _ := env.buildRegistry(t, false)
		args := map[string]interface{}{"id": 1.0}
		_, tok := previewAndGetToken(t, r, "delete_vm", args)

		// Add a new device to VM 1 between preview and confirm -> token must be stale!
		env.vms[0]["devices"] = append(env.vms[0]["devices"].([]interface{}), map[string]interface{}{
			"id": 15.0, "vm": 1.0, "order": 1006.0,
			"attributes": map[string]interface{}{"dtype": "NIC", "type": "VIRTIO", "nic_attach": "br0"},
		})
		_, err := r.CallTool(context.Background(), "delete_vm", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
		if err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("expected stale token error after device list changed, got: %v", err)
		}
		assertNoWriteCalls(t, fake)
	})
}

func lastCall(t *testing.T, fake *fakeMiddleware, method string) fakeCall {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for i := len(fake.calls) - 1; i >= 0; i-- {
		if fake.calls[i].Method == method {
			return fake.calls[i]
		}
	}
	t.Fatalf("no middleware call recorded for %s", method)
	return fakeCall{}
}

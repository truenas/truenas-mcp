package tools

// Checks shared by delete_vm (zvols=true) and delete_vm_device (zvol=true)
// before a VM's backing zvol is destroyed, and the follow-up reads that
// report what a delete actually removed.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/truenas"
)

// vmCleanupTimeout bounds the calls a VM tool makes after its main write
// (rollback, the autostart update, post-delete checks). They run under
// context.WithoutCancel, so a cancelled request cannot stop them halfway.
const vmCleanupTimeout = 2 * time.Minute

// vmCleanupContext detaches ctx from its caller's cancellation and gives it
// vmCleanupTimeout. Values (such as the session) are kept.
func vmCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), vmCleanupTimeout)
}

// zvolSnapshots returns every snapshot of zvol with its holds, userrefs and
// clones. Clones are merged from the clones property and from datasets
// whose origin is one of the snapshots.
func zvolSnapshots(ctx context.Context, client *truenas.Client, zvol string) ([]snapDetail, error) {
	snaps, err := querySnapshotDetails(ctx, client,
		[]interface{}{[]interface{}{"dataset", "=", zvol}},
		func(s snapDetail) bool { return s.Dataset == zvol },
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list snapshots of zvol %s: %w", zvol, err)
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].ID < snaps[j].ID })
	if _, err := attachClones(ctx, client, snaps); err != nil {
		return nil, fmt.Errorf("failed to look up clones of zvol %s: %w", zvol, err)
	}
	return snaps, nil
}

// recursiveZvolDestroyRefusals returns why vm.delete with zvols=true must
// not destroy zvol. From 27.0, vm.delete destroys each zvol with
// zfs.resource.destroy_impl(recursive=True), which releases every hold and
// destroys every clone of the zvol's snapshots (including the disks of a
// VM made with vm.clone). 25.10 runs zfs.dataset.delete(recursive=True),
// which fails on clones and holds, and force=true ignores that failure.
// Either way the tool refuses up front.
func recursiveZvolDestroyRefusals(zvol string, snaps []snapDetail) []string {
	var out []string
	var held, cloned []string
	for _, s := range snaps {
		if s.held() {
			held = append(held, s.holdText())
		}
		if len(s.Clones) > 0 {
			cloned = append(cloned, fmt.Sprintf("%s (clones: %s)", s.ID, strings.Join(s.Clones, ", ")))
		}
	}
	if len(cloned) > 0 {
		out = append(out, fmt.Sprintf("cannot destroy zvol %s: its snapshots have clones, which a recursive destroy would also destroy (or which make it fail): %s; destroy or promote the clones first, or keep the zvol",
			zvol, strings.Join(cloned, "; ")))
	}
	if len(held) > 0 {
		out = append(out, fmt.Sprintf("cannot destroy zvol %s: its snapshots are held, and a recursive destroy would release the holds and destroy them (or fail): %s; release the holds first, or keep the zvol",
			zvol, strings.Join(held, "; ")))
	}
	return out
}

// snapshotIDs lists the snapshot names, in order.
func snapshotIDs(snaps []snapDetail) []string {
	out := make([]string, len(snaps))
	for i, s := range snaps {
		out[i] = s.ID
	}
	return out
}

// zvolConsumers maps a zvol name to the non-VM services that use it, as
// text such as `iSCSI extent "lun0" (id 3)`. These are the consumers the
// middleware's zvol attachment check (zfs.dataset.unlocked_zvols_fast on
// 25.10, zfs.resource.unlocked_zvols_fast on master) knows about, apart from
// VM devices, which the tools check against vm.query themselves.
type zvolConsumers map[string][]string

// zvolFromSharePath maps an iSCSI extent or NVMe-oF namespace path such as
// "zvol/tank/vol" to its zvol name, as the middleware does with
// zvol_path_to_name("/dev/" + path).
func zvolFromSharePath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	return zvolPathToName("/dev/" + strings.TrimPrefix(p, "/dev/"))
}

// fetchZvolConsumers reads iSCSI DISK extents and NVMe-oF ZVOL namespaces on
// every version, and Incus instance disks before 27.0 (the virt plugin was
// removed in 27.0; containers there have no zvol disks). An unknown version
// is treated as 25.10, so the Incus check runs. Any failed read is returned
// as an error: the caller refuses to destroy rather than guess.
func fetchZvolConsumers(ctx context.Context, client *truenas.Client, ver truenas.Version) (zvolConsumers, error) {
	out := zvolConsumers{}
	add := func(zvol, desc string) {
		for _, d := range out[zvol] {
			if d == desc {
				return
			}
		}
		out[zvol] = append(out[zvol], desc)
	}

	raw, err := client.Call(ctx, "iscsi.extent.query", []interface{}{[]interface{}{"type", "=", "DISK"}})
	if err != nil {
		return nil, fmt.Errorf("failed to check iSCSI extents: %w", err)
	}
	var extents []map[string]interface{}
	if err := json.Unmarshal(raw, &extents); err != nil {
		return nil, fmt.Errorf("unexpected iscsi.extent.query result")
	}
	for _, e := range extents {
		name, _ := e["name"].(string)
		id, _ := extractZFSInt64(e["id"])
		for _, key := range []string{"path", "disk"} {
			p, _ := e[key].(string)
			if zn, ok := zvolFromSharePath(p); ok {
				add(zn, fmt.Sprintf("iSCSI extent %q (id %d)", name, id))
			}
		}
	}

	raw, err = client.Call(ctx, "nvmet.namespace.query", []interface{}{[]interface{}{"device_type", "=", "ZVOL"}})
	if err != nil {
		return nil, fmt.Errorf("failed to check NVMe-oF namespaces: %w", err)
	}
	var namespaces []map[string]interface{}
	if err := json.Unmarshal(raw, &namespaces); err != nil {
		return nil, fmt.Errorf("unexpected nvmet.namespace.query result")
	}
	for _, n := range namespaces {
		id, _ := extractZFSInt64(n["id"])
		p, _ := n["device_path"].(string)
		if zn, ok := zvolFromSharePath(p); ok {
			add(zn, fmt.Sprintf("NVMe-oF namespace id %d", id))
		}
	}

	if isVMPre27(ver) {
		raw, err = client.Call(ctx, "virt.instance.query", []interface{}{})
		if err != nil {
			return nil, fmt.Errorf("failed to check Incus instances: %w", err)
		}
		var instances []map[string]interface{}
		if err := json.Unmarshal(raw, &instances); err != nil {
			return nil, fmt.Errorf("unexpected virt.instance.query result")
		}
		for _, inst := range instances {
			id, _ := inst["id"].(string)
			if id == "" {
				id, _ = inst["name"].(string)
			}
			if id == "" {
				continue
			}
			raw, err := client.Call(ctx, "virt.instance.device_list", id)
			if err != nil {
				return nil, fmt.Errorf("failed to list devices of Incus instance %q: %w", id, err)
			}
			var devs []map[string]interface{}
			if err := json.Unmarshal(raw, &devs); err != nil {
				return nil, fmt.Errorf("unexpected virt.instance.device_list result")
			}
			for _, d := range devs {
				if t, _ := d["dev_type"].(string); t != "DISK" {
					continue
				}
				src, _ := d["source"].(string)
				if zn, ok := zvolPathToName(src); ok {
					add(zn, fmt.Sprintf("Incus instance %q", id))
				}
			}
		}
	}

	for k := range out {
		sort.Strings(out[k])
	}
	return out, nil
}

// zvolConsumerRefusal is the refusal for a zvol other services use, or "".
func zvolConsumerRefusal(zvol string, consumers []string) string {
	if len(consumers) == 0 {
		return ""
	}
	return fmt.Sprintf("cannot destroy zvol %s: it is also used by %s; remove that first, or keep the zvol", zvol, strings.Join(consumers, ", "))
}

// zvolStillPresent reports whether zvol exists after a delete. ok is false
// when that could not be read.
func zvolStillPresent(ctx context.Context, client *truenas.Client, zvol string) (present, ok bool) {
	row, err := queryDatasetRow(ctx, client, zvol)
	if err != nil {
		return false, false
	}
	return row != nil, true
}

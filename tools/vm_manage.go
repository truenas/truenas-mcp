package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in this
// file (same-file contract for TestMiddlewareMethodContract).
const (
	vmDeviceUpdateMethod = "vm.device.update"
	vmDeviceDeleteMethod = "vm.device.delete"
	vmDeleteMethod       = "vm.delete"
)

// allowedUpdateFieldsByDType is the authoritative per-dtype allowlist for
// update_vm_device attributes. The top-level schema checker only inspects
// top-level argument names, so this map guards against unknown, read-only,
// create-only, and cross-dtype fields inside attributes.
var allowedUpdateFieldsByDType = map[string]map[string]bool{
	"DISK": {
		"path": true, "type": true, "iotype": true,
		"logical_sectorsize": true, "physical_sectorsize": true,
	},
	"CDROM": {
		"path": true,
	},
	"NIC": {
		"type": true, "nic_attach": true, "mac": true, "trust_guest_rx_filters": true,
	},
	"DISPLAY": {
		"type": true, "bind": true, "web": true, "password": true,
		"port": true, "web_port": true, "resolution": true, "wait": true,
	},
}

func (r *Registry) registerVMManageTools() {
	r.tools["update_vm_device"] = Tool{
		Definition: mcp.Tool{
			Name:        "update_vm_device",
			Description: "DESTRUCTIVE: Modify an existing VM device via vm.device.update (for example changing boot order, swapping a CDROM ISO, or updating DISK, NIC, or DISPLAY settings). Device type (dtype) cannot be changed; to eject an installer CDROM completely, use delete_vm_device instead (which leaves the ISO file on disk). Refuses while the VM is not STOPPED unless force=true (which updates stored configuration only, taking effect after stop and start); a DISK path change always needs the VM STOPPED, even with force=true. Mirrors TrueNAS device validators in the preview. Confirmation is bound to the device row, VM status, and validated targets. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{
						"type":        "integer",
						"minimum":     1,
						"description": "ID of the VM device to update (from query_vms).",
					},
					"order": map[string]interface{}{
						"type":        "integer",
						"minimum":     1,
						"description": "New boot/attach order (lower numbers boot first). Colliding sibling device orders are shifted upward by TrueNAS.",
					},
					"attributes": map[string]interface{}{
						"type":        "object",
						"description": "Partial device attributes to update (merged into existing attributes; do not include dtype). Allowed keys depend on the device's dtype: DISK (path, type, iotype, logical_sectorsize, physical_sectorsize; serial is read-only), CDROM (path), NIC (type, nic_attach, mac, trust_guest_rx_filters), DISPLAY (type, bind, web, password, port, web_port, resolution, wait).",
					},
					"force": map[string]interface{}{
						"type":        "boolean",
						"description": "Allow updating a device while the VM is not STOPPED. Updates stored configuration only; changes take effect after the VM is stopped and started. Does not allow a DISK path change, which always needs the VM STOPPED. Default: false.",
					},
					"allow_wildcard_display_bind": map[string]interface{}{
						"type":        "boolean",
						"description": "Required when setting a DISPLAY device bind address to 0.0.0.0 or ::. Default: false.",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview the device update without executing (default: false).",
					},
				},
				"required": []string{"device_id"},
			},
		},
		Handler: r.handleUpdateVMDevice,
		Plan:    planUpdateVMDevice,
		Execute: r.executeUpdateVMDevice,
	}

	r.tools["delete_vm_device"] = Tool{
		Definition: mcp.Tool{
			Name:        "delete_vm_device",
			Description: "DESTRUCTIVE: Remove a device from a virtual machine via vm.device.delete. This is also how installer CDROMs are ejected in TrueNAS (removing the CDROM device leaves the ISO file on disk). Backing zvols and raw image files are kept by default; pass zvol=true (DISK only) or raw_file=true (RAW only) to permanently destroy them too. Refuses while the VM is RUNNING or SUSPENDED (TrueNAS rejects vm.device.delete on active VMs even with force=true; stop the VM first with stop_vm). Never destroys a zvol or raw file shared with another VM device. A zvol is also refused when it has snapshots (TrueNAS destroys it non-recursively and would refuse) or when an iSCSI extent, NVMe-oF namespace or (before 27.0) Incus instance uses it. After the delete, the result reports whether the device and its backing storage are actually gone. Confirmation is bound to the device row, VM status, and backing storage. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"device_id": map[string]interface{}{
						"type":        "integer",
						"minimum":     1,
						"description": "ID of the VM device to remove (from query_vms).",
					},
					"zvol": map[string]interface{}{
						"type":        "boolean",
						"description": "DISK devices only: also permanently destroy the backing ZFS volume (zvol). Refused if it has snapshots, is shared with another VM device or another service (iSCSI, NVMe-oF), or is outside user datasets. Default: false.",
					},
					"raw_file": map[string]interface{}{
						"type":        "boolean",
						"description": "RAW devices only: also permanently delete the backing raw image file on disk. Refused if shared with another VM device or outside user datasets. Default: false.",
					},
					"force": map[string]interface{}{
						"type":        "boolean",
						"description": "Ignore errors when destroying the backing zvol or raw file and still remove the device record (also allows deletion when VM state is ERROR); the result then reports what is still present. Does not bypass TrueNAS's refusal on RUNNING or SUSPENDED VMs. Default: false.",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview the device deletion without executing (default: false).",
					},
				},
				"required": []string{"device_id"},
			},
		},
		Handler: r.handleDeleteVMDevice,
		Plan:    planDeleteVMDevice,
		Execute: r.executeDeleteVMDevice,
	}

	deleteVMProps := vmSelectorSchemaProps()
	deleteVMProps["zvols"] = map[string]interface{}{
		"type":        "boolean",
		"description": "Also permanently and recursively destroy every attached /dev/zvol/ DISK volume and its snapshots. Refused if any zvol is attached to another VM, used by another service (iSCSI, NVMe-oF, or before 27.0 an Incus instance), has a snapshot with clones or holds, or fails managed-dataset guards. Default: false.",
	}
	deleteVMProps["force"] = map[string]interface{}{
		"type":        "boolean",
		"description": "Allow deleting a VM that is not STOPPED (forcibly powers off a running VM without guest shutdown) and ignore domain or zvol cleanup errors; the result then reports which of the VM and its zvols are still present. Default: false.",
	}
	r.tools["delete_vm"] = Tool{
		Definition: mcp.Tool{
			Name:        "delete_vm",
			Description: "DESTRUCTIVE: Delete a virtual machine via vm.delete. Refuses if the VM is not STOPPED unless force=true (which forcibly powers off a running VM and ignores cleanup errors). Backing zvols are kept by default; pass zvols=true to permanently and recursively destroy every attached /dev/zvol/ DISK volume and its snapshots (refused if any zvol is attached to another VM, is used by an iSCSI extent, NVMe-oF namespace or, before 27.0, an Incus instance, has a snapshot with clones or holds, which a recursive destroy would also destroy or release, or fails managed-dataset guards; the preview lists every zvol with its size and snapshot count). After the delete, the result reports which of the VM and its zvols are actually gone. Confirmation is bound to the VM row, live status, full device list, and each zvol's dataset, snapshots, clones and holds. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": deleteVMProps,
			},
		},
		Handler: r.handleDeleteVM,
		Plan:    planDeleteVM,
		Execute: r.executeDeleteVM,
	}
}

// --- Shared device/VM lookup context ---

type vmDeviceRef struct {
	VMID     int
	VMName   string
	DeviceID int
	DType    string
}

type vmDeviceContext struct {
	DeviceID   int
	DeviceRow  map[string]interface{}
	DeviceAttr map[string]interface{}
	DType      string
	Order      int
	VM         *resolvedVM
	Siblings   []map[string]interface{}
	AllVMs     []map[string]interface{}
	// OtherZvolRefs maps zvolName -> references from any device with id != DeviceID.
	OtherZvolRefs map[string][]vmDeviceRef
	// OtherRawRefs maps cleaned raw path -> references from any device with id != DeviceID.
	OtherRawRefs map[string][]vmDeviceRef
}

func queryAllVMs(ctx context.Context, client *truenas.Client) ([]map[string]interface{}, error) {
	raw, err := client.Call(ctx, "vm.query", []interface{}{}, map[string]interface{}{"select": vmQuerySelect})
	if err != nil {
		return nil, fmt.Errorf("failed to query VMs: %w", err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("failed to parse vm.query response: %w", err)
	}
	return rows, nil
}

func lookupVMDeviceContext(ctx context.Context, client *truenas.Client, deviceID int) (*vmDeviceContext, error) {
	raw, err := client.Call(ctx, "vm.device.query", []interface{}{[]interface{}{"id", "=", deviceID}})
	if err != nil {
		return nil, fmt.Errorf("failed to query VM device %d: %w", deviceID, err)
	}
	var devRows []map[string]interface{}
	if err := json.Unmarshal(raw, &devRows); err != nil {
		return nil, fmt.Errorf("failed to parse vm.device.query response: %w", err)
	}
	var devRow map[string]interface{}
	for _, row := range devRows {
		if id, ok := extractZFSInt64(row["id"]); ok && int(id) == deviceID {
			devRow = row
			break
		}
	}
	if devRow == nil {
		return nil, fmt.Errorf("VM device %d not found", deviceID)
	}

	vmID64, ok := extractZFSInt64(devRow["vm"])
	if !ok || vmID64 < 1 {
		return nil, fmt.Errorf("VM device %d did not report a valid owning VM id", deviceID)
	}
	vmID := int(vmID64)
	order64, _ := extractZFSInt64(devRow["order"])
	attrs, _ := devRow["attributes"].(map[string]interface{})
	if attrs == nil {
		attrs = map[string]interface{}{}
	}
	dtype, _ := attrs["dtype"].(string)

	allVMs, err := queryAllVMs(ctx, client)
	if err != nil {
		return nil, err
	}
	matchedVM, err := matchVMRow(allVMs, vmID, true, "", false)
	if err != nil {
		return nil, fmt.Errorf("owning VM (id %d) for device %d not found: %w", vmID, deviceID, err)
	}
	vm, err := resolveVMRowWithStatus(ctx, client, matchedVM)
	if err != nil {
		return nil, err
	}

	dctx := &vmDeviceContext{
		DeviceID:      deviceID,
		DeviceRow:     devRow,
		DeviceAttr:    attrs,
		DType:         dtype,
		Order:         int(order64),
		VM:            vm,
		AllVMs:        allVMs,
		OtherZvolRefs: map[string][]vmDeviceRef{},
		OtherRawRefs:  map[string][]vmDeviceRef{},
	}

	for _, vmRow := range allVMs {
		rowVMID64, _ := extractZFSInt64(vmRow["id"])
		rowVMID := int(rowVMID64)
		rowVMName, _ := vmRow["name"].(string)
		devs, _ := vmRow["devices"].([]interface{})
		for _, item := range devs {
			dm, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			otherID64, _ := extractZFSInt64(dm["id"])
			otherID := int(otherID64)
			if otherID == deviceID {
				continue
			}
			if rowVMID == vmID {
				dctx.Siblings = append(dctx.Siblings, dm)
			}
			oattrs, _ := dm["attributes"].(map[string]interface{})
			odtype, _ := oattrs["dtype"].(string)
			ref := vmDeviceRef{VMID: rowVMID, VMName: rowVMName, DeviceID: otherID, DType: odtype}
			if odtype == "DISK" {
				if p, _ := oattrs["path"].(string); p != "" {
					if zn, ok := zvolPathToName(p); ok {
						dctx.OtherZvolRefs[zn] = append(dctx.OtherZvolRefs[zn], ref)
					}
				}
			}
			if odtype == "RAW" {
				if p, _ := oattrs["path"].(string); p != "" {
					clean := path.Clean(p)
					dctx.OtherRawRefs[clean] = append(dctx.OtherRawRefs[clean], ref)
				}
			}
		}
	}

	return dctx, nil
}

func formatDeviceRefs(refs []vmDeviceRef) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.DeviceID > 0 {
			parts = append(parts, fmt.Sprintf("VM %q (id %d, device %d)", ref.VMName, ref.VMID, ref.DeviceID))
		} else {
			parts = append(parts, fmt.Sprintf("VM %q (id %d)", ref.VMName, ref.VMID))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func bindSiblingOrders(siblings []map[string]interface{}) []interface{} {
	type pair struct {
		id    int
		order int
	}
	list := make([]pair, 0, len(siblings))
	for _, s := range siblings {
		id, _ := extractZFSInt64(s["id"])
		ord, _ := extractZFSInt64(s["order"])
		list = append(list, pair{id: int(id), order: int(ord)})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].id != list[j].id {
			return list[i].id < list[j].id
		}
		return list[i].order < list[j].order
	})
	out := make([]interface{}, 0, len(list))
	for _, p := range list {
		out = append(out, map[string]interface{}{"id": p.id, "order": p.order})
	}
	return out
}

// --- update_vm_device ---

type vmDeviceUpdateSpec struct {
	DeviceID                 int
	HasOrder                 bool
	Order                    int
	Attributes               map[string]interface{}
	Force                    bool
	AllowWildcardDisplayBind bool
}

func parseUpdateVMDeviceArgs(args map[string]interface{}) (*vmDeviceUpdateSpec, error) {
	devID, hasID, err := vmIntArg(args, "device_id", 0)
	if err != nil || !hasID || devID < 1 {
		return nil, fmt.Errorf("device_id must be a positive integer")
	}
	order, hasOrder, err := vmIntArg(args, "order", 0)
	if err != nil || (hasOrder && order < 1) {
		return nil, fmt.Errorf("order must be a positive integer")
	}

	var attrs map[string]interface{}
	if rawAttrs, exists := args["attributes"]; exists && rawAttrs != nil {
		m, ok := rawAttrs.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("attributes must be an object")
		}
		if _, hasDType := m["dtype"]; hasDType {
			return nil, fmt.Errorf("attributes.dtype cannot be changed; remove and recreate the device instead")
		}
		if len(m) > 0 {
			attrs = copyMapShallow(m)
		}
	}

	if !hasOrder && len(attrs) == 0 {
		return nil, fmt.Errorf("provide order and/or non-empty attributes to update")
	}

	force, _ := args["force"].(bool)
	allowWildcard, _ := args["allow_wildcard_display_bind"].(bool)
	return &vmDeviceUpdateSpec{
		DeviceID:                 devID,
		HasOrder:                 hasOrder,
		Order:                    order,
		Attributes:               attrs,
		Force:                    force,
		AllowWildcardDisplayBind: allowWildcard,
	}, nil
}

func validateUpdateAttributeAllowlist(dtype string, attrs map[string]interface{}) error {
	allowed, supported := allowedUpdateFieldsByDType[dtype]
	if !supported {
		return fmt.Errorf("updating attributes is only supported for DISK, CDROM, NIC, and DISPLAY devices (device dtype is %q); you can still update order alone", dtype)
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if allowed[k] {
			continue
		}
		if dtype == "DISK" && k == "serial" {
			return fmt.Errorf("DISK attribute \"serial\" is read-only and cannot be updated")
		}
		if dtype == "DISK" && (k == "create_zvol" || k == "zvol_name" || k == "zvol_volsize") {
			return fmt.Errorf("DISK attribute %q is only valid when creating a device, not updating one", k)
		}
		return fmt.Errorf("attribute %q is not allowed when updating a %s device", k, dtype)
	}
	return nil
}

type deviceUpdatePlanState struct {
	attrPayload  map[string]interface{}
	attrPreview  map[string]interface{}
	attrBind     map[string]interface{}
	extraBind    map[string]interface{}
	blocked      []string
	warnings     []string
	summaryParts []string
	changed      bool
}

func (s *deviceUpdatePlanState) block(format string, a ...interface{}) {
	s.blocked = append(s.blocked, fmt.Sprintf(format, a...))
}

func (s *deviceUpdatePlanState) warn(format string, a ...interface{}) {
	s.warnings = append(s.warnings, fmt.Sprintf(format, a...))
}

func planUpdateVMDevice(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	spec, err := parseUpdateVMDeviceArgs(args)
	if err != nil {
		return nil, err
	}
	dctx, err := lookupVMDeviceContext(ctx, client, spec.DeviceID)
	if err != nil {
		return nil, err
	}

	st := &deviceUpdatePlanState{
		attrPayload: map[string]interface{}{},
		attrPreview: map[string]interface{}{},
		attrBind:    map[string]interface{}{},
		extraBind:   map[string]interface{}{},
	}

	if dctx.VM.State != "STOPPED" {
		if !spec.Force {
			st.block("VM %q (id %d) is currently %s; stop it with stop_vm before updating device %d (%s), or pass force=true to update stored configuration only (changes take effect after stop and start)",
				dctx.VM.Name, dctx.VM.ID, dctx.VM.State, dctx.DeviceID, dctx.DType)
		} else {
			st.warn("VM %q (id %d) is currently %s; force=true updates stored configuration only and changes will not take effect until the VM is stopped and started",
				dctx.VM.Name, dctx.VM.ID, dctx.VM.State)
		}
	}

	if len(spec.Attributes) > 0 {
		if err := validateUpdateAttributeAllowlist(dctx.DType, spec.Attributes); err != nil {
			return nil, err
		}
		switch dctx.DType {
		case "DISK":
			err = planUpdateDiskAttrs(ctx, client, dctx, spec, st)
		case "CDROM":
			err = planUpdateCDROMAttrs(ctx, client, dctx, spec, st)
		case "NIC":
			err = planUpdateNICAttrs(ctx, client, dctx, spec, st)
		case "DISPLAY":
			err = planUpdateDisplayAttrs(ctx, client, dctx, spec, st)
		}
		if err != nil {
			return nil, err
		}
	}

	payload := map[string]interface{}{}
	previewChanges := map[string]interface{}{}
	bindPayload := map[string]interface{}{}

	if spec.HasOrder {
		payload["order"] = spec.Order
		previewChanges["order"] = map[string]interface{}{"from": dctx.Order, "to": spec.Order}
		bindPayload["order"] = spec.Order
		if spec.Order != dctx.Order {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("order %d -> %d", dctx.Order, spec.Order))
			for _, sib := range dctx.Siblings {
				if so, ok := extractZFSInt64(sib["order"]); ok && int(so) == spec.Order {
					sibID, _ := extractZFSInt64(sib["id"])
					st.warn("Sibling device %d already has order %d; TrueNAS will shift colliding sibling device orders upward", int(sibID), spec.Order)
					break
				}
			}
		}
	}

	if len(st.attrPayload) > 0 {
		payload["attributes"] = st.attrPayload
		previewChanges["attributes"] = st.attrPreview
		bindPayload["attributes"] = st.attrBind
	}

	if !st.changed {
		return nil, fmt.Errorf("no changes requested for device %d (%s): supplied values match current configuration", dctx.DeviceID, dctx.DType)
	}

	sanitizedDevice := sanitizeCreatedDevice(dctx.DeviceRow)
	currentState := map[string]interface{}{
		"device": sanitizedDevice,
		"vm":     dctx.VM.currentState(),
	}

	actionEntry := map[string]interface{}{
		"device_id": dctx.DeviceID,
		"dtype":     dctx.DType,
		"vm_id":     dctx.VM.ID,
		"vm_name":   dctx.VM.Name,
		"changes":   previewChanges,
	}

	bind := map[string]interface{}{
		"device":         sanitizedDevice,
		"vm_id":          dctx.VM.ID,
		"vm_name":        dctx.VM.Name,
		"vm_uuid":        dctx.VM.UUID,
		"vm_status":      dctx.VM.statusBind(),
		"sibling_orders": bindSiblingOrders(dctx.Siblings),
		"force":          spec.Force,
		"payload":        bindPayload,
	}
	for k, v := range st.extraBind {
		bind[k] = v
	}

	headline := fmt.Sprintf("Update %s device %d on VM %q (id %d)", dctx.DType, dctx.DeviceID, dctx.VM.Name, dctx.VM.ID)
	if dctx.VM.State != "STOPPED" && spec.Force {
		headline += fmt.Sprintf(" (VM is %s, force=true)", dctx.VM.State)
	}
	facts := []string{
		confirmFact("vm", fmt.Sprintf("%s (id %d, state %s)", dctx.VM.Name, dctx.VM.ID, dctx.VM.State)),
		confirmFact("device", fmt.Sprintf("%d (%s)", dctx.DeviceID, dctx.DType)),
		confirmFact("changes", strings.Join(st.summaryParts, ", ")),
		confirmBoolFact("force", spec.Force),
	}

	return &writePlan{
		Tool:         "update_vm_device",
		Method:       vmDeviceUpdateMethod,
		CallParams:   []interface{}{dctx.DeviceID, payload},
		ParamNames:   []string{"id", "vm_device_update"},
		Description:  headline,
		Operation:    "update",
		CurrentState: currentState,
		Actions: []PlannedAction{{
			Step:        1,
			Description: headline,
			Operation:   "update",
			Target:      fmt.Sprintf("%s device %d", dctx.DType, dctx.DeviceID),
			Details:     actionEntry,
		}},
		Warnings:       st.warnings,
		Blocked:        st.blocked,
		TargetName:     dctx.VM.Name,
		Bind:           bind,
		ConfirmSummary: newConfirmSummary(headline, facts...),
		Result: map[string]interface{}{
			"device_id": dctx.DeviceID,
			"dtype":     dctx.DType,
			"vm_id":     dctx.VM.ID,
			"vm_name":   dctx.VM.Name,
		},
	}, nil
}

func planUpdateDiskAttrs(ctx context.Context, client *truenas.Client, dctx *vmDeviceContext, spec *vmDeviceUpdateSpec, st *deviceUpdatePlanState) error {
	existing := dctx.DeviceAttr
	attrs := spec.Attributes

	if rawType, ok := attrs["type"]; ok {
		dt, ok := rawType.(string)
		if !ok || (dt != "AHCI" && dt != "VIRTIO") {
			return fmt.Errorf("DISK attributes.type must be AHCI or VIRTIO")
		}
		st.attrPayload["type"] = dt
		st.attrPreview["type"] = dt
		st.attrBind["type"] = dt
		if cur, _ := existing["type"].(string); cur != dt {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("type=%s", dt))
		}
	}

	if rawIO, ok := attrs["iotype"]; ok {
		ioType, ok := rawIO.(string)
		if !ok || (ioType != "NATIVE" && ioType != "THREADS" && ioType != "IO_URING") {
			return fmt.Errorf("DISK attributes.iotype must be NATIVE, THREADS, or IO_URING")
		}
		st.attrPayload["iotype"] = ioType
		st.attrPreview["iotype"] = ioType
		st.attrBind["iotype"] = ioType
		if cur, _ := existing["iotype"].(string); cur != ioType {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("iotype=%s", ioType))
		}
	}

	effLSS64, _ := extractZFSInt64(existing["logical_sectorsize"])
	effPSS64, _ := extractZFSInt64(existing["physical_sectorsize"])
	effLSS, effPSS := int(effLSS64), int(effPSS64)

	if lss, hasLSS, err := vmIntArg(attrs, "logical_sectorsize", 0); err != nil || (hasLSS && lss != 512 && lss != 4096) {
		return fmt.Errorf("DISK attributes.logical_sectorsize must be 512 or 4096")
	} else if hasLSS {
		st.attrPayload["logical_sectorsize"] = lss
		st.attrPreview["logical_sectorsize"] = lss
		st.attrBind["logical_sectorsize"] = lss
		if lss != effLSS {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("logical_sectorsize=%d", lss))
		}
		effLSS = lss
	}

	if pss, hasPSS, err := vmIntArg(attrs, "physical_sectorsize", 0); err != nil || (hasPSS && pss != 512 && pss != 4096) {
		return fmt.Errorf("DISK attributes.physical_sectorsize must be 512 or 4096")
	} else if hasPSS {
		st.attrPayload["physical_sectorsize"] = pss
		st.attrPreview["physical_sectorsize"] = pss
		st.attrBind["physical_sectorsize"] = pss
		if pss != effPSS {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("physical_sectorsize=%d", pss))
		}
		effPSS = pss
	}

	if effLSS > 0 && effPSS > 0 && effLSS > effPSS {
		st.block("DISK logical_sectorsize (%d) cannot exceed physical_sectorsize (%d)", effLSS, effPSS)
	}
	if effPSS > 0 && effLSS == 0 {
		limits, err := fetchVMHostLimits(ctx, client)
		if err != nil {
			return err
		}
		st.extraBind["host_version"] = limits.Version.String()
		if isVMPre27(limits.Version) {
			st.block("DISK physical_sectorsize requires logical_sectorsize on TrueNAS %s", vmVersionLabel(limits.Version))
		}
	}

	if rawPath, ok := attrs["path"]; ok {
		p, ok := rawPath.(string)
		if !ok || p == "" {
			return fmt.Errorf("DISK attributes.path must be a non-empty /dev/zvol/... path")
		}
		zvolName, ok := zvolPathToName(p)
		if !ok {
			return fmt.Errorf("DISK attributes.path %q must start with /dev/zvol/", p)
		}
		if err := validateZvolName(zvolName); err != nil {
			return fmt.Errorf("DISK attributes.path: %w", err)
		}
		curPath, _ := existing["path"].(string)
		if refs := dctx.OtherZvolRefs[zvolName]; len(refs) > 0 {
			st.block("DISK zvol %s is already attached to %s", zvolName, formatDeviceRefs(refs))
		}
		if p != curPath && dctx.VM.State != "STOPPED" {
			// The middleware accepts this update on an active VM, but the
			// guest keeps the old zvol open while the device table no
			// longer shows it attached, so another VM could attach it.
			st.block("DISK path changes need VM %q (id %d) to be STOPPED (it is %s), even with force=true: the running guest keeps %s open while it would no longer be listed as attached",
				dctx.VM.Name, dctx.VM.ID, dctx.VM.State, curPath)
		}
		if p != curPath {
			choices, err := fetchVMDiskChoices(ctx, client)
			if err != nil {
				return err
			}
			if _, inChoices := choices[p]; !inChoices {
				st.block("DISK path %s is not in vm.device.disk_choices (it may not exist or may already be attached)", p)
			}
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("path=%s", p))
		}
		dsRow, err := queryDatasetRow(ctx, client, zvolName)
		if err != nil {
			return err
		}
		if dsRow == nil {
			st.block("DISK zvol %s was not found with pool.dataset.query", zvolName)
		} else {
			if dt, _ := dsRow["type"].(string); dt != "" && dt != "VOLUME" {
				st.block("DISK %s is a %s, not a VOLUME (zvol)", zvolName, dt)
			}
			if dsRow["locked"] != false {
				st.block("DISK zvol %s is locked, or its lock state could not be read", zvolName)
			}
			if ro, ok := extractZFSBool(dsRow["readonly"]); ok && ro {
				st.block("DISK zvol %s is read-only", zvolName)
			}
			if vs, ok := extractZFSInt64(dsRow["volsize"]); ok && vs > 0 {
				st.attrPreview["volsize"] = vs
				st.attrPreview["volsize_human"] = formatBytes(vs)
			}
		}
		st.attrPayload["path"] = p
		st.attrPreview["path"] = p
		st.attrBind["path"] = p
		st.extraBind["disk_dataset"] = bindDatasetRow(dsRow)
	}

	return nil
}

func planUpdateCDROMAttrs(ctx context.Context, client *truenas.Client, dctx *vmDeviceContext, spec *vmDeviceUpdateSpec, st *deviceUpdatePlanState) error {
	rawPath, ok := spec.Attributes["path"]
	if !ok {
		return nil
	}
	p, ok := rawPath.(string)
	if !ok || p == "" {
		return fmt.Errorf("CDROM attributes.path must be a non-empty ISO path under /mnt/<pool>/...; TrueNAS does not support an empty CDROM tray — to eject an installer CDROM, use delete_vm_device (the ISO file on disk is kept)")
	}
	if strings.ContainsAny(p, "{}") {
		return fmt.Errorf("CDROM attributes.path must not contain '{' or '}'")
	}
	cleanPath, err := validateACLPath(p)
	if err != nil {
		return fmt.Errorf("CDROM attributes.path: %w", err)
	}
	if reason := uploadTargetRefusal(cleanPath); reason != "" {
		return fmt.Errorf("CDROM attributes.path: %s", reason)
	}

	for _, sib := range dctx.Siblings {
		sa, _ := sib["attributes"].(map[string]interface{})
		if dtype, _ := sa["dtype"].(string); dtype == "CDROM" {
			if sp, _ := sa["path"].(string); sp == cleanPath {
				sibID, _ := extractZFSInt64(sib["id"])
				st.block("CDROM path %s is already attached to sibling CDROM device %d on VM %q", cleanPath, int(sibID), dctx.VM.Name)
			}
		}
	}

	chk, err := checkCDROMPath(ctx, client, cleanPath, fmt.Sprintf("device %d (CDROM)", dctx.DeviceID))
	if err != nil {
		return err
	}
	st.blocked = append(st.blocked, chk.Blocked...)
	st.attrPayload["path"] = cleanPath
	st.attrPreview["path"] = cleanPath
	st.attrPreview["dataset"] = chk.Dataset
	if chk.HasSize {
		st.attrPreview["size"] = chk.Size
		st.attrPreview["size_human"] = formatBytes(chk.Size)
	}
	st.attrBind["path"] = cleanPath
	st.extraBind["cdrom"] = chk.Bind

	if cur, _ := dctx.DeviceAttr["path"].(string); cur != cleanPath {
		st.changed = true
		st.summaryParts = append(st.summaryParts, fmt.Sprintf("path=%s", cleanPath))
	}
	return nil
}

func planUpdateNICAttrs(ctx context.Context, client *truenas.Client, dctx *vmDeviceContext, spec *vmDeviceUpdateSpec, st *deviceUpdatePlanState) error {
	existing := dctx.DeviceAttr
	attrs := spec.Attributes

	effType, _ := existing["type"].(string)
	if effType == "" {
		effType = "VIRTIO"
	}
	if rawType, ok := attrs["type"]; ok {
		nt, ok := rawType.(string)
		if !ok || (nt != "E1000" && nt != "VIRTIO") {
			return fmt.Errorf("NIC attributes.type must be E1000 or VIRTIO")
		}
		st.attrPayload["type"] = nt
		st.attrPreview["type"] = nt
		st.attrBind["type"] = nt
		if nt != effType {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("type=%s", nt))
		}
		effType = nt
	}

	if rawMAC, ok := attrs["mac"]; ok {
		mac, ok := rawMAC.(string)
		if !ok || hasLineBreak(mac) {
			return fmt.Errorf("NIC attributes.mac must be a single-line string")
		}
		if mac != "" {
			if err := validateMACAddress(mac); err != nil {
				return fmt.Errorf("NIC attributes.mac: %w", err)
			}
			for _, sib := range dctx.Siblings {
				sa, _ := sib["attributes"].(map[string]interface{})
				if dtype, _ := sa["dtype"].(string); dtype == "NIC" {
					if sm, _ := sa["mac"].(string); strings.EqualFold(sm, mac) {
						sibID, _ := extractZFSInt64(sib["id"])
						st.block("NIC MAC address %s is already used by sibling NIC device %d on VM %q", mac, int(sibID), dctx.VM.Name)
					}
				}
			}
		}
		st.attrPayload["mac"] = mac
		st.attrPreview["mac"] = mac
		st.attrBind["mac"] = mac
		if cur, _ := existing["mac"].(string); !strings.EqualFold(cur, mac) {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("mac=%s", mac))
		}
	}

	effAttach, _ := existing["nic_attach"].(string)
	if rawAttach, ok := attrs["nic_attach"]; ok {
		na, ok := rawAttach.(string)
		if !ok || hasLineBreak(na) {
			return fmt.Errorf("NIC attributes.nic_attach must be a single-line interface name")
		}
		st.attrPayload["nic_attach"] = na
		st.attrPreview["nic_attach"] = na
		st.attrBind["nic_attach"] = na
		if na != effAttach {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("nic_attach=%s", na))
		}
		effAttach = na
	}

	effTrust, _ := existing["trust_guest_rx_filters"].(bool)
	if rawTrust, ok := attrs["trust_guest_rx_filters"]; ok {
		tr, ok := rawTrust.(bool)
		if !ok {
			return fmt.Errorf("NIC attributes.trust_guest_rx_filters must be a boolean")
		}
		st.attrPayload["trust_guest_rx_filters"] = tr
		st.attrPreview["trust_guest_rx_filters"] = tr
		st.attrBind["trust_guest_rx_filters"] = tr
		if tr != effTrust {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("trust_guest_rx_filters=%v", tr))
		}
		effTrust = tr
	}

	var attachMode string
	if effAttach != "" {
		choices, err := fetchVMNicAttachChoices(ctx, client)
		if err != nil {
			return err
		}
		st.extraBind["nic_choices"] = choices.bind()
		mode, ok := choices.lookup(effAttach)
		if !ok {
			all := append(append([]string(nil), choices.Bridges...), choices.Macvlans...)
			sort.Strings(all)
			st.block("NIC nic_attach %q is not in vm.device.nic_attach_choices (available: %s)", effAttach, strings.Join(all, ", "))
		} else {
			attachMode = mode
			st.attrPreview["attach_mode"] = mode
			if mode != "bridge" {
				st.warn("NIC attaches to %s via macvtap (not a bridge); the VM will not be able to communicate with the TrueNAS host over that interface", effAttach)
			}
		}
	}

	if effTrust {
		if effType != "VIRTIO" {
			st.block("NIC trust_guest_rx_filters=true requires NIC type VIRTIO (effective type is %s)", effType)
		}
		if attachMode == "bridge" {
			st.block("NIC trust_guest_rx_filters=true is not supported when attached to bridge %q", effAttach)
		}
	}

	return nil
}

// displayUpdate is the effective DISPLAY configuration while an update is
// planned: the stored values with the requested ones applied.
type displayUpdate struct {
	existing map[string]interface{}
	attrs    map[string]interface{}
	version  truenas.Version
	pre27    bool
	curType  string
	effType  string
}

func planUpdateDisplayAttrs(ctx context.Context, client *truenas.Client, dctx *vmDeviceContext, spec *vmDeviceUpdateSpec, st *deviceUpdatePlanState) error {
	limits, err := fetchVMHostLimits(ctx, client)
	if err != nil {
		return err
	}
	st.extraBind["host_version"] = limits.Version.String()
	d := &displayUpdate{
		existing: dctx.DeviceAttr,
		attrs:    spec.Attributes,
		version:  limits.Version,
		pre27:    isVMPre27(limits.Version),
	}
	if err := d.planTypeAndResolution(dctx, st); err != nil {
		return err
	}
	if err := d.planBind(ctx, client, spec, st); err != nil {
		return err
	}
	if err := d.planWebAndWait(st); err != nil {
		return err
	}
	if err := d.planPassword(st); err != nil {
		return err
	}
	return d.planPorts(ctx, client, dctx, st)
}

// planTypeAndResolution applies type (at most one display of each type per
// VM) and resolution.
func (d *displayUpdate) planTypeAndResolution(dctx *vmDeviceContext, st *deviceUpdatePlanState) error {
	existing, attrs := d.existing, d.attrs
	curType, _ := existing["type"].(string)
	if curType == "" {
		curType = "SPICE"
	}
	d.curType, d.effType = curType, curType
	if rawType, ok := attrs["type"]; ok {
		dt, ok := rawType.(string)
		if !ok || (dt != "SPICE" && dt != "VNC") {
			return fmt.Errorf("DISPLAY attributes.type must be SPICE or VNC")
		}
		if dt != curType {
			for _, sib := range dctx.Siblings {
				sa, _ := sib["attributes"].(map[string]interface{})
				if sd, _ := sa["dtype"].(string); sd == "DISPLAY" {
					if stype, _ := sa["type"].(string); stype == dt {
						sibID, _ := extractZFSInt64(sib["id"])
						st.block("VM %q already has a %s DISPLAY device (id %d); TrueNAS allows at most one %s display per VM", dctx.VM.Name, dt, int(sibID), dt)
					}
				}
			}
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("type=%s", dt))
		}
		st.attrPayload["type"] = dt
		st.attrPreview["type"] = dt
		st.attrBind["type"] = dt
		d.effType = dt
	}

	if rawRes, ok := attrs["resolution"]; ok {
		res, ok := rawRes.(string)
		if !ok || !slices.Contains(vmDisplayResolutions, res) {
			return fmt.Errorf("DISPLAY attributes.resolution %q is not a supported resolution", fmt.Sprintf("%v", rawRes))
		}
		st.attrPayload["resolution"] = res
		st.attrPreview["resolution"] = res
		st.attrBind["resolution"] = res
		if cur, _ := existing["resolution"].(string); cur != res {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("resolution=%s", res))
		}
	}

	return nil
}

// planBind applies bind: a wildcard needs allow_wildcard_display_bind, and
// the address must be one of vm.device.bind_choices.
func (d *displayUpdate) planBind(ctx context.Context, client *truenas.Client, spec *vmDeviceUpdateSpec, st *deviceUpdatePlanState) error {
	existing, attrs := d.existing, d.attrs
	effBind, _ := existing["bind"].(string)
	if effBind == "" {
		effBind = "127.0.0.1"
	}
	bindChangedExplicit := false
	if rawBind, ok := attrs["bind"]; ok {
		b, ok := rawBind.(string)
		if !ok || b == "" || hasLineBreak(b) {
			return fmt.Errorf("DISPLAY attributes.bind must be a non-empty single-line address")
		}
		bindChangedExplicit = true
		st.attrPayload["bind"] = b
		st.attrPreview["bind"] = b
		st.attrBind["bind"] = b
		if b != effBind {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("bind=%s", b))
		}
		effBind = b
	}
	if bindChangedExplicit && (effBind == "0.0.0.0" || effBind == "::") {
		if !spec.AllowWildcardDisplayBind {
			st.block("DISPLAY bind %q exposes the VM console on all host interfaces; bind to 127.0.0.1 or set allow_wildcard_display_bind=true", effBind)
		} else {
			st.warn("DISPLAY (%s) binds to %s, exposing the VM console on all host interfaces", d.effType, effBind)
		}
	}

	bindChoices, err := fetchVMBindChoices(ctx, client)
	if err != nil {
		return err
	}
	st.extraBind["bind_choices"] = bindChoices
	if _, ok := bindChoices[effBind]; !ok {
		st.block("DISPLAY bind address %q is not in vm.device.bind_choices", effBind)
	}

	return nil
}

// planWebAndWait applies web and wait; before 27.0, VNC cannot have web.
func (d *displayUpdate) planWebAndWait(st *deviceUpdatePlanState) error {
	existing, attrs := d.existing, d.attrs
	effWeb := true
	if curWeb, ok := existing["web"].(bool); ok {
		effWeb = curWeb
	}
	if rawWeb, ok := attrs["web"]; ok {
		w, ok := rawWeb.(bool)
		if !ok {
			return fmt.Errorf("DISPLAY attributes.web must be a boolean")
		}
		st.attrPayload["web"] = w
		st.attrPreview["web"] = w
		st.attrBind["web"] = w
		if w != effWeb {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("web=%v", w))
		}
		effWeb = w
	}

	if rawWait, ok := attrs["wait"]; ok {
		w, ok := rawWait.(bool)
		if !ok {
			return fmt.Errorf("DISPLAY attributes.wait must be a boolean")
		}
		st.attrPayload["wait"] = w
		st.attrPreview["wait"] = w
		st.attrBind["wait"] = w
		if curWait, _ := existing["wait"].(bool); curWait != w {
			st.changed = true
			st.summaryParts = append(st.summaryParts, fmt.Sprintf("wait=%v", w))
		}
	}

	if d.effType == "VNC" && effWeb && d.pre27 {
		st.block("DISPLAY VNC does not support web=true on TrueNAS %s; set web=false or use type=SPICE", vmVersionLabel(d.version))
	}

	return nil
}

// planPassword applies password; before 27.0 a display needs a non-empty
// one. The password itself is bound only as a hash.
func (d *displayUpdate) planPassword(st *deviceUpdatePlanState) error {
	existing, attrs := d.existing, d.attrs
	effType, curType, pre27 := d.effType, d.curType, d.pre27
	existingPw, _ := existing["password"].(string)
	if rawPw, hasPw := attrs["password"]; hasPw {
		pw, ok := rawPw.(string)
		if !ok || hasLineBreak(pw) {
			return fmt.Errorf("DISPLAY attributes.password must be a single-line string")
		}
		if pw != "" && strings.TrimSpace(pw) == "" {
			return fmt.Errorf("DISPLAY attributes.password must not be blank")
		}
		if effType == "VNC" && len(pw) > vmVNCPasswordMaxLen {
			return fmt.Errorf("DISPLAY VNC password must be at most %d characters", vmVNCPasswordMaxLen)
		}
		if pw == "" && pre27 {
			st.block("TrueNAS %s requires a non-empty display password", vmVersionLabel(d.version))
		}
		if pw == "" && !pre27 {
			st.warn("DISPLAY (%s) password will be cleared", effType)
		}
		st.attrPayload["password"] = pw
		if pw != "" {
			st.attrPreview["password"] = "<redacted>"
			st.attrBind["password_sha256"] = vmSecretHash(pw)
			st.summaryParts = append(st.summaryParts, "password=<updated>")
		} else {
			st.attrPreview["password"] = ""
			st.attrBind["password_sha256"] = ""
			st.summaryParts = append(st.summaryParts, "password=<cleared>")
		}
		st.changed = true
	} else {
		if existingPw == "" && pre27 {
			st.block("TrueNAS %s requires a display password; set attributes.password", vmVersionLabel(d.version))
		}
		if curType == "SPICE" && effType == "VNC" && existingPw != "" {
			st.warn("Switching DISPLAY from SPICE to VNC without setting a new password: VNC requires passwords of at most %d characters; set attributes.password explicitly if the existing password is longer", vmVNCPasswordMaxLen)
		}
	}

	return nil
}

// planPorts applies port and web_port: in range, distinct, and not used by
// another VM's display or reserved by TrueNAS.
func (d *displayUpdate) planPorts(ctx context.Context, client *truenas.Client, dctx *vmDeviceContext, st *deviceUpdatePlanState) error {
	existing, attrs := d.existing, d.attrs
	effPort64, _ := extractZFSInt64(existing["port"])
	effWebPort64, _ := extractZFSInt64(existing["web_port"])
	effPort, effWebPort := int(effPort64), int(effWebPort64)

	for _, key := range []string{"port", "web_port"} {
		port, has, err := vmIntArg(attrs, key, 0)
		if err != nil || (has && (port < vmDisplayPortMin || port > vmDisplayPortMax)) {
			return fmt.Errorf("DISPLAY attributes.%s must be between %d and %d", key, vmDisplayPortMin, vmDisplayPortMax)
		}
		if !has {
			continue
		}
		st.attrPayload[key] = port
		st.attrPreview[key] = port
		st.attrBind[key] = port
		if key == "port" {
			if port != effPort {
				st.changed = true
				st.summaryParts = append(st.summaryParts, fmt.Sprintf("port=%d", port))
			}
			effPort = port
		} else {
			if port != effWebPort {
				st.changed = true
				st.summaryParts = append(st.summaryParts, fmt.Sprintf("web_port=%d", port))
			}
			effWebPort = port
		}
	}

	if effPort > 0 && effWebPort > 0 && effPort == effWebPort {
		st.block("DISPLAY port and web_port cannot both be %d", effPort)
	}
	if effPort > 0 || effWebPort > 0 {
		inUse, err := fetchVMDisplayPortsInUseExcluding(ctx, client, dctx.DeviceID)
		if err != nil {
			return err
		}
		st.extraBind["display_ports_in_use"] = bindDisplayPorts(inUse)
		for _, pair := range []struct {
			key  string
			port int
		}{{"port", effPort}, {"web_port", effWebPort}} {
			if pair.port <= 0 {
				continue
			}
			owner, taken := inUse[pair.port]
			if !taken {
				continue
			}
			if owner == 0 {
				st.block("DISPLAY %s %d is reserved by TrueNAS", pair.key, pair.port)
			} else {
				st.block("DISPLAY %s %d is already in use by VM id %d", pair.key, pair.port, owner)
			}
		}
	}

	return nil
}

func (r *Registry) handleUpdateVMDevice(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	return handlePlanned(ctx, client, args, planUpdateVMDevice, r.executeUpdateVMDevice)
}

func (r *Registry) executeUpdateVMDevice(ctx context.Context, client *truenas.Client, _ map[string]interface{}, plan *writePlan) (string, error) {
	raw, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to update VM device: %w", err)
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(raw, &updated); err != nil {
		return "", fmt.Errorf("failed to parse vm.device.update response: %w", err)
	}
	deviceID, _ := plan.Result["device_id"].(int)
	dtype, _ := plan.Result["dtype"].(string)
	vmID, _ := plan.Result["vm_id"].(int)
	vmName, _ := plan.Result["vm_name"].(string)

	result := map[string]interface{}{
		"device_id": deviceID,
		"vm_id":     vmID,
		"vm_name":   vmName,
		"dtype":     dtype,
		"device":    sanitizeCreatedDevice(updated),
		"message":   fmt.Sprintf("Updated %s device %d on VM %q (id %d).", dtype, deviceID, vmName, vmID),
	}
	if len(plan.Warnings) > 0 {
		result["warnings"] = plan.Warnings
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// --- delete_vm_device ---

func planDeleteVMDevice(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	devID, hasID, err := vmIntArg(args, "device_id", 0)
	if err != nil || !hasID || devID < 1 {
		return nil, fmt.Errorf("device_id must be a positive integer")
	}
	deleteZvol, _ := args["zvol"].(bool)
	deleteRawFile, _ := args["raw_file"].(bool)
	force, _ := args["force"].(bool)
	if deleteZvol && deleteRawFile {
		return nil, fmt.Errorf("zvol and raw_file cannot both be true")
	}

	dctx, err := lookupVMDeviceContext(ctx, client, devID)
	if err != nil {
		return nil, err
	}

	var blocked, warnings []string
	extraBind := map[string]interface{}{}
	action := map[string]interface{}{
		"device_id": dctx.DeviceID,
		"dtype":     dctx.DType,
		"vm_id":     dctx.VM.ID,
		"vm_name":   dctx.VM.Name,
		"zvol":      deleteZvol,
		"raw_file":  deleteRawFile,
		"force":     force,
	}

	switch {
	case dctx.VM.State == "RUNNING" || dctx.VM.State == "SUSPENDED":
		blocked = append(blocked, fmt.Sprintf(
			"VM %q (id %d) is currently %s; TrueNAS refuses vm.device.delete on active VMs even with force=true — stop the VM first with stop_vm",
			dctx.VM.Name, dctx.VM.ID, dctx.VM.State))
	case dctx.VM.State != "STOPPED":
		if !force {
			blocked = append(blocked, fmt.Sprintf(
				"VM %q (id %d) is in state %s; pass force=true to allow removing device %d (%s) while ignoring backing storage cleanup errors",
				dctx.VM.Name, dctx.VM.ID, dctx.VM.State, dctx.DeviceID, dctx.DType))
		} else {
			warnings = append(warnings, fmt.Sprintf(
				"VM %q (id %d) is in state %s; force=true ignores backing storage cleanup errors and removes the device record",
				dctx.VM.Name, dctx.VM.ID, dctx.VM.State))
		}
	}
	if force && dctx.VM.State == "STOPPED" {
		warnings = append(warnings, "force=true ignores errors when deleting backing storage and still removes the device record")
	}

	summary := fmt.Sprintf("Delete %s device %d from VM %q (id %d)", dctx.DType, dctx.DeviceID, dctx.VM.Name, dctx.VM.ID)

	if deleteZvol {
		zvolSummary, zBlocked, zWarnings, zBind, err := planDeleteDeviceZvol(ctx, client, dctx, action)
		if err != nil {
			return nil, err
		}
		blocked = append(blocked, zBlocked...)
		warnings = append(warnings, zWarnings...)
		for k, v := range zBind {
			extraBind[k] = v
		}
		if zvolSummary != "" {
			summary += " AND PERMANENTLY DESTROY " + zvolSummary
		}
	} else if deleteRawFile {
		rawSummary, rBlocked, rWarnings, rBind, err := planDeleteDeviceRawFile(ctx, client, dctx, action)
		if err != nil {
			return nil, err
		}
		blocked = append(blocked, rBlocked...)
		warnings = append(warnings, rWarnings...)
		for k, v := range rBind {
			extraBind[k] = v
		}
		if rawSummary != "" {
			summary += " AND PERMANENTLY DELETE " + rawSummary
		}
	} else {
		switch dctx.DType {
		case "DISK":
			if p, _ := dctx.DeviceAttr["path"].(string); p != "" {
				action["backing_zvol_kept"] = p
				if zn, ok := zvolPathToName(p); ok {
					if dsRow, qErr := queryDatasetRow(ctx, client, zn); qErr == nil && dsRow != nil {
						if vs, ok := extractZFSInt64(dsRow["volsize"]); ok && vs > 0 {
							action["zvol_size"] = vs
							action["zvol_size_human"] = formatBytes(vs)
						}
					}
				}
			}
			summary += " (backing zvol kept)"
		case "CDROM":
			if p, _ := dctx.DeviceAttr["path"].(string); p != "" {
				action["iso_file_kept"] = p
			}
			summary += " (ejects CDROM; ISO file on disk is kept)"
		case "RAW":
			if p, _ := dctx.DeviceAttr["path"].(string); p != "" {
				action["raw_file_kept"] = p
			}
			summary += " (backing raw file kept)"
		}
	}

	opts := map[string]interface{}{
		"zvol":     deleteZvol,
		"raw_file": deleteRawFile,
		"force":    force,
	}
	sanitizedDevice := sanitizeCreatedDevice(dctx.DeviceRow)
	bind := map[string]interface{}{
		"device":    sanitizedDevice,
		"vm_id":     dctx.VM.ID,
		"vm_name":   dctx.VM.Name,
		"vm_uuid":   dctx.VM.UUID,
		"vm_status": dctx.VM.statusBind(),
		"options":   opts,
	}
	for k, v := range extraBind {
		bind[k] = v
	}

	facts := []string{
		confirmFact("vm", fmt.Sprintf("%s (id %d, state %s)", dctx.VM.Name, dctx.VM.ID, dctx.VM.State)),
		confirmFact("device", fmt.Sprintf("%d (%s)", dctx.DeviceID, dctx.DType)),
		confirmBoolFact("zvol", deleteZvol),
		confirmBoolFact("raw_file", deleteRawFile),
		confirmBoolFact("force", force),
	}

	return &writePlan{
		Tool:        "delete_vm_device",
		Method:      vmDeviceDeleteMethod,
		CallParams:  []interface{}{dctx.DeviceID, opts},
		ParamNames:  []string{"id", "options"},
		Description: summary,
		Operation:   "delete",
		CurrentState: map[string]interface{}{
			"device": sanitizedDevice,
			"vm":     dctx.VM.currentState(),
		},
		Actions: []PlannedAction{{
			Step:        1,
			Description: summary,
			Operation:   "delete",
			Target:      fmt.Sprintf("%s device %d", dctx.DType, dctx.DeviceID),
			Details:     action,
		}},
		Warnings:       warnings,
		Blocked:        blocked,
		TargetName:     dctx.VM.Name,
		Bind:           bind,
		ConfirmSummary: newConfirmSummary(summary, facts...),
		Result: map[string]interface{}{
			"device_id":   dctx.DeviceID,
			"dtype":       dctx.DType,
			"vm_id":       dctx.VM.ID,
			"vm_name":     dctx.VM.Name,
			"zvol":        deleteZvol,
			"raw_file":    deleteRawFile,
			"force":       force,
			"zvol_target": action["zvol_to_delete"],
			"raw_target":  action["raw_file_to_delete"],
		},
	}, nil
}

func planDeleteDeviceZvol(ctx context.Context, client *truenas.Client, dctx *vmDeviceContext, action map[string]interface{}) (string, []string, []string, map[string]interface{}, error) {
	var blocked, warnings []string
	bind := map[string]interface{}{}

	if dctx.DType != "DISK" {
		blocked = append(blocked, fmt.Sprintf("zvol=true is only valid for DISK devices (device %d is %s)", dctx.DeviceID, dctx.DType))
		return "", blocked, warnings, bind, nil
	}
	devPath, _ := dctx.DeviceAttr["path"].(string)
	zvolName, ok := zvolPathToName(devPath)
	if !ok {
		blocked = append(blocked, fmt.Sprintf("DISK device %d path %q is not under /dev/zvol/; refusing to destroy backing storage", dctx.DeviceID, devPath))
		return "", blocked, warnings, bind, nil
	}
	if err := validateZvolName(zvolName); err != nil {
		blocked = append(blocked, fmt.Sprintf("refusing to destroy zvol %q: %v", zvolName, err))
		return "", blocked, warnings, bind, nil
	}
	if refs := dctx.OtherZvolRefs[zvolName]; len(refs) > 0 {
		blocked = append(blocked, fmt.Sprintf("cannot destroy zvol %s: it is also attached to %s", zvolName, formatDeviceRefs(refs)))
	}

	dsRow, err := queryDatasetRow(ctx, client, zvolName)
	if err != nil {
		return "", nil, nil, nil, err
	}
	bind["zvol_name"] = zvolName
	bind["zvol_dataset"] = bindDatasetRow(dsRow)
	bind["other_zvol_refs"] = formatDeviceRefs(dctx.OtherZvolRefs[zvolName])

	sizeLabel := "size unknown"
	if dsRow == nil {
		warnings = append(warnings, fmt.Sprintf("zvol %s was not found in pool.dataset.query; vm.device.delete may fail unless force=true", zvolName))
		action["zvol_to_delete"] = map[string]interface{}{
			"name":   zvolName,
			"path":   devPath,
			"exists": false,
		}
	} else {
		if dt, _ := dsRow["type"].(string); dt != "" && dt != "VOLUME" {
			blocked = append(blocked, fmt.Sprintf("dataset %s is a %s, not a VOLUME; refusing to destroy it", zvolName, dt))
		}
		info := map[string]interface{}{
			"name":   zvolName,
			"path":   devPath,
			"exists": true,
		}
		if vs, ok := extractZFSInt64(dsRow["volsize"]); ok && vs > 0 {
			info["volsize"] = vs
			info["volsize_human"] = formatBytes(vs)
			sizeLabel = formatBytes(vs)
		}
		limits, err := fetchVMHostLimits(ctx, client)
		if err != nil {
			return "", nil, nil, nil, err
		}
		consumers, err := fetchZvolConsumers(ctx, client, limits.Version)
		if err != nil {
			return "", nil, nil, nil, err
		}
		if reason := zvolConsumerRefusal(zvolName, consumers[zvolName]); reason != "" {
			blocked = append(blocked, reason)
		}
		snaps, err := zvolSnapshots(ctx, client, zvolName)
		if err != nil {
			return "", nil, nil, nil, err
		}
		// vm.device.delete destroys the zvol non-recursively on 25.10
		// (zfs.dataset.delete) and on master (destroy_impl), so ZFS refuses
		// a zvol that has snapshots; with force=true that failure is
		// ignored and only the device record is removed.
		if len(snaps) > 0 {
			blocked = append(blocked, fmt.Sprintf(
				"cannot destroy zvol %s: it has %d snapshot(s) (%s), and TrueNAS destroys a device's zvol non-recursively, so it would refuse (with force=true it would remove the device and keep the zvol); delete the snapshots first, or keep the zvol",
				zvolName, len(snaps), strings.Join(snapshotIDs(snaps), ", ")))
		}
		info["snapshot_count"] = len(snaps)
		if len(consumers[zvolName]) > 0 {
			info["used_by"] = consumers[zvolName]
		}
		action["zvol_to_delete"] = info
		warnings = append(warnings, fmt.Sprintf("PERMANENT DATA LOSS: zvol %s (%s) will be destroyed", zvolName, sizeLabel))
	}

	return fmt.Sprintf("zvol %s (%s)", zvolName, sizeLabel), blocked, warnings, bind, nil
}

func planDeleteDeviceRawFile(ctx context.Context, client *truenas.Client, dctx *vmDeviceContext, action map[string]interface{}) (string, []string, []string, map[string]interface{}, error) {
	var blocked, warnings []string
	bind := map[string]interface{}{}

	if dctx.DType != "RAW" {
		blocked = append(blocked, fmt.Sprintf("raw_file=true is only valid for RAW devices (device %d is %s)", dctx.DeviceID, dctx.DType))
		return "", blocked, warnings, bind, nil
	}
	rawPath, _ := dctx.DeviceAttr["path"].(string)
	cleanPath, err := validateACLPath(rawPath)
	if err != nil {
		blocked = append(blocked, fmt.Sprintf("refusing to delete raw file %q: %v", rawPath, err))
		return "", blocked, warnings, bind, nil
	}
	if reason := uploadTargetRefusal(cleanPath); reason != "" {
		blocked = append(blocked, fmt.Sprintf("refusing to delete raw file %s: %s", cleanPath, reason))
		return "", blocked, warnings, bind, nil
	}
	if refs := dctx.OtherRawRefs[cleanPath]; len(refs) > 0 {
		blocked = append(blocked, fmt.Sprintf("cannot delete raw file %s: it is also used by %s", cleanPath, formatDeviceRefs(refs)))
	}

	parent := path.Dir(cleanPath)
	resolvedParent, parentSt, err := resolveACLPath(ctx, client, parent)
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("raw_file parent %s cannot be read: %w", parent, err)
	}
	parentRealpath, _ := parentSt["realpath"].(string)
	if resolvedParent != parent || (parentRealpath != "" && parentRealpath != parent) {
		blocked = append(blocked, fmt.Sprintf("raw file parent directory %s goes through a symlink", parent))
	} else if pt, _ := parentSt["type"].(string); pt != "DIRECTORY" {
		blocked = append(blocked, fmt.Sprintf("raw file parent %s is a %v, not a directory", parent, parentSt["type"]))
	}

	fs, err := readACLPathFS(ctx, client, parent)
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("raw_file filesystem check failed: %w", err)
	}
	pool, _, _ := strings.Cut(strings.TrimPrefix(cleanPath, "/mnt/"), "/")
	dsPool, _, _ := strings.Cut(fs.Source, "/")
	switch {
	case fs.FSType != "zfs":
		blocked = append(blocked, fmt.Sprintf("raw file parent %s is not on a ZFS dataset (filesystem type %q)", parent, fs.FSType))
	case fs.Source == "" || dsPool != pool:
		blocked = append(blocked, fmt.Sprintf("raw file parent %s is on %q, not on a dataset of pool '%s'", parent, fs.Source, pool))
	case fs.Source == pool:
		blocked = append(blocked, fmt.Sprintf("raw file parent %s is on the root dataset of pool '%s'; refusing to delete", parent, pool))
	}
	if part, ok := managedDatasetPart(strings.ToLower(fs.Source)); ok {
		blocked = append(blocked, fmt.Sprintf("raw file %s is on dataset %s, inside '%s', which TrueNAS manages itself", cleanPath, fs.Source, part))
	}

	ds, _, err := datasetAndChildren(ctx, client, fs.Source)
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("raw_file dataset check failed: %w", err)
	}
	if ds == nil {
		blocked = append(blocked, fmt.Sprintf("dataset %s of %s was not found with pool.dataset.query", fs.Source, parent))
	} else if ds["locked"] != false {
		blocked = append(blocked, fmt.Sprintf("dataset %s is locked, or its lock state could not be read", fs.Source))
	}

	sizeLabel := "size unknown"
	fileInfo := map[string]interface{}{"path": cleanPath, "dataset": fs.Source}
	fileSt, statErr := statACLPath(ctx, client, cleanPath)
	if statErr != nil {
		if !isENOENT(statErr) {
			return "", nil, nil, nil, fmt.Errorf("cannot stat raw file %s: %w", cleanPath, statErr)
		}
		fileInfo["exists"] = false
		warnings = append(warnings, fmt.Sprintf("raw file %s does not exist on disk", cleanPath))
	} else {
		fileInfo["exists"] = true
		ft, _ := fileSt["type"].(string)
		switch {
		case ft == "SYMLINK":
			blocked = append(blocked, fmt.Sprintf("raw file %s is a symlink; refusing to delete", cleanPath))
		case ft != "FILE":
			blocked = append(blocked, fmt.Sprintf("raw file %s is a %v, not a regular file", cleanPath, fileSt["type"]))
		case fileSt["realpath"] != cleanPath:
			blocked = append(blocked, fmt.Sprintf("raw file %s resolves to %v; refusing to delete", cleanPath, fileSt["realpath"]))
		}
		if sz, ok := fileSt["size"].(float64); ok && sz >= 0 {
			fileInfo["size"] = int64(sz)
			fileInfo["size_human"] = formatBytes(int64(sz))
			sizeLabel = formatBytes(int64(sz))
		}
		bind["raw_file_stat"] = map[string]interface{}{
			"realpath": fileSt["realpath"], "type": fileSt["type"],
			"inode": fileSt["inode"], "dev": fileSt["dev"], "size": fileSt["size"], "mtime": fileSt["mtime"],
		}
		warnings = append(warnings, fmt.Sprintf("PERMANENT DATA LOSS: raw image file %s (%s) will be deleted", cleanPath, sizeLabel))
	}

	action["raw_file_to_delete"] = fileInfo
	bind["raw_file_fs"] = fs.bind()
	bind["raw_file_dataset"] = ds
	bind["other_raw_refs"] = formatDeviceRefs(dctx.OtherRawRefs[cleanPath])

	return fmt.Sprintf("raw file %s (%s)", cleanPath, sizeLabel), blocked, warnings, bind, nil
}

func (r *Registry) handleDeleteVMDevice(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	return handlePlanned(ctx, client, args, planDeleteVMDevice, r.executeDeleteVMDevice)
}

// deviceStillExists reports whether VM device id is still listed. ok is
// false when that could not be read.
func deviceStillExists(ctx context.Context, client *truenas.Client, id int) (exists, ok bool) {
	raw, err := client.Call(ctx, "vm.device.query", []interface{}{[]interface{}{"id", "=", id}})
	if err != nil {
		return false, false
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return false, false
	}
	for _, row := range rows {
		if rid, _ := extractZFSInt64(row["id"]); int(rid) == id {
			return true, true
		}
	}
	return false, true
}

// backingStorageOutcome reads back whether the storage a delete was asked
// to remove is gone: "destroyed", "still_present", "absent" (it did not
// exist at preview time) or "unverified".
func backingStorageOutcome(ctx context.Context, client *truenas.Client, target interface{}, isZvol bool) (string, string) {
	info, _ := target.(map[string]interface{})
	if info == nil {
		return "", ""
	}
	if existed, _ := info["exists"].(bool); !existed {
		name, _ := info["name"].(string)
		if !isZvol {
			name, _ = info["path"].(string)
		}
		return name, "absent"
	}
	if isZvol {
		name, _ := info["name"].(string)
		present, ok := zvolStillPresent(ctx, client, name)
		switch {
		case !ok:
			return name, "unverified"
		case present:
			return name, "still_present"
		}
		return name, "destroyed"
	}
	p, _ := info["path"].(string)
	if _, err := statACLPath(ctx, client, p); err != nil {
		if isENOENT(err) {
			return p, "destroyed"
		}
		return p, "unverified"
	}
	return p, "still_present"
}

func (r *Registry) executeDeleteVMDevice(ctx context.Context, client *truenas.Client, _ map[string]interface{}, plan *writePlan) (string, error) {
	if _, err := callPlan(ctx, client, plan); err != nil {
		return "", fmt.Errorf("failed to delete VM device: %w", err)
	}
	deviceID, _ := plan.Result["device_id"].(int)
	dtype, _ := plan.Result["dtype"].(string)
	vmID, _ := plan.Result["vm_id"].(int)
	vmName, _ := plan.Result["vm_name"].(string)
	deleteZvol, _ := plan.Result["zvol"].(bool)
	deleteRawFile, _ := plan.Result["raw_file"].(bool)
	force, _ := plan.Result["force"].(bool)

	result := map[string]interface{}{
		"device_id":        deviceID,
		"vm_id":            vmID,
		"vm_name":          vmName,
		"dtype":            dtype,
		"deleted":          true,
		"zvol_deleted":     false,
		"raw_file_deleted": false,
		"force":            force,
	}

	msg := fmt.Sprintf("Deleted %s device %d from VM %q (id %d).", dtype, deviceID, vmName, vmID)
	if dtype == "CDROM" && !deleteZvol && !deleteRawFile {
		msg = fmt.Sprintf("Ejected and removed CDROM device %d from VM %q (id %d); ISO file on disk was kept.", deviceID, vmName, vmID)
	}

	// With force=true, vm.device.delete ignores a failed zvol destroy or
	// unlink and still removes the record, so what is gone is read back.
	// The reads run even if the request was cancelled.
	if force || deleteZvol || deleteRawFile {
		cctx, cancel := vmCleanupContext(ctx)
		defer cancel()
		exists, ok := deviceStillExists(cctx, client, deviceID)
		switch {
		case !ok:
			result["deleted"] = false
			result["device_unverified"] = true
			msg = fmt.Sprintf("vm.device.delete for %s device %d on VM %q (id %d) returned, but whether the device is gone could not be read back.", dtype, deviceID, vmName, vmID)
		case exists:
			result["deleted"] = false
			result["device_still_exists"] = true
			msg = fmt.Sprintf("vm.device.delete for %s device %d on VM %q (id %d) returned, but the device still exists.", dtype, deviceID, vmName, vmID)
		}
		kind, key, target := "", "", interface{}(nil)
		switch {
		case deleteZvol:
			kind, key, target = "zvol", "zvol_deleted", plan.Result["zvol_target"]
		case deleteRawFile:
			kind, key, target = "raw file", "raw_file_deleted", plan.Result["raw_target"]
		}
		if kind != "" {
			name, outcome := backingStorageOutcome(cctx, client, target, deleteZvol)
			result[key] = outcome == "destroyed"
			if outcome != "" {
				result["backing_storage"] = map[string]interface{}{"name": name, "outcome": outcome}
			}
			switch outcome {
			case "destroyed":
				msg += fmt.Sprintf(" Its backing %s %s was destroyed.", kind, name)
			case "still_present":
				why := ""
				if force {
					why = " (force=true ignored the failure)"
				}
				msg += fmt.Sprintf(" Its backing %s %s is still present%s.", kind, name, why)
			case "absent":
				msg += fmt.Sprintf(" Its backing %s %s did not exist.", kind, name)
			case "unverified":
				msg += fmt.Sprintf(" Whether its backing %s %s was destroyed could not be read back.", kind, name)
			}
		}
	}
	result["message"] = msg
	if len(plan.Warnings) > 0 {
		result["warnings"] = plan.Warnings
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// --- delete_vm ---

// deleteVMZvol is one /dev/zvol/ DISK of the VM being deleted.
type deleteVMZvol struct {
	Name      string
	Path      string
	Dataset   map[string]interface{}
	Snapshots []snapDetail
	Consumers []string
	Invalid   bool
}

func (z *deleteVMZvol) sizeLabel() string {
	if z.Dataset != nil {
		if vs, ok := extractZFSInt64(z.Dataset["volsize"]); ok && vs > 0 {
			return formatBytes(vs)
		}
	}
	return "size unknown"
}

// entry is the zvol as the preview and result show it.
func (z *deleteVMZvol) entry(withSnapshots bool) map[string]interface{} {
	e := map[string]interface{}{
		"name":   z.Name,
		"path":   z.Path,
		"exists": z.Dataset != nil,
	}
	if z.Dataset != nil {
		if vs, ok := extractZFSInt64(z.Dataset["volsize"]); ok && vs > 0 {
			e["volsize"] = vs
			e["volsize_human"] = formatBytes(vs)
		}
		if locked, ok := z.Dataset["locked"].(bool); ok {
			e["locked"] = locked
		}
	}
	if withSnapshots {
		e["snapshot_count"] = len(z.Snapshots)
		if len(z.Snapshots) > 0 {
			e["snapshots"] = snapStates(z.Snapshots)
		}
		if len(z.Consumers) > 0 {
			e["used_by"] = z.Consumers
		}
	}
	return e
}

// otherVMZvolRefs maps every zvol attached to a VM other than vmID to its
// device references.
func otherVMZvolRefs(allVMs []map[string]interface{}, vmID int) map[string][]vmDeviceRef {
	out := map[string][]vmDeviceRef{}
	for _, row := range allVMs {
		rowID64, _ := extractZFSInt64(row["id"])
		rowID := int(rowID64)
		if rowID == vmID {
			continue
		}
		rowName, _ := row["name"].(string)
		devs, _ := row["devices"].([]interface{})
		for _, item := range devs {
			dm, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			attrs, _ := dm["attributes"].(map[string]interface{})
			if dtype, _ := attrs["dtype"].(string); dtype != "DISK" {
				continue
			}
			p, _ := attrs["path"].(string)
			if zn, ok := zvolPathToName(p); ok {
				devID64, _ := extractZFSInt64(dm["id"])
				out[zn] = append(out[zn], vmDeviceRef{
					VMID: rowID, VMName: rowName, DeviceID: int(devID64), DType: "DISK",
				})
			}
		}
	}
	return out
}

// deleteVMStateChecks returns the refusals and warnings for the VM's state.
func deleteVMStateChecks(vm *resolvedVM, force bool) (blocked, warnings []string) {
	if vm.State != "STOPPED" {
		if !force {
			blocked = append(blocked, fmt.Sprintf(
				"VM %q (id %d) is currently %s; stop it with stop_vm first, or pass force=true to forcibly power it off and delete it (and ignore domain/zvol cleanup errors)",
				vm.Name, vm.ID, vm.State))
		} else {
			warnings = append(warnings, fmt.Sprintf(
				"VM %q (id %d) is currently %s; force=true will forcibly power it off without guest shutdown and ignore domain/zvol cleanup errors",
				vm.Name, vm.ID, vm.State))
		}
	} else if force {
		warnings = append(warnings, fmt.Sprintf(
			"force=true will ignore domain and zvol cleanup errors while deleting VM %q (id %d)",
			vm.Name, vm.ID))
	}
	return blocked, warnings
}

// collectDeleteVMZvols returns the VM's sanitized devices and its distinct
// /dev/zvol/ DISKs, each with its dataset row. With deleteZvols it warns
// about DISK paths vm.delete does not destroy.
func collectDeleteVMZvols(ctx context.Context, client *truenas.Client, vm *resolvedVM, deleteZvols bool) ([]interface{}, []*deleteVMZvol, []string, error) {
	rawDevs, _ := vm.Raw["devices"].([]interface{})
	devices := make([]interface{}, 0, len(rawDevs))
	seen := map[string]bool{}
	var zvols []*deleteVMZvol
	var warnings []string
	for _, item := range rawDevs {
		dm, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		devices = append(devices, sanitizeCreatedDevice(dm))
		attrs, _ := dm["attributes"].(map[string]interface{})
		if dtype, _ := attrs["dtype"].(string); dtype != "DISK" {
			continue
		}
		devPath, _ := attrs["path"].(string)
		name, isZvol := zvolPathToName(devPath)
		if !isZvol {
			if deleteZvols && devPath != "" {
				warnings = append(warnings, fmt.Sprintf("DISK path %q is not under /dev/zvol/ and will not be destroyed by vm.delete", devPath))
			}
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		z := &deleteVMZvol{Name: name, Path: devPath}
		if deleteZvols && validateZvolName(name) != nil {
			z.Invalid = true
			zvols = append(zvols, z)
			continue
		}
		row, err := queryDatasetRow(ctx, client, name)
		if err != nil {
			return nil, nil, nil, err
		}
		z.Dataset = row
		zvols = append(zvols, z)
	}
	return devices, zvols, warnings, nil
}

// checkDeleteVMZvols applies the destroy guards to every zvol vm.delete
// would destroy: name, other VMs, dataset type, other services, and the
// snapshots a recursive destroy would take with it. It fills Snapshots and
// Consumers.
func checkDeleteVMZvols(ctx context.Context, client *truenas.Client, zvols []*deleteVMZvol, others map[string][]vmDeviceRef) (blocked, warnings []string, err error) {
	var consumers zvolConsumers
	for _, z := range zvols {
		if z.Invalid {
			reason := validateZvolName(z.Name)
			blocked = append(blocked, fmt.Sprintf("refusing to destroy zvol %q: %v", z.Name, reason))
			continue
		}
		if refs := others[z.Name]; len(refs) > 0 {
			blocked = append(blocked, fmt.Sprintf("cannot destroy zvol %s: it is also attached to %s", z.Name, formatDeviceRefs(refs)))
		}
		if z.Dataset == nil {
			warnings = append(warnings, fmt.Sprintf("zvol %s was not found in pool.dataset.query; vm.delete may fail unless force=true", z.Name))
			continue
		}
		if dt, _ := z.Dataset["type"].(string); dt != "" && dt != "VOLUME" {
			blocked = append(blocked, fmt.Sprintf("dataset %s is a %s, not a VOLUME; refusing to destroy it", z.Name, dt))
		}
		if consumers == nil {
			limits, err := fetchVMHostLimits(ctx, client)
			if err != nil {
				return nil, nil, err
			}
			if consumers, err = fetchZvolConsumers(ctx, client, limits.Version); err != nil {
				return nil, nil, err
			}
		}
		z.Consumers = consumers[z.Name]
		if reason := zvolConsumerRefusal(z.Name, z.Consumers); reason != "" {
			blocked = append(blocked, reason)
		}
		snaps, err := zvolSnapshots(ctx, client, z.Name)
		if err != nil {
			return nil, nil, err
		}
		z.Snapshots = snaps
		blocked = append(blocked, recursiveZvolDestroyRefusals(z.Name, snaps)...)
	}
	return blocked, warnings, nil
}

func planDeleteVM(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	vmID, hasID, name, hasName, err := parseVMSelector(args)
	if err != nil {
		return nil, err
	}
	deleteZvols, _ := args["zvols"].(bool)
	force, _ := args["force"].(bool)

	allVMs, err := queryAllVMs(ctx, client)
	if err != nil {
		return nil, err
	}
	matchedVM, err := matchVMRow(allVMs, vmID, hasID, name, hasName)
	if err != nil {
		return nil, err
	}
	vm, err := resolveVMRowWithStatus(ctx, client, matchedVM)
	if err != nil {
		return nil, err
	}

	blocked, warnings := deleteVMStateChecks(vm, force)
	others := otherVMZvolRefs(allVMs, vm.ID)
	devices, zvols, zWarnings, err := collectDeleteVMZvols(ctx, client, vm, deleteZvols)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, zWarnings...)
	if deleteZvols {
		zBlocked, zWarn, err := checkDeleteVMZvols(ctx, client, zvols, others)
		if err != nil {
			return nil, err
		}
		blocked = append(blocked, zBlocked...)
		warnings = append(warnings, zWarn...)
	}
	return buildDeleteVMPlan(vm, devices, zvols, others, deleteZvols, force, blocked, warnings), nil
}

// buildDeleteVMPlan assembles the vm.delete preview, confirmation binding
// and result.
func buildDeleteVMPlan(vm *resolvedVM, devices []interface{}, zvols []*deleteVMZvol, others map[string][]vmDeviceRef, deleteZvols, force bool, blocked, warnings []string) *writePlan {
	var toDelete, kept []map[string]interface{}
	var zvolBinds []interface{}
	var parts []string
	for _, z := range zvols {
		if z.Invalid {
			zvolBinds = append(zvolBinds, map[string]interface{}{"name": z.Name, "path": z.Path, "invalid": true})
			continue
		}
		b := map[string]interface{}{
			"name":            z.Name,
			"path":            z.Path,
			"dataset":         bindDatasetRow(z.Dataset),
			"other_zvol_refs": formatDeviceRefs(others[z.Name]),
		}
		if deleteZvols {
			// A zvol used by another service is refused, so only the
			// snapshots (with their clones and holds) are bound.
			b["snapshots"] = snapBinds(z.Snapshots)
			toDelete = append(toDelete, z.entry(true))
			parts = append(parts, fmt.Sprintf("%s (%s, %d snapshot(s))", z.Name, z.sizeLabel(), len(z.Snapshots)))
		} else {
			kept = append(kept, z.entry(false))
		}
		zvolBinds = append(zvolBinds, b)
	}

	if deleteZvols {
		if len(toDelete) > 0 {
			warnings = append(warnings, fmt.Sprintf("PERMANENT DATA LOSS: %d zvol(s) and all of their snapshots will be recursively destroyed: %s",
				len(toDelete), strings.Join(parts, ", ")))
		} else {
			warnings = append(warnings, fmt.Sprintf("zvols=true was requested, but VM %q (id %d) has no attached /dev/zvol/ disks", vm.Name, vm.ID))
		}
	}

	opts := map[string]interface{}{
		"zvols": deleteZvols,
		"force": force,
	}
	action := map[string]interface{}{
		"vm_id":        vm.ID,
		"name":         vm.Name,
		"device_count": len(devices),
		"zvols":        deleteZvols,
		"force":        force,
	}
	if len(toDelete) > 0 {
		action["zvols_to_delete"] = toDelete
	}
	if len(kept) > 0 {
		action["zvols_kept"] = kept
	}

	currentState := vm.currentState()
	currentState["device_count"] = len(devices)
	currentState["devices"] = devices

	summary := fmt.Sprintf("Delete VM %q (id %d, %d device(s))", vm.Name, vm.ID, len(devices))
	if deleteZvols && len(parts) > 0 {
		summary += " AND PERMANENTLY DESTROY zvols: " + strings.Join(parts, ", ")
	} else if !deleteZvols && len(kept) > 0 {
		summary += fmt.Sprintf(" (%d backing zvol(s) kept)", len(kept))
	}
	if vm.State != "STOPPED" && force {
		summary += fmt.Sprintf(" (VM is %s, force=true)", vm.State)
	}

	facts := []string{
		confirmFact("vm", fmt.Sprintf("%s (id %d, state %s)", vm.Name, vm.ID, vm.State)),
		confirmFact("devices", fmt.Sprintf("%d", len(devices))),
		confirmBoolFact("zvols", deleteZvols),
		confirmBoolFact("force", force),
	}
	if deleteZvols && len(parts) > 0 {
		facts = append(facts, confirmFact("zvols_to_destroy", strings.Join(parts, ", ")))
	}

	return &writePlan{
		Tool:         "delete_vm",
		Method:       vmDeleteMethod,
		CallParams:   []interface{}{vm.ID, opts},
		ParamNames:   []string{"id", "options"},
		Description:  summary,
		Operation:    "delete",
		CurrentState: currentState,
		Actions: []PlannedAction{{
			Step:        1,
			Description: summary,
			Operation:   "delete",
			Target:      fmt.Sprintf("VM %s (id %d)", vm.Name, vm.ID),
			Details:     action,
		}},
		Warnings:   warnings,
		Blocked:    blocked,
		TargetName: vm.Name,
		Bind: map[string]interface{}{
			"vm_id":     vm.ID,
			"name":      vm.Name,
			"uuid":      vm.UUID,
			"memory":    vm.MemoryMiB,
			"autostart": vm.Autostart,
			"status":    vm.statusBind(),
			"devices":   devices,
			"zvols":     zvolBinds,
			"options":   opts,
		},
		ConfirmSummary: newConfirmSummary(summary, facts...),
		Result: map[string]interface{}{
			"vm_id":         vm.ID,
			"name":          vm.Name,
			"zvols":         deleteZvols,
			"zvols_deleted": toDelete,
			"zvols_kept":    kept,
			"force":         force,
		},
	}
}

func (r *Registry) handleDeleteVM(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	return handlePlanned(ctx, client, args, planDeleteVM, r.executeDeleteVM)
}

// vmStillExists reports whether VM vmID is still listed by vm.query. ok is
// false when that could not be read.
func vmStillExists(ctx context.Context, client *truenas.Client, vmID int) (exists, ok bool) {
	rows, err := queryAllVMs(ctx, client)
	if err != nil {
		return false, false
	}
	for _, row := range rows {
		if id, _ := extractZFSInt64(row["id"]); int(id) == vmID {
			return true, true
		}
	}
	return false, true
}

func (r *Registry) executeDeleteVM(ctx context.Context, client *truenas.Client, _ map[string]interface{}, plan *writePlan) (string, error) {
	// TrueNAS 25.10 returns bool (true); master (28.0+) returns null.
	if _, err := callPlan(ctx, client, plan); err != nil {
		return "", fmt.Errorf("failed to delete VM: %w", err)
	}
	vmID, _ := plan.Result["vm_id"].(int)
	name, _ := plan.Result["name"].(string)
	deleteZvols, _ := plan.Result["zvols"].(bool)
	force, _ := plan.Result["force"].(bool)
	zvolsDeleted, _ := plan.Result["zvols_deleted"].([]map[string]interface{})
	zvolsKept, _ := plan.Result["zvols_kept"].([]map[string]interface{})

	result := map[string]interface{}{
		"vm_id": vmID,
		"name":  name,
		"zvols": deleteZvols,
		"force": force,
	}
	if len(zvolsKept) > 0 {
		result["zvols_kept"] = zvolsKept
	}

	// vm.delete succeeds with force=true even when it could not destroy a
	// zvol or undefine the domain, so what remains is read back rather than
	// assumed. The reads run even if the request was cancelled.
	vmGone, vmChecked := true, true
	var destroyed, remaining, unverified, absent []string
	if force || deleteZvols {
		cctx, cancel := vmCleanupContext(ctx)
		defer cancel()
		exists, ok := vmStillExists(cctx, client, vmID)
		vmGone, vmChecked = !exists, ok
		for _, z := range zvolsDeleted {
			zn, _ := z["name"].(string)
			if existed, _ := z["exists"].(bool); !existed {
				absent = append(absent, zn)
				continue
			}
			present, ok := zvolStillPresent(cctx, client, zn)
			switch {
			case !ok:
				unverified = append(unverified, zn)
			case present:
				remaining = append(remaining, zn)
			default:
				destroyed = append(destroyed, zn)
			}
		}
	}
	result["deleted"] = vmGone && vmChecked
	if !vmChecked {
		result["vm_status_unverified"] = true
	} else if !vmGone {
		result["vm_still_exists"] = true
	}
	if len(zvolsDeleted) > 0 {
		result["zvols_deleted"] = zvolsDeleted
		result["zvols_destroyed"] = nonNilStrings(destroyed)
	}
	if len(remaining) > 0 {
		result["zvols_still_present"] = remaining
	}
	if len(unverified) > 0 {
		result["zvols_unverified"] = unverified
	}
	if len(absent) > 0 {
		result["zvols_absent_before_delete"] = absent
	}

	var msg string
	switch {
	case !vmChecked:
		msg = fmt.Sprintf("vm.delete for VM %q (id %d) returned, but whether the VM is gone could not be read back.", name, vmID)
	case !vmGone:
		msg = fmt.Sprintf("vm.delete for VM %q (id %d) returned, but the VM still exists.", name, vmID)
	case deleteZvols && len(zvolsDeleted) > 0:
		msg = fmt.Sprintf("Deleted VM %q (id %d) and destroyed %d of %d backing zvol(s).", name, vmID, len(destroyed), len(zvolsDeleted)-len(absent))
	case !deleteZvols && len(zvolsKept) > 0:
		msg = fmt.Sprintf("Deleted VM %q (id %d); %d backing zvol(s) were kept.", name, vmID, len(zvolsKept))
	default:
		msg = fmt.Sprintf("Deleted VM %q (id %d).", name, vmID)
	}
	if len(remaining) > 0 {
		why := ""
		if force {
			why = " (force=true ignored the destroy failure)"
		}
		msg += fmt.Sprintf(" Still present%s: %s.", why, strings.Join(remaining, ", "))
	}
	if len(absent) > 0 {
		msg += fmt.Sprintf(" Already absent before the delete: %s.", strings.Join(absent, ", "))
	}
	if len(unverified) > 0 {
		msg += fmt.Sprintf(" Could not verify: %s.", strings.Join(unverified, ", "))
	}
	result["message"] = msg
	if len(plan.Warnings) > 0 {
		result["warnings"] = plan.Warnings
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// nonNilStrings returns s, or an empty list for nil, so JSON shows [].
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

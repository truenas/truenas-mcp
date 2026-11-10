package tools

// Per-tool allow lists for former raw-passthrough tools. The field lists
// follow the TrueNAS 25.10 / 27.0 API models. Unlisted keys, including
// surprise secret-looking fields, are dropped before redact.ToolOutput
// runs as a backstop.

var (
	poolOutputFields = []string{
		"id", "name", "guid", "path", "status", "status_code", "status_detail",
		"healthy", "warning", "size", "allocated", "free", "freeing",
		"fragmentation", "size_str", "allocated_str", "free_str", "is_upgraded",
		"autotrim", "scan", "topology",
	}
	poolQuerySelect = []interface{}{
		"id", "name", "guid", "path", "status", "status_code", "status_detail",
		"healthy", "warning", "size", "allocated", "free", "freeing",
		"fragmentation", "size_str", "allocated_str", "free_str", "is_upgraded",
		"autotrim", "scan", "topology",
	}
	poolScanFields = []string{
		"function", "state", "start_time", "end_time", "percentage", "errors",
		"total_secs_left", "pause", "bytes_to_process", "bytes_processed", "bytes_issued",
	}
	poolTopologyFields = []string{"type", "path", "name", "guid", "status", "children"}

	smbShareFields = []string{
		"id", "name", "path", "enabled", "comment", "purpose", "readonly", "browsable",
		"access_based_share_enumeration", "locked", "audit", "options",
	}
	smbShareQuerySelect = []interface{}{
		"id", "name", "path", "enabled", "comment", "purpose", "readonly", "browsable",
		"access_based_share_enumeration", "locked", "audit", "options",
	}
	smbAuditFields = []string{"enable", "watch_list", "ignore_list"}
	// Union of 25.10 / 27.0 SMB purpose option fields, except auxsmbconf,
	// which can hold arbitrary smb.conf text.
	smbOptionFields = []string{
		"purpose", "aapl_name_mangling", "recyclebin", "path_suffix", "hostsallow", "hostsdeny",
		"guestok", "streams", "durablehandle", "shadowcopy", "fsrvp", "home", "acl", "afp",
		"timemachine", "timemachine_quota", "vuid", "auto_snapshot", "auto_dataset_creation",
		"dataset_naming_schema", "grace_period", "auto_quota", "remote_path",
	}

	nfsShareFields = []string{
		"id", "path", "enabled", "ro", "comment", "networks", "hosts",
		"maproot_user", "maproot_group", "mapall_user", "mapall_group",
		"security", "expose_snapshots", "locked", "aliases",
	}
	nfsShareQuerySelect = []interface{}{
		"id", "path", "enabled", "ro", "comment", "networks", "hosts",
		"maproot_user", "maproot_group", "mapall_user", "mapall_group",
		"security", "expose_snapshots", "locked", "aliases",
	}

	// Alert "key" is omitted: it is a non-secret dedup id, but the
	// redaction backstop would mask any bare field named key. uuid is
	// the identifier dismiss_alert and restore_alert use. mail is omitted
	// as notification internals.
	alertOutputFields = []string{
		"uuid", "id", "source", "klass", "node", "datetime", "last_occurrence",
		"dismissed", "text", "level", "formatted", "one_shot", "args",
	}

	updateVersionFields     = []string{"train", "version"}
	updateVersionInfoFields = []string{"version", "release_notes", "release_notes_url"}
)

func pickAllowed(m map[string]interface{}, keys []string) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

func pickAllowedMap(v interface{}, keys []string) interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return v
	}
	return pickAllowed(m, keys)
}

// zfsPropValue returns the parsed (or human) value of a ZFS property object.
func zfsPropValue(v interface{}) interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return v
	}
	if parsed, ok := m["parsed"]; ok && parsed != nil {
		return parsed
	}
	if val, ok := m["value"]; ok && val != nil {
		return val
	}
	return v
}

func simplifyPool(pool map[string]interface{}) map[string]interface{} {
	out := pickAllowed(pool, poolOutputFields)
	if v, ok := out["autotrim"]; ok {
		out["autotrim"] = zfsPropValue(v)
	}
	if v, ok := out["scan"]; ok {
		if scan, ok := v.(map[string]interface{}); ok {
			out["scan"] = readableDatesMap(pickAllowed(scan, poolScanFields))
		}
	}
	if v, ok := out["topology"]; ok {
		out["topology"] = simplifyPoolTopology(v)
	}
	return out
}

func simplifyPoolTopology(v interface{}) interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for kind, raw := range m {
		out[kind] = simplifyPoolVdevs(raw)
	}
	return out
}

func simplifyPoolVdevs(v interface{}) interface{} {
	list, ok := v.([]interface{})
	if !ok {
		return []interface{}{}
	}
	out := make([]interface{}, 0, len(list))
	for _, item := range list {
		vm, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, simplifyPoolVdev(vm))
	}
	return out
}

func simplifyPoolVdev(m map[string]interface{}) map[string]interface{} {
	out := pickAllowed(m, poolTopologyFields)
	if kids, ok := out["children"]; ok {
		out["children"] = simplifyPoolVdevs(kids)
	}
	return out
}

func simplifySMBShare(share map[string]interface{}) map[string]interface{} {
	out := pickAllowed(share, smbShareFields)
	if v, ok := out["audit"]; ok {
		out["audit"] = pickAllowedMap(v, smbAuditFields)
	}
	if v, ok := out["options"]; ok {
		out["options"] = pickAllowedMap(v, smbOptionFields)
	}
	return out
}

func simplifyNFSShare(share map[string]interface{}) map[string]interface{} {
	return pickAllowed(share, nfsShareFields)
}

func simplifyAlert(alert map[string]interface{}) map[string]interface{} {
	return readableDatesMap(pickAllowed(alert, alertOutputFields))
}

func simplifyAvailableVersion(item map[string]interface{}) map[string]interface{} {
	out := pickAllowed(item, updateVersionFields)
	if v, ok := out["version"]; ok {
		out["version"] = pickAllowedMap(v, updateVersionInfoFields)
	}
	return out
}

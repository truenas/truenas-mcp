package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// Sentinels planted on fixtures as extra secret-looking fields. After the
// allow lists land, these must not appear in the tool result even before
// redact.ToolOutput runs.
const issue117Secret = "issue117-secret"

func issue117Secrets() map[string]interface{} {
	return map[string]interface{}{
		"password":  issue117Secret + "-password",
		"secret":    issue117Secret + "-secret",
		"pin":       issue117Secret + "-pin",
		"session":   issue117Secret + "-session",
		"api_key":   issue117Secret + "-api-key",
		"community": issue117Secret + "-community",
	}
}

func withSecrets(base map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(base)+8)
	for k, v := range base {
		out[k] = v
	}
	for k, v := range issue117Secrets() {
		out[k] = v
	}
	return out
}

func assertNoSecretSentinel(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, issue117Secret) {
		t.Errorf("tool output still contains the secret sentinel: %s", out)
	}
}

func assertKeysAllowed(t *testing.T, label string, got map[string]interface{}, allowed []string) {
	t.Helper()
	allow := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		allow[k] = struct{}{}
	}
	for k := range got {
		if _, ok := allow[k]; !ok {
			t.Errorf("%s: unexpected field %q (value %v)", label, k, got[k])
		}
	}
}

// TestIssue117PassthroughToolsAllowListOutput is the #117 regression: each
// former raw-passthrough tool (except get_app_config, which must still
// return the full values object for update_app) projects middleware records
// onto a fixed field list, so extra secret-looking fields never appear.
func TestIssue117PassthroughToolsAllowListOutput(t *testing.T) {
	poolScan := withSecrets(map[string]interface{}{
		"function": "SCRUB", "state": "FINISHED", "errors": 0.0,
		"percentage": 100.0, "end_time": map[string]interface{}{"$date": 1.0},
		"start_time":      map[string]interface{}{"$date": 1.0},
		"total_secs_left": nil, "pause": nil,
		"bytes_to_process": 1.0, "bytes_processed": 1.0, "bytes_issued": 1.0,
	})
	disk := withSecrets(map[string]interface{}{
		"name": "sda", "type": "DISK",
		"path":   "/dev/disk/by-partuuid/00000000-0000-0000-0000-00000000000a",
		"guid":   "1",
		"status": "FAULTED",
		"disk":   "sda",
		"device": "sda1",
		"stats": withSecrets(map[string]interface{}{
			"read_errors": 3.0, "write_errors": 1.0, "checksum_errors": 2.0,
			"bytes": 1.0, "timestamp": 1.0,
		}),
		"unavail_disk": withSecrets(map[string]interface{}{"devname": "sda"}),
	})
	vdev := withSecrets(map[string]interface{}{
		"type": "RAIDZ2", "status": "DEGRADED", "path": nil, "guid": "2",
		"stats": withSecrets(map[string]interface{}{
			"read_errors": 3.0, "write_errors": 1.0, "checksum_errors": 2.0,
			"bytes": 1.0,
		}),
		"children": []interface{}{disk},
	})
	fullPool := withSecrets(map[string]interface{}{
		"id": 1.0, "name": "tank", "guid": "pool-guid", "status": "ONLINE",
		"size": 1.0e12, "allocated": 4.0e11, "free": 6.0e11, "freeing": 0.0,
		"healthy": true, "warning": false, "status_code": "OK",
		"status_detail": "healthy", "path": "/mnt/tank", "is_upgraded": true,
		"fragmentation": "5%", "size_str": "1T", "allocated_str": "400G", "free_str": "600G",
		"autotrim":         map[string]interface{}{"parsed": "off", "rawvalue": "off", "source": "DEFAULT", "value": "off"},
		"dedup_table_size": 0.0,
		"expand": withSecrets(map[string]interface{}{
			"state": "FINISHED", "expanding_vdev": 0.0,
			"start_time":      map[string]interface{}{"$date": 1.0},
			"end_time":        map[string]interface{}{"$date": 1.0},
			"bytes_to_reflow": 835584.0, "bytes_reflowed": 978944.0,
			"waiting_for_resilver": 0.0, "total_secs_left": nil,
			"percentage": 85.35564853556485,
		}),
		"scan":     poolScan,
		"topology": map[string]interface{}{"data": []interface{}{vdev}},
	})
	fullSMB := withSecrets(map[string]interface{}{
		"id": 1.0, "name": "media", "path": "/mnt/tank/media", "enabled": true,
		"comment": "media", "purpose": "DEFAULT_SHARE", "readonly": false, "browsable": true,
		"access_based_share_enumeration": true, "locked": false,
		"audit":   withSecrets(map[string]interface{}{"enable": true, "watch_list": []interface{}{}, "ignore_list": []interface{}{}}),
		"options": withSecrets(map[string]interface{}{"hostsallow": []interface{}{"192.168.1.0/24"}, "hostsdeny": []interface{}{}, "aapl_name_mangling": false, "auxsmbconf": "passdb backend = " + issue117Secret}),
	})
	fullNFS := withSecrets(map[string]interface{}{
		"id": 2.0, "path": "/mnt/tank/nfs", "enabled": true, "ro": true,
		"comment": "nfs", "networks": []interface{}{}, "hosts": []interface{}{},
		"maproot_user": "nobody", "maproot_group": "nogroup",
		"mapall_user": nil, "mapall_group": nil,
		"security": []interface{}{"SYS"}, "expose_snapshots": true, "locked": false,
		"aliases": []interface{}{},
	})
	fullAlert := withSecrets(map[string]interface{}{
		"uuid": "00000000-0000-0000-0000-000000000001", "id": "00000000-0000-0000-0000-000000000001",
		"source": "Test", "klass": "ExampleAlert", "level": "WARNING",
		"formatted": "Example alert", "text": "Example alert", "dismissed": false,
		"node": "nas", "one_shot": false, "args": map[string]interface{}{"pool": "tank"},
		"datetime":        map[string]interface{}{"$date": 1700000000000.0},
		"last_occurrence": map[string]interface{}{"$date": 1700000000000.0},
		"mail":            map[string]interface{}{"password": issue117Secret},
		"key":             "alert-dedup-key",
	})
	fullUpdate := withSecrets(map[string]interface{}{
		"train": "TrueNAS-Fangtooth",
		"version": withSecrets(map[string]interface{}{
			"version": "25.10.1", "release_notes": "notes", "release_notes_url": "https://example.invalid/notes",
			"manifest": map[string]interface{}{"filename": "update.squashfs", "password": issue117Secret},
		}),
	})

	methods := map[string]json.RawMessage{}
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		methods[method] = params
		_, opts := parseQueryParams(t, params)
		applySelect := func(row map[string]interface{}) map[string]interface{} {
			sel, _ := opts["select"].([]interface{})
			if len(sel) == 0 {
				return row
			}
			return pickFields(row, sel)
		}
		switch method {
		case "pool.query":
			return []interface{}{applySelect(fullPool)}, nil, true
		case "sharing.smb.query":
			return []interface{}{applySelect(fullSMB)}, nil, true
		case "sharing.nfs.query":
			return []interface{}{applySelect(fullNFS)}, nil, true
		case "alert.list":
			return []interface{}{fullAlert}, nil, true
		case "update.available_versions":
			return []interface{}{fullUpdate}, nil, true
		default:
			return nil, nil, false
		}
	}}
	r := queryFilterRegistry(t, fake)

	t.Run("query_pools", func(t *testing.T) {
		out, err := r.CallTool(t.Context(), "query_pools", map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecretSentinel(t, out)
		pools := decodeList(t, out)
		if len(pools) != 1 {
			t.Fatalf("query_pools = %s", out)
		}
		assertKeysAllowed(t, "query_pools", pools[0], []string{
			"id", "name", "guid", "path", "status", "status_code", "status_detail",
			"healthy", "warning", "size", "allocated", "free", "freeing",
			"fragmentation", "size_str", "allocated_str", "free_str", "is_upgraded",
			"autotrim", "scan", "topology", "expand",
		})
		if pools[0]["name"] != "tank" || pools[0]["autotrim"] != "off" {
			t.Errorf("query_pools name/autotrim = %v %v", pools[0]["name"], pools[0]["autotrim"])
		}
		scan, _ := pools[0]["scan"].(map[string]interface{})
		assertKeysAllowed(t, "query_pools scan", scan, []string{
			"function", "state", "start_time", "end_time", "percentage", "errors",
			"total_secs_left", "pause", "bytes_to_process", "bytes_processed", "bytes_issued",
		})
		topo, _ := pools[0]["topology"].(map[string]interface{})
		data, _ := topo["data"].([]interface{})
		if len(data) != 1 {
			t.Fatalf("topology = %v", pools[0]["topology"])
		}
		vdevOut, _ := data[0].(map[string]interface{})
		assertKeysAllowed(t, "query_pools vdev", vdevOut, []string{"type", "path", "name", "guid", "status", "disk", "device", "stats", "children"})
		assertTrimmedVdevStats(t, "query_pools vdev stats", vdevOut)
		kids, _ := vdevOut["children"].([]interface{})
		if len(kids) != 1 {
			t.Fatalf("vdev children = %v", vdevOut["children"])
		}
		leaf, _ := kids[0].(map[string]interface{})
		assertKeysAllowed(t, "query_pools disk", leaf, []string{"type", "path", "name", "guid", "status", "disk", "device", "stats", "children"})
		if leaf["disk"] != "sda" || leaf["device"] != "sda1" {
			t.Errorf("query_pools leaf disk/device = %v %v, want sda sda1", leaf["disk"], leaf["device"])
		}
		if _, ok := leaf["unavail_disk"]; ok {
			t.Error("query_pools leaf should omit unavail_disk")
		}
		assertTrimmedVdevStats(t, "query_pools disk stats", leaf)
		expand, _ := pools[0]["expand"].(map[string]interface{})
		assertKeysAllowed(t, "query_pools expand", expand, []string{
			"state", "expanding_vdev", "start_time", "end_time",
			"bytes_to_reflow", "bytes_reflowed", "waiting_for_resilver",
			"total_secs_left", "percentage",
		})
		if expand["state"] != "FINISHED" {
			t.Errorf("query_pools expand.state = %v", expand["state"])
		}
		_, opts := parseQueryParams(t, methods["pool.query"])
		if !selectContains(opts, "name") || !selectContains(opts, "scan") || !selectContains(opts, "expand") {
			t.Errorf("pool.query should request a select list, got %s", methods["pool.query"])
		}
		extra, _ := opts["extra"].(map[string]interface{})
		if extra["is_upgraded"] != true {
			t.Errorf("pool.query should send extra.is_upgraded, got %s", methods["pool.query"])
		}
	})

	t.Run("query_shares", func(t *testing.T) {
		out, err := r.CallTool(t.Context(), "query_shares", map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecretSentinel(t, out)
		shares := decode(t, out)
		assertKeysAllowed(t, "query_shares", shares, []string{"smb_shares", "nfs_shares"})
		smb := shares["smb_shares"].([]interface{})[0].(map[string]interface{})
		nfs := shares["nfs_shares"].([]interface{})[0].(map[string]interface{})
		assertKeysAllowed(t, "query_shares SMB", smb, []string{
			"id", "name", "path", "enabled", "comment", "purpose", "readonly", "browsable",
			"access_based_share_enumeration", "locked", "audit", "options",
		})
		assertKeysAllowed(t, "query_shares NFS", nfs, []string{
			"id", "path", "enabled", "ro", "comment", "networks", "hosts",
			"maproot_user", "maproot_group", "mapall_user", "mapall_group",
			"security", "expose_snapshots", "locked", "aliases",
		})
		audit, _ := smb["audit"].(map[string]interface{})
		assertKeysAllowed(t, "query_shares SMB audit", audit, []string{"enable", "watch_list", "ignore_list"})
		opts, _ := smb["options"].(map[string]interface{})
		assertKeysAllowed(t, "query_shares SMB options", opts, []string{
			"purpose", "aapl_name_mangling", "recyclebin", "path_suffix", "hostsallow", "hostsdeny",
			"guestok", "streams", "durablehandle", "shadowcopy", "fsrvp", "home", "acl", "afp",
			"timemachine", "timemachine_quota", "vuid", "auto_snapshot", "auto_dataset_creation",
			"dataset_naming_schema", "grace_period", "auto_quota", "remote_path",
		})
		if _, ok := opts["auxsmbconf"]; ok {
			t.Error("auxsmbconf must not be in SMB options (it can hold arbitrary smb.conf)")
		}
		_, smbOpts := parseQueryParams(t, methods["sharing.smb.query"])
		_, nfsOpts := parseQueryParams(t, methods["sharing.nfs.query"])
		if !selectContains(smbOpts, "name") || !selectContains(nfsOpts, "path") {
			t.Errorf("share queries should request a select list, smb=%s nfs=%s", methods["sharing.smb.query"], methods["sharing.nfs.query"])
		}
	})

	t.Run("list_alerts", func(t *testing.T) {
		out, err := r.CallTool(t.Context(), "list_alerts", map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecretSentinel(t, out)
		alerts := decodeList(t, out)
		if len(alerts) != 1 {
			t.Fatalf("list_alerts = %s", out)
		}
		assertKeysAllowed(t, "list_alerts", alerts[0], []string{
			"uuid", "id", "source", "klass", "node", "datetime", "last_occurrence",
			"dismissed", "text", "level", "formatted", "one_shot", "args",
		})
		if _, ok := alerts[0]["key"]; ok {
			t.Error("list_alerts should omit key: the redaction backstop would mask a non-secret alert key")
		}
		if _, ok := alerts[0]["mail"]; ok {
			t.Error("list_alerts should omit mail")
		}
		if alerts[0]["uuid"] != "00000000-0000-0000-0000-000000000001" {
			t.Errorf("uuid = %v", alerts[0]["uuid"])
		}
	})

	t.Run("check_updates", func(t *testing.T) {
		out, err := r.CallTool(t.Context(), "check_updates", map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecretSentinel(t, out)
		versions := decodeList(t, out)
		if len(versions) != 1 {
			t.Fatalf("check_updates = %s", out)
		}
		assertKeysAllowed(t, "check_updates", versions[0], []string{"train", "version"})
		ver, _ := versions[0]["version"].(map[string]interface{})
		assertKeysAllowed(t, "check_updates version", ver, []string{"version", "release_notes", "release_notes_url"})
		if _, ok := ver["manifest"]; ok {
			t.Error("check_updates should omit version.manifest")
		}
		if versions[0]["train"] != "TrueNAS-Fangtooth" || ver["version"] != "25.10.1" {
			t.Errorf("check_updates = %s", out)
		}
	})
}

func assertTrimmedVdevStats(t *testing.T, label string, vdev map[string]interface{}) {
	t.Helper()
	stats, ok := vdev["stats"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: stats missing or not an object: %v", label, vdev["stats"])
	}
	assertKeysAllowed(t, label, stats, []string{"read_errors", "write_errors", "checksum_errors"})
	if stats["read_errors"] != 3.0 || stats["write_errors"] != 1.0 || stats["checksum_errors"] != 2.0 {
		t.Errorf("%s = %v, want read_errors=3 write_errors=1 checksum_errors=2", label, stats)
	}
	if _, ok := stats["bytes"]; ok {
		t.Errorf("%s still has planted key bytes: %v", label, stats)
	}
}

func TestIssue117FailClosedProjection(t *testing.T) {
	t.Run("pickAllowedMap non-object", func(t *testing.T) {
		if got := pickAllowedMap("not-an-object", smbAuditFields); got != nil {
			t.Errorf("pickAllowedMap(string) = %#v, want nil", got)
		}
		if got := pickAllowedMap([]interface{}{"x"}, smbAuditFields); got != nil {
			t.Errorf("pickAllowedMap(list) = %#v, want nil", got)
		}
	})
	t.Run("non-object scan", func(t *testing.T) {
		out := simplifyPool(map[string]interface{}{"name": "tank", "scan": "not-an-object"})
		if v, ok := out["scan"]; ok {
			t.Errorf("non-object scan was kept as %#v", v)
		}
		kept := simplifyPool(map[string]interface{}{"name": "tank", "scan": nil})
		if v, ok := kept["scan"]; !ok || v != nil {
			t.Errorf("null scan = %#v, want a kept null", kept["scan"])
		}
	})
	t.Run("zfsPropValue neither parsed nor value", func(t *testing.T) {
		if got := zfsPropValue(map[string]interface{}{"rawvalue": "off", "source": "DEFAULT"}); got != nil {
			t.Errorf("zfsPropValue(no parsed/value) = %#v, want nil", got)
		}
		out := simplifyPool(map[string]interface{}{
			"name":     "tank",
			"autotrim": map[string]interface{}{"rawvalue": "off", "source": "DEFAULT"},
		})
		if v, ok := out["autotrim"]; ok {
			t.Errorf("autotrim without parsed/value was kept as %#v", v)
		}
	})
}

func TestIssue117AlertDryRunCurrentStateIsSimplified(t *testing.T) {
	alert := withSecrets(map[string]interface{}{
		"uuid": "00000000-0000-0000-0000-000000000001", "id": "00000000-0000-0000-0000-000000000001",
		"source": "Test", "klass": "ExampleAlert", "level": "WARNING",
		"formatted": "Example alert", "text": "Example alert", "dismissed": false,
		"node": "nas", "one_shot": false, "args": map[string]interface{}{"pool": "tank"},
		"datetime":        map[string]interface{}{"$date": 1700000000000.0},
		"last_occurrence": map[string]interface{}{"$date": 1700000000000.0},
		"mail":            map[string]interface{}{"password": issue117Secret},
		"key":             "alert-dedup-key",
	})
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"alert.list":    []interface{}{alert},
		"alert.dismiss": nil,
		"alert.restore": nil,
	}, nil)
	r := NewRegistry(client, nil)

	for _, tool := range []string{"dismiss_alert", "restore_alert"} {
		t.Run(tool, func(t *testing.T) {
			out, err := r.CallTool(t.Context(), tool, map[string]interface{}{
				"uuid":    "00000000-0000-0000-0000-000000000001",
				"dry_run": true,
			})
			if err != nil {
				t.Fatalf("dry-run: %v\n%s", err, out)
			}
			assertNoSecretSentinel(t, out)
			preview := decode(t, out)
			state, _ := preview["current_state"].(map[string]interface{})
			if state == nil {
				t.Fatalf("current_state missing: %s", out)
			}
			assertKeysAllowed(t, tool+" current_state", state, alertOutputFields)
			if _, ok := state["mail"]; ok {
				t.Errorf("%s dry-run current_state still has mail: %s", tool, out)
			}
			if _, ok := state["key"]; ok {
				t.Errorf("%s dry-run current_state still has key: %s", tool, out)
			}
			if state["uuid"] != "00000000-0000-0000-0000-000000000001" {
				t.Errorf("current_state.uuid = %v", state["uuid"])
			}
			if n := len(fake.called(map[string]string{"dismiss_alert": "alert.dismiss", "restore_alert": "alert.restore"}[tool])); n != 0 {
				t.Errorf("dry-run called the write method %d times", n)
			}
		})
	}
}

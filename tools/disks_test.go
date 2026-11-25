package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/redact"
)

// sentinelDiskPasswd stands in for a SED password that disk.query returns
// only with extra.passwords; it must never reach a result.
const sentinelDiskPasswd = "sentinel-sed-passwd"

func diskQueryFixtures() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"identifier": "{serial_lunid}FAKE-SERIAL-0001_5000c500aaaa0001", "name": "sda", "serial": "FAKE-SERIAL-0001",
			"model": "FAKE HDD 8TB", "size": 8001563222016.0, "type": "HDD", "rotationrate": 7200.0, "bus": "ATA",
			"pool": "tank", "zfs_guid": "1001", "passwd": sentinelDiskPasswd, "kmip_uid": "sentinel-kmip",
			"description": "sentinel free text",
		},
		map[string]interface{}{
			"identifier": "{serial}FAKE-SERIAL-0002", "name": "sdb", "serial": "FAKE-SERIAL-0002",
			"model": "FAKE HDD 8TB", "size": 8001563222016.0, "type": "HDD", "rotationrate": 7200.0, "bus": "ATA",
			"pool": "tank", "zfs_guid": "1002",
		},
		map[string]interface{}{
			"identifier": "{serial}FAKE-SERIAL-0003", "name": "sdc", "serial": "FAKE-SERIAL-0003",
			"model": "FAKE SSD 1TB", "size": 1000204886016.0, "type": "SSD", "rotationrate": nil, "bus": "ATA", "pool": nil,
		},
		map[string]interface{}{
			"identifier": "{serial}FAKE-NVME-0001", "name": "nvme0n1", "serial": "FAKE-NVME-0001",
			"model": "FAKE NVMe 500GB", "size": 500107862016.0, "type": "SSD", "bus": "NVME", "pool": "boot-pool",
		},
		map[string]interface{}{
			"identifier": "{serial}FAKE-SERIAL-0007", "name": "sdg", "serial": "FAKE-SERIAL-0007",
			"model": "FAKE HDD 4TB", "size": 4000787030016.0, "type": "HDD", "bus": "ATA", "pool": nil,
		},
	}
}

func diskDetailsFixture() map[string]interface{} {
	row := func(name, serial, typ, bus, driver string, imported, exported interface{}) map[string]interface{} {
		return map[string]interface{}{
			"name": name, "serial": serial, "type": typ, "bus": bus, "driver": driver, "size": 4000787030016.0,
			"model": "FAKE DISK", "imported_zpool": imported, "exported_zpool": exported, "duplicate_serial": []interface{}{},
		}
	}
	return map[string]interface{}{
		"used": []interface{}{
			row("sda", "FAKE-SERIAL-0001", "HDD", "ATA", "ahci", "tank", nil),
			row("sdb", "FAKE-SERIAL-0002", "HDD", "ATA", "ahci", "tank", nil),
			row("nvme0n1", "FAKE-NVME-0001", "SSD", "NVME", "nvme", "boot-pool", nil),
			row("sdh", "FAKE-SERIAL-0008", "HDD", "ATA", "ahci", nil, "oldpool"),
		},
		"unused": []interface{}{
			row("sdg", "FAKE-SERIAL-0007", "HDD", "ATA", "ahci", nil, nil),
		},
	}
}

func leaf(disk, guid, status string, r, w, c float64) map[string]interface{} {
	m := map[string]interface{}{
		"type": "DISK", "name": guid, "guid": guid, "status": status, "children": []interface{}{},
		"stats": map[string]interface{}{"read_errors": r, "write_errors": w, "checksum_errors": c, "ops": []interface{}{0.0}},
	}
	if disk != "" {
		m["disk"], m["device"], m["path"] = disk, disk+"1", "/dev/"+disk+"1"
	} else {
		m["disk"], m["device"], m["path"] = nil, nil, "/dev/disk/by-partuuid/fake"
	}
	return m
}

func tankPoolFixture() map[string]interface{} {
	removed := leaf("", "1005", "REMOVED", 0, 0, 0)
	removed["unavail_disk"] = map[string]interface{}{"name": "sde", "serial": "FAKE-SERIAL-0005", "model": "FAKE HDD 8TB", "passwd": sentinelDiskPasswd}
	return map[string]interface{}{
		"name": "tank", "status": "DEGRADED", "healthy": false,
		"topology": map[string]interface{}{
			"data": []interface{}{
				map[string]interface{}{
					"type": "MIRROR", "name": "mirror-0", "status": "DEGRADED",
					"children": []interface{}{leaf("sda", "1001", "ONLINE", 0, 0, 0), leaf("sdb", "1002", "FAULTED", 3, 0, 12)},
				},
				map[string]interface{}{
					"type": "MIRROR", "name": "mirror-1", "status": "DEGRADED",
					"children": []interface{}{removed, leaf("sdc", "1003", "ONLINE", 0, 0, 0)},
				},
			},
			"log": []interface{}{}, "cache": []interface{}{}, "special": []interface{}{}, "dedup": []interface{}{},
			"spare": []interface{}{leaf("sdf", "1006", "AVAIL", 0, 0, 0)},
		},
	}
}

func bootPoolFixture() map[string]interface{} {
	return map[string]interface{}{
		"name": "boot-pool", "status": "ONLINE", "healthy": true,
		"topology": map[string]interface{}{"data": []interface{}{leaf("nvme0n1", "2001", "ONLINE", 0, 0, 0)}},
	}
}

type diskFakeOpts struct {
	detailsErr, tempsErr bool
}

func diskFake(t *testing.T, opts diskFakeOpts) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		var p []interface{}
		_ = json.Unmarshal(params, &p)
		switch method {
		case "disk.query":
			return diskQueryFixtures(), nil, true
		case "disk.details":
			if opts.detailsErr {
				return nil, map[string]interface{}{"code": -32001, "message": "details failed"}, true
			}
			return diskDetailsFixture(), nil, true
		case "pool.query":
			filters, _ := p[0].([]interface{})
			out := []interface{}{}
			if row := tankPoolFixture(); jobMatchesFilters(row, filters) {
				out = append(out, row)
			}
			return out, nil, true
		case "boot.get_state":
			return bootPoolFixture(), nil, true
		case "disk.temperatures":
			if opts.tempsErr {
				return nil, map[string]interface{}{"code": -32001, "message": "temps failed"}, true
			}
			all := map[string]interface{}{
				"sda": []interface{}{40.0, 70.0}, "sdb": []interface{}{75.0, 70.0},
				"nvme0n1": []interface{}{50.0, nil}, "sdg": nil,
			}
			names, _ := p[0].([]interface{})
			out := map[string]interface{}{}
			for _, n := range names {
				out[n.(string)] = all[n.(string)]
			}
			return out, nil, true
		case "disk.temperature_alerts":
			return []interface{}{map[string]interface{}{
				"uuid": "fake-uuid", "klass": "DiskTemperatureTooHot", "level": "CRITICAL",
				"args":      map[string]interface{}{"device": "/dev/sdb", "serial": "FAKE-SERIAL-0002", "crit_threshold": 70.0, "temp": 75.0},
				"formatted": "Disk /dev/sdb (with serial: FAKE-SERIAL-0002) critical temperature threshold is 70 degrees celsius and the current temp is 75 degrees celsius",
				"mail":      "sentinel-mail", "key": "sentinel-key", "dismissed": false,
			}}, nil, true
		}
		return nil, nil, false
	}}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

func disksByName(t *testing.T, resp map[string]interface{}) map[string]map[string]interface{} {
	t.Helper()
	out := map[string]map[string]interface{}{}
	for _, item := range resp["disks"].([]interface{}) {
		m := item.(map[string]interface{})
		name, _ := m["name"].(string)
		out[name] = m
	}
	return out
}

func TestQueryDisksMergesQueryAndDetails(t *testing.T) {
	r, fake := diskFake(t, diskFakeOpts{})
	out, err := r.CallTool(t.Context(), "query_disks", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{sentinelDiskPasswd, "sentinel-kmip", "sentinel free text", `"passwd"`, `"identifier"`} {
		if strings.Contains(out, s) {
			t.Errorf("output contains %q", s)
		}
	}
	if final := redact.ToolOutput(out); final != out {
		t.Errorf("redaction backstop changed the projected output:\nbefore: %s\nafter: %s", out, final)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["count"] != 6.0 || resp["total_disks"] != 6.0 {
		t.Errorf("count/total = %v/%v, want 6/6", resp["count"], resp["total_disks"])
	}
	var order []string
	for _, item := range resp["disks"].([]interface{}) {
		order = append(order, item.(map[string]interface{})["name"].(string))
	}
	if got := strings.Join(order, ","); got != "sda,sdb,sdc,sdg,sdh,nvme0n1" {
		t.Errorf("order = %s", got)
	}
	disks := disksByName(t, resp)

	sda := disks["sda"]
	if sda["pool"] != "tank" || sda["usage"] != "USED" || sda["in_use"] != true || sda["type"] != "HDD" ||
		sda["serial"] != "FAKE-SERIAL-0001" || sda["size_bytes"] != 8001563222016.0 || sda["rotation_rate_rpm"] != 7200.0 {
		t.Errorf("sda = %v", sda)
	}
	if nvme := disks["nvme0n1"]; nvme["type"] != "NVMe" || nvme["pool"] != "boot-pool" || nvme["usage"] != "USED" {
		t.Errorf("nvme0n1 = %v", nvme)
	}
	if sdg := disks["sdg"]; sdg["usage"] != "UNUSED" || sdg["in_use"] != false || sdg["pool"] != nil {
		t.Errorf("sdg = %v", sdg)
	}
	sdc := disks["sdc"]
	if sdc["usage"] != "UNKNOWN" || sdc["in_use"] != false || !strings.Contains(sdc["note"].(string), "Not reported by disk.details") {
		t.Errorf("sdc = %v", sdc)
	}
	sdh := disks["sdh"]
	if sdh["usage"] != "USED" || sdh["exported_pool"] != "oldpool" || sdh["pool"] != nil ||
		!strings.Contains(sdh["note"].(string), "Not yet in the TrueNAS disk database") {
		t.Errorf("sdh = %v", sdh)
	}

	p := lastParams(t, fake, "disk.query")
	if got, _ := json.Marshal(p); string(got) != `[[],{"extra":{"pools":true}}]` {
		t.Errorf("disk.query params = %s", got)
	}
	p = lastParams(t, fake, "disk.details")
	if got, _ := json.Marshal(p); string(got) != `[{"join_partitions":false,"type":"BOTH"}]` {
		t.Errorf("disk.details params = %s", got)
	}
}

func TestQueryDisksFilters(t *testing.T) {
	r, fake := diskFake(t, diskFakeOpts{})
	names := func(args map[string]interface{}) string {
		resp := callTool(t, r, "query_disks", args)
		var out []string
		for _, item := range resp["disks"].([]interface{}) {
			out = append(out, item.(map[string]interface{})["name"].(string))
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		args map[string]interface{}
		want string
	}{
		{map[string]interface{}{"pool": "tank"}, "sda,sdb"},
		{map[string]interface{}{"pool": "oldpool"}, "sdh"},
		{map[string]interface{}{"unused_only": true}, "sdg"},
		{map[string]interface{}{"name": "nvme0n1"}, "nvme0n1"},
		{map[string]interface{}{"limit": 2.0}, "sda,sdb"},
	} {
		if got := names(tc.args); got != tc.want {
			t.Errorf("%v: got %s, want %s", tc.args, got, tc.want)
		}
	}
	resp := callTool(t, r, "query_disks", map[string]interface{}{"limit": 2.0})
	if resp["total_disks"] != 6.0 || !strings.Contains(resp["note"].(string), "Showing 2 of 6 disks") {
		t.Errorf("limited response = %v", resp)
	}

	if _, err := r.CallTool(t.Context(), "query_disks", map[string]interface{}{"name": "sdz"}); err == nil || !strings.Contains(err.Error(), `disk "sdz" not found`) {
		t.Errorf("unknown disk err = %v", err)
	}
	before := len(fake.called("disk.query"))
	_, err := r.CallTool(t.Context(), "query_disks", map[string]interface{}{"pool": "tank", "unused_only": true})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined with pool") {
		t.Errorf("contradictory args err = %v", err)
	}
	if len(fake.called("disk.query")) != before {
		t.Error("contradictory arguments still queried the middleware")
	}
}

func TestQueryDisksDetailsFailure(t *testing.T) {
	r, _ := diskFake(t, diskFakeOpts{detailsErr: true})
	resp := callTool(t, r, "query_disks", map[string]interface{}{})
	if resp["count"] != 5.0 {
		t.Errorf("count = %v, want 5 (disk.query rows only)", resp["count"])
	}
	disks := disksByName(t, resp)
	if sda := disks["sda"]; sda["usage"] != "UNKNOWN" || sda["in_use"] != true || sda["note"] != nil {
		t.Errorf("sda = %v", sda)
	}
	if sdg := disks["sdg"]; sdg["usage"] != "UNKNOWN" || sdg["in_use"] != false {
		t.Errorf("sdg = %v", sdg)
	}
	if note, _ := resp["note"].(string); !strings.Contains(note, "disk.details failed") {
		t.Errorf("note = %q", note)
	}
	if _, err := r.CallTool(t.Context(), "query_disks", map[string]interface{}{"unused_only": true}); err == nil || !strings.Contains(err.Error(), "failed to classify disks") {
		t.Errorf("unused_only without disk.details err = %v", err)
	}
}

func TestGetDiskHealthFindsFailingDisks(t *testing.T) {
	r, fake := diskFake(t, diskFakeOpts{})
	out, err := r.CallTool(t.Context(), "get_disk_health", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{sentinelDiskPasswd, "sentinel-mail", "sentinel-key"} {
		if strings.Contains(out, s) {
			t.Errorf("output contains %q", s)
		}
	}
	if final := redact.ToolOutput(out); final != out {
		t.Errorf("redaction backstop changed the projected output:\nbefore: %s\nafter: %s", out, final)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(resp["unhealthy"]); string(got) != `["sdb","sde"]` {
		t.Errorf("unhealthy = %s", got)
	}
	list := resp["disks"].([]interface{})
	if first := list[0].(map[string]interface{}); first["healthy"] != false {
		t.Errorf("first disk is healthy: %v", first)
	}
	if resp["count"] != 7.0 {
		t.Errorf("count = %v, want 7 (6 pool devices + sdg)", resp["count"])
	}
	disks := disksByName(t, resp)

	sdb := disks["sdb"]
	problems := strings.Join(stringList(sdb["problems"]), " | ")
	for _, want := range []string{"ZFS reports the device as FAULTED", "3 read, 0 write and 12 checksum errors", "temperature 75 C is at or above the critical 70 C", "temperature alert: Disk /dev/sdb"} {
		if !strings.Contains(problems, want) {
			t.Errorf("sdb problems %q lack %q", problems, want)
		}
	}
	if sdb["pool"] != "tank" || sdb["vdev"] != "mirror-0" || sdb["vdev_status"] != "DEGRADED" || sdb["serial"] != "FAKE-SERIAL-0002" {
		t.Errorf("sdb = %v", sdb)
	}
	sde := disks["sde"]
	if sde["zfs_status"] != "REMOVED" || sde["serial"] != "FAKE-SERIAL-0005" || sde["guid"] != "1005" {
		t.Errorf("sde = %v", sde)
	}
	if sda := disks["sda"]; sda["healthy"] != true || sda["temperature_c"] != 40.0 || sda["critical_temperature_c"] != 70.0 {
		t.Errorf("sda = %v", sda)
	}
	if sdf := disks["sdf"]; sdf["healthy"] != true || sdf["vdev_kind"] != "spare" {
		t.Errorf("spare sdf = %v", sdf)
	}
	nvme := disks["nvme0n1"]
	if nvme["pool"] != "boot-pool" || nvme["healthy"] != true || nvme["type"] != "NVMe" || nvme["critical_temperature_c"] != nil {
		t.Errorf("nvme0n1 = %v", nvme)
	}
	if sdg := disks["sdg"]; sdg["pool"] != nil || sdg["healthy"] != true || sdg["temperature_c"] != nil {
		t.Errorf("sdg = %v", sdg)
	}
	if !strings.Contains(resp["smart_note"].(string), "no public SMART test API") {
		t.Errorf("smart_note = %v", resp["smart_note"])
	}
	if alerts := resp["temperature_alerts"].([]interface{}); len(alerts) != 1 {
		t.Errorf("temperature_alerts = %v", alerts)
	}
	pools, _ := json.Marshal(resp["pools"])
	if !strings.Contains(string(pools), `"boot":true`) || !strings.Contains(string(pools), `"status":"DEGRADED"`) {
		t.Errorf("pools = %s", pools)
	}

	p := lastParams(t, fake, "disk.temperatures")
	if got, _ := json.Marshal(p); string(got) != `[["nvme0n1","sda","sdb","sdc","sde","sdf","sdg"],true]` {
		t.Errorf("disk.temperatures params = %s", got)
	}
	p = lastParams(t, fake, "disk.temperature_alerts")
	if got, _ := json.Marshal(p); string(got) != `[["nvme0n1","sda","sdb","sdc","sde","sdf","sdg"]]` {
		t.Errorf("disk.temperature_alerts params = %s", got)
	}
}

func TestGetDiskHealthFilters(t *testing.T) {
	r, fake := diskFake(t, diskFakeOpts{})
	resp := callTool(t, r, "get_disk_health", map[string]interface{}{"pool": "tank"})
	disks := disksByName(t, resp)
	if len(disks) != 5 || disks["nvme0n1"] != nil || disks["sdg"] != nil {
		t.Errorf("pool=tank disks = %v", disks)
	}
	if got, _ := json.Marshal(lastParams(t, fake, "pool.query")[0]); string(got) != `[["name","=","tank"]]` {
		t.Errorf("pool.query filters = %s", got)
	}

	resp = callTool(t, r, "get_disk_health", map[string]interface{}{"pool": "boot-pool"})
	if disks := disksByName(t, resp); len(disks) != 1 || disks["nvme0n1"] == nil {
		t.Errorf("pool=boot-pool disks = %v", disks)
	}

	resp = callTool(t, r, "get_disk_health", map[string]interface{}{"name": "sda"})
	if resp["count"] != 1.0 || resp["unhealthy_count"] != 0.0 {
		t.Errorf("name=sda = %v", resp)
	}

	if _, err := r.CallTool(t.Context(), "get_disk_health", map[string]interface{}{"pool": "nosuch"}); err == nil || !strings.Contains(err.Error(), `pool "nosuch" not found`) {
		t.Errorf("unknown pool err = %v", err)
	}
	if _, err := r.CallTool(t.Context(), "get_disk_health", map[string]interface{}{"name": "sdg", "pool": "tank"}); err == nil || !strings.Contains(err.Error(), `disk "sdg" not found in pool "tank"`) {
		t.Errorf("disk outside pool err = %v", err)
	}
}

func TestGetDiskHealthTemperatureFailureIsANote(t *testing.T) {
	r, _ := diskFake(t, diskFakeOpts{tempsErr: true})
	resp := callTool(t, r, "get_disk_health", map[string]interface{}{})
	if note, _ := resp["note"].(string); !strings.Contains(note, "temperatures unavailable") {
		t.Errorf("note = %q", note)
	}
	disks := disksByName(t, resp)
	if _, ok := disks["sda"]["temperature_c"]; ok {
		t.Errorf("sda has a temperature without disk.temperatures: %v", disks["sda"])
	}
	// The temperature alert still marks sdb, and ZFS state still counts.
	if got, _ := json.Marshal(resp["unhealthy"]); string(got) != `["sdb","sde"]` {
		t.Errorf("unhealthy = %s", got)
	}
}

func TestDiskAndServiceToolsUnderPolicy(t *testing.T) {
	newTools := []string{"query_disks", "get_disk_health", "query_services"}
	r := NewRegistry(nil, nil)
	stubHandlers(r)
	if err := r.SetPolicy(Policy{ReadOnly: true, Allow: newTools}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	if len(names) != len(newTools) {
		t.Errorf("listed = %v, want only %v", names, newTools)
	}
	for _, name := range newTools {
		if !names[name] {
			t.Errorf("%s not listed under --read-only", name)
		}
		if _, err := r.CallTool(t.Context(), name, map[string]interface{}{}); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}

	r = NewRegistry(nil, nil)
	stubHandlers(r)
	if err := r.SetPolicy(Policy{Deny: []string{"get_disk_health"}}); err != nil {
		t.Fatal(err)
	}
	if listed(r)["get_disk_health"] {
		t.Error("denied get_disk_health is listed")
	}
	if _, err := r.CallTool(t.Context(), "get_disk_health", map[string]interface{}{}); err == nil || !strings.Contains(err.Error(), "--deny-tools") {
		t.Errorf("denied call err = %v", err)
	}
}

package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Fixtures mimic TrueNAS 27.0 middleware shapes with fake values.

// multiGraph is a reporting.get_data entry with several series: rows are
// [v1, v2, ...] per 10 s sample ending at end.
func multiGraph(end time.Time, name, identifier string, legend []string, rows ...[]float64) map[string]interface{} {
	start := end.Unix() - int64(10*(len(rows)-1))
	data := make([]interface{}, len(rows))
	for i, r := range rows {
		row := []interface{}{float64(start + int64(i*10))}
		for _, v := range r {
			row = append(row, v)
		}
		data[i] = row
	}
	lg := []interface{}{"time"}
	for _, l := range legend {
		lg = append(lg, l)
	}
	return map[string]interface{}{
		"name": name, "identifier": identifier, "data": data, "legend": lg,
		"start": start, "end": end.Unix(),
		"aggregations": map[string]interface{}{"min": map[string]interface{}{}, "mean": map[string]interface{}{}, "max": map[string]interface{}{}},
	}
}

func decode(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("bad JSON %v: %s", err, out)
	}
	return m
}

// Item 1: network. The "interface" graph is in Kilobits/s with series
// "received" and "sent"; each is analysed by name.
func TestAnalyzeNetworkCapacityLegendAndUnits(t *testing.T) {
	// 500,000 Kb/s = 500 Mbps received on a 1000 Mbps link; 100 Mbps sent.
	g := multiGraph(time.Now(), "interface", "eth0", []string{"received", "sent"},
		[]float64{500000, 100000}, []float64{500000, 100000}, []float64{500000, 100000})
	fake := reportingFake(t, map[string]map[string]interface{}{"interface": g}, map[string]interface{}{
		"interface.query": []interface{}{map[string]interface{}{"name": "eth0", "state": map[string]interface{}{"link_speed": 1000.0}}},
	}, nil)
	client, _ := newFakeClientFrom(t, fake)
	a, err := analyzeNetworkCapacity(client, "HOUR")
	if err != nil {
		t.Fatal(err)
	}
	eth0 := a["eth0"].(map[string]interface{})
	if _, ok := eth0[""]; ok {
		t.Errorf("series keyed by empty legend: %v", eth0)
	}
	rx, _ := eth0["received"].(map[string]interface{})
	tx, _ := eth0["sent"].(map[string]interface{})
	if rx["current_mbps"] != "500.00" || rx["current_utilization_pct"] != "50.00" {
		t.Errorf("received = %v", rx)
	}
	if tx["current_mbps"] != "100.00" || tx["current_utilization_pct"] != "10.00" {
		t.Errorf("sent = %v", tx)
	}
	// Recommendations still skip non-series fields.
	recs := generateCapacityRecommendations(map[string]interface{}{"network": a})
	if b, _ := json.Marshal(recs); strings.Contains(string(b), "unit_source") {
		t.Errorf("unexpected recommendation: %s", b)
	}
}

// Item 1: disk. The "disk" graph is in Kibibytes/s with series "reads"
// and "writes" (writes may be negative).
func TestAnalyzeDiskCapacityLegendAndUnits(t *testing.T) {
	g := multiGraph(time.Now(), "disk", "sda", []string{"reads", "writes"},
		[]float64{100, -200}, []float64{100, -200})
	fake := reportingFake(t, map[string]map[string]interface{}{"disk": g}, map[string]interface{}{
		"reporting.graphs": []interface{}{map[string]interface{}{"name": "disk", "vertical_label": "Kibibytes/s", "identifiers": []interface{}{"sda | Type: SSD"}}},
	}, nil)
	client, _ := newFakeClientFrom(t, fake)
	a, err := analyzeDiskCapacity(client, "HOUR")
	if err != nil {
		t.Fatal(err)
	}
	sda := a["sda"].(map[string]interface{})
	if sda["unit"] != "KiB/s" {
		t.Errorf("unit = %v", sda["unit"])
	}
	r, _ := sda["reads"].(map[string]interface{})
	w, _ := sda["writes"].(map[string]interface{})
	if r["current"] != "100.00" || w["current"] != "200.00" || w["peak"] != "200.00" {
		t.Errorf("reads = %v, writes = %v", r, w)
	}
}

func TestNetworkAndDiskMetricsUnit(t *testing.T) {
	g := multiGraph(time.Now(), "interface", "eth0", []string{"received", "sent"}, []float64{1, 2})
	d := multiGraph(time.Now(), "disk", "sda", []string{"reads", "writes"}, []float64{1, 2})
	fake := reportingFake(t, map[string]map[string]interface{}{"interface": g, "disk": d}, map[string]interface{}{
		"interface.query":  []interface{}{map[string]interface{}{"name": "eth0"}},
		"reporting.graphs": []interface{}{map[string]interface{}{"name": "disk", "vertical_label": "Kibibytes/s", "identifiers": []interface{}{"sda"}}},
	}, nil)
	client, _ := newFakeClientFrom(t, fake)
	out, err := handleGetNetworkMetrics(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"unit": "Kilobits/s"`) {
		t.Errorf("network unit missing: %s", out)
	}
	out, err = handleGetDiskMetrics(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"unit": "Kibibytes/s"`) {
		t.Errorf("disk unit missing: %s", out)
	}
}

// Item 2: VM CPU topology and memory in MiB.
func TestSimplifyVMTopologyAndMemory(t *testing.T) {
	vm := map[string]interface{}{
		"id": 1.0, "name": "vm1", "vcpus": 2.0, "cores": 4.0, "threads": 2.0,
		"memory": 3072.0, "min_memory": 1024.0,
	}
	s := simplifyVM(vm)
	want := map[string]interface{}{
		"sockets": 2, "cores_per_socket": 4, "threads_per_core": 2, "total_vcpus": 16,
		"memory_mib": 3072, "memory_gib": "3.0 GiB", "min_memory_mib": 1024,
	}
	for k, v := range want {
		if s[k] != v {
			t.Errorf("%s = %v, want %v", k, s[k], v)
		}
	}
	for _, k := range []string{"vcpus", "cores", "threads", "memory_mb", "memory_gb"} {
		if _, ok := s[k]; ok {
			t.Errorf("old field %s still present", k)
		}
	}
	vms := []map[string]interface{}{simplifyVM(map[string]interface{}{"name": "a", "memory": 512.0}), s}
	sortVMs(vms, "memory")
	if vms[0]["name"] != "vm1" {
		t.Errorf("sort by memory: %v", vms)
	}
}

func snapProp(parsed, raw interface{}) map[string]interface{} {
	return map[string]interface{}{"value": "x", "rawvalue": raw, "source": "NONE", "parsed": parsed}
}

// Item 3: snapshot creation time and sizes from requested properties;
// holds requested so holds_only works on 27.0.
func TestQuerySnapshotsPropertiesAndHolds(t *testing.T) {
	snaps := []interface{}{
		map[string]interface{}{
			"id": "tank/data@manual", "name": "tank/data@manual", "snapshot_name": "manual", "dataset": "tank/data", "pool": "tank",
			"createtxg": "100", "holds": map[string]interface{}{"truenas": 1.0},
			"properties": map[string]interface{}{
				"creation":   snapProp(1767225600.0, "1767225600"),
				"used":       snapProp(1048576.0, "1048576"),
				"referenced": snapProp(nil, "2147483648"),
			},
		},
		map[string]interface{}{
			// Older shape: creation as a $date object.
			"id": "tank/data@auto-2025-01-01_00-00", "name": "tank/data@auto-2025-01-01_00-00", "snapshot_name": "auto-2025-01-01_00-00",
			"dataset": "tank/data", "pool": "tank", "createtxg": "50", "holds": map[string]interface{}{},
			"properties": map[string]interface{}{"creation": snapProp(map[string]interface{}{"$date": 1735689600000.0}, "1735689600")},
		},
		map[string]interface{}{
			// No properties: date from the name.
			"id": "tank/data@auto-2026-06-01_00-00", "name": "tank/data@auto-2026-06-01_00-00", "snapshot_name": "auto-2026-06-01_00-00",
			"dataset": "tank/data", "pool": "tank", "createtxg": "70", "properties": map[string]interface{}{},
		},
	}
	var gotOptions json.RawMessage
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.snapshot.query" {
			return nil, nil, false
		}
		gotOptions = params
		return snaps, nil, true
	}}
	client, _ := newFakeClientFrom(t, fake)
	out, err := handleQuerySnapshots(client, map[string]interface{}{"order_by": "created"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotOptions), `"holds":true`) || !strings.Contains(string(gotOptions), `"creation"`) {
		t.Errorf("query options = %s", gotOptions)
	}
	resp := decode(t, out)
	list := resp["snapshots"].([]interface{})
	first := list[0].(map[string]interface{})
	second := list[1].(map[string]interface{})
	third := list[2].(map[string]interface{})
	if first["snapshot_name"] != "manual" || first["created"] != "2026-01-01T00:00:00Z" || first["created_source"] != "zfs_property" {
		t.Errorf("first = %v", first)
	}
	if first["used_bytes"] != 1048576.0 || first["used"] != formatBytes(1048576) || first["referenced_bytes"] != 2147483648.0 {
		t.Errorf("sizes = %v", first)
	}
	if second["created"] != "2025-01-01T00:00:00Z" {
		t.Errorf("second = %v", second)
	}
	if third["created_date"] != "2026-06-01 00:00" || third["created_source"] != "snapshot_name" {
		t.Errorf("third = %v", third)
	}

	out, err = handleQuerySnapshots(client, map[string]interface{}{"holds_only": true})
	if err != nil {
		t.Fatal(err)
	}
	if resp := decode(t, out); resp["snapshot_count"] != 1.0 {
		t.Errorf("holds_only: %s", out)
	}
}

// Item 4: system_info, query_jobs and list_alerts emit readable times.
func TestReadableDatesAcrossTools(t *testing.T) {
	date := map[string]interface{}{"$date": 1767225600000.0}
	results := map[string]interface{}{
		"system.info": map[string]interface{}{"hostname": "nas.example.invalid", "boottime": date, "datetime": date,
			"buildtime": map[string]interface{}{"$date": 1.0}},
		"core.get_jobs": []interface{}{map[string]interface{}{"id": 1.0, "method": "pool.scrub", "state": "SUCCESS",
			"time_started": date, "time_finished": date}},
		"alert.list": []interface{}{fakeAlert("00000000-0000-0000-0000-000000000001", "WARNING", false)},
	}
	client, _ := newFakeClientWith(t, results, nil)
	out, err := handleSystemInfo(client, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := decode(t, out)
	if info["boottime"] != "2026-01-01T00:00:00Z" || info["datetime"] != "2026-01-01T00:00:00Z" {
		t.Errorf("system_info = %s", out)
	}
	// Out of range: raw value passed through, as for boot environments.
	if info["buildtime"] != "1" {
		t.Errorf("buildtime = %v", info["buildtime"])
	}
	out, err = handleQueryJobs(client, map[string]interface{}{"state": "all"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "$date") || !strings.Contains(out, `"time_started": "2026-01-01T00:00:00Z"`) {
		t.Errorf("query_jobs = %s", out)
	}
	out, err = handleListAlerts(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "$date") || !strings.Contains(out, `"2023-11-14T22:13:20Z"`) {
		t.Errorf("list_alerts = %s", out)
	}
	out, err = handleSystemHealth(client, map[string]interface{}{})
	if err == nil && strings.Contains(out, "$date") {
		t.Errorf("system_health = %s", out)
	}
}

// Item 5 and 7: gap rows excluded; last bucket past end reported.
func TestGetSystemMetricsGapsAndEnd(t *testing.T) {
	end := time.Unix(1767225600, 0)
	g := multiGraph(end, "load", "", []string{"shortterm", "midterm"},
		[]float64{0, 0}, []float64{1, 2}, []float64{0, 0}, []float64{3, 4})
	g["end"] = end.Unix() - 120 // last bucket stamped 2 minutes after end
	reported := g["aggregations"]
	client, _ := newFakeClientFrom(t, reportingFake(t, map[string]map[string]interface{}{"load": g}, nil, nil))
	out, err := handleGetSystemMetrics(client, map[string]interface{}{"graphs": []interface{}{"load"}, "unit": "MONTH"})
	if err != nil {
		t.Fatal(err)
	}
	load := decode(t, out)["load"].(map[string]interface{})
	if load["data_points_total"] != 4.0 || load["data_points_valid"] != 2.0 || load["gap_samples_excluded"] != 2.0 {
		t.Errorf("counts = %v", load)
	}
	if n := len(load["data_sample"].([]interface{})); n != 2 {
		t.Errorf("data_sample has %d rows", n)
	}
	agg := load["aggregations"].(map[string]interface{})
	if agg["mean"].(map[string]interface{})["shortterm"] != 2.0 || agg["min"].(map[string]interface{})["midterm"] != 2.0 {
		t.Errorf("aggregations = %v", agg)
	}
	if b1, _ := json.Marshal(load["aggregations_reported"]); string(b1) != mustJSON(t, reported) {
		t.Errorf("aggregations_reported = %s", b1)
	}
	if load["last_sample_after_end_seconds"] != 120.0 || load["unit"] != "Load" {
		t.Errorf("end/unit = %v", load)
	}
}

func TestGetSystemMetricsNoGapsUnchanged(t *testing.T) {
	g := multiGraph(time.Now(), "cpu", "", []string{"cpu"}, []float64{10}, []float64{20})
	client, _ := newFakeClientFrom(t, reportingFake(t, map[string]map[string]interface{}{"cpu": g}, nil, nil))
	out, err := handleGetSystemMetrics(client, map[string]interface{}{"graphs": []interface{}{"cpu"}})
	if err != nil {
		t.Fatal(err)
	}
	cpu := decode(t, out)["cpu"].(map[string]interface{})
	for _, k := range []string{"gap_samples_excluded", "aggregations_reported", "last_sample_after_end_seconds"} {
		if _, ok := cpu[k]; ok {
			t.Errorf("%s present without gaps: %v", k, cpu)
		}
	}
}

// Item 5 (related): CPU analysis ignores gap rows.
func TestAnalyzeCPUCapacitySkipsGaps(t *testing.T) {
	g := reportingGraph("cpu", "cpu", 0, 0, 50, 50, 0)
	client, _ := newFakeClientFrom(t, reportingFake(t, map[string]map[string]interface{}{"cpu": g}, nil, nil))
	a, err := analyzeCPUCapacity(client, "MONTH")
	if err != nil {
		t.Fatal(err)
	}
	if a["average_utilization_pct"] != "50.00" {
		t.Errorf("average = %v", a["average_utilization_pct"])
	}
}

// Item 6: covered by TestAnalyzeMemoryCapacityExcludesArc; without ARC
// the basis says the figure includes it.
func TestAnalyzeMemoryCapacityBasisWithoutArc(t *testing.T) {
	graphs := map[string]map[string]interface{}{"memory": reportingGraph("memory", "available", 12*gib)}
	results := map[string]interface{}{"system.info": map[string]interface{}{"hostname": "nas.example.invalid", "physmem": 16 * gib}}
	client, _ := newFakeClientFrom(t, reportingFake(t, graphs, results, nil))
	a, err := analyzeMemoryCapacity(client, "HOUR")
	if err != nil {
		t.Fatal(err)
	}
	if a["current_utilization_basis"] != "including_arc" || a["current_utilization_pct"] != "25.00" {
		t.Errorf("analysis = %v", a)
	}
}

// Item 8: system_health gives the ARC freshness caveat.
func TestSystemHealthArcFreshnessNote(t *testing.T) {
	mem := reportingGraph("memory", "available", 1*gib)
	mem["data"] = []interface{}{[]interface{}{nil, 1 * gib}}
	delete(mem, "end")
	arc := reportingGraph("arcsize", "size", 12*gib)
	arc["data"] = []interface{}{[]interface{}{nil, 12 * gib}}
	delete(arc, "end")
	results := map[string]interface{}{
		"alert.list": []interface{}{}, "core.get_jobs": []interface{}{}, "pool.query": []interface{}{},
		"system.info": map[string]interface{}{"hostname": "nas.example.invalid", "physmem": 16 * gib},
	}
	graphs := map[string]map[string]interface{}{"cpu": reportingGraph("cpu", "cpu", 10), "memory": mem, "arcsize": arc}
	client, _ := newFakeClientFrom(t, reportingFake(t, graphs, results, nil))
	out, err := handleSystemHealth(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	if resp["arc_freshness_note"] != arcFreshnessUnknownNote {
		t.Errorf("arc_freshness_note missing: %s", out)
	}

	// Fresh ARC sample: no note.
	client, _ = newFakeClientFrom(t, reportingFake(t, map[string]map[string]interface{}{
		"cpu": reportingGraph("cpu", "cpu", 10), "memory": reportingGraph("memory", "available", 1*gib),
		"arcsize": reportingGraph("arcsize", "size", 12*gib),
	}, results, nil))
	out, err = handleSystemHealth(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decode(t, out)["arc_freshness_note"]; ok {
		t.Errorf("unexpected note: %s", out)
	}
}

func TestReadableDatesHelper(t *testing.T) {
	in := map[string]interface{}{
		"a": map[string]interface{}{"$date": 1767225600000.0},
		"b": []interface{}{map[string]interface{}{"$date": 1767225600000.0}},
		"c": map[string]interface{}{"$date": 1.0, "other": true}, // not a datetime object
	}
	out := readableDatesMap(in)
	if out["a"] != "2026-01-01T00:00:00Z" || out["b"].([]interface{})[0] != "2026-01-01T00:00:00Z" {
		t.Errorf("out = %v", out)
	}
	if _, ok := out["c"].(map[string]interface{}); !ok {
		t.Errorf("c changed: %v", out["c"])
	}
	if _, ok := in["a"].(map[string]interface{}); !ok {
		t.Errorf("input mutated")
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Fixtures below mimic TrueNAS 25.04+/27.0 middleware shapes with fake values.

const gib = float64(1 << 30)

// reportingGraph is one reporting.get_data result entry (netdata export).
func reportingGraph(name, legend string, values ...float64) map[string]interface{} {
	data := make([]interface{}, len(values))
	for i, v := range values {
		data[i] = []interface{}{float64(1700000000 + i*10), v}
	}
	return map[string]interface{}{
		"name":         name,
		"identifier":   nil,
		"data":         data,
		"aggregations": map[string]interface{}{"min": values[0], "mean": values[0], "max": values[0]},
		"start":        1700000000,
		"end":          1700003600,
		"legend":       []interface{}{"time", legend},
	}
}

// middlewareError is the JSON-RPC error TrueNAS returns for a failed call,
// including a traceback the tool must not surface.
func middlewareError(errname, reason string) map[string]interface{} {
	return map[string]interface{}{
		"code":    -32001,
		"message": "Method call error",
		"data": map[string]interface{}{
			"error":   2,
			"errname": errname,
			"reason":  reason,
			"trace": map[string]interface{}{
				"class":     "CallError",
				"formatted": "Traceback (most recent call last):\n  File \"/usr/lib/python3/dist-packages/middlewared/main.py\", line 1, in call\nmiddlewared.service_exception.CallError: [" + errname + "] " + reason,
			},
			"extra": nil,
		},
	}
}

// reportingGraphName extracts the graph name from reporting.get_data params.
func reportingGraphName(t *testing.T, params json.RawMessage) string {
	t.Helper()
	var p []json.RawMessage
	if err := json.Unmarshal(params, &p); err != nil || len(p) == 0 {
		t.Fatalf("bad reporting.get_data params %s: %v", params, err)
	}
	var graphs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(p[0], &graphs); err != nil || len(graphs) == 0 {
		t.Fatalf("bad graphs param %s: %v", p[0], err)
	}
	return graphs[0].Name
}

// reportingFake answers reporting.get_data per graph name: a value in graphs
// is the result entry, a missing graph gets errFor(name).
func reportingFake(t *testing.T, graphs map[string]map[string]interface{}, results map[string]interface{}, errFor func(string) map[string]interface{}) *fakeMiddleware {
	return &fakeMiddleware{
		results: results,
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method != "reporting.get_data" {
				return nil, nil, false
			}
			name := reportingGraphName(t, params)
			if g, ok := graphs[name]; ok {
				return []interface{}{g}, nil, true
			}
			if errFor == nil {
				return nil, middlewareError("ENOENT", "'"+name+"' is not a valid graph plugin."), true
			}
			return nil, errFor(name), true
		},
	}
}

func TestGetArcMetricsAllFailedIsError(t *testing.T) {
	fake := reportingFake(t, nil, nil, func(string) map[string]interface{} {
		return middlewareError("EFAULT", "Failed to connect to netdata")
	})
	client, _ := newFakeClientFrom(t, fake)

	out, err := handleGetArcMetrics(client, map[string]interface{}{})
	if err == nil {
		t.Fatalf("expected an error when every graph fails, got output %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "Failed to connect to netdata") || !strings.Contains(msg, "arcsize") {
		t.Errorf("error should name the graph and reason, got %q", msg)
	}
	for _, leak := range []string{"Traceback", "Trace:", "Params:", "\n"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error should be concise, found %q in %q", leak, msg)
		}
	}

	// Through the registry the failure reaches the MCP layer as an error.
	r := NewRegistry(client, nil)
	if _, err := r.CallTool("get_arc_metrics", map[string]interface{}{}); err == nil {
		t.Error("CallTool should return an error (isError=true) when every graph fails")
	}
}

func TestGetArcMetricsUnknownGraphRejected(t *testing.T) {
	client, fake := newFakeClient(t)
	_, err := handleGetArcMetrics(client, map[string]interface{}{"graphs": []interface{}{"arcsize", "arcrate"}})
	if err == nil || !strings.Contains(err.Error(), `"arcrate"`) || !strings.Contains(err.Error(), "supported graphs on this server: arcsize") {
		t.Fatalf("expected unknown graph error naming arcrate and listing arcsize, got %v", err)
	}
	if calls := fake.called("reporting.get_data"); len(calls) != 0 {
		t.Errorf("no reporting call should be made for an invalid request, got %d", len(calls))
	}
}

func TestGetArcMetricsPartialFailure(t *testing.T) {
	graphs := map[string]map[string]interface{}{
		"arcsize": reportingGraph("arcsize", "size", 4*gib, 4.5*gib),
	}
	// Discovery offers both graphs but the server fails one of them.
	results := map[string]interface{}{"reporting.graphs": reportingGraphsResult("arcfreememory", "arcsize")}
	fake := reportingFake(t, graphs, results, func(name string) map[string]interface{} {
		return middlewareError("ENOENT", "'"+name+"' is not a valid graph plugin.")
	})
	client, _ := newFakeClientFrom(t, fake)

	out, err := handleGetArcMetrics(client, map[string]interface{}{"graphs": []interface{}{"arcsize", "arcfreememory"}})
	if err != nil {
		t.Fatalf("partial success should not be an error: %v", err)
	}
	if strings.Contains(out, "Traceback") {
		t.Errorf("output leaks a traceback: %s", out)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	size, _ := resp["arcsize"].(map[string]interface{})
	if size["data_points_total"] != float64(2) {
		t.Errorf("arcsize summary missing data: %v", resp["arcsize"])
	}
	errs, _ := resp["errors"].(map[string]interface{})
	if got, _ := errs["arcfreememory"].(string); !strings.Contains(got, "not a valid graph plugin") || strings.Contains(got, "\n") {
		t.Errorf("errors.arcfreememory = %q, want concise middleware reason", got)
	}
}

// graphs27 is the reporting.get_data graph name enum on 27.0.
var graphs27 = []string{
	"cpu", "cputemp", "disk", "interface", "load", "processes", "memory", "uptime",
	"arcsize", "disktemp", "upscharge", "upsruntime", "upsvoltage", "upscurrent", "upsfrequency",
	"upsload", "upstemperature",
}

// coreGetMethodsResult mimics core.get_methods("reporting"): accepts is the
// list of argument JSON schemas with refs inlined.
func coreGetMethodsResult(graphNames []string) map[string]interface{} {
	enum := make([]interface{}, len(graphNames))
	for i, n := range graphNames {
		enum[i] = n
	}
	return map[string]interface{}{
		"reporting.get_data": map[string]interface{}{
			"description": "Get reporting data for given graphs.",
			"accepts": []interface{}{
				map[string]interface{}{
					"title": "graphs",
					"type":  "array",
					"items": map[string]interface{}{
						"title": "GraphIdentifier",
						"type":  "object",
						"properties": map[string]interface{}{
							"name":       map[string]interface{}{"enum": enum, "type": "string", "title": "Name"},
							"identifier": map[string]interface{}{"anyOf": []interface{}{map[string]interface{}{"type": "string"}, map[string]interface{}{"type": "null"}}},
						},
					},
				},
				map[string]interface{}{
					"title": "query",
					"type":  "object",
					"properties": map[string]interface{}{
						"unit": map[string]interface{}{"enum": []interface{}{"HOUR", "DAY", "WEEK", "MONTH", "YEAR"}},
					},
				},
			},
		},
		"reporting.graphs": map[string]interface{}{"accepts": []interface{}{}},
	}
}

// reportingGraphsResult mimics reporting.graphs items.
func reportingGraphsResult(names ...string) []interface{} {
	out := make([]interface{}, len(names))
	for i, n := range names {
		out[i] = map[string]interface{}{"name": n, "title": n, "vertical_label": "bytes", "identifiers": nil}
	}
	return out
}

// pluginNames27 are the graph plugins reporting.graphs lists on 27.0: more
// ARC plugins than reporting.get_data accepts.
var pluginNames27 = []string{
	"cpu", "memory", "arcfreememory", "arcavailablememory", "arcsize",
	"demandaccessespersecond", "l2archhitspersecond", "disktemp",
}

func validationError(name string) map[string]interface{} {
	return middlewareError("EINVAL", "graphs.0.name: Input should be 'cpu','cputemp','disk','interface','load','processes','memory','uptime','arcsize','disktemp' or 'ups...' (got "+name+")")
}

func TestGetArcMetricsDefault27OnlySupportedGraphs(t *testing.T) {
	graphs := map[string]map[string]interface{}{
		"arcsize": reportingGraph("arcsize", "size", 4*gib, 4.5*gib),
	}
	results := map[string]interface{}{
		"core.get_methods": coreGetMethodsResult(graphs27),
		"reporting.graphs": reportingGraphsResult(pluginNames27...),
	}
	fake := reportingFake(t, graphs, results, func(name string) map[string]interface{} {
		t.Errorf("unsupported graph requested: %s", name)
		return validationError(name)
	})
	client, fake := newFakeClientFrom(t, fake)

	for i := 0; i < 2; i++ {
		out, err := handleGetArcMetrics(client, map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		if _, ok := resp["arcsize"]; !ok || len(resp) != 1 {
			t.Errorf("want only arcsize and no errors, got %s", out)
		}
	}
	if n := len(fake.called("core.get_methods")); n != 1 {
		t.Errorf("discovery should be cached per connection, core.get_methods called %d times", n)
	}
	if n := len(fake.called("reporting.graphs")); n != 1 {
		t.Errorf("discovery should be cached per connection, reporting.graphs called %d times", n)
	}

	// A graph the 27.0 schema rejects gets a clean error, not a call.
	_, err := handleGetArcMetrics(client, map[string]interface{}{"graphs": []interface{}{"arcfreememory"}})
	if err == nil || !strings.Contains(err.Error(), "supported graphs on this server: arcsize") {
		t.Errorf("expected unsupported graph error listing arcsize, got %v", err)
	}
}

func TestSupportedArcGraphsIntersectsSchemaAndPlugins(t *testing.T) {
	// 25.x shape: the schema enum carries the older ARC graph names.
	accepted := []string{"cpu", "memory", "arcactualrate", "arcrate", "arcsize", "arcresult", "disktemp"}
	results := map[string]interface{}{
		"core.get_methods": coreGetMethodsResult(accepted),
		"reporting.graphs": reportingGraphsResult("cpu", "memory", "arcsize", "arcrate", "arcresult", "arcactualrate", "arcfreememory"),
	}
	client, _ := newFakeClientWith(t, results, nil)
	got := supportedArcGraphs(client)
	want := []string{"arcactualrate", "arcrate", "arcresult", "arcsize"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("supportedArcGraphs = %v, want %v", got, want)
	}
}

func TestSupportedArcGraphsFallback(t *testing.T) {
	fake := &fakeMiddleware{handle: func(method string, _ json.RawMessage) (interface{}, map[string]interface{}, bool) {
		switch method {
		case "core.get_methods", "reporting.graphs":
			return nil, middlewareError("EPERM", "Not authorized"), true
		}
		return nil, nil, false
	}}
	client, _ := newFakeClientFrom(t, fake)
	if got := supportedArcGraphs(client); strings.Join(got, ",") != "arcsize" {
		t.Errorf("fallback = %v, want [arcsize]", got)
	}

	// No discovery data at all (null results) also falls back.
	client2, _ := newFakeClient(t)
	if got := supportedArcGraphs(client2); strings.Join(got, ",") != "arcsize" {
		t.Errorf("fallback with null results = %v, want [arcsize]", got)
	}
}

func TestConciseError(t *testing.T) {
	err := errors.New("API error: [ENOENT] 'x' is not a valid graph plugin. (code 2)\n\nRequest:\n  Method: reporting.get_data\n\nTrace: Traceback ...")
	if got := conciseError(err); got != "API error: [ENOENT] 'x' is not a valid graph plugin. (code 2)" {
		t.Errorf("conciseError = %q", got)
	}
}

func TestAnalyzeMemoryCapacityUsesAvailable(t *testing.T) {
	// 16 GiB total, 12 GiB available -> 4 GiB (25%) used.
	graphs := map[string]map[string]interface{}{
		"memory": reportingGraph("memory", "available", 12*gib, 12*gib, 12*gib, 12*gib, 12*gib),
	}
	results := map[string]interface{}{
		"system.info": map[string]interface{}{"hostname": "nas.example.invalid", "physmem": 16 * gib},
	}
	client, _ := newFakeClientFrom(t, reportingFake(t, graphs, results, nil))

	analysis, err := analyzeMemoryCapacity(client, "HOUR")
	if err != nil {
		t.Fatal(err)
	}
	if got := analysis["current_utilization_pct"]; got != "25.00" {
		t.Errorf("current_utilization_pct = %v, want 25.00 (used = total - available)", got)
	}
	if got := analysis["current_used_bytes"]; got != int64(4*gib) {
		t.Errorf("current_used_bytes = %v, want %d", got, int64(4*gib))
	}
	if got := analysis["current_available_bytes"]; got != int64(12*gib) {
		t.Errorf("current_available_bytes = %v, want %d", got, int64(12*gib))
	}
	if analysis["capacity_status"] != determineCapacityStatus(25, 70, 85) {
		t.Errorf("capacity_status = %v", analysis["capacity_status"])
	}
	// No arcsize graph: keep the plain figure and say it may include ARC.
	if note, _ := analysis["note"].(string); !strings.Contains(note, "may include the ZFS ARC") {
		t.Errorf("note = %q, want ARC caveat", note)
	}
	if _, ok := analysis["arc_bytes"]; ok {
		t.Errorf("arc_bytes should be absent without ARC data")
	}
}

func TestAnalyzeMemoryCapacityExcludesArc(t *testing.T) {
	// 100 GiB total, 11 GiB available -> 89 GiB (89%) used, of which
	// 72 GiB is ARC -> 17 GiB (17%) used excluding ARC: healthy.
	graphs := map[string]map[string]interface{}{
		"memory":  reportingGraph("memory", "available", 11*gib, 11*gib, 11*gib, 11*gib, 11*gib),
		"arcsize": reportingGraph("arcsize", "size", 70*gib, 71*gib, 72*gib),
	}
	results := map[string]interface{}{
		"system.info": map[string]interface{}{"hostname": "nas.example.invalid", "physmem": 100 * gib},
	}
	client, _ := newFakeClientFrom(t, reportingFake(t, graphs, results, nil))

	analysis, err := analyzeMemoryCapacity(client, "WEEK")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]interface{}{
		"current_utilization_pct":       "89.00",
		"arc_bytes":                     int64(72 * gib),
		"used_excluding_arc_bytes":      int64(17 * gib),
		"utilization_excluding_arc_pct": "17.00",
		"capacity_status":               "healthy",
	}
	for k, want := range checks {
		if got := analysis[k]; got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	if _, ok := analysis["note"]; ok {
		t.Errorf("unexpected note with ARC data: %v", analysis["note"])
	}
	recs := generateCapacityRecommendations(map[string]interface{}{"memory": analysis})
	if b, _ := json.Marshal(recs); strings.Contains(string(b), "RAM") {
		t.Errorf("no RAM recommendation expected when non-ARC usage is low: %s", b)
	}
}

func TestAnalyzeMemoryCapacitySkipsNullSamples(t *testing.T) {
	// MONTH data with gaps: null, zero and short rows must not read as
	// "0 bytes available" (100% used).
	mem := reportingGraph("memory", "available", 4*gib)
	mem["data"] = []interface{}{
		[]interface{}{float64(1700000000), 4 * gib},
		[]interface{}{float64(1700000300), nil},
		[]interface{}{float64(1700000600), float64(0)},
		[]interface{}{float64(1700000900)},
		[]interface{}{float64(1700001200), 4 * gib},
		[]interface{}{float64(1700001500), 4 * gib},
	}
	graphs := map[string]map[string]interface{}{"memory": mem}
	results := map[string]interface{}{
		"system.info": map[string]interface{}{"hostname": "nas.example.invalid", "physmem": 16 * gib},
	}
	client, _ := newFakeClientFrom(t, reportingFake(t, graphs, results, nil))

	analysis, err := analyzeMemoryCapacity(client, "MONTH")
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]interface{}{
		"peak_utilization_pct":    "75.00",
		"average_utilization_pct": "75.00",
		"current_utilization_pct": "75.00",
		"trend":                   "stable",
		"samples_used":            3,
	} {
		if got := analysis[k]; got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
}

func TestMemoryUsedPct(t *testing.T) {
	cases := []struct{ total, avail, want float64 }{
		{16 * gib, 4 * gib, 75},
		{16 * gib, 16 * gib, 0},
		{16 * gib, 20 * gib, 0}, // clamped
		{0, 1, 0},
	}
	for _, c := range cases {
		if got := memoryUsedPct(c.total, c.avail); got != c.want {
			t.Errorf("memoryUsedPct(%v, %v) = %v, want %v", c.total, c.avail, got, c.want)
		}
	}
}

func systemHealthFake(t *testing.T, alerts []interface{}, availableMem float64) *fakeMiddleware {
	graphs := map[string]map[string]interface{}{
		"cpu":    reportingGraph("cpu", "cpu", 10),
		"memory": reportingGraph("memory", "available", availableMem),
	}
	results := map[string]interface{}{
		"alert.list":    alerts,
		"core.get_jobs": []interface{}{},
		"pool.query":    []interface{}{},
		"system.info":   map[string]interface{}{"hostname": "nas.example.invalid", "physmem": 16 * gib},
	}
	return reportingFake(t, graphs, results, func(string) map[string]interface{} {
		return middlewareError("ENOENT", "invalid")
	})
}

func fakeAlert(uuid, level string, dismissed bool) map[string]interface{} {
	return map[string]interface{}{
		"uuid":      uuid,
		"klass":     "ExampleAlert",
		"level":     level,
		"formatted": "Example alert " + uuid,
		"dismissed": dismissed,
		"datetime":  map[string]interface{}{"$date": 1700000000000},
	}
}

func TestSystemHealthExcludesDismissedAlerts(t *testing.T) {
	alerts := []interface{}{
		fakeAlert("00000000-0000-0000-0000-000000000001", "CRITICAL", true),
		fakeAlert("00000000-0000-0000-0000-000000000002", "WARNING", false),
		fakeAlert("00000000-0000-0000-0000-000000000003", "CRITICAL", false),
		fakeAlert("00000000-0000-0000-0000-000000000004", "ERROR", true),
	}
	client, _ := newFakeClientFrom(t, systemHealthFake(t, alerts, 12*gib))

	out, err := handleSystemHealth(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["alert_count"] != float64(2) {
		t.Errorf("alert_count = %v, want 2 (dismissed excluded)", resp["alert_count"])
	}
	if resp["critical_alert_count"] != float64(1) {
		t.Errorf("critical_alert_count = %v, want 1", resp["critical_alert_count"])
	}
	if resp["dismissed_alert_count"] != float64(2) {
		t.Errorf("dismissed_alert_count = %v, want 2", resp["dismissed_alert_count"])
	}
	if resp["health_check"] != "ALERTS_PRESENT" {
		t.Errorf("health_check = %v, want ALERTS_PRESENT", resp["health_check"])
	}
}

func TestSystemHealthOnlyDismissedAlertsIsOK(t *testing.T) {
	alerts := []interface{}{fakeAlert("00000000-0000-0000-0000-000000000001", "CRITICAL", true)}
	client, _ := newFakeClientFrom(t, systemHealthFake(t, alerts, 12*gib))

	out, err := handleSystemHealth(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["health_check"] != "OK" || resp["alert_count"] != float64(0) || resp["critical_alert_count"] != float64(0) {
		t.Errorf("only dismissed alerts should be healthy: %s", out)
	}
}

func TestSystemHealthMemoryWarningUsesAvailable(t *testing.T) {
	// 12 GiB of 16 GiB available is 25% used: no warning (the old maths
	// reported 75% "used"). 1 GiB available is 93.75% used: critical.
	client, _ := newFakeClientFrom(t, systemHealthFake(t, nil, 12*gib))
	out, err := handleSystemHealth(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Memory utilization") {
		t.Errorf("unexpected memory warning with 75%% available: %s", out)
	}

	client, _ = newFakeClientFrom(t, systemHealthFake(t, nil, 1*gib))
	out, err = handleSystemHealth(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Memory utilization critical: 93.8% (may include ZFS ARC cache)") {
		t.Errorf("expected critical memory warning, got %s", out)
	}
}

func TestSystemHealthMemoryWarningExcludesArc(t *testing.T) {
	// 1 GiB of 16 GiB available, but 12 GiB is ARC: 18.75% used excluding
	// ARC, so no warning.
	fake := systemHealthFake(t, nil, 1*gib)
	// systemHealthFake has no arcsize graph; wrap it to add one.
	inner := fake.handle
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "reporting.get_data" && reportingGraphName(t, params) == "arcsize" {
			return []interface{}{reportingGraph("arcsize", "size", 12*gib)}, nil, true
		}
		return inner(method, params)
	}
	client, _ := newFakeClientFrom(t, fake)
	out, err := handleSystemHealth(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Memory utilization") {
		t.Errorf("ARC should not trigger a memory warning: %s", out)
	}
}

func TestParseBootEnvCreated(t *testing.T) {
	cases := []struct {
		name   string
		in     interface{}
		want   string
		wantTS int64
	}{
		{"25.04+ $date ms", map[string]interface{}{"$date": float64(1736038861000)}, "2025-01-05T01:01:01Z", 1736038861},
		{"RFC3339 string", "2025-01-05T01:01:01Z", "2025-01-05T01:01:01Z", 1736038861},
		{"offset string", "2025-01-04T17:01:01-08:00", "2025-01-05T01:01:01Z", 1736038861},
		{"naive string", "2025-01-05T01:01:01", "2025-01-05T01:01:01Z", 1736038861},
		{"epoch seconds", float64(1736038861), "2025-01-05T01:01:01Z", 1736038861},
		{"epoch ms", float64(1736038861000), "2025-01-05T01:01:01Z", 1736038861},
		{"missing", nil, "", 0},
		{"garbage", "not a date", "", 0},
		{"bad map", map[string]interface{}{"$time": "01:01:01"}, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ts := parseBootEnvCreated(c.in)
			if got != c.want || ts != c.wantTS {
				t.Errorf("parseBootEnvCreated(%v) = (%q, %d), want (%q, %d)", c.in, got, ts, c.want, c.wantTS)
			}
		})
	}
}

func TestQueryBootEnvironmentsDateObject(t *testing.T) {
	// boot.environment.query on 25.04+/27.0: "created" is a datetime the
	// middleware serialises as {"$date": ms}.
	envs := []interface{}{
		map[string]interface{}{
			"id": "25.04.0", "dataset": "boot-pool/ROOT/25.04.0", "active": false, "activated": false,
			"created": map[string]interface{}{"$date": float64(1736038861000)}, "used_bytes": float64(1 << 30),
			"used": "1 GiB", "keep": false, "can_activate": true,
		},
		map[string]interface{}{
			"id": "27.0.0-RC.1", "dataset": "boot-pool/ROOT/27.0.0-RC.1", "active": true, "activated": true,
			"created": map[string]interface{}{"$date": float64(1767225600000)}, "used_bytes": float64(2 << 30),
			"used": "2 GiB", "keep": false, "can_activate": true,
		},
	}
	client, _ := newFakeClientWith(t, map[string]interface{}{"boot.environment.query": envs}, nil)

	out, err := handleQueryBootEnvironments(client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		BootEnvironments []struct {
			ID               string `json:"id"`
			Created          string `json:"created"`
			CreatedTimestamp int64  `json:"created_timestamp"`
		} `json:"boot_environments"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.BootEnvironments) != 2 {
		t.Fatalf("got %d boot environments", len(resp.BootEnvironments))
	}
	// Newest first.
	first, second := resp.BootEnvironments[0], resp.BootEnvironments[1]
	if first.ID != "27.0.0-RC.1" || first.Created != "2026-01-01T00:00:00Z" || first.CreatedTimestamp != 1767225600 {
		t.Errorf("first = %+v", first)
	}
	if second.ID != "25.04.0" || second.Created != "2025-01-05T01:01:01Z" || second.CreatedTimestamp != 1736038861 {
		t.Errorf("second = %+v", second)
	}
}

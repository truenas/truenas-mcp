package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func capacityDetailsRegistry(t *testing.T, fake *fakeMiddleware) *Registry {
	t.Helper()
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil)
}

func samplePools() []interface{} {
	return []interface{}{
		map[string]interface{}{"name": "tank", "status": "ONLINE", "healthy": true, "allocated": 1.0e12, "free": 2.0e12},
		map[string]interface{}{"name": "tank2", "status": "ONLINE", "healthy": true, "allocated": 1.0e11, "free": 2.0e11},
	}
}

func findPool(resp map[string]interface{}, name string) (map[string]interface{}, bool) {
	list, _ := resp["pools"].([]interface{})
	for _, raw := range list {
		p, _ := raw.(map[string]interface{})
		if p["name"] == name {
			return p, true
		}
	}
	return nil, false
}

func findPoolDataset(pool map[string]interface{}, name string) (map[string]interface{}, bool) {
	list, _ := pool["datasets"].([]interface{})
	for _, raw := range list {
		ds, _ := raw.(map[string]interface{})
		if ds["name"] == name {
			return ds, true
		}
	}
	return nil, false
}

func datasetQueryCalls(t *testing.T, fake *fakeMiddleware) []struct {
	filters []interface{}
	options map[string]interface{}
} {
	t.Helper()
	var out []struct {
		filters []interface{}
		options map[string]interface{}
	}
	for _, c := range fake.called("pool.dataset.query") {
		filters, options := parseQueryParams(t, c.Params)
		out = append(out, struct {
			filters []interface{}
			options map[string]interface{}
		}{filters, options})
	}
	return out
}

func hasTypeInFilter(filters []interface{}) bool {
	for _, item := range filters {
		f, ok := item.([]interface{})
		if !ok || len(f) < 3 || f[0] != "type" || f[1] != "in" {
			continue
		}
		list, _ := f[2].([]interface{})
		hasFS, hasVol := false, false
		for _, v := range list {
			switch v {
			case "FILESYSTEM":
				hasFS = true
			case "VOLUME":
				hasVol = true
			}
		}
		if hasFS && hasVol {
			return true
		}
	}
	return false
}

func TestGetPoolCapacityDetailsPoolFilterAndTopLevelUsed(t *testing.T) {
	rows := sampleDatasets()
	// Nested properties.used is a decoy: real middleware flattens used to
	// the top level and deletes properties. The old parser reads only the
	// nested key and would report this value instead of the top-level 200.
	for _, raw := range rows {
		ds, _ := raw.(map[string]interface{})
		if ds["name"] == "tank/data" {
			ds["properties"] = map[string]interface{}{
				"used": map[string]interface{}{"parsed": 99999.0, "rawvalue": "99999", "value": "decoy"},
			}
		}
	}
	fake := &fakeMiddleware{
		results: map[string]interface{}{"pool.query": samplePools()},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method != "pool.dataset.query" {
				return nil, nil, false
			}
			return datasetQueryLikeMiddleware(t, params, rows), nil, true
		},
	}
	r := capacityDetailsRegistry(t, fake)

	out, err := r.CallTool(t.Context(), "get_pool_capacity_details", map[string]interface{}{"pool_name": "tank"})
	if err != nil {
		t.Fatal(err)
	}
	calls := datasetQueryCalls(t, fake)
	if len(calls) != 1 {
		t.Fatalf("dataset.query calls = %d, want 1; body=%s", len(calls), out)
	}
	filters, options := calls[0].filters, calls[0].options
	if !hasFilter(filters, "pool", "=", "tank") {
		t.Errorf("pool filter = %v, want [pool, =, tank]; params=%v", filters, filters)
	}
	if hasFilter(filters, "name", "^", "tank") {
		t.Errorf("still using name prefix filter: %v", filters)
	}
	extra, _ := options["extra"].(map[string]interface{})
	if extra["retrieve_children"] != false {
		t.Errorf("retrieve_children = %v, want false; options=%v", extra["retrieve_children"], options)
	}
	props, _ := extra["properties"].([]interface{})
	if len(props) == 0 {
		t.Errorf("missing extra.properties: %v", options)
	}
	if !hasTypeInFilter(filters) {
		t.Errorf("missing type IN [FILESYSTEM, VOLUME] filter; filters=%v", filters)
	}

	resp := decode(t, out)
	tank, ok := findPool(resp, "tank")
	if !ok {
		t.Fatalf("missing pool tank; body=%s", out)
	}
	if _, ok := findPool(resp, "tank2"); ok {
		t.Errorf("pool_name=tank included tank2; body=%s", out)
	}
	if _, ok := findPoolDataset(tank, "tank2/media"); ok {
		t.Errorf("tank analysis included tank2/media; body=%s", out)
	}
	data, ok := findPoolDataset(tank, "tank/data")
	if !ok {
		t.Fatalf("missing tank/data; body=%s", out)
	}
	if data["used_bytes"] != 200.0 {
		t.Errorf("tank/data used_bytes = %v (%T), want 200 from the top-level used property (not the nested decoy 99999); body=%s", data["used_bytes"], data["used_bytes"], out)
	}
	if data["available_bytes"] != 1.0 {
		t.Errorf("tank/data available_bytes = %v, want 1 from the top-level available property; body=%s", data["available_bytes"], out)
	}
	if tank["total_datasets"] != 3.0 {
		t.Errorf("total_datasets = %v, want 3; body=%s", tank["total_datasets"], out)
	}
}

func TestGetPoolCapacityDetailsBoundsTopN(t *testing.T) {
	n := defaultQueryLimit + 10
	rows := make([]interface{}, 0, n)
	prop := func(v float64) map[string]interface{} {
		return map[string]interface{}{"parsed": v, "value": "x"}
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tank/ds%02d", i)
		used := float64(i)
		if i == 0 {
			// Put the largest used value on an early name so a missing
			// sort cannot hide behind "last in is largest".
			used = float64(n + 100)
		}
		rows = append(rows, map[string]interface{}{
			"name": name, "type": "FILESYSTEM", "pool": "tank",
			"used": prop(used), "available": prop(1),
		})
	}
	fake := &fakeMiddleware{
		results: map[string]interface{}{"pool.query": []interface{}{
			map[string]interface{}{"name": "tank", "status": "ONLINE", "healthy": true, "allocated": 1.0, "free": 1.0},
		}},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method != "pool.dataset.query" {
				return nil, nil, false
			}
			return datasetQueryLikeMiddleware(t, params, rows), nil, true
		},
	}
	r := capacityDetailsRegistry(t, fake)
	out, err := r.CallTool(t.Context(), "get_pool_capacity_details", map[string]interface{}{"pool_name": "tank"})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	tank, ok := findPool(resp, "tank")
	if !ok {
		t.Fatalf("missing pool tank; body=%s", out)
	}
	list, _ := tank["datasets"].([]interface{})
	if tank["total_datasets"] != float64(n) {
		t.Errorf("total_datasets = %v, want %d; body=%s", tank["total_datasets"], n, out)
	}
	if tank["dataset_count"] != float64(defaultQueryLimit) || len(list) != defaultQueryLimit {
		t.Errorf("dataset_count = %v len=%d, want %d; body=%s", tank["dataset_count"], len(list), defaultQueryLimit, out)
	}
	note, _ := tank["datasets_note"].(string)
	if !strings.Contains(note, fmt.Sprintf("%d of %d", defaultQueryLimit, n)) {
		t.Errorf("datasets_note = %q, want a %d of %d limited note; body=%s", note, defaultQueryLimit, n, out)
	}
	if len(list) == 0 {
		t.Fatal("no datasets returned")
	}
	first, _ := list[0].(map[string]interface{})
	if first["name"] != "tank/ds00" || first["used_bytes"] != float64(n+100) {
		t.Errorf("first dataset = %v, want tank/ds00 used_bytes=%d (largest by used, early name); body=%s", first, n+100, out)
	}
	if len(list) < 2 {
		t.Fatal("want at least two datasets so a missing sort is visible")
	}
	second, _ := list[1].(map[string]interface{})
	// Next-largest is the last inserted name (used=n-1).
	wantSecond := fmt.Sprintf("tank/ds%02d", n-1)
	if second["name"] != wantSecond || second["used_bytes"] != float64(n-1) {
		t.Errorf("second dataset = %v, want %s used_bytes=%d; body=%s", second, wantSecond, n-1, out)
	}
}

func TestSortDatasetsUsedTiesBreakByName(t *testing.T) {
	datasets := []map[string]interface{}{
		{"name": "tank/b", "used_bytes": 100.0},
		{"name": "tank/a", "used_bytes": 100.0},
		{"name": "tank/c", "used_bytes": 50.0},
	}
	sortDatasets(datasets, "used")
	if datasets[0]["name"] != "tank/a" || datasets[1]["name"] != "tank/b" || datasets[2]["name"] != "tank/c" {
		t.Fatalf("sortDatasets used ties = %v, want tank/a, tank/b, tank/c (used desc, name asc on ties)", datasets)
	}
}

func TestGetPoolCapacityDetailsReportsQueryAndParseErrors(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		fake := &fakeMiddleware{
			results: map[string]interface{}{"pool.query": []interface{}{
				map[string]interface{}{"name": "tank", "status": "ONLINE", "healthy": true},
			}},
			handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
				if method != "pool.dataset.query" {
					return nil, nil, false
				}
				return nil, map[string]interface{}{"code": 1, "message": "dataset query failed"}, true
			},
		}
		r := capacityDetailsRegistry(t, fake)
		out, err := r.CallTool(t.Context(), "get_pool_capacity_details", map[string]interface{}{"pool_name": "tank"})
		if err != nil {
			t.Fatalf("tool error = %v, want a result that reports the query failure; body=%s", err, out)
		}
		resp := decode(t, out)
		tank, ok := findPool(resp, "tank")
		if !ok {
			t.Fatalf("missing pool tank; body=%s", out)
		}
		msg, _ := tank["datasets_error"].(string)
		if !strings.Contains(msg, "dataset query failed") {
			t.Errorf("datasets_error = %q, want it to mention the query failure; body=%s", msg, out)
		}
		if _, ok := tank["datasets"]; !ok {
			t.Errorf("datasets key was dropped after a query error; body=%s", out)
		}
	})
	t.Run("parse", func(t *testing.T) {
		fake := &fakeMiddleware{
			results: map[string]interface{}{"pool.query": []interface{}{
				map[string]interface{}{"name": "tank", "status": "ONLINE", "healthy": true},
			}},
			handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
				if method != "pool.dataset.query" {
					return nil, nil, false
				}
				return "not-a-dataset-list", nil, true
			},
		}
		r := capacityDetailsRegistry(t, fake)
		out, err := r.CallTool(t.Context(), "get_pool_capacity_details", map[string]interface{}{"pool_name": "tank"})
		if err != nil {
			t.Fatalf("tool error = %v, want a result that reports the parse failure; body=%s", err, out)
		}
		resp := decode(t, out)
		tank, ok := findPool(resp, "tank")
		if !ok {
			t.Fatalf("missing pool tank; body=%s", out)
		}
		msg, _ := tank["datasets_error"].(string)
		if msg == "" {
			t.Errorf("datasets_error missing after a parse failure; body=%s", out)
		}
		if _, ok := tank["datasets"]; !ok {
			t.Errorf("datasets key was dropped after a parse error; body=%s", out)
		}
	})
}

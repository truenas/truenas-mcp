package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

// TestGetPoolCapacityDetailsLimitsFullRowQuery checks that
// get_pool_capacity_details asks the middleware for only the top
// datasets (order_by used desc, name asc, limit) and gets totals and
// children_count from a compact name listing instead of full rows.
func TestGetPoolCapacityDetailsLimitsFullRowQuery(t *testing.T) {
	prop := func(v float64) map[string]interface{} {
		return map[string]interface{}{"parsed": v, "value": "x"}
	}
	row := func(name string, used float64) map[string]interface{} {
		return map[string]interface{}{
			"name": name, "type": "FILESYSTEM", "pool": "tank", "encrypted": false,
			"used": prop(used), "available": prop(1),
		}
	}
	const distinct = 30
	n := defaultQueryLimit + 40
	children := n - 2
	rows := []interface{}{row("tank", 1e9), row("tank/p", 1e8)}
	// Insert children in reverse name order. The first `distinct` children
	// have distinct used values; the rest all tie at 7 and straddle the
	// limit, so only a server-side name tie-break picks the lowest names.
	for i := children - 1; i >= 0; i-- {
		used := 7.0
		if i < distinct {
			used = float64(1000 - i)
		}
		rows = append(rows, row(fmt.Sprintf("tank/p/c%03d", i), used))
	}

	type call struct{ options map[string]interface{} }
	var calls []call
	fake := &fakeMiddleware{
		results: map[string]interface{}{"pool.query": []interface{}{
			map[string]interface{}{"name": "tank", "status": "ONLINE", "healthy": true, "allocated": 1.0, "free": 1.0},
		}},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method != "pool.dataset.query" {
				return nil, nil, false
			}
			_, options := parseQueryParams(t, params)
			calls = append(calls, call{options})
			if options["count"] == true {
				return float64(len(rows)), nil, true
			}
			return datasetQueryLikeMiddleware(t, params, rows), nil, true
		},
	}
	r := capacityDetailsRegistry(t, fake)
	out, err := r.CallTool(t.Context(), "get_pool_capacity_details", map[string]interface{}{"pool_name": "tank"})
	if err != nil {
		t.Fatal(err)
	}

	// Every dataset query must be either limited or a compact listing.
	heavy := 0
	for i, c := range calls {
		sel, _ := c.options["select"].([]interface{})
		compact := len(sel) > 0
		for _, f := range sel {
			if f != "name" && f != "encrypted" {
				compact = false
			}
		}
		if compact || c.options["count"] == true {
			continue
		}
		heavy++
		if lim, ok := c.options["limit"].(float64); !ok || int(lim) != defaultQueryLimit {
			t.Errorf("full-row call %d: limit = %v, want %d; options=%v", i, c.options["limit"], defaultQueryLimit, c.options)
		}
		order, _ := c.options["order_by"].([]interface{})
		if len(order) < 2 || order[0] != "-used.parsed" || order[1] != "name" {
			t.Errorf("full-row call %d: order_by = %v, want [-used.parsed name]; options=%v", i, c.options["order_by"], c.options)
		}
	}
	if heavy != 1 {
		t.Errorf("full-row dataset queries = %d, want 1; body=%s", heavy, out)
	}

	resp := decode(t, out)
	tank, ok := findPool(resp, "tank")
	if !ok {
		t.Fatalf("missing pool tank; body=%s", out)
	}
	if tank["total_datasets"] != float64(n) {
		t.Errorf("total_datasets = %v, want %d; body=%s", tank["total_datasets"], n, out)
	}
	if tank["dataset_count"] != float64(defaultQueryLimit) {
		t.Errorf("dataset_count = %v, want %d; body=%s", tank["dataset_count"], defaultQueryLimit, out)
	}

	// Expected top-N: used desc, name asc on ties.
	want := []string{"tank", "tank/p"}
	for i := 0; i < distinct; i++ {
		want = append(want, fmt.Sprintf("tank/p/c%03d", i))
	}
	var tied []string
	for i := distinct; i < children; i++ {
		tied = append(tied, fmt.Sprintf("tank/p/c%03d", i))
	}
	sort.Strings(tied)
	want = append(want, tied[:defaultQueryLimit-len(want)]...)

	list, _ := tank["datasets"].([]interface{})
	got := make([]string, 0, len(list))
	for _, raw := range list {
		ds, _ := raw.(map[string]interface{})
		name, _ := ds["name"].(string)
		got = append(got, name)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("top datasets =\n%v\nwant\n%v", got, want)
	}

	// children_count comes from the full listing, not the limited page.
	if p, ok := findPoolDataset(tank, "tank/p"); !ok {
		t.Errorf("missing tank/p; body=%s", out)
	} else if p["children_count"] != float64(children) {
		t.Errorf("tank/p children_count = %v, want %d; body=%s", p["children_count"], children, out)
	}
	if root, ok := findPoolDataset(tank, "tank"); !ok {
		t.Errorf("missing tank; body=%s", out)
	} else if root["children_count"] != 1.0 {
		t.Errorf("tank children_count = %v, want 1; body=%s", root["children_count"], out)
	}
}

package tools

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestGetPoolCapacityDetailsTopNIsLargestByUsed reproduces the v1.0.0
// regression where the top-N list was not the largest datasets. The
// middleware applies each order_by key as its own full sort in list order,
// so ["-used.parsed", "name"] ends up ordered by name and limit keeps the
// alphabetically first rows. Here the large datasets have names that sort
// after many small ones, so a name-primary page drops them.
func TestGetPoolCapacityDetailsTopNIsLargestByUsed(t *testing.T) {
	prop := func(v float64) map[string]interface{} {
		return map[string]interface{}{"parsed": v, "value": "x"}
	}
	row := func(name string, used float64) map[string]interface{} {
		return map[string]interface{}{
			"name": name, "type": "FILESYSTEM", "pool": "tank", "encrypted": false,
			"used": prop(used), "available": prop(1),
		}
	}
	const tib = float64(1 << 40)
	const kib = float64(1 << 10)
	rows := []interface{}{row("tank", 10*tib)}
	small := defaultQueryLimit + 20
	for i := 0; i < small; i++ {
		rows = append(rows, row(fmt.Sprintf("tank/a%03d", i), 128*kib+float64(i)))
	}
	// Large datasets sort after every small one by name.
	big := []string{"tank/z1", "tank/z2", "tank/z3"}
	for i, name := range big {
		rows = append(rows, row(name, float64(3-i)*tib))
	}
	n := len(rows)

	var orders [][]interface{}
	fake := &fakeMiddleware{
		results: map[string]interface{}{"pool.query": []interface{}{
			map[string]interface{}{"name": "tank", "status": "ONLINE", "healthy": true, "allocated": 1.0, "free": 1.0},
		}},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method != "pool.dataset.query" {
				return nil, nil, false
			}
			_, options := parseQueryParams(t, params)
			if options["count"] == true {
				return float64(len(rows)), nil, true
			}
			if _, ok := options["limit"]; ok {
				order, _ := options["order_by"].([]interface{})
				orders = append(orders, order)
			}
			return datasetQueryLikeMiddleware(t, params, rows), nil, true
		},
	}
	r := capacityDetailsRegistry(t, fake)
	out, err := r.CallTool(t.Context(), "get_pool_capacity_details", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	for i, order := range orders {
		if fmt.Sprint(order) != "[-used.parsed]" {
			t.Errorf("limited call %d: order_by = %v, want [-used.parsed] (later keys become the primary sort)", i, order)
		}
	}

	tank, ok := findPool(decode(t, out), "tank")
	if !ok {
		t.Fatalf("missing pool tank; body=%s", out)
	}
	if tank["total_datasets"] != float64(n) {
		t.Errorf("total_datasets = %v, want %d", tank["total_datasets"], n)
	}
	list, _ := tank["datasets"].([]interface{})
	if len(list) != defaultQueryLimit {
		t.Fatalf("dataset_count = %d, want %d; body=%s", len(list), defaultQueryLimit, out)
	}
	got := make([]string, 0, len(list))
	for _, raw := range list {
		ds, _ := raw.(map[string]interface{})
		name, _ := ds["name"].(string)
		got = append(got, name)
	}
	want := append([]string{"tank"}, big...)
	// Then the largest small datasets, used desc.
	for i := small - 1; len(want) < defaultQueryLimit; i-- {
		want = append(want, fmt.Sprintf("tank/a%03d", i))
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("top datasets =\n%v\nwant\n%v", got, want)
	}
}

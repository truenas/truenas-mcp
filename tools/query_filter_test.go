package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func parseQueryParams(t *testing.T, params json.RawMessage) (filters []interface{}, options map[string]interface{}) {
	t.Helper()
	var raw []interface{}
	if err := json.Unmarshal(params, &raw); err != nil {
		t.Fatalf("params: %v (%s)", err, params)
	}
	if len(raw) > 0 {
		filters, _ = raw[0].([]interface{})
	}
	if len(raw) > 1 {
		options, _ = raw[1].(map[string]interface{})
	}
	if options == nil {
		options = map[string]interface{}{}
	}
	return filters, options
}

func hasFilter(filters []interface{}, field, op, value string) bool {
	for _, item := range filters {
		f, ok := item.([]interface{})
		if !ok || len(f) < 3 {
			continue
		}
		if f[0] == field && f[1] == op && f[2] == value {
			return true
		}
	}
	return false
}

func queryFilterRegistry(t *testing.T, fake *fakeMiddleware) *Registry {
	t.Helper()
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil)
}

func sampleDatasets() []interface{} {
	prop := func(n float64) map[string]interface{} {
		return map[string]interface{}{"parsed": n, "value": "x"}
	}
	ds := func(name, pool string, enc bool, used float64) map[string]interface{} {
		return map[string]interface{}{
			"name": name, "type": "FILESYSTEM", "pool": pool, "encrypted": enc,
			"mountpoint": "/mnt/" + name, "used": prop(used), "available": prop(1),
		}
	}
	return []interface{}{
		ds("tank", "tank", false, 300),
		ds("tank/data", "tank", true, 200),
		ds("tank/data/sub", "tank", false, 50),
		ds("tank2", "tank2", false, 100),
		ds("tank2/media", "tank2", true, 80),
	}
}

// TestQueryDatasetsPoolFilterAndBounds is the item-5 regression: pool "tank"
// must not match tank2, the dataset query must be bounded, and totals must
// respect encrypted_only.
func TestQueryDatasetsPoolFilterAndBounds(t *testing.T) {
	var gotParams json.RawMessage
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.dataset.query" {
			return nil, nil, false
		}
		gotParams = params
		filters, _ := parseQueryParams(t, params)
		var out []interface{}
		for _, raw := range sampleDatasets() {
			ds := raw.(map[string]interface{})
			keep := true
			for _, item := range filters {
				f, ok := item.([]interface{})
				if !ok || len(f) < 3 {
					continue
				}
				field, _ := f[0].(string)
				op, _ := f[1].(string)
				if op == "=" && ds[field] != f[2] {
					keep = false
				}
				if op == "^" {
					name, _ := ds["name"].(string)
					pref, _ := f[2].(string)
					if field == "name" && !strings.HasPrefix(name, pref) {
						keep = false
					}
				}
			}
			if keep {
				out = append(out, ds)
			}
		}
		return out, nil, true
	}}
	r := queryFilterRegistry(t, fake)

	out, err := r.CallTool("query_datasets", map[string]interface{}{"pool": "tank"})
	if err != nil {
		t.Fatal(err)
	}
	filters, options := parseQueryParams(t, gotParams)
	if !hasFilter(filters, "pool", "=", "tank") {
		t.Fatalf("pool filter = %v, want [pool, =, tank]; params=%s", filters, gotParams)
	}
	if hasFilter(filters, "name", "^", "tank") {
		t.Fatalf("still using name prefix filter: %s", gotParams)
	}
	extra, _ := options["extra"].(map[string]interface{})
	if extra["retrieve_children"] != false {
		t.Errorf("retrieve_children = %v, want false; options=%v", extra["retrieve_children"], options)
	}
	props, _ := extra["properties"].([]interface{})
	if len(props) == 0 {
		t.Errorf("missing extra.properties: %s", gotParams)
	}
	resp := decode(t, out)
	counts := map[string]float64{}
	for _, raw := range resp["datasets"].([]interface{}) {
		ds := raw.(map[string]interface{})
		name, _ := ds["name"].(string)
		if strings.HasPrefix(name, "tank2") {
			t.Errorf("pool=tank included %q", name)
		}
		if n, ok := ds["children_count"].(float64); ok {
			counts[name] = n
		}
	}
	if counts["tank"] != 1 || counts["tank/data"] != 1 {
		// tank has tank/data only (tank/data/sub is not a direct child)
		// wait, sample has tank, tank/data, tank/data/sub — tank's direct child is tank/data only (1)
		t.Errorf("children_count = %v, want tank=1 tank/data=1", counts)
	}

	out, err = r.CallTool("query_datasets", map[string]interface{}{"pool": "tank", "encrypted_only": true})
	if err != nil {
		t.Fatal(err)
	}
	resp = decode(t, out)
	if resp["total_datasets"] != 1.0 {
		t.Errorf("encrypted_only total_datasets = %v, want 1; body=%s", resp["total_datasets"], out)
	}
	if resp["dataset_count"] != 1.0 {
		t.Errorf("encrypted_only dataset_count = %v, want 1", resp["dataset_count"])
	}
	list := resp["datasets"].([]interface{})
	if len(list) != 1 || list[0].(map[string]interface{})["name"] != "tank/data" {
		t.Errorf("encrypted_only datasets = %s", out)
	}
}

func TestQuerySnapshotsUnfilteredCarriesLimit(t *testing.T) {
	var gotParams json.RawMessage
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.snapshot.query" {
			return nil, nil, false
		}
		_, options := parseQueryParams(t, params)
		if options["count"] == true {
			return 2.0, nil, true
		}
		gotParams = params
		return []interface{}{
			map[string]interface{}{"id": "tank@a", "snapshot_name": "b", "dataset": "tank", "pool": "tank", "properties": map[string]interface{}{}},
			map[string]interface{}{"id": "tank@b", "snapshot_name": "a", "dataset": "tank", "pool": "tank", "properties": map[string]interface{}{}},
		}, nil, true
	}}
	r := queryFilterRegistry(t, fake)
	out, err := r.CallTool("query_snapshots", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	_, options := parseQueryParams(t, gotParams)
	limit, ok := options["limit"].(float64)
	if !ok || limit <= 0 {
		t.Errorf("unfiltered query_snapshots options = %s, want a positive limit", gotParams)
	}
	_ = out
}

func TestDatasetChildrenCountFromNames(t *testing.T) {
	names := []string{"tank", "tank/data", "tank/data/sub", "tank/home", "tank2", "tank2/x"}
	if got := datasetChildrenCount("tank", names); got != 2 {
		t.Errorf("tank = %d, want 2", got)
	}
	if got := datasetChildrenCount("tank/data", names); got != 1 {
		t.Errorf("tank/data = %d, want 1", got)
	}
	if got := datasetChildrenCount("tank/home", names); got != 0 {
		t.Errorf("tank/home = %d, want 0", got)
	}
}

func TestQueryPoolsAndSharesRequestSelect(t *testing.T) {
	var methods = map[string]json.RawMessage{}
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		methods[method] = params
		switch method {
		case "pool.query":
			return []interface{}{map[string]interface{}{
				"id": 1.0, "name": "tank", "status": "ONLINE", "size": 1.0e12, "healthy": true,
				"topology": map[string]interface{}{"data": []interface{}{"ignored"}},
				"scan":     map[string]interface{}{"function": "SCRUB", "state": "FINISHED"},
			}}, nil, true
		case "sharing.smb.query":
			return []interface{}{map[string]interface{}{"id": 1.0, "name": "media", "path": "/mnt/tank/media", "enabled": true, "secret": "ignore"}}, nil, true
		case "sharing.nfs.query":
			return []interface{}{map[string]interface{}{"id": 2.0, "path": "/mnt/tank/nfs", "enabled": true, "ro": true}}, nil, true
		case "vm.query":
			return []interface{}{map[string]interface{}{"id": 1.0, "name": "web", "vcpus": 1.0, "cores": 1.0, "threads": 1.0, "memory": 1024.0, "autostart": true, "status": map[string]interface{}{"state": "STOPPED"}}}, nil, true
		default:
			return nil, nil, false
		}
	}}
	r := queryFilterRegistry(t, fake)

	out, err := r.CallTool("query_pools", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	_, opts := parseQueryParams(t, methods["pool.query"])
	if !selectContains(opts, "name") || selectContains(opts, "topology") {
		t.Errorf("pool.query options = %s, want select without topology", methods["pool.query"])
	}
	if strings.Contains(out, "topology") || !strings.Contains(out, `"name": "tank"`) {
		t.Errorf("query_pools should be simplified: %s", out)
	}

	out, err = r.CallTool("query_shares", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	_, smbOpts := parseQueryParams(t, methods["sharing.smb.query"])
	if !selectContains(smbOpts, "name") {
		t.Errorf("sharing.smb.query options = %s", methods["sharing.smb.query"])
	}
	if strings.Contains(out, "secret") {
		t.Errorf("query_shares leaked unselected field: %s", out)
	}

	if _, err := r.CallTool("query_vms", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	_, vmOpts := parseQueryParams(t, methods["vm.query"])
	if !selectContains(vmOpts, "name") || !selectContains(vmOpts, "devices") {
		t.Errorf("vm.query options = %s", methods["vm.query"])
	}
}

func selectContains(options map[string]interface{}, field string) bool {
	sel, _ := options["select"].([]interface{})
	for _, v := range sel {
		if v == field {
			return true
		}
	}
	return false
}

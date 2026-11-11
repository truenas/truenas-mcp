package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func existenceOptionsOK(options map[string]interface{}) error {
	if len(options) == 0 {
		return fmt.Errorf("empty query options")
	}
	if !selectContains(options, "name") {
		return fmt.Errorf("select is missing name: %v", options["select"])
	}
	extra, _ := options["extra"].(map[string]interface{})
	if extra == nil {
		return fmt.Errorf("missing extra")
	}
	if extra["retrieve_children"] != false {
		return fmt.Errorf("retrieve_children = %v, want false", extra["retrieve_children"])
	}
	if extra["retrieve_user_props"] != false {
		return fmt.Errorf("retrieve_user_props = %v, want false", extra["retrieve_user_props"])
	}
	props, ok := extra["properties"].([]interface{})
	if !ok {
		return fmt.Errorf("properties = %v, want an empty list", extra["properties"])
	}
	if len(props) != 0 {
		return fmt.Errorf("properties = %v, want an empty list", props)
	}
	return nil
}

func TestDatasetExistenceChecksSendReducedOptions(t *testing.T) {
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.dataset.query" {
			return nil, nil, false
		}
		return []interface{}{map[string]interface{}{"name": "tank/data"}}, nil, true
	}}
	client, _ := newFakeClientFrom(t, fake)

	exists, err := datasetNameExists(context.Background(), client, "tank/data")
	if err != nil {
		t.Fatalf("datasetNameExists: %v", err)
	}
	if !exists {
		t.Fatal("datasetNameExists = false, want true")
	}

	missing, err := verifyDatasetPathsExist(context.Background(), client, []string{"/mnt/tank/data"})
	if err != nil {
		t.Fatalf("verifyDatasetPathsExist: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("verifyDatasetPathsExist missing = %v, want none", missing)
	}

	calls := fake.called("pool.dataset.query")
	if len(calls) < 2 {
		t.Fatalf("dataset.query calls = %d, want at least 2 (existence + path check)", len(calls))
	}
	for i, c := range calls {
		_, options := parseQueryParams(t, c.Params)
		if err := existenceOptionsOK(options); err != nil {
			t.Errorf("call %d: %v; params=%s", i, err, c.Params)
		}
	}
}

func TestQuerySnapshotsFilteredQueriesCarryServerLimit(t *testing.T) {
	snaps := []interface{}{
		map[string]interface{}{
			"id": "tank/data@keep", "snapshot_name": "keep", "dataset": "tank/data", "pool": "tank",
			"createtxg": "10", "holds": map[string]interface{}{"truenas": 1.0},
			"properties": map[string]interface{}{"creation": snapProp(100.0, "100")},
		},
		map[string]interface{}{
			"id": "tank/data@old", "snapshot_name": "old", "dataset": "tank/data", "pool": "tank",
			"createtxg": "9", "holds": map[string]interface{}{},
			"properties": map[string]interface{}{"creation": snapProp(90.0, "90")},
		},
	}
	cases := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"pool", map[string]interface{}{"pool": "tank"}, "name"},
		{"dataset", map[string]interface{}{"dataset": "tank/data"}, "name"},
		{"created", map[string]interface{}{"order_by": "created"}, "created"},
		{"holds_only", map[string]interface{}{"holds_only": true, "pool": "tank"}, "name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var listCalls []map[string]interface{}
			fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
				if method != "pool.snapshot.query" {
					return nil, nil, false
				}
				filters, options := parseQueryParams(t, params)
				_ = filters
				if options["count"] == true {
					return float64(len(snaps)), nil, true
				}
				listCalls = append(listCalls, options)
				return applySnapshotQueryPage(snaps, options), nil, true
			}}
			client, _ := newFakeClientFrom(t, fake)
			if _, err := handleQuerySnapshots(context.Background(), client, tc.args); err != nil {
				t.Fatal(err)
			}
			if len(listCalls) == 0 {
				t.Fatal("no snapshot list query was sent")
			}
			for i, options := range listCalls {
				limit, ok := options["limit"].(float64)
				if !ok || limit <= 0 {
					t.Errorf("list call %d: limit = %v, want a positive server-side limit; options=%v", i, options["limit"], options)
				}
				order, _ := options["order_by"].([]interface{})
				if len(order) == 0 {
					t.Errorf("list call %d: missing order_by; options=%v", i, options)
					continue
				}
				got, _ := order[0].(string)
				if err := checkSnapshotServerOrder(tc.want, got); err != nil {
					t.Errorf("list call %d: %v; options=%v", i, err, options)
				}
			}
		})
	}
}

func checkSnapshotServerOrder(want, got string) error {
	switch want {
	case "created":
		// createtxg is a string and is not zero-padded, so lexical
		// -createtxg mis-orders values such as "9" and "10".
		if got != "-properties.creation.parsed" {
			return fmt.Errorf("order_by = %q, want -properties.creation.parsed", got)
		}
	case "dataset":
		if got != "dataset" {
			return fmt.Errorf("order_by = %q, want dataset", got)
		}
	default:
		if got != "-snapshot_name" {
			return fmt.Errorf("order_by = %q, want -snapshot_name", got)
		}
	}
	return nil
}

func TestQuerySnapshotsCreatedOrderDoesNotUseLexicalCreatetxg(t *testing.T) {
	// Unpadded createtxg strings: lexical descending is "9","10","100",
	// which is the wrong creation order (100 is newest).
	snaps := []interface{}{
		map[string]interface{}{
			"id": "tank@nine", "snapshot_name": "nine", "dataset": "tank", "pool": "tank",
			"createtxg":  "9",
			"properties": map[string]interface{}{"creation": snapProp(1_000_000_000.0, "1000000000")},
		},
		map[string]interface{}{
			"id": "tank@ten", "snapshot_name": "ten", "dataset": "tank", "pool": "tank",
			"createtxg":  "10",
			"properties": map[string]interface{}{"creation": snapProp(1_500_000_000.0, "1500000000")},
		},
		map[string]interface{}{
			"id": "tank@hundred", "snapshot_name": "hundred", "dataset": "tank", "pool": "tank",
			"createtxg":  "100",
			"properties": map[string]interface{}{"creation": snapProp(1_700_000_000.0, "1700000000")},
		},
	}
	var listOrder string
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.snapshot.query" {
			return nil, nil, false
		}
		_, options := parseQueryParams(t, params)
		if options["count"] == true {
			return 3.0, nil, true
		}
		if raw, ok := options["order_by"].([]interface{}); ok && len(raw) > 0 {
			listOrder, _ = raw[0].(string)
		}
		return applySnapshotQueryPage(snaps, options), nil, true
	}}
	client, _ := newFakeClientFrom(t, fake)
	out, err := handleQuerySnapshots(context.Background(), client, map[string]interface{}{
		"order_by": "created",
		"limit":    2.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if listOrder != "-properties.creation.parsed" {
		t.Errorf("order_by = %q, want -properties.creation.parsed (not lexical -createtxg)", listOrder)
	}
	resp := decode(t, out)
	list, _ := resp["snapshots"].([]interface{})
	if len(list) != 2 {
		t.Fatalf("snapshots = %s, want 2", out)
	}
	first, _ := list[0].(map[string]interface{})["snapshot_name"].(string)
	second, _ := list[1].(map[string]interface{})["snapshot_name"].(string)
	if first != "hundred" || second != "ten" {
		t.Errorf("order_by=created limit=2 names = %q, %q, want hundred, ten (newest first); body=%s", first, second, out)
	}
}

func TestQuerySnapshotsHoldsOnlyPagesUpToBudget(t *testing.T) {
	// budget+1 snapshots, default order -snapshot_name. The only hold is
	// on the last name in that order (s0000), so a budgeted scan misses
	// it; a full unbounded fetch would find it.
	n := snapshotHoldsScanBudget + 1
	snaps := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		holds := map[string]interface{}{}
		if i == 0 {
			holds["truenas"] = 1.0
		}
		snaps = append(snaps, map[string]interface{}{
			"id": fmt.Sprintf("tank@s%04d", i), "snapshot_name": fmt.Sprintf("s%04d", i),
			"dataset": "tank", "pool": "tank", "holds": holds,
			"properties": map[string]interface{}{"creation": snapProp(float64(i), "x")},
		})
	}
	var listCalls []map[string]interface{}
	scanned := 0
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.snapshot.query" {
			return nil, nil, false
		}
		_, options := parseQueryParams(t, params)
		if options["count"] == true {
			return float64(len(snaps)), nil, true
		}
		listCalls = append(listCalls, options)
		page := applySnapshotQueryPage(snaps, options)
		if rows, ok := page.([]interface{}); ok {
			scanned += len(rows)
		}
		return page, nil, true
	}}
	client, _ := newFakeClientFrom(t, fake)
	out, err := handleQuerySnapshots(context.Background(), client, map[string]interface{}{
		"holds_only": true,
		"pool":       "tank",
		"limit":      10.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listCalls) == 0 {
		t.Fatal("no snapshot list query was sent")
	}
	for i, options := range listCalls {
		limit, ok := options["limit"].(float64)
		if !ok || limit <= 0 {
			t.Errorf("page %d: missing positive limit; options=%v", i, options)
		}
		if i > 0 {
			offset, _ := options["offset"].(float64)
			if offset <= 0 {
				t.Errorf("page %d: offset = %v, want a positive offset after the first page; options=%v", i, options["offset"], options)
			}
		}
	}
	if scanned > snapshotHoldsScanBudget {
		t.Errorf("scanned %d rows, want at most the %d-row budget", scanned, snapshotHoldsScanBudget)
	}
	resp := decode(t, out)
	list, _ := resp["snapshots"].([]interface{})
	if len(list) != 0 {
		t.Errorf("holds_only over budget returned %d snapshots, want 0 (the only hold is past the scan budget); body=%s", len(list), out)
	}
	note, _ := resp["note"].(string)
	if !strings.Contains(strings.ToLower(note), "budget") && !strings.Contains(strings.ToLower(note), "scanning") {
		t.Errorf("missing note that the holds scan reached its budget: %s", out)
	}
}

func TestQueryDatasetsPushesServerLimit(t *testing.T) {
	var heavy []map[string]interface{}
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.dataset.query" {
			return nil, nil, false
		}
		_, options := parseQueryParams(t, params)
		if options["count"] == true {
			return float64(len(sampleDatasets())), nil, true
		}
		// The full-property query (not the name listing) must carry limit.
		extra, _ := options["extra"].(map[string]interface{})
		props, _ := extra["properties"].([]interface{})
		if len(props) > 1 {
			heavy = append(heavy, options)
		}
		return datasetQueryLikeMiddleware(t, params, sampleDatasets()), nil, true
	}}
	r := queryFilterRegistry(t, fake)
	out, err := r.CallTool(t.Context(), "query_datasets", map[string]interface{}{
		"pool":     "tank",
		"limit":    2.0,
		"order_by": "used",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(heavy) == 0 {
		t.Fatal("no full-property dataset query was sent")
	}
	for i, options := range heavy {
		limit, ok := options["limit"].(float64)
		if !ok || limit != 2 {
			t.Errorf("heavy call %d: limit = %v, want 2; options=%v", i, options["limit"], options)
		}
		order, _ := options["order_by"].([]interface{})
		if len(order) == 0 || order[0] != "-properties.used.parsed" {
			t.Errorf("heavy call %d: order_by = %v, want [-properties.used.parsed]; options=%v", i, options["order_by"], options)
		}
	}
	resp := decode(t, out)
	if resp["total_datasets"] != 3.0 {
		t.Errorf("total_datasets = %v, want 3; body=%s", resp["total_datasets"], out)
	}
	if resp["dataset_count"] != 2.0 {
		t.Errorf("dataset_count = %v, want 2; body=%s", resp["dataset_count"], out)
	}
	// children_count must still reflect the full tree, not only the page.
	if data, ok := findDataset(resp, "tank"); !ok {
		t.Errorf("missing tank on the used-desc page; body=%s", out)
	} else if data["children_count"] != 1.0 {
		t.Errorf("tank children_count = %v, want 1 (from the full name list); body=%s", data["children_count"], out)
	}
}

// applySnapshotQueryPage models middleware filter_list limit/offset/order_by
// on an already-filtered snapshot list. Dotted paths are supported. String
// fields (including createtxg) sort lexically; numbers sort numerically.
func applySnapshotQueryPage(rows []interface{}, options map[string]interface{}) interface{} {
	sorted := append([]interface{}{}, rows...)
	if raw, ok := options["order_by"].([]interface{}); ok && len(raw) > 0 {
		key, _ := raw[0].(string)
		desc := strings.HasPrefix(key, "-")
		if desc {
			key = key[1:]
		}
		sort.SliceStable(sorted, func(i, j int) bool {
			cmp := compareQueryValues(lookupDotted(sorted[i], key), lookupDotted(sorted[j], key))
			if desc {
				return cmp > 0
			}
			return cmp < 0
		})
	}
	start := 0
	if off, ok := options["offset"].(float64); ok && off > 0 {
		start = int(off)
		if start > len(sorted) {
			start = len(sorted)
		}
	}
	end := len(sorted)
	if lim, ok := options["limit"].(float64); ok && lim >= 0 {
		end = start + int(lim)
		if end > len(sorted) {
			end = len(sorted)
		}
	}
	return sorted[start:end]
}

func lookupDotted(row interface{}, path string) interface{} {
	cur := row
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func compareQueryValues(a, b interface{}) int {
	an, aNum := toFloat(a)
	bn, bNum := toFloat(b)
	if aNum && bNum {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}
	as, aStr := a.(string)
	bs, bStr := b.(string)
	if aStr && bStr {
		return strings.Compare(as, bs)
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

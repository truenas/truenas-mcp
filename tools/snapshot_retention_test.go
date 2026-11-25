package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuerySnapshotsRetention(t *testing.T) {
	snaps := []interface{}{
		map[string]interface{}{
			"id": "tank/data@auto-2026-01-01_00-00", "snapshot_name": "auto-2026-01-01_00-00", "dataset": "tank/data", "pool": "tank",
			"properties": map[string]interface{}{},
			"retention": map[string]interface{}{
				"datetime": map[string]interface{}{"$date": 1769904000000.0}, "source": "periodic_snapshot_task",
				"periodic_snapshot_task_id": 3.0,
			},
		},
		map[string]interface{}{
			"id": "tank/data@kept", "snapshot_name": "kept", "dataset": "tank/data", "pool": "tank",
			"properties": map[string]interface{}{},
			"retention":  map[string]interface{}{"datetime": map[string]interface{}{"$date": 1772323200000.0}, "source": "property"},
		},
		map[string]interface{}{
			"id": "tank/data@manual", "snapshot_name": "manual", "dataset": "tank/data", "pool": "tank",
			"properties": map[string]interface{}{}, "retention": nil,
		},
	}
	var gotOptions map[string]interface{}
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.snapshot.query" {
			return nil, nil, false
		}
		_, options := parseQueryParams(t, params)
		if options["count"] == true {
			return 3.0, nil, true
		}
		gotOptions = options
		return snaps, nil, true
	}}
	r := queryFilterRegistry(t, fake)

	out, err := r.CallTool(t.Context(), "query_snapshots", map[string]interface{}{"include_retention": true})
	if err != nil {
		t.Fatal(err)
	}
	extra, _ := gotOptions["extra"].(map[string]interface{})
	if extra["retention"] != true {
		t.Errorf("extra = %v, want retention: true", extra)
	}
	// 25.10 refuses extra.retention without a limit or an id filter.
	if l, ok := gotOptions["limit"].(float64); !ok || l < 1 {
		t.Errorf("retention query without a limit: %v", gotOptions)
	}
	resp := decode(t, out)
	if resp["retention_included"] != true {
		t.Errorf("retention_included = %v", resp["retention_included"])
	}
	byName := map[string]map[string]interface{}{}
	for _, s := range resp["snapshots"].([]interface{}) {
		m := s.(map[string]interface{})
		byName[m["snapshot_name"].(string)] = m
	}
	task := byName["auto-2026-01-01_00-00"]["retention"].(map[string]interface{})
	if task["removal_date"] != "2026-02-01T00:00:00Z" || task["removal_timestamp"] != 1769904000.0 ||
		task["source"] != "periodic_snapshot_task" || task["periodic_snapshot_task_id"] != 3.0 {
		t.Errorf("task retention = %v", task)
	}
	prop := byName["kept"]["retention"].(map[string]interface{})
	if prop["removal_date"] != "2026-03-01T00:00:00Z" || prop["source"] != "property" {
		t.Errorf("property retention = %v", prop)
	}
	if _, ok := prop["periodic_snapshot_task_id"]; ok {
		t.Errorf("property retention has a task id: %v", prop)
	}
	if v, ok := byName["manual"]["retention"]; !ok || v != nil {
		t.Errorf("manual retention = %v (present %v), want null", v, ok)
	}

	// holds_only pages with a limit too, so retention combines with it.
	if _, err := r.CallTool(t.Context(), "query_snapshots", map[string]interface{}{"include_retention": true, "holds_only": true}); err != nil {
		t.Fatal(err)
	}
	if extra, _ := gotOptions["extra"].(map[string]interface{}); extra["retention"] != true || extra["holds"] != true {
		t.Errorf("holds_only extra = %v", extra)
	}
	if l, ok := gotOptions["limit"].(float64); !ok || l < 1 {
		t.Errorf("holds_only retention query without a limit: %v", gotOptions)
	}

	// Off by default: not requested and no retention key in the output.
	for i := range snaps {
		delete(snaps[i].(map[string]interface{}), "retention")
	}
	out, err = r.CallTool(t.Context(), "query_snapshots", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if extra, _ := gotOptions["extra"].(map[string]interface{}); extra["retention"] != nil {
		t.Errorf("default query requested retention: %v", extra)
	}
	if strings.Contains(out, `"retention"`) || decode(t, out)["retention_included"] != false {
		t.Errorf("default output: %s", out)
	}
}

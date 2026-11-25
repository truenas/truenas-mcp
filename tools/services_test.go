package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func serviceFixtures() []interface{} {
	return []interface{}{
		map[string]interface{}{"id": 3.0, "service": "ssh", "enable": true, "state": "RUNNING", "pids": []interface{}{1234.0}},
		map[string]interface{}{"id": 1.0, "service": "cifs", "enable": false, "state": "STOPPED", "pids": []interface{}{}},
		map[string]interface{}{"id": 2.0, "service": "nfs", "enable": true, "state": "UNKNOWN", "pids": []interface{}{}},
	}
}

func serviceFake(t *testing.T) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "service.query" {
			return nil, nil, false
		}
		var p []interface{}
		_ = json.Unmarshal(params, &p)
		filters, _ := p[0].([]interface{})
		out := []interface{}{}
		for _, row := range serviceFixtures() {
			if jobMatchesFilters(row.(map[string]interface{}), filters) {
				out = append(out, row)
			}
		}
		return out, nil, true
	}}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

func TestQueryServicesSummary(t *testing.T) {
	r, _ := serviceFake(t)
	resp := callTool(t, r, "query_services", map[string]interface{}{})
	if resp["count"] != 3.0 || resp["running"] != 1.0 {
		t.Errorf("count/running = %v/%v, want 3/1", resp["count"], resp["running"])
	}
	list := resp["services"].([]interface{})
	var names []string
	for _, item := range list {
		names = append(names, item.(map[string]interface{})["name"].(string))
	}
	if strings.Join(names, ",") != "cifs,nfs,ssh" {
		t.Errorf("order = %v, want cifs,nfs,ssh", names)
	}
	ssh := list[2].(map[string]interface{})
	if ssh["state"] != "RUNNING" || ssh["running"] != true || ssh["enabled_at_boot"] != true {
		t.Errorf("ssh = %v", ssh)
	}
	cifs := list[0].(map[string]interface{})
	if cifs["running"] != false || cifs["enabled_at_boot"] != false {
		t.Errorf("cifs = %v", cifs)
	}
	for _, item := range list {
		m := item.(map[string]interface{})
		if _, ok := m["pids"]; ok {
			t.Errorf("pids returned: %v", m)
		}
	}
	if note, _ := resp["note"].(string); !strings.Contains(note, "1 service(s) report UNKNOWN") {
		t.Errorf("note = %q", note)
	}
}

func TestQueryServicesByName(t *testing.T) {
	r, fake := serviceFake(t)
	resp := callTool(t, r, "query_services", map[string]interface{}{"name": "ssh"})
	if resp["count"] != 1.0 {
		t.Fatalf("count = %v, want 1", resp["count"])
	}
	p := lastParams(t, fake, "service.query")
	if got, _ := json.Marshal(p[0]); string(got) != `[["service","=","ssh"]]` {
		t.Errorf("filters = %s", got)
	}
	if _, ok := resp["note"]; ok {
		t.Errorf("unexpected note: %v", resp["note"])
	}

	_, err := r.CallTool(t.Context(), "query_services", map[string]interface{}{"name": "nosuch"})
	if err == nil || !strings.Contains(err.Error(), `service "nosuch" not found`) {
		t.Errorf("err = %v", err)
	}
}

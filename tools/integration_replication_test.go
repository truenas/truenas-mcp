//go:build integration

package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestIntegrationReplicationQueries runs both task query tools against the
// live middleware, checks the replication output carries no SSH connection
// or key fields, and looks up the first task of each kind by id.
func TestIntegrationReplicationQueries(t *testing.T) {
	r, _ := integrationRegistry(t)
	for _, tc := range []struct{ tool, list string }{
		{"query_replications", "replications"},
		{"query_snapshot_tasks", "snapshot_tasks"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			out, err := r.CallTool(t.Context(), tc.tool, map[string]interface{}{"limit": 5.0})
			dumpToolResult(t, tc.tool, out, err)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{`"ssh_credentials"`, `"attributes"`, `"private_key"`, `"remote_host_key"`, `"encryption_key"`, `"netcat_active_side_listen_address"`, `"netcat_passive_side_connect_address"`} {
				if strings.Contains(out, field) {
					t.Errorf("%s output contains %s", tc.tool, field)
				}
			}
			var resp map[string]interface{}
			if err := json.Unmarshal([]byte(out), &resp); err != nil {
				t.Fatal(err)
			}
			list, _ := resp[tc.list].([]interface{})
			if len(list) == 0 {
				t.Skipf("no %s on this system", tc.list)
			}
			id := list[0].(map[string]interface{})["id"]
			one, err := r.CallTool(t.Context(), tc.tool, map[string]interface{}{"task_id": id})
			if err != nil {
				t.Fatalf("task_id %v: %v", id, err)
			}
			if err := json.Unmarshal([]byte(one), &resp); err != nil {
				t.Fatal(err)
			}
			if resp["count"] != 1.0 {
				t.Errorf("task_id %v: count = %v, want 1", id, resp["count"])
			}
		})
	}
}

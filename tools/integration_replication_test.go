//go:build integration

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
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

// TestIntegrationReplicationRunCycle builds a scratch dataset tree, a
// periodic snapshot task and a LOCAL replication task bound to it, then
// drives create_snapshot, run_snapshot_task and run_replication (twice:
// once with a new snapshot, once with nothing new) and cleans up.
func TestIntegrationReplicationRunCycle(t *testing.T) {
	if !integrationWritesEnabled() {
		t.Skip("replication run cycle requires TRUENAS_INTEGRATION_WRITES=1")
	}
	pool, skip := scratchWritePool()
	if skip != "" {
		t.Skip(skip)
	}
	client := integrationClient(t)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: 2 * time.Second, CleanupInterval: time.Minute})
	mgr.Start()
	t.Cleanup(mgr.Shutdown)
	r := NewRegistry(client, mgr)

	root := pool + "/mcp-it-repl-" + time.Now().UTC().Format("20060102-150405")
	src, dst := root+"/src", root+"/dst"
	cleanup := func(method string, params ...interface{}) {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if _, err := client.Call(ctx, method, params...); err != nil {
				t.Errorf("cleanup %s: %v", method, err)
			}
		})
	}
	call := func(method string, params ...interface{}) json.RawMessage {
		t.Helper()
		raw, err := client.Call(t.Context(), method, params...)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return raw
	}
	for _, name := range []string{root, src, src + "/child", dst} {
		call("pool.dataset.create", map[string]interface{}{"name": name})
	}
	cleanup("pool.dataset.delete", root, map[string]interface{}{"recursive": true})

	out, err := r.CallTool(t.Context(), "create_snapshot", map[string]interface{}{"dataset": src, "name": "mcp-it-manual", "recursive": true})
	dumpToolResult(t, "create_snapshot-cycle", out, err)
	if err != nil {
		t.Fatalf("create_snapshot: %v", err)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal([]byte(out), &resp)
	if resp["snapshot"] != src+"@mcp-it-manual" || resp["snapshots_created_count"] != 2.0 {
		t.Errorf("create_snapshot = %v", resp)
	}

	var pst map[string]interface{}
	_ = json.Unmarshal(call("pool.snapshottask.create", map[string]interface{}{
		"dataset": src, "recursive": true, "lifetime_value": 1, "lifetime_unit": "DAY",
		"naming_schema": "mcp-it-%Y-%m-%d_%H-%M-%S", "allow_empty": true, "enabled": true,
		"schedule": map[string]interface{}{"minute": "0", "hour": "0", "dom": "1", "month": "1", "dow": "*"},
	}), &pst)
	pstID := pst["id"].(float64)
	cleanup("pool.snapshottask.delete", int(pstID))

	confirmed := func(name string, args map[string]interface{}) map[string]interface{} {
		t.Helper()
		preview, err := r.CallTool(t.Context(), name, args)
		dumpToolResult(t, name+"-cycle-preview", preview, err)
		if err != nil {
			t.Fatalf("%s preview: %v", name, err)
		}
		tok := replyToken(t, preview)
		if tok == "" {
			t.Fatalf("%s preview returned no confirmation_token:\n%s", name, preview)
		}
		out, err := r.CallTool(t.Context(), name, mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
		dumpToolResult(t, name+"-cycle", out, err)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp = confirmed("run_snapshot_task", map[string]interface{}{"task_id": pstID})
	if resp["state"] != "FINISHED" || resp["new_snapshot"] != true || resp["snapshots_created_count"] != 2.0 {
		t.Errorf("run_snapshot_task = %v", resp)
	}

	var repl map[string]interface{}
	_ = json.Unmarshal(call("replication.create", map[string]interface{}{
		"name": "mcp-it-" + root, "direction": "PUSH", "transport": "LOCAL",
		"source_datasets": []string{src}, "target_dataset": dst + "/copy", "recursive": true,
		"periodic_snapshot_tasks": []int{int(pstID)}, "auto": false, "retention_policy": "NONE",
		"readonly": "IGNORE", "enabled": true,
	}), &repl)
	replID := repl["id"].(float64)
	cleanup("replication.delete", int(replID))

	waitTask := func(taskID string) map[string]interface{} {
		t.Helper()
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			out, err := r.CallTool(t.Context(), "tasks_get", map[string]interface{}{"task_id": taskID})
			if err != nil {
				t.Fatal(err)
			}
			var task map[string]interface{}
			_ = json.Unmarshal([]byte(out), &task)
			if s, _ := task["status"].(string); s == "completed" || s == "failed" || s == "cancelled" {
				return task
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("task %s did not finish", taskID)
		return nil
	}

	resp = confirmed("run_replication", map[string]interface{}{"task_id": replID})
	task := waitTask(resp["task_id"].(string))
	result, _ := task["result"].(map[string]interface{})
	if task["status"] != "completed" || result["transferred"] != true {
		t.Errorf("first run_replication = %v, want snapshots sent", task)
	}

	resp = confirmed("run_replication", map[string]interface{}{"task_id": replID})
	task = waitTask(resp["task_id"].(string))
	result, _ = task["result"].(map[string]interface{})
	if task["status"] != "completed" || result["transferred"] != false || !strings.Contains(fmt.Sprint(task["statusMessage"]), "nothing transferred") {
		t.Errorf("second run_replication = %v, want nothing transferred", task)
	}
}

package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

// Sentinels that must never reach a query_replications result. They model
// what a 25.10 / 27.0 replication.query entry carries: the full keychain
// SSH connection (host, user, key pair id, host key), an encryption key and
// its location, netcat addresses, property overrides and the raw job.
const (
	sentinelHost        = "backup-sentinel.example.com"
	sentinelUser        = "repl-sentinel-user"
	sentinelHostKey     = "ssh-ed25519 AAAAC3NzaSentinelHostKey"
	sentinelPrivateKey  = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQSentinel\n-----END OPENSSH PRIVATE KEY-----"
	sentinelEncKey      = "8f2d1c0b9a7e6d5c4b3a29181716151413121110f0e0d0c0b0a0908070605040"
	sentinelKeyLocation = "/data/sentinel-keyfile"
	sentinelNetcatAddr  = "198.51.100.77"
	sentinelOverride    = "sentinel-override-value"
	sentinelCredName    = "sentinel-connection-name"
	sentinelJobArg      = "sentinel-job-argument"
)

var replicationSentinels = []string{
	sentinelHost, sentinelUser, sentinelHostKey, "OPENSSH PRIVATE KEY", "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQSentinel",
	sentinelEncKey, sentinelKeyLocation, sentinelNetcatAddr, sentinelOverride, sentinelCredName, sentinelJobArg,
}

func pushReplicationFixture() map[string]interface{} {
	return map[string]interface{}{
		"id":        1.0,
		"name":      "tank/data - backup/data",
		"direction": "PUSH",
		"transport": "SSH+NETCAT",
		"ssh_credentials": map[string]interface{}{
			"id":   7.0,
			"name": sentinelCredName,
			"type": "SSH_CREDENTIALS",
			"attributes": map[string]interface{}{
				"host":            sentinelHost,
				"port":            22.0,
				"username":        sentinelUser,
				"private_key":     sentinelPrivateKey,
				"remote_host_key": sentinelHostKey,
				"connect_timeout": 10.0,
			},
		},
		"netcat_active_side":                  "REMOTE",
		"netcat_active_side_listen_address":   sentinelNetcatAddr,
		"netcat_active_side_port_min":         51000.0,
		"netcat_active_side_port_max":         51100.0,
		"netcat_passive_side_connect_address": sentinelNetcatAddr,
		"sudo":                                false,
		"source_datasets":                     []interface{}{"tank/data"},
		"target_dataset":                      "backup/data",
		"recursive":                           true,
		"exclude":                             []interface{}{},
		"properties":                          true,
		"properties_exclude":                  []interface{}{},
		"properties_override":                 map[string]interface{}{"comment": sentinelOverride},
		"replicate":                           false,
		"encryption":                          true,
		"encryption_inherit":                  false,
		"encryption_key":                      sentinelEncKey,
		"encryption_key_format":               "HEX",
		"encryption_key_location":             sentinelKeyLocation,
		"periodic_snapshot_tasks": []interface{}{map[string]interface{}{
			"id": 3.0, "dataset": "tank/data", "recursive": true, "lifetime_value": 2.0, "lifetime_unit": "WEEK",
			"enabled": true, "exclude": []interface{}{}, "naming_schema": "auto-%Y-%m-%d_%H-%M", "allow_empty": true,
			"schedule": map[string]interface{}{"minute": "0", "hour": "*", "dom": "*", "month": "*", "dow": "*", "begin": "00:00", "end": "23:59"},
			"state":    "PENDING",
		}},
		"naming_schema":              []interface{}{},
		"also_include_naming_schema": []interface{}{},
		"name_regex":                 nil,
		"auto":                       true,
		"schedule":                   nil,
		"restrict_schedule":          nil,
		"only_matching_schedule":     false,
		"allow_from_scratch":         false,
		"readonly":                   "SET",
		"hold_pending_snapshots":     false,
		"retention_policy":           "SOURCE",
		"lifetime_value":             nil,
		"lifetime_unit":              nil,
		"lifetimes":                  []interface{}{},
		"compression":                nil,
		"speed_limit":                nil,
		"large_block":                true,
		"embed":                      false,
		"compressed":                 true,
		"retries":                    5.0,
		"logging_level":              nil,
		"enabled":                    true,
		"state": map[string]interface{}{
			"state":         "FINISHED",
			"datetime":      map[string]interface{}{"$date": 1767225600000.0},
			"warnings":      []interface{}{"No incremental base on dataset 'tank/data/new'"},
			"last_snapshot": "tank/data@auto-2026-01-01_00-00",
		},
		"job": map[string]interface{}{
			"id":        42.0,
			"method":    "replication.run",
			"arguments": []interface{}{1.0, sentinelJobArg},
			"state":     "SUCCESS",
			"progress":  map[string]interface{}{"percent": 100.0, "description": "Sending 2 of 2: tank/data@auto-2026-01-01_00-00", "extra": nil},
		},
		"has_encrypted_dataset_keys": true,
	}
}

func pullReplicationFixture() map[string]interface{} {
	return map[string]interface{}{
		"id": 2.0, "name": "pull nightly", "direction": "PULL", "transport": "LOCAL", "ssh_credentials": nil,
		"source_datasets": []interface{}{"tank/src"}, "target_dataset": "tank/dst", "recursive": false,
		"periodic_snapshot_tasks": []interface{}{}, "naming_schema": []interface{}{"auto-%Y-%m-%d_%H-%M"},
		"auto": true, "enabled": false, "retention_policy": "CUSTOM", "lifetime_value": 3.0, "lifetime_unit": "DAY",
		"schedule": map[string]interface{}{"minute": "30", "hour": "2", "dom": "*", "month": "*", "dow": "*", "begin": "00:00", "end": "23:59"},
		"state":    map[string]interface{}{"state": "PENDING"},
		"job":      nil,
	}
}

// taskQueryFake answers replication.query, pool.snapshottask.query,
// pool.dataset.query and pool.snapshot.query from fixtures, applying id and
// dataset filters, limit and count like the middleware does.
func taskQueryFake(t *testing.T, replications, snapTasks []interface{}, datasets []string, snapshots []map[string]interface{}) (*truenas.Client, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{}
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		var p []interface{}
		_ = json.Unmarshal(params, &p)
		var filters []interface{}
		var opts map[string]interface{}
		if len(p) > 0 {
			filters, _ = p[0].([]interface{})
		}
		if len(p) > 1 {
			opts, _ = p[1].(map[string]interface{})
		}
		page := func(rows []interface{}) interface{} {
			var out []interface{}
			for _, row := range rows {
				if jobMatchesFilters(row.(map[string]interface{}), filters) {
					out = append(out, row)
				}
			}
			if opts["count"] == true {
				return len(out)
			}
			if l, ok := opts["limit"].(float64); ok && int(l) < len(out) {
				out = out[:int(l)]
			}
			if out == nil {
				out = []interface{}{}
			}
			return out
		}
		switch method {
		case "replication.query":
			return page(replications), nil, true
		case "pool.snapshottask.query":
			return page(snapTasks), nil, true
		case "pool.dataset.query":
			rows := []interface{}{}
			for _, d := range datasets {
				rows = append(rows, map[string]interface{}{"name": d, "pool": datasetPool(d), "type": "FILESYSTEM"})
			}
			var out []interface{}
			for _, row := range rows {
				// Only the pool filter matters here; the type filter is "in".
				m := row.(map[string]interface{})
				keep := true
				for _, f := range filters {
					arr, _ := f.([]interface{})
					if len(arr) == 3 && arr[0] == "pool" && m["pool"] != arr[2] {
						keep = false
					}
				}
				if keep {
					out = append(out, row)
				}
			}
			return out, nil, true
		case "pool.snapshot.query":
			rows := []interface{}{}
			for _, s := range snapshots {
				rows = append(rows, s)
			}
			var out []interface{}
			for _, row := range rows {
				if jobMatchesFilters(row.(map[string]interface{}), filters) {
					out = append(out, row)
				}
			}
			if out == nil {
				out = []interface{}{}
			}
			return out, nil, true
		}
		return nil, nil, false
	}
	return newFakeClientFrom(t, fake)
}

func callTool(t *testing.T, r *Registry, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	out, err := r.CallTool(t.Context(), name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("%s: not JSON: %v\n%s", name, err, out)
	}
	return resp
}

func lastParams(t *testing.T, fake *fakeMiddleware, method string) []interface{} {
	t.Helper()
	calls := fake.called(method)
	if len(calls) == 0 {
		t.Fatalf("%s was not called", method)
	}
	var p []interface{}
	if err := json.Unmarshal(calls[0].Params, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestQueryReplicationsNeverLeaksCredentials checks the projection and the
// server's redaction backstop together: no SSH connection detail, key
// material, encryption key or netcat address reaches the client, and the
// keychain connection id is still visible.
func TestQueryReplicationsNeverLeaksCredentials(t *testing.T) {
	client, _ := taskQueryFake(t, []interface{}{pushReplicationFixture(), pullReplicationFixture()}, nil, nil, nil)
	r := NewRegistry(client, nil)
	out, err := r.CallTool(t.Context(), "query_replications", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	final := redact.ToolOutput(out)
	for _, body := range []string{out, final} {
		for _, s := range replicationSentinels {
			if strings.Contains(body, s) {
				t.Errorf("output contains %q:\n%s", s, body)
			}
		}
		for _, k := range []string{"ssh_credentials", "attributes", "encryption_key", "netcat_active_side_listen_address", "properties_override", "arguments"} {
			if strings.Contains(body, `"`+k+`"`) {
				t.Errorf("output contains field %q:\n%s", k, body)
			}
		}
	}
	if final != out {
		t.Errorf("redaction backstop changed the projected output, so a field name looks sensitive:\nbefore: %s\nafter: %s", out, final)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(final), &resp); err != nil {
		t.Fatal(err)
	}
	list := resp["replications"].([]interface{})
	push := list[0].(map[string]interface{})
	if push["ssh_connection_id"] != 7.0 {
		t.Errorf("ssh_connection_id = %v, want 7", push["ssh_connection_id"])
	}
	if push["encryption"] != true {
		t.Errorf("encryption flag = %v, want true", push["encryption"])
	}
	pull := list[1].(map[string]interface{})
	if _, ok := pull["ssh_connection_id"]; ok {
		t.Errorf("LOCAL task has ssh_connection_id: %v", pull)
	}
}

func TestQueryReplicationsSummary(t *testing.T) {
	client, fake := taskQueryFake(t, []interface{}{pushReplicationFixture(), pullReplicationFixture()}, nil, nil, nil)
	r := NewRegistry(client, nil)
	resp := callTool(t, r, "query_replications", map[string]interface{}{})
	if resp["count"] != 2.0 || resp["total_replications"] != 2.0 {
		t.Errorf("count/total = %v/%v, want 2/2", resp["count"], resp["total_replications"])
	}
	push := resp["replications"].([]interface{})[0].(map[string]interface{})
	for k, want := range map[string]interface{}{"direction": "PUSH", "transport": "SSH+NETCAT", "target_dataset": "backup/data", "recursive": true, "enabled": true, "netcat_active_side": "REMOTE"} {
		if push[k] != want {
			t.Errorf("%s = %v, want %v", k, push[k], want)
		}
	}
	if src := push["source_datasets"].([]interface{}); len(src) != 1 || src[0] != "tank/data" {
		t.Errorf("source_datasets = %v", src)
	}
	bound := push["bound_snapshot_tasks"].([]interface{})
	if len(bound) != 1 || bound[0].(map[string]interface{})["id"] != 3.0 {
		t.Errorf("bound_snapshot_tasks = %v", bound)
	}
	if trig, _ := push["trigger"].(string); !strings.Contains(trig, "periodic snapshot task(s) 3") || !strings.Contains(trig, "does not create") {
		t.Errorf("trigger = %q, want it to say it runs after task 3 and creates nothing", trig)
	}
	last := push["last_run"].(map[string]interface{})
	if last["state"] != "FINISHED" || last["last_snapshot"] != "tank/data@auto-2026-01-01_00-00" || last["datetime"] != "2026-01-01T00:00:00Z" {
		t.Errorf("last_run = %v", last)
	}
	if w := last["warnings"].([]interface{}); len(w) != 1 {
		t.Errorf("warnings = %v", w)
	}
	if ret := push["retention"].(map[string]interface{}); ret["policy"] != "SOURCE" {
		t.Errorf("retention = %v", ret)
	}
	job := push["current_job"].(map[string]interface{})
	if job["id"] != 42.0 || job["state"] != "SUCCESS" {
		t.Errorf("current_job = %v", job)
	}

	pull := resp["replications"].([]interface{})[1].(map[string]interface{})
	sched := pull["schedule"].(map[string]interface{})
	if sched["human"] != "Daily at 2:30" {
		t.Errorf("schedule.human = %v", sched["human"])
	}
	if ret := pull["retention"].(map[string]interface{}); ret["policy"] != "CUSTOM" || ret["lifetime_value"] != 3.0 || ret["lifetime_unit"] != "DAY" {
		t.Errorf("retention = %v", ret)
	}
	if pull["trigger"] != "runs on its own schedule" {
		t.Errorf("trigger = %v", pull["trigger"])
	}
	if pull["last_run"].(map[string]interface{})["last_snapshot"] != nil {
		t.Errorf("never-run task last_snapshot = %v, want null", pull["last_run"])
	}

	p := lastParams(t, fake, "replication.query")
	opts := p[1].(map[string]interface{})
	if ob := opts["order_by"].([]interface{}); len(ob) != 1 || ob[0] != "id" {
		t.Errorf("order_by = %v, want a single key [id]", ob)
	}
	if opts["limit"] != 50.0 {
		t.Errorf("limit = %v, want default 50", opts["limit"])
	}
}

func TestQueryReplicationsTaskIDAndLimit(t *testing.T) {
	client, fake := taskQueryFake(t, []interface{}{pushReplicationFixture(), pullReplicationFixture()}, nil, nil, nil)
	r := NewRegistry(client, nil)

	resp := callTool(t, r, "query_replications", map[string]interface{}{"task_id": 2.0})
	if resp["count"] != 1.0 || resp["replications"].([]interface{})[0].(map[string]interface{})["id"] != 2.0 {
		t.Errorf("task_id 2: %v", resp)
	}
	p := lastParams(t, fake, "replication.query")
	if f := p[0].([]interface{}); len(f) != 1 || f[0].([]interface{})[0] != "id" {
		t.Errorf("filters = %v, want [[id = 2]]", f)
	}

	if _, err := r.CallTool(t.Context(), "query_replications", map[string]interface{}{"task_id": 9.0}); err == nil || !strings.Contains(err.Error(), "replication task 9 not found") {
		t.Errorf("unknown task_id err = %v", err)
	}

	resp = callTool(t, r, "query_replications", map[string]interface{}{"limit": 1.0})
	if resp["count"] != 1.0 || resp["total_replications"] != 2.0 {
		t.Errorf("limit 1: count/total = %v/%v, want 1/2", resp["count"], resp["total_replications"])
	}
	if note, _ := resp["note"].(string); !strings.Contains(note, "Showing 1 of 2") {
		t.Errorf("note = %q", note)
	}

	resp = callTool(t, r, "query_replications", map[string]interface{}{"limit": 5000.0})
	if note, _ := resp["note"].(string); !strings.Contains(note, "capped to 1000") {
		t.Errorf("note = %q, want capped", note)
	}
}

func snapshotTaskFixture(id float64, dataset string, recursive bool, exclude []interface{}, last interface{}) map[string]interface{} {
	state := map[string]interface{}{"state": "PENDING"}
	if last != nil {
		state = map[string]interface{}{"state": "FINISHED", "datetime": map[string]interface{}{"$date": 1767225600000.0}, "last_snapshot": last}
	}
	return map[string]interface{}{
		"id": id, "dataset": dataset, "recursive": recursive, "exclude": exclude, "enabled": true,
		"naming_schema": "auto-%Y-%m-%d_%H-%M", "allow_empty": true, "lifetime_value": 2.0, "lifetime_unit": "WEEK",
		"schedule":    map[string]interface{}{"minute": "0", "hour": "*", "dom": "*", "month": "*", "dow": "*", "begin": "09:00", "end": "18:00"},
		"vmware_sync": false,
		"state":       state,
	}
}

func snap(dataset, name string) map[string]interface{} {
	return map[string]interface{}{"id": dataset + "@" + name, "name": dataset + "@" + name, "dataset": dataset, "snapshot_name": name, "pool": datasetPool(dataset)}
}

// TestQuerySnapshotTasksCoverage is the recursive-scope case: a child
// created after the last run has no snapshot from it, an excluded child is
// not covered, and both are reported.
func TestQuerySnapshotTasksCoverage(t *testing.T) {
	const s = "auto-2026-01-01_00-00"
	tasksFx := []interface{}{
		snapshotTaskFixture(3, "tank/data", true, []interface{}{"tank/data/scratch"}, "tank/data@"+s),
		snapshotTaskFixture(4, "tank/vm", false, []interface{}{}, nil),
		snapshotTaskFixture(5, "tank/new", true, []interface{}{}, nil),
	}
	datasets := []string{"tank", "tank/data", "tank/data/a", "tank/data/b", "tank/data/scratch", "tank/data/scratch/x", "tank/dataset2", "tank/vm", "tank/new"}
	snaps := []map[string]interface{}{snap("tank/data", s), snap("tank/data/a", s), snap("tank/dataset2", s)}
	client, fake := taskQueryFake(t, []interface{}{pushReplicationFixture()}, tasksFx, datasets, snaps)
	r := NewRegistry(client, nil)
	resp := callTool(t, r, "query_snapshot_tasks", map[string]interface{}{})
	list := resp["snapshot_tasks"].([]interface{})
	if len(list) != 3 {
		t.Fatalf("snapshot_tasks = %v", list)
	}

	rec := list[0].(map[string]interface{})
	if rec["last_snapshot"] != "tank/data@"+s {
		t.Errorf("last_snapshot = %v", rec["last_snapshot"])
	}
	cov := rec["coverage"].(map[string]interface{})
	if cov["datasets_covered"] != 3.0 {
		t.Errorf("datasets_covered = %v, want 3 (data, data/a, data/b; scratch excluded, dataset2 is not a child)", cov["datasets_covered"])
	}
	missing := cov["datasets_missing_last_run_snapshot"].([]interface{})
	if len(missing) != 1 || missing[0] != "tank/data/b" {
		t.Errorf("missing = %v, want [tank/data/b]", missing)
	}
	if cov["datasets_with_last_run_snapshot"] != 2.0 {
		t.Errorf("datasets_with_last_run_snapshot = %v, want 2", cov["datasets_with_last_run_snapshot"])
	}
	if note, _ := cov["note"].(string); !strings.Contains(note, "created (or un-excluded) after the last run") {
		t.Errorf("note = %q", note)
	}
	if b := rec["bound_replications"].([]interface{}); len(b) != 1 || b[0] != 1.0 {
		t.Errorf("bound_replications = %v, want [1]", b)
	}
	sched := rec["schedule"].(map[string]interface{})
	if h, _ := sched["human"].(string); !strings.Contains(h, "between 09:00 and 18:00") {
		t.Errorf("schedule.human = %q", h)
	}
	if ret := rec["retention"].(map[string]interface{}); ret["human"] != "2 week(s)" {
		t.Errorf("retention = %v", ret)
	}

	nonRec := list[1].(map[string]interface{})
	if _, ok := nonRec["coverage"]; ok {
		t.Errorf("non-recursive task has coverage: %v", nonRec)
	}
	if nonRec["last_snapshot"] != nil {
		t.Errorf("never-run task last_snapshot = %v", nonRec["last_snapshot"])
	}
	neverRun := list[2].(map[string]interface{})
	if note, _ := neverRun["coverage"].(map[string]interface{})["note"].(string); !strings.Contains(note, "no recorded last snapshot") {
		t.Errorf("never-run recursive coverage note = %q", note)
	}

	// The coverage lookup asks for exactly the covered datasets and the
	// last-run snapshot name.
	sp := lastParams(t, fake, "pool.snapshot.query")
	filters := sp[0].([]interface{})
	in := filters[0].([]interface{})
	if in[0] != "dataset" || in[1] != "in" || len(in[2].([]interface{})) != 3 {
		t.Errorf("snapshot filter = %v", in)
	}
	if name := filters[1].([]interface{}); name[0] != "snapshot_name" || name[2] != s {
		t.Errorf("snapshot name filter = %v", name)
	}
	// One dataset listing for the pool, not one per task.
	if n := len(fake.called("pool.dataset.query")); n != 1 {
		t.Errorf("pool.dataset.query calls = %d, want 1", n)
	}
	p := lastParams(t, fake, "pool.snapshottask.query")
	if ob := p[1].(map[string]interface{})["order_by"].([]interface{}); len(ob) != 1 {
		t.Errorf("order_by = %v, want one key", ob)
	}
}

func TestQuerySnapshotTasksFilters(t *testing.T) {
	tasksFx := []interface{}{
		snapshotTaskFixture(3, "tank/data", false, []interface{}{}, "tank/data@x"),
		snapshotTaskFixture(4, "tank/vm", false, []interface{}{}, nil),
	}
	client, fake := taskQueryFake(t, nil, tasksFx, nil, nil)
	r := NewRegistry(client, nil)

	resp := callTool(t, r, "query_snapshot_tasks", map[string]interface{}{"dataset": "tank/vm"})
	if resp["count"] != 1.0 || resp["dataset_filter"] != "tank/vm" {
		t.Errorf("dataset filter: %v", resp)
	}
	resp = callTool(t, r, "query_snapshot_tasks", map[string]interface{}{"task_id": 3.0})
	if resp["count"] != 1.0 || resp["snapshot_tasks"].([]interface{})[0].(map[string]interface{})["id"] != 3.0 {
		t.Errorf("task_id filter: %v", resp)
	}
	if _, err := r.CallTool(t.Context(), "query_snapshot_tasks", map[string]interface{}{"task_id": 99.0}); err == nil || !strings.Contains(err.Error(), "periodic snapshot task 99 not found") {
		t.Errorf("unknown task_id err = %v", err)
	}
	resp = callTool(t, r, "query_snapshot_tasks", map[string]interface{}{"limit": 1.0})
	if resp["count"] != 1.0 || resp["total_snapshot_tasks"] != 2.0 {
		t.Errorf("limit: %v", resp)
	}
	if len(fake.called("pool.snapshot.query")) != 0 {
		t.Error("non-recursive tasks should not need a snapshot lookup")
	}
}

func TestQuerySnapshotTasksReplicationQueryFails(t *testing.T) {
	client, fake := taskQueryFake(t, nil, []interface{}{snapshotTaskFixture(3, "tank/data", false, []interface{}{}, nil)}, nil, nil)
	inner := fake.handle
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "replication.query" {
			return nil, map[string]interface{}{"message": "Not authorized", "code": -32001}, true
		}
		return inner(method, params)
	}
	r := NewRegistry(client, nil)
	resp := callTool(t, r, "query_snapshot_tasks", map[string]interface{}{})
	if note, _ := resp["note"].(string); !strings.Contains(note, "bound replication tasks are unknown") {
		t.Errorf("note = %q", note)
	}
	if _, ok := resp["snapshot_tasks"].([]interface{})[0].(map[string]interface{})["bound_replications"]; ok {
		t.Error("bound_replications should be omitted when unknown")
	}
}

func TestCoveredDatasets(t *testing.T) {
	names := []string{"p", "p/a", "p/a/x", "p/ab", "p/b", "p/b/c"}
	got := coveredDatasets("p/a", true, nil, names)
	if strings.Join(got, ",") != "p/a,p/a/x" {
		t.Errorf("recursive p/a = %v (p/ab is not a child)", got)
	}
	got = coveredDatasets("p", true, []string{"p/b"}, names)
	if strings.Join(got, ",") != "p,p/a,p/a/x,p/ab" {
		t.Errorf("recursive p excluding p/b = %v", got)
	}
	got = coveredDatasets("p/b", false, nil, names)
	if strings.Join(got, ",") != "p/b" {
		t.Errorf("non-recursive p/b = %v", got)
	}
	if got := coveredDatasets("q", true, nil, names); len(got) != 0 {
		t.Errorf("missing dataset = %v", got)
	}
}

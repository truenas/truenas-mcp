package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

func dateDaysAgoMS(days int) float64 {
	t := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	return float64(t.Unix() * 1000)
}

func scrubPoolRecord(name, status string, size interface{}, lastScrubDays *int) map[string]interface{} {
	rec := map[string]interface{}{
		"id": 1.0, "name": name, "size": size, "status": status,
	}
	if lastScrubDays != nil {
		rec["scan"] = map[string]interface{}{
			"function": "SCRUB", "state": "FINISHED", "errors": 0.0,
			"end_time": map[string]interface{}{"$date": dateDaysAgoMS(*lastScrubDays)},
		}
	}
	return rec
}

func scrubRunResults(version string, pool map[string]interface{}, jobs []interface{}) map[string]interface{} {
	if jobs == nil {
		jobs = []interface{}{}
	}
	return map[string]interface{}{
		"system.info":      map[string]interface{}{"hostname": "nas", "timezone": "UTC", "version": version},
		"pool.query":       []interface{}{pool},
		"core.get_jobs":    jobs,
		"pool.scrub.scrub": 99,
		"zpool.scrub.run":  77,
		"pool.scrub.run":   nil,
	}
}

// validateGetJobsFilters mirrors middleware validate_filters: each filter is
// a 3-element [field, op, value] or an OR/AND node. The run_scrub dry-run
// used to wrap two filters in an extra list, which this rejects.
func validateGetJobsFilters(params json.RawMessage) error {
	var raw []interface{}
	if err := json.Unmarshal(params, &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	filters, ok := raw[0].([]interface{})
	if !ok {
		return fmt.Errorf("Invalid operation")
	}
	return validateFilterList(filters)
}

func validateFilterList(filters []interface{}) error {
	for _, f := range filters {
		if err := validateFilterNode(f); err != nil {
			return err
		}
	}
	return nil
}

func validateFilterNode(f interface{}) error {
	arr, ok := f.([]interface{})
	if !ok || len(arr) == 0 {
		return fmt.Errorf("Invalid operation")
	}
	if op, ok := arr[0].(string); ok && (op == "OR" || op == "AND") {
		if len(arr) != 2 {
			return fmt.Errorf("Invalid operation")
		}
		inner, ok := arr[1].([]interface{})
		if !ok {
			return fmt.Errorf("Invalid operation")
		}
		return validateFilterList(inner)
	}
	if len(arr) != 3 {
		return fmt.Errorf("Invalid operation")
	}
	if _, ok := arr[1].(string); !ok {
		return fmt.Errorf("Invalid operation")
	}
	return nil
}

func filterGetJobs(jobs []interface{}, params json.RawMessage) []interface{} {
	var raw []interface{}
	if err := json.Unmarshal(params, &raw); err != nil || len(raw) == 0 {
		return jobs
	}
	filters, _ := raw[0].([]interface{})
	var out []interface{}
	for _, j := range jobs {
		m, ok := j.(map[string]interface{})
		if !ok {
			continue
		}
		if jobMatchesFilters(m, filters) {
			out = append(out, j)
		}
	}
	return out
}

func jobMatchesFilters(job map[string]interface{}, filters []interface{}) bool {
	for _, f := range filters {
		if !jobMatchesNode(job, f) {
			return false
		}
	}
	return true
}

func jobMatchesNode(job map[string]interface{}, node interface{}) bool {
	arr, ok := node.([]interface{})
	if !ok || len(arr) == 0 {
		return false
	}
	if op, ok := arr[0].(string); ok && op == "OR" {
		inner, _ := arr[1].([]interface{})
		for _, n := range inner {
			if jobMatchesNode(job, n) {
				return true
			}
		}
		return false
	}
	if op, ok := arr[0].(string); ok && op == "AND" {
		inner, _ := arr[1].([]interface{})
		return jobMatchesFilters(job, inner)
	}
	if len(arr) != 3 {
		return false
	}
	field, _ := arr[0].(string)
	op, _ := arr[1].(string)
	got := job[field]
	switch op {
	case "=":
		return fmt.Sprint(got) == fmt.Sprint(arr[2])
	case "in":
		list, _ := arr[2].([]interface{})
		want := fmt.Sprint(got)
		for _, v := range list {
			if fmt.Sprint(v) == want {
				return true
			}
		}
		// also allow []string
		if sl, ok := arr[2].([]string); ok {
			for _, v := range sl {
				if v == want {
					return true
				}
			}
		}
	}
	return false
}

func newScrubRunFake(t *testing.T, results map[string]interface{}) (*truenas.Client, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{results: results}
	poolResults := results
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "core.get_jobs" {
			if err := validateGetJobsFilters(params); err != nil {
				return nil, map[string]interface{}{"message": err.Error(), "code": -32001}, true
			}
			jobs, _ := poolResults["core.get_jobs"].([]interface{})
			return filterGetJobs(jobs, params), nil, true
		}
		if method != "pool.query" {
			return nil, nil, false
		}
		pools, ok := poolResults["pool.query"].([]interface{})
		if !ok {
			return nil, nil, false
		}
		name := poolQueryNameFilter(params)
		if name == "" {
			return pools, nil, true
		}
		var matched []interface{}
		for _, p := range pools {
			m, _ := p.(map[string]interface{})
			if m["name"] == name {
				matched = append(matched, p)
			}
		}
		return matched, nil, true
	}
	return newFakeClientFrom(t, fake)
}

func newScrubRunRegistry(t *testing.T, results map[string]interface{}) (*Registry, *fakeMiddleware, *tasks.Manager) {
	t.Helper()
	client, fake := newScrubRunFake(t, results)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: 5 * time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake, mgr
}

func noStartCalls(t *testing.T, fake *fakeMiddleware) {
	t.Helper()
	for _, m := range []string{"pool.scrub.run", "pool.scrub.scrub", "zpool.scrub.run"} {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("%s was sent %d time(s), want 0", m, n)
		}
	}
}

// TestRunScrubJobLookupAndStart is the regression for run_scrub reporting
// a started scrub that never began, 26+ zpool.scrub.run jobs being missed,
// and the dry-run core.get_jobs filter being rejected by validate_filters.
func TestRunScrubJobLookupAndStart(t *testing.T) {
	t.Run("within threshold does not start a scrub", func(t *testing.T) {
		days := 2
		r, fake, _ := newScrubRunRegistry(t, scrubRunResults("25.10.0", scrubPoolRecord("tank", "ONLINE", 1.0e12, &days), nil))
		out, err := r.CallTool("run_scrub", map[string]interface{}{"pool": "tank"})
		if err != nil {
			t.Fatalf("run_scrub: %v", err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["scrub_started"] != false {
			t.Errorf("scrub_started = %v, want false; body=%s", resp["scrub_started"], out)
		}
		if _, ok := resp["task_id"]; ok {
			t.Errorf("task_id set although nothing started: %s", out)
		}
		noStartCalls(t, fake)
	})

	t.Run("successful start tracks the start-call job id", func(t *testing.T) {
		days := 30
		oldJob := map[string]interface{}{
			"id": 10.0, "method": "pool.scrub.scrub", "state": "SUCCESS",
			"arguments":    []interface{}{"tank"},
			"time_started": map[string]interface{}{"$date": dateDaysAgoMS(40)},
		}
		r, fake, _ := newScrubRunRegistry(t, scrubRunResults("25.10.0", scrubPoolRecord("tank", "ONLINE", 1.0e12, &days), []interface{}{oldJob}))
		out, err := r.CallTool("run_scrub", map[string]interface{}{"pool": "tank"})
		if err != nil {
			t.Fatalf("run_scrub: %v", err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["scrub_started"] != true {
			t.Errorf("scrub_started = %v, want true; body=%s", resp["scrub_started"], out)
		}
		if resp["job_id"] != float64(99) {
			t.Errorf("job_id = %v, want 99 (the start call), not an older job; body=%s", resp["job_id"], out)
		}
		if resp["poll_interval"] != float64(5) {
			t.Errorf("poll_interval = %v, want the configured task poll interval (5)", resp["poll_interval"])
		}
		if n := len(fake.called("pool.scrub.scrub")); n != 1 {
			t.Errorf("pool.scrub.scrub calls = %d, want 1", n)
		}
		if n := len(fake.called("pool.scrub.run")); n != 0 {
			t.Errorf("pool.scrub.run was sent %d time(s)", n)
		}
	})

	t.Run("27.x uses zpool.scrub.run and job lookup matches pool_name", func(t *testing.T) {
		days := 30
		running := map[string]interface{}{
			"id": 77.0, "method": "zpool.scrub.run", "state": "RUNNING",
			"arguments":    []interface{}{map[string]interface{}{"pool_name": "tank", "action": "START"}},
			"progress":     map[string]interface{}{"percent": 15.0, "description": "scrubbing"},
			"time_started": map[string]interface{}{"$date": dateDaysAgoMS(0)},
		}
		results := scrubRunResults("27.0.0", scrubPoolRecord("tank", "ONLINE", 1.0e12, &days), []interface{}{running})
		r, fake, _ := newScrubRunRegistry(t, results)

		out, err := r.CallTool("get_scrub_status", map[string]interface{}{"pool": "tank"})
		if err != nil {
			t.Fatalf("get_scrub_status: %v", err)
		}
		var status map[string]interface{}
		if err := json.Unmarshal([]byte(out), &status); err != nil {
			t.Fatal(err)
		}
		tank := findPoolByName(t, status, "tank")
		cur, _ := tank["current_scrub"].(map[string]interface{})
		if cur["running"] != true {
			t.Errorf("get_scrub_status missed zpool.scrub.run job: %v", cur)
		}

		out, err = r.CallTool("run_scrub", map[string]interface{}{"pool": "tank"})
		if err == nil {
			t.Fatalf("second run_scrub should be blocked by the running zpool job; out=%s", out)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "already running") {
			t.Errorf("run_scrub error = %v, want already running", err)
		}
		if n := len(fake.called("zpool.scrub.run")); n != 0 {
			t.Errorf("zpool.scrub.run start was sent although a scrub is running")
		}

		// A start on a quiet 27.x system still uses pool.scrub.scrub,
		// which fails the job when the start fails; zpool.scrub.run is
		// only used for job lookups (scheduled scrubs).
		results = scrubRunResults("27.0.0", scrubPoolRecord("tank", "ONLINE", 1.0e12, &days), nil)
		r, fake, _ = newScrubRunRegistry(t, results)
		out, err = r.CallTool("run_scrub", map[string]interface{}{"pool": "tank"})
		if err != nil {
			t.Fatalf("27.x run_scrub: %v", err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		if n := len(fake.called("zpool.scrub.run")); n != 0 {
			t.Errorf("zpool.scrub.run was used to start (%d call(s)); body=%s", n, out)
		}
		if n := len(fake.called("pool.scrub.scrub")); n != 1 {
			t.Errorf("pool.scrub.scrub calls = %d, want 1; body=%s", n, out)
		}
		if resp["job_id"] != float64(99) || resp["scrub_started"] != true {
			t.Errorf("27.x start result = %s", out)
		}
	})

	t.Run("dry-run core.get_jobs filters pass validate_filters", func(t *testing.T) {
		days := 30
		r, fake, _ := newScrubRunRegistry(t, scrubRunResults("25.10.0", scrubPoolRecord("tank", "ONLINE", 1.0e12, &days), nil))
		out, err := r.CallTool("run_scrub", map[string]interface{}{"pool": "tank", "dry_run": true})
		if err != nil {
			t.Fatalf("run_scrub dry-run: %v (filters may have been rejected)", err)
		}
		if !strings.Contains(out, `"dry_run"`) && !strings.Contains(out, `"planned_actions"`) && !strings.Contains(out, "Start manual scrub") {
			t.Errorf("unexpected dry-run body: %s", out)
		}
		noStartCalls(t, fake)
		if n := len(fake.called("core.get_jobs")); n == 0 {
			t.Error("dry-run did not query jobs")
		}
	})

	t.Run("start result without a job id is an error", func(t *testing.T) {
		days := 30
		results := scrubRunResults("25.10.0", scrubPoolRecord("tank", "ONLINE", 1.0e12, &days), nil)
		results["pool.scrub.scrub"] = nil
		results["pool.scrub.run"] = nil
		r, _, _ := newScrubRunRegistry(t, results)
		out, err := r.CallTool("run_scrub", map[string]interface{}{"pool": "tank"})
		if err == nil {
			t.Fatalf("run_scrub succeeded without a job id: %s", out)
		}
		if strings.Contains(out, `"scrub_started": true`) || strings.Contains(out, `"job_id": 0`) {
			t.Errorf("claimed a started scrub with job_id 0: %s", out)
		}
	})

	for _, status := range []string{"FAULTED", "UNAVAIL", "REMOVED", "OFFLINE"} {
		status := status
		t.Run("refuses "+status+" pool", func(t *testing.T) {
			r, fake, _ := newScrubRunRegistry(t, scrubRunResults("25.10.0", scrubPoolRecord("tank", status, nil, nil), nil))
			for _, dry := range []bool{false, true} {
				args := map[string]interface{}{"pool": "tank"}
				if dry {
					args["dry_run"] = true
				}
				out, err := r.CallTool("run_scrub", args)
				if err == nil {
					t.Fatalf("dry_run=%v on %s succeeded: %s", dry, status, out)
				}
				if !strings.Contains(err.Error(), status) {
					t.Errorf("dry_run=%v error = %v, want %s", dry, err, status)
				}
				if strings.Contains(out, `"scrub_started": true`) {
					t.Errorf("dry_run=%v reported a started scrub: %s", dry, out)
				}
			}
			noStartCalls(t, fake)
		})
	}

	t.Run("27.x start uses pool.scrub.scrub not zpool.scrub.run", func(t *testing.T) {
		days := 30
		r, fake, _ := newScrubRunRegistry(t, scrubRunResults("27.0.0", scrubPoolRecord("tank", "ONLINE", 1.0e12, &days), nil))
		out, err := r.CallTool("run_scrub", map[string]interface{}{"pool": "tank"})
		if err != nil {
			t.Fatalf("27.x run_scrub: %v", err)
		}
		if n := len(fake.called("zpool.scrub.run")); n != 0 {
			t.Errorf("zpool.scrub.run was used to start (%d call(s)); it can succeed without starting a scrub: %s", n, out)
		}
		if n := len(fake.called("pool.scrub.scrub")); n != 1 {
			t.Errorf("pool.scrub.scrub calls = %d, want 1; body=%s", n, out)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["job_id"] != float64(99) || resp["scrub_started"] != true {
			t.Errorf("27.x start result = %s", out)
		}
	})

	t.Run("threshold allows a scrub at 6.5 days with default 7", func(t *testing.T) {
		// Middleware is due once age >= (threshold-1)*24h; 6.5 days must start.
		ended := time.Now().UTC().Add(-13 * 12 * time.Hour)
		pool := scrubPoolRecord("tank", "ONLINE", 1.0e12, nil)
		pool["scan"] = map[string]interface{}{
			"function": "SCRUB", "state": "FINISHED", "errors": 0.0,
			"end_time": map[string]interface{}{"$date": float64(ended.Unix() * 1000)},
		}
		r, fake, _ := newScrubRunRegistry(t, scrubRunResults("25.10.0", pool, nil))
		out, err := r.CallTool("run_scrub", map[string]interface{}{"pool": "tank"})
		if err != nil {
			t.Fatalf("6.5-day run_scrub: %v", err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["scrub_started"] != true {
			t.Errorf("6.5 days with threshold 7 was refused: %s", out)
		}
		if n := len(fake.called("pool.scrub.scrub")); n != 1 {
			t.Errorf("start calls = %d, want 1; body=%s", n, out)
		}
	})

	t.Run("DEGRADED pool is allowed", func(t *testing.T) {
		days := 30
		r, fake, _ := newScrubRunRegistry(t, scrubRunResults("25.10.0", scrubPoolRecord("tank", "DEGRADED", 1.0e12, &days), nil))
		out, err := r.CallTool("run_scrub", map[string]interface{}{"pool": "tank"})
		if err != nil {
			t.Fatalf("DEGRADED run_scrub: %v", err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["scrub_started"] != true {
			t.Errorf("DEGRADED start = %s", out)
		}
		if n := len(fake.called("pool.scrub.scrub")); n != 1 {
			t.Errorf("start calls = %d, want 1", n)
		}
	})
}

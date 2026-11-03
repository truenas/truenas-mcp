package tools

import (
	"encoding/json"
	"testing"
)

// jobsQueries returns the filters and options of each core.get_jobs call.
func jobsQueries(t *testing.T, fake *fakeMiddleware) (filters []string, opts []map[string]interface{}) {
	t.Helper()
	for _, c := range fake.called("core.get_jobs") {
		var raw []json.RawMessage
		if err := json.Unmarshal(c.Params, &raw); err != nil || len(raw) != 2 {
			t.Fatalf("bad core.get_jobs params %s", c.Params)
		}
		var o map[string]interface{}
		if err := json.Unmarshal(raw[1], &o); err != nil {
			t.Fatalf("bad options %s", raw[1])
		}
		var f interface{}
		_ = json.Unmarshal(raw[0], &f)
		norm, _ := json.Marshal(f)
		filters = append(filters, string(norm))
		opts = append(opts, o)
	}
	return filters, opts
}

// Without a state, query_jobs returns jobs in every state, not only RUNNING.
func TestQueryJobsDefaultsToAllStates(t *testing.T) {
	for _, args := range []map[string]interface{}{
		{"limit": 10.0},
		{},
		{"state": "all"},
		{"state": "ALL"},
		{"state": ""},
	} {
		resp, fake := queryJobs(t, numberedJobs(3), 3, args)
		if resp["state_filter"] != "ALL" {
			t.Errorf("%v: state_filter = %v, want ALL", args, resp["state_filter"])
		}
		if resp["job_count"] != 3.0 {
			t.Errorf("%v: job_count = %v, want 3", args, resp["job_count"])
		}
		if _, has := resp["method_filter"]; has {
			t.Errorf("%v: unexpected method_filter %v", args, resp["method_filter"])
		}
		filters, _ := jobsQueries(t, fake)
		if len(filters) != 1 || filters[0] != "[]" {
			t.Errorf("%v: filters = %v, want no filter", args, filters)
		}
	}
}

// state and method are applied by the middleware, so limit, has_more and
// total count only matching jobs.
func TestQueryJobsStateAndMethodFilters(t *testing.T) {
	cases := []struct {
		args        map[string]interface{}
		wantFilters string
		wantState   string
		wantMethod  interface{}
	}{
		{map[string]interface{}{"state": "RUNNING"}, `[["state","=","RUNNING"]]`, "RUNNING", nil},
		{map[string]interface{}{"method": "app.upgrade"}, `[["method","=","app.upgrade"]]`, "ALL", "app.upgrade"},
		{map[string]interface{}{"state": "FAILED", "method": "app.upgrade"},
			`[["state","=","FAILED"],["method","=","app.upgrade"]]`, "FAILED", "app.upgrade"},
	}
	for _, c := range cases {
		// More matches than the limit: the count query must use the same filters.
		args := map[string]interface{}{"limit": 2.0}
		for k, v := range c.args {
			args[k] = v
		}
		resp, fake := queryJobs(t, numberedJobs(5), 5, args)
		if resp["state_filter"] != c.wantState || resp["method_filter"] != c.wantMethod {
			t.Errorf("%v: state_filter=%v method_filter=%v", c.args, resp["state_filter"], resp["method_filter"])
		}
		if resp["has_more"] != true || resp["total"] != 5.0 || resp["job_count"] != 2.0 {
			t.Errorf("%v: has_more=%v total=%v job_count=%v", c.args, resp["has_more"], resp["total"], resp["job_count"])
		}
		filters, opts := jobsQueries(t, fake)
		if len(filters) != 2 {
			t.Fatalf("%v: %d core.get_jobs calls, want list and count", c.args, len(filters))
		}
		for i, f := range filters {
			if f != c.wantFilters {
				t.Errorf("%v: call %d filters = %s, want %s", c.args, i, f, c.wantFilters)
			}
		}
		if opts[0]["limit"] != 3.0 {
			t.Errorf("%v: options.limit = %v, want 3", c.args, opts[0]["limit"])
		}
	}
}

// The method filter keeps the 9999 cap on limit.
func TestQueryJobsMethodFilterKeepsLimitCap(t *testing.T) {
	resp, fake := queryJobs(t, numberedJobs(3), 3, map[string]interface{}{"method": "app.upgrade", "limit": 50000.0})
	if resp["limit"] != 9999.0 {
		t.Errorf("limit = %v, want 9999", resp["limit"])
	}
	_, opts := jobsQueries(t, fake)
	if len(opts) == 0 || opts[0]["limit"] != 10000.0 {
		t.Errorf("options = %v, want limit 10000", opts)
	}
}

// method is declared, so the unknown-argument check accepts it; the schema
// default for state is all states.
func TestQueryJobsSchema(t *testing.T) {
	r := NewRegistry(nil, nil)
	props := r.tools["query_jobs"].Definition.InputSchema["properties"].(map[string]interface{})
	m, ok := props["method"].(map[string]interface{})
	if !ok || m["type"] != "string" {
		t.Errorf("method not declared as a string: %v", props["method"])
	}
	if s := props["state"].(map[string]interface{}); s["default"] != "all" {
		t.Errorf("state default = %v, want all", s["default"])
	}
}

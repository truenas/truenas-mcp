package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
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
		if err := json.Unmarshal(raw[0], &f); err != nil {
			t.Fatalf("bad filters %s: %v", raw[0], err)
		}
		norm, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
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
		{"state": "All"},
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
		{map[string]interface{}{"state": "running"}, `[["state","=","RUNNING"]]`, "RUNNING", nil},
		{map[string]interface{}{"state": "Failed", "method": "app.upgrade"},
			`[["state","=","FAILED"],["method","=","app.upgrade"]]`, "FAILED", "app.upgrade"},
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

// The method filter works with the largest allowed limit.
func TestQueryJobsMethodFilterKeepsLimitCap(t *testing.T) {
	resp, fake := queryJobs(t, numberedJobs(3), 3, map[string]interface{}{"method": "app.upgrade", "limit": 9999.0})
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

// Bad state or method values are rejected before anything is sent, and the
// value itself is not echoed.
func TestQueryJobsRejectsBadStateAndMethod(t *testing.T) {
	for _, c := range []struct {
		args map[string]interface{}
		want []string
	}{
		{map[string]interface{}{"state": "DONE-s3cret"}, []string{"unknown state", "RUNNING, WAITING, SUCCESS, FAILED, ABORTED, or all"}},
		{map[string]interface{}{"state": "pending"}, []string{"unknown state"}},
		{map[string]interface{}{"state": 1.0}, []string{"state must be a string"}},
		{map[string]interface{}{"method": 42.0}, []string{"method must be a string"}},
		{map[string]interface{}{"method": []interface{}{"app.upgrade"}}, []string{"method must be a string"}},
		{map[string]interface{}{"method": map[string]interface{}{"x": "y"}}, []string{"method must be a string"}},
	} {
		fake := &fakeMiddleware{handle: jobsHandle(numberedJobs(3), 3)}
		client, _ := newFakeClientFrom(t, fake)
		mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
		_, err := NewRegistry(client, mgr).CallTool(t.Context(), "query_jobs", c.args)
		mgr.Shutdown()
		if err == nil {
			t.Errorf("%v: no error", c.args)
			continue
		}
		msg := err.Error()
		for _, w := range append(c.want, "invalid arguments for tool query_jobs", "nothing was executed") {
			if !strings.Contains(msg, w) {
				t.Errorf("%v: error %q lacks %q", c.args, msg, w)
			}
		}
		if strings.Contains(msg, "s3cret") {
			t.Errorf("%v: error echoes the value: %q", c.args, msg)
		}
		if n := len(fake.called("core.get_jobs")); n != 0 {
			t.Errorf("%v: core.get_jobs called %d times, want 0", c.args, n)
		}
	}
}

// query_jobs never asks for raw_result and never passes one through, even
// when the middleware includes it.
func TestQueryJobsNeverReturnsRawResult(t *testing.T) {
	jobs := []interface{}{map[string]interface{}{"id": 1.0, "method": "app.upgrade", "state": "SUCCESS",
		"result": "ok", "raw_result": map[string]interface{}{"blob": "raw-only-marker"}}}
	for _, args := range []map[string]interface{}{{}, {"method": "app.upgrade", "state": "success", "limit": 1.0}} {
		fake := &fakeMiddleware{handle: jobsHandle(jobs, 1)}
		client, _ := newFakeClientFrom(t, fake)
		mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
		out, err := NewRegistry(client, mgr).CallTool(t.Context(), "query_jobs", args)
		mgr.Shutdown()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "raw_result") || strings.Contains(out, "raw-only-marker") {
			t.Errorf("%v: output contains raw_result: %s", args, out)
		}
		for _, c := range fake.called("core.get_jobs") {
			if strings.Contains(string(c.Params), "raw_result") || strings.Contains(string(c.Params), "extra") {
				t.Errorf("%v: core.get_jobs params request extra data: %s", args, c.Params)
			}
		}
	}
}

// limit must be a whole number from 1 to 9999. Anything else is rejected
// before anything is sent, like a bad state or method, instead of being
// silently replaced by the default or clamped; the value is not echoed.
func TestQueryJobsRejectsBadLimit(t *testing.T) {
	for _, limit := range []interface{}{
		0.0, -1.0, -50.0, 1.5, 0.5, 9999.5, 10000.0, 20000.0, 1e300,
		"abc", "50", "", true, []interface{}{10.0}, map[string]interface{}{"n": 10.0},
		json.Number("1.5"), json.Number("abc"), json.Number("20000"),
	} {
		args := map[string]interface{}{"limit": limit}
		fake := &fakeMiddleware{handle: jobsHandle(numberedJobs(3), 3)}
		client, _ := newFakeClientFrom(t, fake)
		mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
		_, err := NewRegistry(client, mgr).CallTool(t.Context(), "query_jobs", args)
		mgr.Shutdown()
		if err == nil {
			t.Errorf("limit %#v: no error", limit)
			continue
		}
		msg := err.Error()
		for _, w := range []string{"invalid arguments for tool query_jobs", "limit must be an integer from 1 to 9999", "nothing was executed"} {
			if !strings.Contains(msg, w) {
				t.Errorf("limit %#v: error %q lacks %q", limit, msg, w)
			}
		}
		for _, echoed := range []string{"abc", "1.5", "20000", "-1"} {
			if strings.Contains(msg, echoed) {
				t.Errorf("limit %#v: error echoes a value: %q", limit, msg)
			}
		}
		if n := len(fake.called("core.get_jobs")); n != 0 {
			t.Errorf("limit %#v: core.get_jobs called %d times, want 0", limit, n)
		}
	}
}

// Valid limits, including the bounds, an integer-valued JSON number and an
// explicit null (the default), are accepted and passed through unchanged.
func TestQueryJobsAcceptsValidLimit(t *testing.T) {
	for _, c := range []struct {
		limit interface{}
		want  float64
	}{
		{1.0, 1}, {2.0, 2}, {50.0, 50}, {9999.0, 9999}, {7, 7}, {json.Number("25"), 25}, {nil, 50},
	} {
		resp, fake := queryJobs(t, numberedJobs(3), 3, map[string]interface{}{"limit": c.limit})
		if resp["limit"] != c.want {
			t.Errorf("limit %#v: reported limit %v, want %v", c.limit, resp["limit"], c.want)
		}
		_, opts := jobsQueries(t, fake)
		if len(opts) == 0 || opts[0]["limit"] != c.want+1 {
			t.Errorf("limit %#v: options = %v, want limit %v", c.limit, opts, c.want+1)
		}
	}
}

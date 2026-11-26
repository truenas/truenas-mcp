package tools

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// registryServiceFixtures are service.query rows for the registry-wide
// safety tests: cifs and ssh, both running and enabled at boot.
func registryServiceFixtures() []interface{} {
	return []interface{}{
		map[string]interface{}{"id": 4.0, "service": "cifs", "enable": true, "state": "RUNNING", "pids": []interface{}{}},
		map[string]interface{}{"id": 11.0, "service": "ssh", "enable": true, "state": "RUNNING", "pids": []interface{}{}},
	}
}

// svcActionFake answers service.query, service.control, service.update
// and core.get_jobs from mutable rows, the way 25.10 does:
// service.control is a job whose result is a boolean, and a successful
// action changes the service's state.
type svcActionFake struct {
	mu       sync.Mutex
	services map[string]map[string]interface{}
	jobs     map[float64]map[string]interface{}
	nextJob  float64
	fail     map[string]map[string]interface{} // method -> RPC error

	jobState   string      // final state of the next service.control job; default SUCCESS
	jobResult  interface{} // its result; default true
	jobError   string
	jobMethod  string // method reported by core.get_jobs; default service.control
	jobPending bool   // the job never finishes
	noReadback bool   // service.update stores nothing
}

func newSvcActionFake() *svcActionFake {
	f := &svcActionFake{
		services: map[string]map[string]interface{}{},
		jobs:     map[float64]map[string]interface{}{},
		nextJob:  100,
		fail:     map[string]map[string]interface{}{},
	}
	id := 1.0
	for _, s := range []struct {
		name, state string
		enable      bool
	}{
		{"cifs", "RUNNING", true},
		{"nfs", "STOPPED", false},
		{"iscsitarget", "RUNNING", true},
		{"nvmet", "RUNNING", true},
		{"ups", "RUNNING", true},
		{"snmp", "RUNNING", false},
		{"ftp", "UNKNOWN", false},
		{"ssh", "RUNNING", true},
		// Not service.query rows on 25.10; listed to exercise the
		// refusals in case a release exposes them.
		{"sssd", "RUNNING", true},
		{"idmap", "RUNNING", true},
		{"nscd", "RUNNING", true},
		{"keepalived", "RUNNING", true},
		{"http", "RUNNING", true},
	} {
		f.services[s.name] = map[string]interface{}{"id": id, "service": s.name, "enable": s.enable, "state": s.state, "pids": []interface{}{}}
		id++
	}
	return f
}

func (f *svcActionFake) set(name, key string, v interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services[name][key] = v
}

func (f *svcActionFake) get(name, key string) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services[name][key]
}

func (f *svcActionFake) handler(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p []interface{}
	_ = json.Unmarshal(params, &p)
	if e, ok := f.fail[method]; ok {
		return nil, e, true
	}
	switch method {
	case "service.query":
		filters, _ := p[0].([]interface{})
		out := []interface{}{}
		for _, row := range f.services {
			if jobMatchesFilters(row, filters) {
				out = append(out, row)
			}
		}
		return out, nil, true
	case "service.control":
		verb, _ := p[0].(string)
		name, _ := p[1].(string)
		f.nextJob++
		job := map[string]interface{}{"id": f.nextJob, "method": "service.control", "state": "RUNNING"}
		if f.jobMethod != "" {
			job["method"] = f.jobMethod
		}
		if !f.jobPending {
			state := f.jobState
			if state == "" {
				state = "SUCCESS"
			}
			job["state"] = state
			if state == "SUCCESS" {
				result := f.jobResult
				if result == nil {
					result = true
				}
				job["result"] = result
				if result == true && f.services[name] != nil {
					f.services[name]["state"] = map[string]string{"START": "RUNNING", "RESTART": "RUNNING", "RELOAD": "RUNNING", "STOP": "STOPPED"}[verb]
				}
			} else {
				job["error"] = f.jobError
			}
		}
		f.jobs[f.nextJob] = job
		return f.nextJob, nil, true
	case "core.get_jobs":
		filters, _ := p[0].([]interface{})
		out := []interface{}{}
		for _, j := range f.jobs {
			if jobMatchesFilters(j, filters) {
				out = append(out, j)
			}
		}
		return out, nil, true
	case "service.update":
		name, _ := p[0].(string)
		data, _ := p[1].(map[string]interface{})
		row := f.services[name]
		if row == nil {
			return nil, map[string]interface{}{"code": -32001, "message": "Service " + name + " not found."}, true
		}
		if !f.noReadback {
			row["enable"] = data["enable"]
		}
		return row["id"], nil, true
	}
	return nil, nil, false
}

func (f *svcActionFake) registry(t *testing.T, withTasks bool) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{handle: f.handler}
	client, _ := newFakeClientFrom(t, fake)
	var mgr *tasks.Manager
	if withTasks {
		mgr = tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
		t.Cleanup(mgr.Shutdown)
	}
	return NewRegistry(client, mgr), fake
}

func fastServiceJobWait(t *testing.T, wait time.Duration) {
	t.Helper()
	oldWait, oldPoll := serviceJobWait, serviceJobPoll
	serviceJobWait, serviceJobPoll = wait, 5*time.Millisecond
	t.Cleanup(func() { serviceJobWait, serviceJobPoll = oldWait, oldPoll })
}

func ctl(service, action string) map[string]interface{} {
	return map[string]interface{}{"service": service, "action": action}
}

// ---- control_service ----

func TestControlServiceRestartSuccess(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	preview, out := confirmAndRun(t, r, fake, "control_service", "service.control", ctl("cifs", "RESTART"))

	p := snapActionParams(t, fake, "service.control")
	if got := previewText(t, p); got != `["RESTART","cifs",{"silent":false}]` {
		t.Errorf("service.control params = %s, want [verb, service, {silent: false}]", got)
	}
	pv := preview["preview"].(map[string]interface{})
	cur := pv["current_state"].(map[string]interface{})
	if cur["service"] != "cifs" || cur["state"] != "RUNNING" || cur["enabled_at_boot"] != true || cur["action"] != "RESTART" {
		t.Errorf("current_state = %v", cur)
	}
	if w := previewText(t, pv["warnings"]); !strings.Contains(w, "Connected SMB clients are disconnected") {
		t.Errorf("warnings = %s", w)
	}
	summary := previewText(t, pv["confirm_summary"])
	for _, want := range []string{"Restart service cifs", `state: \"RUNNING\"`, `starts at boot: \"yes\"`} {
		if !strings.Contains(summary, want) {
			t.Errorf("confirm_summary missing %q: %s", want, summary)
		}
	}
	resp := decodeObj(t, out)
	if resp["status"] != "completed" || resp["result"] != true || resp["state"] != "RUNNING" || resp["job_id"] != 101.0 || resp["enabled_at_boot"] != true {
		t.Errorf("response = %v", resp)
	}
}

func TestControlServiceStartAndStop(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	_, out := confirmAndRun(t, r, fake, "control_service", "service.control", ctl("nfs", "START"))
	if resp := decodeObj(t, out); resp["state"] != "RUNNING" || !strings.Contains(resp["message"].(string), "START finished") {
		t.Errorf("start response = %v", resp)
	}
	if f.get("nfs", "state") != "RUNNING" {
		t.Errorf("nfs not started")
	}
	first, err := r.CallTool(t.Context(), "control_service", ctl("snmp", "STOP"))
	if err != nil {
		t.Fatal(err)
	}
	out, err = r.CallTool(t.Context(), "control_service", mergeArgs(ctl("snmp", "STOP"), map[string]interface{}{"confirmation_token": replyToken(t, first)}))
	if err != nil {
		t.Fatal(err)
	}
	if resp := decodeObj(t, out); resp["state"] != "STOPPED" || resp["running"] != false {
		t.Errorf("stop response = %v", resp)
	}
}

func TestControlServiceDryRun(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	resp := callTool(t, r, "control_service", mergeArgs(ctl("cifs", "STOP"), map[string]interface{}{"dry_run": true}))
	if n := len(fake.called("service.control")); n != 0 {
		t.Fatalf("dry run sent service.control %d time(s)", n)
	}
	if _, ok := resp["confirmation_token"]; ok {
		t.Error("dry run issued a token")
	}
	if resp["dry_run"] != true || resp["executed"] != false {
		t.Errorf("dry run flags = %v", resp)
	}
	cur := resp["current_state"].(map[string]interface{})
	if cur["state"] != "RUNNING" || cur["enabled_at_boot"] != true || cur["running"] != true {
		t.Errorf("current_state = %v", cur)
	}
	actions := resp["planned_actions"].([]interface{})
	if d := actions[0].(map[string]interface{})["description"].(string); !strings.Contains(d, "Stop service 'cifs' (currently RUNNING, starts at boot: yes)") {
		t.Errorf("planned action = %q", d)
	}
	if w := previewText(t, resp["warnings"]); !strings.Contains(w, "still starts at boot") {
		t.Errorf("warnings = %s", w)
	}
}

// Every refusal is an error for the preview, the dry run and a call with a
// token, issues no token and sends no service.control.
func TestControlServiceRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    map[string]interface{}
		want    string
		prepare func(f *svcActionFake)
	}{
		{"unknown service", ctl("nosuch", "START"), "unknown service 'nosuch'", nil},
		{"invalid name", ctl("Bad Name", "START"), "invalid service name", nil},
		{"self dependency", ctl("http", "RESTART"), "service 'http' is refused: it is the web server (nginx)", nil},
		{"self dependency with override", mergeArgs(ctl("http", "RELOAD"), map[string]interface{}{"allow_protected_service": true}), "service 'http' is refused", nil},
		{"self dependency start", ctl("http", "START"), "service 'http' is refused", func(f *svcActionFake) { f.services["http"]["state"] = "STOPPED" }},
		{"ssh stop", ctl("ssh", "STOP"), "service 'ssh' provides SSH sessions", nil},
		{"ssh restart", ctl("ssh", "RESTART"), "needs allow_protected_service=true", nil},
		{"sssd stop", ctl("sssd", "STOP"), "service 'sssd' provides LDAP and FreeIPA logins", nil},
		{"sssd reload is a restart", ctl("sssd", "RELOAD"), "service 'sssd' provides", nil},
		{"idmap restart", ctl("idmap", "RESTART"), "service 'idmap' provides Active Directory identity mapping", nil},
		{"nscd stop", ctl("nscd", "STOP"), "service 'nscd' provides user and group lookups", nil},
		{"keepalived restart", ctl("keepalived", "RESTART"), "service 'keepalived' provides HA failover", nil},
		{"start running", ctl("cifs", "START"), "already RUNNING; START would change nothing", nil},
		{"stop stopped", ctl("nfs", "STOP"), "already STOPPED; STOP would change nothing", nil},
		{"reload stopped", ctl("nfs", "RELOAD"), "already STOPPED; RELOAD would change nothing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSvcActionFake()
			if tc.prepare != nil {
				tc.prepare(f)
			}
			r, fake := f.registry(t, false)
			for _, extra := range []map[string]interface{}{{}, {"dry_run": true}, {"confirmation_token": strings.Repeat("a", 32)}} {
				out, err := r.CallTool(t.Context(), "control_service", mergeArgs(tc.args, extra))
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%v: err = %v, want %q", extra, err, tc.want)
				}
				if strings.Contains(out, `"confirmation_token": "`) {
					t.Errorf("%v: token issued", extra)
				}
			}
			if n := len(fake.called("service.control")); n != 0 {
				t.Errorf("refused call sent service.control %d time(s)", n)
			}
		})
	}
}

func TestControlServiceRejectsUnknownAction(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	for _, action := range []string{"KILL", "stop", ""} {
		if _, err := r.CallTool(t.Context(), "control_service", ctl("cifs", action)); err == nil || !strings.Contains(err.Error(), "action") {
			t.Errorf("action %q: err = %v", action, err)
		}
	}
	if n := len(fake.called("service.control")); n != 0 {
		t.Errorf("service.control sent %d time(s)", n)
	}
}

// RELOAD of a reloadable protected service does not interrupt it and is
// allowed without the override; the other protected actions run with it.
func TestControlServiceProtectedOverride(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	confirmAndRun(t, r, fake, "control_service", "service.control", ctl("ssh", "RELOAD"))

	r, fake = f.registry(t, false)
	args := mergeArgs(ctl("ssh", "RESTART"), map[string]interface{}{"allow_protected_service": true})
	preview, out := confirmAndRun(t, r, fake, "control_service", "service.control", args)
	pv := preview["preview"].(map[string]interface{})
	cur := pv["current_state"].(map[string]interface{})
	if cur["allow_protected_service"] != true || cur["protected"] != true {
		t.Errorf("current_state = %v", cur)
	}
	if s := previewText(t, pv["confirm_summary"]); !strings.Contains(s, `allow_protected_service: \"true\"`) {
		t.Errorf("confirm_summary = %s", s)
	}
	if w := previewText(t, pv["warnings"]); !strings.Contains(w, "allow_protected_service is set") {
		t.Errorf("warnings = %s", w)
	}
	if resp := decodeObj(t, out); resp["status"] != "completed" {
		t.Errorf("response = %v", resp)
	}
	if n := len(fake.called("service.control")); n != 1 {
		t.Errorf("service.control calls = %d, want 1", n)
	}
}

// A token issued for the override cannot run the call without it (or the
// other way round): the override is in the argument hash and the plan.
func TestControlServiceOverrideBoundToToken(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	with := mergeArgs(ctl("ssh", "STOP"), map[string]interface{}{"allow_protected_service": true})
	first, err := r.CallTool(t.Context(), "control_service", with)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CallTool(t.Context(), "control_service", mergeArgs(ctl("ssh", "STOP"), map[string]interface{}{"confirmation_token": replyToken(t, first)}))
	if err == nil || !strings.Contains(err.Error(), "needs allow_protected_service=true") {
		t.Errorf("err = %v", err)
	}
	if n := len(fake.called("service.control")); n != 0 {
		t.Errorf("service.control sent %d time(s)", n)
	}

	plan := func(override bool) [32]byte {
		p, err := planControlService(t.Context(), r.client, mergeArgs(ctl("cifs", "STOP"), map[string]interface{}{"allow_protected_service": override}))
		if err != nil {
			t.Fatal(err)
		}
		h, err := p.planHash()
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if plan(true) == plan(false) {
		t.Error("plan hash ignores allow_protected_service")
	}
}

func TestControlServiceClientWarnings(t *testing.T) {
	f := newSvcActionFake()
	r, _ := f.registry(t, false)
	for name, want := range map[string]string{
		"cifs":        "SMB clients",
		"nfs":         "NFS clients",
		"iscsitarget": "iSCSI initiators",
		"nvmet":       "NVMe-oF hosts",
		"ups":         "NUT clients",
	} {
		f.set(name, "state", "RUNNING")
		for _, action := range []string{"STOP", "RESTART"} {
			resp := callTool(t, r, "control_service", mergeArgs(ctl(name, action), map[string]interface{}{"dry_run": true}))
			if w := previewText(t, resp["warnings"]); !strings.Contains(w, want) {
				t.Errorf("%s %s: warnings = %s, want %q", name, action, w, want)
			}
		}
		f.set(name, "state", "STOPPED")
		resp := callTool(t, r, "control_service", mergeArgs(ctl(name, "START"), map[string]interface{}{"dry_run": true}))
		if w := previewText(t, resp["warnings"]); strings.Contains(w, want) {
			t.Errorf("%s START warns about clients: %s", name, w)
		}
	}
	f.set("ups", "state", "RUNNING")
	resp := callTool(t, r, "control_service", mergeArgs(ctl("ups", "RELOAD"), map[string]interface{}{"dry_run": true}))
	w := previewText(t, resp["warnings"])
	if !strings.Contains(w, "cannot reload: TrueNAS restarts it instead") || !strings.Contains(w, "NUT clients") {
		t.Errorf("ups RELOAD warnings = %s", w)
	}
	f.set("cifs", "state", "RUNNING")
	resp = callTool(t, r, "control_service", mergeArgs(ctl("cifs", "RELOAD"), map[string]interface{}{"dry_run": true}))
	if w := previewText(t, resp["warnings"]); strings.Contains(w, "SMB clients") || strings.Contains(w, "cannot reload") {
		t.Errorf("cifs RELOAD warnings = %s", w)
	}
	resp = callTool(t, r, "control_service", mergeArgs(ctl("ftp", "RESTART"), map[string]interface{}{"dry_run": true}))
	if w := previewText(t, resp["warnings"]); !strings.Contains(w, "UNKNOWN") {
		t.Errorf("ftp UNKNOWN warnings = %s", w)
	}
}

// The token is bound to the previewed state and boot flag: a change after
// the preview rejects it and nothing runs.
func TestControlServiceTokenRejectedAfterStateChange(t *testing.T) {
	for _, tc := range []struct {
		name       string
		key        string
		v          interface{}
		wantReason string
	}{
		{"state", "state", "UNKNOWN", ""},
		{"stopped", "state", "STOPPED", "already STOPPED"},
		{"enable", "enable", false, ""},
		{"recreated", "id", 99.0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSvcActionFake()
			r, fake := f.registry(t, false)
			first, err := r.CallTool(t.Context(), "control_service", ctl("cifs", "STOP"))
			if err != nil {
				t.Fatal(err)
			}
			tok := replyToken(t, first)
			f.set("cifs", tc.key, tc.v)
			_, err = r.CallTool(t.Context(), "control_service", mergeArgs(ctl("cifs", "STOP"), map[string]interface{}{"confirmation_token": tok}))
			if err == nil || !strings.Contains(err.Error(), "issued for a different planned operation") {
				t.Errorf("token not rejected by its plan binding: err = %v", err)
			}
			if tc.wantReason != "" && (err == nil || !strings.Contains(err.Error(), tc.wantReason)) {
				t.Errorf("err = %v, want %q", err, tc.wantReason)
			}
			if n := len(fake.called("service.control")); n != 0 {
				t.Errorf("service.control sent %d time(s)", n)
			}
		})
	}
}

func TestControlServiceJobFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *svcActionFake)
		want  []string
	}{
		{"failed", func(f *svcActionFake) {
			f.jobState, f.jobError = "FAILED", "[EFAULT] smbd: unable to bind port 445"
		}, []string{"failed to restart service 'cifs': job 101 failed", "unable to bind port 445"}},
		{"aborted", func(f *svcActionFake) { f.jobState = "ABORTED" }, []string{"job 101 aborted", "no error text was recorded"}},
		{"result false", func(f *svcActionFake) { f.jobResult = false }, []string{"finished but TrueNAS reports the action did not succeed", "state now RUNNING"}},
		{"start rejected", func(f *svcActionFake) {
			f.fail["service.control"] = map[string]interface{}{"code": -32001, "message": "permission denied"}
		}, []string{"failed to restart service 'cifs'", "permission denied"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSvcActionFake()
			r, fake := f.registry(t, false)
			first, err := r.CallTool(t.Context(), "control_service", ctl("cifs", "RESTART"))
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			tc.setup(f)
			f.mu.Unlock()
			out, err := r.CallTool(t.Context(), "control_service", mergeArgs(ctl("cifs", "RESTART"), map[string]interface{}{"confirmation_token": replyToken(t, first)}))
			for _, want := range tc.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want %q (out %s)", err, want, out)
				}
			}
			if n := len(fake.called("service.control")); n != 1 {
				t.Errorf("service.control calls = %d, want 1", n)
			}
		})
	}
}

// A job that has not finished when the wait ends is reported as running
// (and tracked as a task when the server tracks tasks), never as done.
func TestControlServiceJobStillRunning(t *testing.T) {
	fastServiceJobWait(t, 50*time.Millisecond)
	for _, withTasks := range []bool{false, true} {
		f := newSvcActionFake()
		f.jobPending = true
		r, fake := f.registry(t, withTasks)
		_, out := confirmAndRun(t, r, fake, "control_service", "service.control", ctl("cifs", "RESTART"))
		resp := decodeObj(t, out)
		if resp["status"] != "running" || resp["result"] != nil {
			t.Errorf("tasks=%v: response = %v", withTasks, resp)
		}
		if msg, _ := resp["message"].(string); !strings.Contains(msg, "was not seen to finish within") {
			t.Errorf("tasks=%v: message = %q", withTasks, msg)
		}
		if _, ok := resp["task_id"]; ok != withTasks {
			t.Errorf("tasks=%v: task_id present = %v", withTasks, ok)
		}
		if n := len(fake.called("core.get_jobs")); n < 2 {
			t.Errorf("tasks=%v: core.get_jobs polled %d time(s)", withTasks, n)
		}
	}
}

// A job id that now belongs to a different job is not taken as the result.
func TestControlServiceJobIdentity(t *testing.T) {
	fastServiceJobWait(t, time.Second)
	f := newSvcActionFake()
	f.jobMethod = "pool.scrub.scrub"
	r, fake := f.registry(t, false)
	_, out := confirmAndRun(t, r, fake, "control_service", "service.control", ctl("cifs", "RESTART"))
	resp := decodeObj(t, out)
	if resp["status"] != "running" || !strings.Contains(resp["message"].(string), "is not a service.control job") {
		t.Errorf("response = %v", resp)
	}
}

// ---- set_service_enabled ----

func TestSetServiceEnabledSuccess(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	resp := callTool(t, r, "set_service_enabled", map[string]interface{}{"service": "cifs", "enable": false})
	if got := previewText(t, snapActionParams(t, fake, "service.update")); got != `["cifs",{"enable":false}]` {
		t.Errorf("service.update params = %s", got)
	}
	if resp["changed"] != true || resp["enabled_at_boot"] != false || resp["state"] != "RUNNING" {
		t.Errorf("response = %v", resp)
	}
	if f.get("cifs", "enable") != false {
		t.Error("cifs still enabled")
	}
	if n := len(fake.called("service.control")); n != 0 {
		t.Errorf("set_service_enabled sent service.control %d time(s)", n)
	}

	resp = callTool(t, r, "set_service_enabled", map[string]interface{}{"service": "nfs", "enable": true})
	if resp["changed"] != true || resp["enabled_at_boot"] != true || resp["state"] != "STOPPED" {
		t.Errorf("enable response = %v", resp)
	}
}

func TestSetServiceEnabledDryRun(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	resp := callTool(t, r, "set_service_enabled", map[string]interface{}{"service": "cifs", "enable": false, "dry_run": true})
	if n := len(fake.called("service.update")); n != 0 {
		t.Fatalf("dry run sent service.update %d time(s)", n)
	}
	cur := resp["current_state"].(map[string]interface{})
	if cur["state"] != "RUNNING" || cur["enabled_at_boot"] != true {
		t.Errorf("current_state = %v", cur)
	}
	w := previewText(t, resp["warnings"])
	for _, want := range []string{"stays stopped until it is started: connected SMB clients", "keeps running until it is stopped"} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings missing %q: %s", want, w)
		}
	}
	resp = callTool(t, r, "set_service_enabled", map[string]interface{}{"service": "nfs", "enable": true, "dry_run": true})
	if w := previewText(t, resp["warnings"]); !strings.Contains(w, "is not started now") {
		t.Errorf("enable warnings = %s", w)
	}
}

func TestSetServiceEnabledUnchanged(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	for _, args := range []map[string]interface{}{{"service": "cifs", "enable": true}, {"service": "nfs", "enable": false}, {"service": "ssh", "enable": true}} {
		resp := callTool(t, r, "set_service_enabled", args)
		if resp["changed"] != false || resp["enabled_at_boot"] != args["enable"] {
			t.Errorf("%v: response = %v", args, resp)
		}
	}
	if n := len(fake.called("service.update")); n != 0 {
		t.Errorf("unchanged setting sent service.update %d time(s)", n)
	}
}

func TestSetServiceEnabledRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"unknown", map[string]interface{}{"service": "nosuch", "enable": true}, "unknown service 'nosuch'"},
		{"self dependency", map[string]interface{}{"service": "http", "enable": false, "allow_protected_service": true}, "service 'http' is refused"},
		{"self dependency enable", map[string]interface{}{"service": "http", "enable": true}, "service 'http' is refused"},
		{"ssh disable", map[string]interface{}{"service": "ssh", "enable": false}, "service 'ssh' provides SSH sessions"},
		{"sssd disable", map[string]interface{}{"service": "sssd", "enable": false}, "needs allow_protected_service=true"},
		{"idmap disable", map[string]interface{}{"service": "idmap", "enable": false}, "service 'idmap' provides"},
		{"nscd disable", map[string]interface{}{"service": "nscd", "enable": false}, "service 'nscd' provides"},
		{"keepalived disable", map[string]interface{}{"service": "keepalived", "enable": false}, "service 'keepalived' provides"},
		{"invalid name", map[string]interface{}{"service": "../etc", "enable": false}, "invalid service name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSvcActionFake()
			r, fake := f.registry(t, false)
			for _, dry := range []bool{true, false} {
				_, err := r.CallTool(t.Context(), "set_service_enabled", mergeArgs(tc.args, map[string]interface{}{"dry_run": dry}))
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("dry_run=%v: err = %v, want %q", dry, err, tc.want)
				}
			}
			if n := len(fake.called("service.update")); n != 0 {
				t.Errorf("refused call sent service.update %d time(s)", n)
			}
		})
	}
}

func TestSetServiceEnabledProtectedOverride(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	args := map[string]interface{}{"service": "ssh", "enable": false, "allow_protected_service": true}
	dry := callTool(t, r, "set_service_enabled", mergeArgs(args, map[string]interface{}{"dry_run": true}))
	cur := dry["current_state"].(map[string]interface{})
	if cur["allow_protected_service"] != true || cur["protected"] != true {
		t.Errorf("current_state = %v", cur)
	}
	if w := previewText(t, dry["warnings"]); !strings.Contains(w, "allow_protected_service is set") {
		t.Errorf("warnings = %s", w)
	}
	resp := callTool(t, r, "set_service_enabled", args)
	if resp["changed"] != true || resp["enabled_at_boot"] != false {
		t.Errorf("response = %v", resp)
	}
	if n := len(fake.called("service.update")); n != 1 {
		t.Errorf("service.update calls = %d", n)
	}
}

func TestSetServiceEnabledErrors(t *testing.T) {
	f := newSvcActionFake()
	f.noReadback = true
	r, _ := f.registry(t, false)
	if _, err := r.CallTool(t.Context(), "set_service_enabled", map[string]interface{}{"service": "cifs", "enable": false}); err == nil || !strings.Contains(err.Error(), "reads back as true, not false") {
		t.Errorf("read-back mismatch: err = %v", err)
	}

	f = newSvcActionFake()
	f.fail["service.update"] = map[string]interface{}{"code": -32001, "message": "permission denied"}
	r, _ = f.registry(t, false)
	if _, err := r.CallTool(t.Context(), "set_service_enabled", map[string]interface{}{"service": "cifs", "enable": false}); err == nil || !strings.Contains(err.Error(), "failed to set service cifs enabled_at_boot=false") {
		t.Errorf("update error: err = %v", err)
	}

	f = newSvcActionFake()
	f.fail["service.query"] = map[string]interface{}{"code": -32001, "message": "boom"}
	r, fake := f.registry(t, false)
	if _, err := r.CallTool(t.Context(), "set_service_enabled", map[string]interface{}{"service": "cifs", "enable": false}); err == nil || !strings.Contains(err.Error(), "failed to query services") {
		t.Errorf("query error: err = %v", err)
	}
	if n := len(fake.called("service.update")); n != 0 {
		t.Errorf("service.update sent after a failed lookup")
	}
}

// ---- shared ----

func TestServiceActionToolsSafety(t *testing.T) {
	for name, want := range map[string]Safety{
		"control_service":     {Access: Destructive},
		"set_service_enabled": {Access: Write, Idempotent: true},
	} {
		if got := safetyOf(name); got != want {
			t.Errorf("%s safety = %+v, want %+v", name, got, want)
		}
	}
	r := NewRegistry(nil, nil)
	if !hasParam(r.tools["control_service"], "confirmation_token") {
		t.Error("control_service does not take a confirmation_token")
	}
	if hasParam(r.tools["set_service_enabled"], "confirmation_token") {
		t.Error("set_service_enabled takes a confirmation_token")
	}
	for name := range protectedServices {
		if _, ok := selfDependentServices[name]; ok {
			t.Errorf("%s is both protected and self-dependent", name)
		}
	}
}

func TestServiceActionToolsReadOnly(t *testing.T) {
	f := newSvcActionFake()
	r, fake := f.registry(t, false)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	for name, args := range map[string]map[string]interface{}{
		"control_service":     ctl("nfs", "START"),
		"set_service_enabled": {"service": "cifs", "enable": false},
	} {
		if names[name] {
			t.Errorf("read-only lists %s", name)
		}
		for _, dry := range []bool{true, false} {
			if _, err := r.CallTool(t.Context(), name, mergeArgs(args, map[string]interface{}{"dry_run": dry})); err == nil || !strings.Contains(err.Error(), "read-only mode") {
				t.Errorf("read-only %s dry_run=%v: err = %v", name, dry, err)
			}
		}
	}
	if !names["query_services"] {
		t.Error("read-only hides query_services")
	}
	assertNoWriteCalls(t, fake)
	if n := len(fake.called("service.query")); n != 0 {
		t.Errorf("read-only refusal queried services %d time(s)", n)
	}
}

package tools

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// fakeMiddleware is a minimal TrueNAS JSON-RPC 2.0 server that records the
// methods and params it receives and answers from a canned result table.
type fakeMiddleware struct {
	mu    sync.Mutex
	calls []fakeCall
	// results overrides fakeResults per method; fallback answers methods
	// found in neither (nil means a JSON null result).
	results  map[string]interface{}
	fallback interface{}
	// handle, when set, answers a call first: a non-nil rpcErr is sent as
	// the JSON-RPC error; ok=false falls through to the tables above.
	handle func(method string, params json.RawMessage) (result interface{}, rpcErr map[string]interface{}, ok bool)
}

type fakeCall struct {
	Method string
	Params json.RawMessage
}

var fakeResults = map[string]interface{}{
	"auth.login_ex": map[string]interface{}{"response_type": "SUCCESS"},
	"system.info":   map[string]interface{}{"hostname": "nas", "version": "25.10.0", "uptime_seconds": 100},
}

func (f *fakeMiddleware) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, fakeCall{req.Method, req.Params})
		var result interface{}
		var rpcErr map[string]interface{}
		ok := false
		if f.handle != nil {
			result, rpcErr, ok = f.handle(req.Method, req.Params)
		}
		if !ok {
			result, ok = f.results[req.Method]
		}
		if !ok {
			result, ok = fakeResults[req.Method]
		}
		if !ok {
			result = f.fallback
		}
		f.mu.Unlock()
		resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": result}
		if rpcErr != nil {
			resp = map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "error": rpcErr}
		}
		if err := conn.WriteJSON(resp); err != nil {
			return
		}
	}
}

func (f *fakeMiddleware) called(method string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func newFakeClient(t *testing.T) (*truenas.Client, *fakeMiddleware) {
	return newFakeClientWith(t, nil, nil)
}

func newFakeClientWith(t *testing.T, results map[string]interface{}, fallback interface{}) (*truenas.Client, *fakeMiddleware) {
	t.Helper()
	return newFakeClientFrom(t, &fakeMiddleware{results: results, fallback: fallback})
}

// newFakeClientFrom serves fake (configured before any call) to a client.
func newFakeClientFrom(t *testing.T, fake *fakeMiddleware) (*truenas.Client, *fakeMiddleware) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(fake.serve))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	endpoint := "wss" + strings.TrimPrefix(srv.URL, "https") + "/api/current"
	client, err := truenas.NewClientWithOptions(endpoint, "1-testkey", &tls.Config{RootCAs: roots}, truenas.Options{APIUser: "svc"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client, fake
}

// stubHandlers replaces every handler with one that records its arguments
// and touches no client.
func stubHandlers(r *Registry) map[string][]map[string]interface{} {
	seen := map[string][]map[string]interface{}{}
	for name, tool := range r.tools {
		name := name
		tool.Handler = func(_ *truenas.Client, args map[string]interface{}) (string, error) {
			seen[name] = append(seen[name], args)
			return `{"ok":true}`, nil
		}
		r.tools[name] = tool
	}
	return seen
}

func listed(r *Registry) map[string]bool {
	names := map[string]bool{}
	for _, tool := range r.ListTools() {
		names[tool.Name] = true
	}
	return names
}

func TestEveryToolIsClassified(t *testing.T) {
	r := NewRegistry(nil, nil)
	for name := range r.tools {
		if s, ok := toolSafety[name]; !ok || s.Access == Unclassified {
			t.Errorf("tool %s has no entry in toolSafety (tools/safety.go): classify it as Read, Write or Destructive", name)
		}
	}
	for name := range toolSafety {
		if _, ok := r.tools[name]; !ok {
			t.Errorf("toolSafety lists %s, which is not registered", name)
		}
	}
}

func TestWriteToolsMatchExpectedList(t *testing.T) {
	want := map[string]Access{
		"system_reboot": Destructive, "apply_update": Destructive, "delete_boot_environment": Destructive,
		"delete_app": Destructive, "leave_directory_service": Destructive, "delete_scrub_schedule": Destructive,
		"stop_app": Destructive, "upgrade_app": Destructive, "configure_directory_service": Destructive,
		"download_update": Write, "install_app": Write, "update_app": Write,
		"start_app": Write, "refresh_directory_cache": Write,
		"create_dataset": Write, "create_smb_share": Write, "create_nfs_share": Write,
		"create_scrub_schedule": Write, "run_scrub": Write, "dismiss_alert": Write, "restore_alert": Write,
	}
	for name, s := range toolSafety {
		if got, exp := s.Access, want[name]; exp != 0 && got != exp || exp == 0 && got != Read {
			t.Errorf("%s classified %v", name, got)
		}
	}
	destructive := 0
	for _, s := range toolSafety {
		if s.Access == Destructive {
			destructive++
		}
	}
	if destructive != 9 {
		t.Errorf("%d destructive tools, want 9; update the README and docs/full-features.md too", destructive)
	}
	for name, s := range toolSafety {
		if s.Overwrites != (name == "update_app") {
			t.Errorf("%s Overwrites = %v", name, s.Overwrites)
		}
	}
}

func TestReadOnlyHidesAndRefusesWrites(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	for name := range r.tools {
		readOnly := safetyOf(name).Access == Read
		if names[name] != readOnly {
			t.Errorf("read-only: %s listed=%v, want %v", name, names[name], readOnly)
		}
		if readOnly {
			continue
		}
		for _, args := range []map[string]interface{}{{}, {"confirm": true}, {"dry_run": true}} {
			_, err := r.CallTool(name, args)
			if err == nil || !strings.Contains(err.Error(), "read-only mode") {
				t.Errorf("read-only: CallTool(%s, %v) err = %v, want read-only refusal", name, args, err)
			}
		}
		if len(seen[name]) > 0 {
			t.Errorf("read-only: handler for %s ran", name)
		}
	}
	if _, err := r.CallTool("query_pools", nil); err != nil {
		t.Errorf("read-only: read tool refused: %v", err)
	}
}

func TestAllowAndDenyLists(t *testing.T) {
	r := NewRegistry(nil, nil)
	stubHandlers(r)
	if err := r.SetPolicy(Policy{Allow: []string{"query_pools", "create_dataset"}}); err != nil {
		t.Fatal(err)
	}
	if names := listed(r); len(names) != 2 || !names["query_pools"] || !names["create_dataset"] {
		t.Errorf("allowlist exposed %v", names)
	}
	if _, err := r.CallTool("create_dataset", map[string]interface{}{}); err != nil {
		t.Errorf("allowed tool refused: %v", err)
	}
	if _, err := r.CallTool("query_apps", nil); err == nil || !strings.Contains(err.Error(), "--allow-tools") {
		t.Errorf("unlisted tool: err = %v", err)
	}

	if err := r.SetPolicy(Policy{Deny: []string{"system_reboot"}}); err != nil {
		t.Fatal(err)
	}
	if listed(r)["system_reboot"] || !listed(r)["query_pools"] {
		t.Error("deny list not applied to tools/list")
	}
	if _, err := r.CallTool("system_reboot", map[string]interface{}{"confirm": true}); err == nil {
		t.Error("denied tool was callable")
	}

	for _, p := range []Policy{
		{Allow: []string{"query_pools", "no_such_tool"}},
		{Deny: []string{"bogus"}},
		{ReadOnly: true, Allow: []string{"delete_app"}},
	} {
		if err := r.SetPolicy(p); err == nil {
			t.Errorf("SetPolicy(%+v) accepted", p)
		}
	}
}

func TestDestructiveToolsRequireConfirm(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	for name := range r.tools {
		if safetyOf(name).Access != Destructive {
			continue
		}
		args := map[string]interface{}{}
		if hasParam(r.tools[name], "app_name") {
			args["app_name"] = "plex"
		}
		out, err := r.CallTool(name, args)
		if err != nil {
			t.Fatalf("%s without confirm: %v", name, err)
		}
		var reply map[string]interface{}
		if err := json.Unmarshal([]byte(out), &reply); err != nil || reply["status"] != "confirmation_required" || reply["preview"] == nil {
			t.Errorf("%s without confirm returned %s", name, out)
		}
		if calls := seen[name]; len(calls) != 1 || calls[0]["dry_run"] != true {
			t.Errorf("%s without confirm: handler calls %v, want one dry run", name, calls)
		}
		if _, ok := args["dry_run"]; ok {
			t.Errorf("%s: preview mutated the caller's arguments", name)
		}

		if _, err := r.CallTool(name, map[string]interface{}{"dry_run": true}); err != nil {
			t.Errorf("%s dry run: %v", name, err)
		}
		if _, err := r.CallTool(name, map[string]interface{}{"confirm": true}); err != nil {
			t.Errorf("%s with confirm: %v", name, err)
		}
		calls := seen[name]
		if len(calls) != 3 || calls[1]["dry_run"] != true || calls[2]["confirm"] != true || calls[2]["dry_run"] != nil {
			t.Errorf("%s: handler calls %v", name, calls)
		}
	}
	// Write tools that are not destructive run without confirm.
	if _, err := r.CallTool("create_dataset", map[string]interface{}{}); err != nil || len(seen["create_dataset"]) != 1 {
		t.Errorf("create_dataset gated: %v", err)
	}
}

func TestSystemRebootAgainstFakeTrueNAS(t *testing.T) {
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)

	out, err := r.CallTool("system_reboot", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "confirmation_required") || !strings.Contains(out, `"hostname": "nas"`) {
		t.Errorf("unconfirmed reboot reply: %s", out)
	}
	if _, err := r.CallTool("system_reboot", map[string]interface{}{"dry_run": true}); err != nil {
		t.Fatal(err)
	}
	if calls := fake.called("system.reboot"); len(calls) != 0 {
		t.Fatalf("system.reboot sent without confirm: %v", calls)
	}

	if _, err := r.CallTool("system_reboot", map[string]interface{}{"confirm": true, "delay": float64(30)}); err != nil {
		t.Fatal(err)
	}
	calls := fake.called("system.reboot")
	if len(calls) != 1 {
		t.Fatalf("system.reboot calls = %d, want 1", len(calls))
	}
	var params []interface{}
	if err := json.Unmarshal(calls[0].Params, &params); err != nil || len(params) != 2 {
		t.Fatalf("system.reboot params = %s", calls[0].Params)
	}
	if opts, _ := params[1].(map[string]interface{}); opts["delay"] != float64(30) {
		t.Errorf("system.reboot options = %v, want delay 30", params[1])
	}

	for _, d := range []interface{}{-1.0, 86401.0, 1e12, 1.5, "60", true} {
		if _, err := r.CallTool("system_reboot", map[string]interface{}{"confirm": true, "delay": d}); err == nil {
			t.Errorf("delay %v accepted", d)
		}
	}
	if n := len(fake.called("system.reboot")); n != 1 {
		t.Errorf("system.reboot calls = %d after invalid delays, want 1", n)
	}
	if props := r.tools["system_reboot"].Definition.InputSchema["properties"].(map[string]interface{}); props["delay"].(map[string]interface{})["maximum"] != maxRebootDelay {
		t.Error("delay schema has no maximum")
	}
}

func TestAnnotationsPresent(t *testing.T) {
	r := NewRegistry(nil, nil)
	for _, tool := range r.ListTools() {
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s has no annotations", tool.Name)
			continue
		}
		s := safetyOf(tool.Name)
		if a.ReadOnlyHint != (s.Access == Read) || a.DestructiveHint != (s.Access == Destructive || s.Overwrites) || a.OpenWorldHint {
			t.Errorf("%s annotations %+v do not match %v", tool.Name, *a, s.Access)
		}
		props := tool.InputSchema["properties"].(map[string]interface{})
		if _, ok := props["confirm"]; ok != (s.Access == Destructive) {
			t.Errorf("%s: confirm parameter present=%v", tool.Name, ok)
		}
		if s.Access == Destructive {
			if _, ok := props["dry_run"]; !ok {
				t.Errorf("destructive tool %s has no dry_run", tool.Name)
			}
		}
	}

	// update_app replaces config in place: destructiveHint without confirm.
	if a := r.tools["update_app"].Definition.Annotations; !a.DestructiveHint {
		t.Error("update_app: destructiveHint false")
	}
	if _, err := r.CallTool("update_app", map[string]interface{}{}); err != nil && strings.Contains(err.Error(), "confirm") {
		t.Errorf("update_app gated: %v", err)
	}

	// destructiveHint defaults to true in the spec, so false must be sent.
	data, _ := json.Marshal(r.tools["create_dataset"].Definition)
	if !strings.Contains(string(data), `"annotations":{"readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":false}`) {
		t.Errorf("create_dataset JSON: %s", data)
	}
}

func TestConfirmAndDryRunMustBeBooleans(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	for _, v := range []interface{}{"true", 1.0, nil, []interface{}{true}, map[string]interface{}{"x": true}} {
		for _, key := range []string{"confirm", "dry_run"} {
			out, err := r.CallTool("delete_app", map[string]interface{}{"app_name": "plex", key: v})
			if err != nil || !strings.Contains(out, "confirmation_required") {
				t.Errorf("%s=%#v: out=%s err=%v, want preview", key, v, out, err)
			}
		}
	}
	for _, args := range seen["delete_app"] {
		if args["dry_run"] != true {
			t.Errorf("delete_app handler ran without dry_run: %v", args)
		}
	}
}

func TestDryRunOnlyCountsWhereDeclared(t *testing.T) {
	r := NewRegistry(nil, nil)
	ran := 0
	// Unclassified, so treated as destructive, and without a dry_run param.
	// The schema is permissive so that argument validation lets dry_run
	// through and the confirmation gate itself is what is tested.
	r.tools["test_unclassified"] = Tool{
		Definition: mcp.Tool{Name: "test_unclassified", InputSchema: map[string]interface{}{"type": "object", "additionalProperties": true}},
		Handler: func(*truenas.Client, map[string]interface{}) (string, error) {
			ran++
			return "done", nil
		},
	}
	out, err := r.CallTool("test_unclassified", map[string]interface{}{"dry_run": true})
	if err != nil || !strings.Contains(out, "confirmation_required") || ran != 0 {
		t.Errorf("dry_run bypassed the gate: out=%s err=%v ran=%d", out, err, ran)
	}
	if _, err := r.CallTool("test_unclassified", map[string]interface{}{"confirm": true}); err != nil || ran != 1 {
		t.Errorf("confirm: err=%v ran=%d", err, ran)
	}
}

func TestPreviewFailureDoesNotInviteConfirm(t *testing.T) {
	r := NewRegistry(nil, nil)
	tool := r.tools["delete_app"]
	tool.Handler = func(*truenas.Client, map[string]interface{}) (string, error) {
		return "", fmt.Errorf("app not found: plex")
	}
	r.tools["delete_app"] = tool
	_, err := r.CallTool("delete_app", map[string]interface{}{"app_name": "plex"})
	if err == nil || !strings.Contains(err.Error(), "Preview failed: app not found: plex. Resolve this before confirming") ||
		!strings.Contains(err.Error(), "NOT executed") {
		t.Errorf("preview failure error = %v", err)
	}
}

func TestCallToolRecoversFromPanics(t *testing.T) {
	r := NewRegistry(nil, nil)
	tool := r.tools["query_pools"]
	tool.Handler = func(*truenas.Client, map[string]interface{}) (string, error) {
		var m map[string]interface{}
		return m["x"].(string), nil
	}
	r.tools["query_pools"] = tool
	_, err := r.CallTool("query_pools", nil)
	if err == nil || !strings.Contains(err.Error(), "internal error in tool query_pools") {
		t.Errorf("err = %v", err)
	}
}

// TestDestructiveDryRunsAgainstFakeTrueNAS runs every destructive tool's real
// preview (no stubs) with missing, malformed and plausible arguments against
// a fake middleware that returns well-formed, null and malformed results.
// Nothing may panic, and only query methods may reach the middleware.
func TestDestructiveDryRunsAgainstFakeTrueNAS(t *testing.T) {
	valid := map[string]map[string]interface{}{
		"system_reboot":           {"delay": 60.0},
		"apply_update":            {"reboot": true},
		"delete_boot_environment": {"id": "25.04.1"},
		"delete_app":              {"app_name": "plex", "remove_images": true},
		"stop_app":                {"app_name": "plex"},
		"upgrade_app":             {"app_name": "plex", "version": "2.0.0", "snapshot_hostpaths": false},
		"leave_directory_service": {
			"credential": map[string]interface{}{"credential_type": "KERBEROS_USER", "username": "admin", "password": "pw"},
		},
		"delete_scrub_schedule": {"id": 1.0},
		"configure_directory_service": {
			"service_type": "ACTIVEDIRECTORY", "enable": true,
			"credential":    map[string]interface{}{"credential_type": "KERBEROS_USER", "username": "admin", "password": "pw"},
			"configuration": map[string]interface{}{"domain": "example.com", "hostname": "nas"},
		},
	}
	modes := []struct {
		name     string
		results  map[string]interface{}
		fallback interface{}
	}{
		{"null results", nil, nil},
		// An enabled directory service, so leave_directory_service and
		// configure_directory_service previews run past their pre-checks.
		{"directory service enabled", map[string]interface{}{
			"directoryservices.status": map[string]interface{}{"type": "ACTIVEDIRECTORY", "status": "HEALTHY", "status_msg": nil},
		}, nil},
		{"null version", map[string]interface{}{"system.info": map[string]interface{}{"hostname": "nas", "version": nil}}, nil},
		{"missing version", map[string]interface{}{"system.info": map[string]interface{}{}}, nil},
		{"numeric version", map[string]interface{}{"system.info": map[string]interface{}{"version": 42}}, nil},
		{"object version", map[string]interface{}{"system.info": map[string]interface{}{"version": map[string]interface{}{"a": 1}}}, nil},
		{"string system.info", map[string]interface{}{"system.info": "garbage"}, "garbage"},
		{"list of numbers", nil, []interface{}{42, nil}},
		{"list of empty objects", nil, []interface{}{map[string]interface{}{}}},
		{"empty list", nil, []interface{}{}},
		{"empty object", nil, map[string]interface{}{}},
		// A well-formed boot environment list, so delete_boot_environment's
		// preview gets past its lookup instead of stopping at "not found".
		// If its handler stopped honouring dry_run, boot.environment.destroy
		// would reach the fake and the query-only check below would fail.
		{"plausible boot environments", map[string]interface{}{
			"boot.environment.query": []interface{}{map[string]interface{}{"id": "25.04.1", "active": false, "activated": false, "keep": false}},
		}, nil},
		{"object of wrong types", nil, map[string]interface{}{
			"config": 1, "schedule": "x", "state": 3, "name": 4, "version": nil, "enable": "yes",
			"service_type": 5, "status": []interface{}{1}, "active": "no", "activated": 1,
		}},
	}
	allowed := func(method string) bool {
		for _, suffix := range []string{".query", ".info", ".status", ".config", ".list", ".login_ex", ".get_instance", "upgrade_summary", ".pool_choices"} {
			if strings.HasSuffix(method, suffix) {
				return true
			}
		}
		return false
	}
	for _, mode := range modes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			client, fake := newFakeClientWith(t, mode.results, mode.fallback)
			r := NewRegistry(client, nil)
			for name, tool := range r.tools {
				if safetyOf(name).Access != Destructive {
					continue
				}
				argSets := []map[string]interface{}{nil, {}, valid[name]}
				if name == "configure_directory_service" {
					// Schema-conformant LDAP, and arguments in the old (handler-only) names.
					argSets = append(argSets,
						map[string]interface{}{
							"service_type": "LDAP", "enable": false,
							"credential":    map[string]interface{}{"credential_type": "LDAP_PLAIN", "binddn": "cn=a,dc=x", "bindpw": "pw"},
							"configuration": map[string]interface{}{"server_urls": []interface{}{"ldaps://ldap"}, "basedn": "dc=x"},
						},
						map[string]interface{}{"type": "activedirectory", "domain": "example.com", "enable": false})
				}
				props, _ := tool.Definition.InputSchema["properties"].(map[string]interface{})
				for _, bad := range []interface{}{nil, "x", -1.5, true, []interface{}{1}, map[string]interface{}{"a": 1}} {
					args := map[string]interface{}{}
					for p := range props {
						if p != "confirm" && p != "dry_run" {
							args[p] = bad
						}
					}
					argSets = append(argSets, args)
				}
				for _, args := range argSets {
					for _, extra := range []map[string]interface{}{{}, {"dry_run": true}} {
						call := map[string]interface{}{}
						for k, v := range args {
							call[k] = v
						}
						for k, v := range extra {
							call[k] = v
						}
						out, err := r.CallTool(name, call)
						if err != nil && strings.Contains(err.Error(), "internal error") {
							t.Errorf("%s(%v) panicked", name, call)
						}
						if err == nil && len(extra) == 0 && !strings.Contains(out, "confirmation_required") {
							t.Errorf("%s(%v) without confirm returned %s", name, call, out)
						}
					}
				}
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			for _, c := range fake.calls {
				if !allowed(c.Method) {
					t.Errorf("preview sent non-query method %s", c.Method)
				}
			}
		})
	}
}

// TestDeleteBootEnvironmentDryRunAgainstFakeTrueNAS drives delete_boot_environment
// past its lookup, so the dry-run path itself is exercised: a preview (explicit
// dry_run or no confirm) never sends boot.environment.destroy, while a
// confirmed call does.
func TestDeleteBootEnvironmentDryRunAgainstFakeTrueNAS(t *testing.T) {
	bootEnvs := []interface{}{
		map[string]interface{}{"id": "25.04.1", "active": false, "activated": false, "keep": false, "used_bytes": 1048576.0},
		map[string]interface{}{"id": "running", "active": true, "activated": false, "keep": false},
		map[string]interface{}{"id": "protected", "active": false, "activated": false, "keep": true},
	}
	client, fake := newFakeClientWith(t, map[string]interface{}{"boot.environment.query": bootEnvs}, nil)
	r := NewRegistry(client, nil)

	// Previews: explicit dry_run and the default unconfirmed call.
	for _, args := range []map[string]interface{}{
		{"id": "25.04.1", "dry_run": true},
		{"id": "25.04.1"},
	} {
		out, err := r.CallTool("delete_boot_environment", args)
		if err != nil {
			t.Fatalf("delete_boot_environment(%v): %v", args, err)
		}
		for _, want := range []string{`"tool": "delete_boot_environment"`, "Delete boot environment '25.04.1'", "PERMANENT", `"deletion_allowed": true`} {
			if !strings.Contains(out, want) {
				t.Errorf("delete_boot_environment(%v) preview lacks %q:\n%s", args, want, out)
			}
		}
	}
	if calls := fake.called("boot.environment.destroy"); len(calls) != 0 {
		t.Fatalf("a preview sent boot.environment.destroy: %v", calls)
	}

	// Boot environments that cannot be deleted are reported as blocked in
	// the preview, with no planned action.
	for id, want := range map[string]string{"running": "currently running", "protected": "keep flag is set"} {
		out, err := r.CallTool("delete_boot_environment", map[string]interface{}{"id": id, "dry_run": true})
		if err != nil {
			t.Fatalf("delete_boot_environment(%s) dry run: %v", id, err)
		}
		if !strings.Contains(out, "BLOCKED") || !strings.Contains(out, want) || !strings.Contains(out, `"deletion_allowed": false`) || strings.Contains(out, "Delete boot environment") {
			t.Errorf("delete_boot_environment(%s) preview:\n%s", id, out)
		}
	}
	if calls := fake.called("boot.environment.destroy"); len(calls) != 0 {
		t.Fatalf("a preview sent boot.environment.destroy: %v", calls)
	}

	// Confirmed: the real handler runs and destroys exactly that environment.
	if _, err := r.CallTool("delete_boot_environment", map[string]interface{}{"id": "25.04.1", "confirm": true}); err != nil {
		t.Fatal(err)
	}
	calls := fake.called("boot.environment.destroy")
	if len(calls) != 1 || !strings.Contains(string(calls[0].Params), `"25.04.1"`) {
		t.Fatalf("boot.environment.destroy calls = %v, want one for 25.04.1", calls)
	}
}

// TestDeleteBootEnvironmentConfirmedRefusesProtected checks that a confirmed
// delete_boot_environment still refuses the running, the next-boot and the
// protected boot environment, and never sends boot.environment.destroy.
func TestDeleteBootEnvironmentConfirmedRefusesProtected(t *testing.T) {
	bootEnvs := []interface{}{
		map[string]interface{}{"id": "running", "active": true, "activated": false, "keep": false},
		map[string]interface{}{"id": "next-boot", "active": false, "activated": true, "keep": false},
		map[string]interface{}{"id": "protected", "active": false, "activated": false, "keep": true},
		map[string]interface{}{"id": "running-minimal", "active": true},
	}
	client, fake := newFakeClientWith(t, map[string]interface{}{"boot.environment.query": bootEnvs}, nil)
	r := NewRegistry(client, nil)

	for id, want := range map[string]string{
		"running":         "currently running",
		"next-boot":       "will boot on next restart",
		"protected":       "keep flag is set",
		"running-minimal": "currently running",
	} {
		_, err := r.CallTool("delete_boot_environment", map[string]interface{}{"id": id, "confirm": true})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("confirmed delete of %q: err = %v, want an error containing %q", id, err, want)
		}
	}
	if calls := fake.called("boot.environment.destroy"); len(calls) != 0 {
		t.Fatalf("a refused delete sent boot.environment.destroy: %v", calls)
	}
}

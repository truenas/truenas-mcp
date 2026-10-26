package tools

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/truenas/truenas-mcp/truenas"
)

// fakeMiddleware is a minimal TrueNAS JSON-RPC 2.0 server that records the
// methods and params it receives and answers from a canned result table.
type fakeMiddleware struct {
	mu    sync.Mutex
	calls []fakeCall
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
		f.mu.Unlock()
		if err := conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": fakeResults[req.Method]}); err != nil {
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
	t.Helper()
	fake := &fakeMiddleware{}
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
		"stop_app":        Destructive,
		"download_update": Write, "install_app": Write, "update_app": Write, "upgrade_app": Write,
		"start_app": Write, "configure_directory_service": Write, "refresh_directory_cache": Write,
		"create_dataset": Write, "create_smb_share": Write, "create_nfs_share": Write,
		"create_scrub_schedule": Write, "run_scrub": Write, "dismiss_alert": Write, "restore_alert": Write,
	}
	for name, s := range toolSafety {
		if got, exp := s.Access, want[name]; exp != 0 && got != exp || exp == 0 && got != Read {
			t.Errorf("%s classified %v", name, got)
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
		args := map[string]interface{}{"app_name": "plex"}
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

	if _, err := r.CallTool("system_reboot", map[string]interface{}{"confirm": true, "delay": -1.0}); err == nil {
		t.Error("negative delay accepted")
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
		if a.ReadOnlyHint != (s.Access == Read) || a.DestructiveHint != (s.Access == Destructive) || a.OpenWorldHint {
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

	// destructiveHint defaults to true in the spec, so false must be sent.
	data, _ := json.Marshal(r.tools["create_dataset"].Definition)
	if !strings.Contains(string(data), `"annotations":{"readOnlyHint":false,"destructiveHint":false,"idempotentHint":false,"openWorldHint":false}`) {
		t.Errorf("create_dataset JSON: %s", data)
	}
}

package tools

import (
	"strings"
	"testing"
)

// readOnlyUpgradeRegistry is an upgrade_app registry in read-only mode.
func readOnlyUpgradeRegistry(t *testing.T) (*Registry, *upgradeFake) {
	t.Helper()
	r, _, f := newUpgradeRegistry(t, upgradeSummaryFixture("Fixes"))
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	return r, f
}

// readOnlyMethods are the only middleware methods a read-only dry run may
// call: login and queries.
var readOnlyMethods = map[string]bool{
	"auth.login_ex":       true,
	"app.query":           true,
	"app.upgrade_summary": true,
}

func assertOnlyReadOnlyCalls(t *testing.T, f *upgradeFake) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if !readOnlyMethods[c.Method] {
			t.Errorf("read-only mode called middleware method %s", c.Method)
		}
	}
}

func TestReadOnlyListsUpgradeAppForDryRunsOnly(t *testing.T) {
	r, _ := readOnlyUpgradeRegistry(t)
	var found bool
	for _, tool := range r.ListTools() {
		if tool.Name != "upgrade_app" {
			continue
		}
		found = true
		if !strings.HasPrefix(tool.Description, "**READ-ONLY MODE: only dry runs are allowed.**") {
			t.Errorf("description = %q", tool.Description)
		}
		if strings.Contains(tool.Description, "requires confirm=true") {
			t.Errorf("read-only description still asks for confirm: %q", tool.Description)
		}
		props := tool.InputSchema["properties"].(map[string]interface{})
		if _, ok := props["confirm"]; ok {
			t.Error("read-only schema still declares confirm")
		}
		dryRun := props["dry_run"].(map[string]interface{})
		if dryRun["const"] != true || !strings.Contains(dryRun["description"].(string), "read-only mode") {
			t.Errorf("dry_run schema = %v", dryRun)
		}
		if req := strings.Join(tool.InputSchema["required"].([]string), ","); req != "app_name,dry_run" {
			t.Errorf("required = %s", req)
		}
		if a := tool.Annotations; a == nil || !a.ReadOnlyHint || a.DestructiveHint {
			t.Errorf("annotations = %+v", a)
		}
	}
	if !found {
		t.Fatal("upgrade_app not listed in read-only mode")
	}

	// The registered tool is unchanged, and so is the listing outside
	// read-only mode.
	if err := r.SetPolicy(Policy{}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range r.ListTools() {
		if tool.Name == "upgrade_app" {
			props := tool.InputSchema["properties"].(map[string]interface{})
			if _, ok := props["confirm"]; !ok || strings.Contains(tool.Description, "READ-ONLY MODE") {
				t.Errorf("normal listing changed: %q", tool.Description)
			}
			if req := strings.Join(tool.InputSchema["required"].([]string), ","); req != "app_name" {
				t.Errorf("normal required = %s", req)
			}
		}
	}
}

func TestReadOnlyDryRunMakesNoWrites(t *testing.T) {
	for name, extra := range map[string]map[string]interface{}{
		"dry_run":              {"dry_run": true},
		"dry_run with confirm": {"dry_run": true, "confirm": true},
	} {
		t.Run(name, func(t *testing.T) {
			r, f := readOnlyUpgradeRegistry(t)
			args := map[string]interface{}{"app_name": "plex"}
			for k, v := range extra {
				args[k] = v
			}
			out, err := r.CallTool("upgrade_app", args)
			if err != nil {
				t.Fatal(err)
			}
			reply := decode(t, out)
			if reply["dry_run"] != true || reply["executed"] != false || reply["planned_actions"] == nil {
				t.Errorf("reply = %v", reply)
			}
			if len(f.called("app.upgrade_summary")) != 1 {
				t.Error("dry run did not read app.upgrade_summary")
			}
			assertOnlyReadOnlyCalls(t, f)
		})
	}
}

func TestReadOnlyRefusesRealUpgrade(t *testing.T) {
	for name, extra := range map[string]map[string]interface{}{
		"no dry_run":             {},
		"no dry_run, confirm":    {"confirm": true},
		"dry_run false":          {"dry_run": false},
		"dry_run false, confirm": {"dry_run": false, "confirm": true},
		"dry_run string":         {"dry_run": "true", "confirm": true},
	} {
		t.Run(name, func(t *testing.T) {
			r, f := readOnlyUpgradeRegistry(t)
			args := map[string]interface{}{"app_name": "plex"}
			for k, v := range extra {
				args[k] = v
			}
			out, err := r.CallTool("upgrade_app", args)
			if err == nil || !strings.Contains(err.Error(), "read-only mode") || !strings.Contains(err.Error(), "NOT executed") {
				t.Fatalf("out=%q err=%v, want the read-only refusal", out, err)
			}
			// Refused before anything runs: not even the preview.
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, c := range f.calls {
				if c.Method != "auth.login_ex" {
					t.Errorf("refused call reached middleware: %s", c.Method)
				}
			}
		})
	}
}

// The exception is limited to readOnlyDryRunTools: other destructive tools
// with a dry_run parameter stay hidden and refused.
func TestReadOnlyDryRunExceptionIsExplicit(t *testing.T) {
	if len(readOnlyDryRunTools) != 1 || !readOnlyDryRunTools["upgrade_app"] {
		t.Fatalf("readOnlyDryRunTools = %v; every entry needs a no-writes dry-run test", readOnlyDryRunTools)
	}
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	for name, tool := range r.tools {
		if safetyOf(name).Access == Read || readOnlyDryRunTools[name] || !hasParam(tool, "dry_run") {
			continue
		}
		if _, err := r.CallTool(name, map[string]interface{}{"dry_run": true}); err == nil || !strings.Contains(err.Error(), "read-only mode") {
			t.Errorf("%s dry run in read-only mode: err = %v", name, err)
		}
		if len(seen[name]) > 0 {
			t.Errorf("handler for %s ran", name)
		}
	}

	// --allow-tools may name upgrade_app with --read-only; it is still
	// limited to dry runs. --deny-tools still hides it.
	if err := r.SetPolicy(Policy{ReadOnly: true, Allow: []string{"query_apps", "upgrade_app"}}); err != nil {
		t.Fatalf("allow upgrade_app in read-only mode: %v", err)
	}
	if names := listed(r); len(names) != 2 || !names["upgrade_app"] {
		t.Errorf("listed %v", names)
	}
	if _, err := r.CallTool("upgrade_app", map[string]interface{}{"app_name": "plex", "confirm": true}); err == nil || !strings.Contains(err.Error(), "read-only mode") {
		t.Errorf("allowlisted real run: err = %v", err)
	}
	if err := r.SetPolicy(Policy{ReadOnly: true, Deny: []string{"upgrade_app"}}); err != nil {
		t.Fatal(err)
	}
	if listed(r)["upgrade_app"] {
		t.Error("denied upgrade_app listed")
	}
	if _, err := r.CallTool("upgrade_app", map[string]interface{}{"app_name": "plex", "dry_run": true}); err == nil || !strings.Contains(err.Error(), "--deny-tools") {
		t.Errorf("denied dry run: err = %v", err)
	}
	if len(seen["upgrade_app"]) > 0 {
		t.Error("upgrade_app handler ran")
	}
}

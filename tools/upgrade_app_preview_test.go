package tools

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// upgradePreview returns the dry-run result of upgrade_app, through the
// explicit dry_run=true path or the confirmation_required path (no confirm).
// The second return value is the confirmation_required envelope, if any.
func upgradePreview(t *testing.T, r *Registry, explicit bool, extra map[string]interface{}) (map[string]interface{}, map[string]interface{}) {
	t.Helper()
	args := map[string]interface{}{"app_name": "plex"}
	if explicit {
		args["dry_run"] = true
	}
	for k, v := range extra {
		args[k] = v
	}
	out, err := r.CallTool("upgrade_app", args)
	if err != nil {
		t.Fatal(err)
	}
	reply := decode(t, out)
	if explicit {
		return reply, nil
	}
	if reply["status"] != "confirmation_required" {
		t.Fatalf("no confirm: want confirmation_required, got %v", reply)
	}
	return reply["preview"].(map[string]interface{}), reply
}

func plannedStep(t *testing.T, preview map[string]interface{}, operation string) map[string]interface{} {
	t.Helper()
	for _, a := range preview["planned_actions"].([]interface{}) {
		if step := a.(map[string]interface{}); step["operation"] == operation {
			return step
		}
	}
	t.Fatalf("no %q step in %v", operation, preview["planned_actions"])
	return nil
}

func previewPaths(t *testing.T, fn func(t *testing.T, explicit bool)) {
	for name, explicit := range map[string]bool{"dry_run": true, "no confirm": false} {
		t.Run(name, func(t *testing.T) { fn(t, explicit) })
	}
}

// Without an explicit version the plan names the version upgrade_summary
// resolved "latest" to, not the word "latest" alone.
func TestUpgradeAppPreviewNamesResolvedTarget(t *testing.T) {
	previewPaths(t, func(t *testing.T, explicit bool) {
		s := upgradeSummaryFixture("x")
		s["upgrade_human_version"], s["upgrade_version"] = "2.8.0_1.3.30", "1.3.30"
		r, _, f := newUpgradeRegistry(t, s)
		preview, _ := upgradePreview(t, r, explicit, nil)
		desc := plannedStep(t, preview, "upgrade")["description"].(string)
		if !strings.Contains(desc, "to 2.8.0_1.3.30 (catalog version 1.3.30, latest)") {
			t.Errorf("upgrade step = %q", desc)
		}
		if strings.HasSuffix(desc, "to latest") {
			t.Errorf("target not resolved: %q", desc)
		}
		if n := len(f.called("app.upgrade")); n != 0 {
			t.Fatalf("preview called app.upgrade %d times", n)
		}
	})
}

func TestUpgradeTargetDescription(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		summary interface{}
		want    string
	}{
		{"explicit version", "1.1.0", upgradeSummaryFixture("x"), "1.1.0"},
		{"upgrade fields", "latest", map[string]interface{}{"upgrade_human_version": "2.0.0_1.2.0", "upgrade_version": "1.2.0", "latest_version": "9"}, "2.0.0_1.2.0 (catalog version 1.2.0, latest)"},
		{"only catalog", "latest", map[string]interface{}{"upgrade_version": "1.2.0"}, "1.2.0 (latest)"},
		{"latest fallback", "latest", map[string]interface{}{"latest_human_version": "2.0.0_1.2.0", "latest_version": "1.2.0"}, "2.0.0_1.2.0 (catalog version 1.2.0, latest)"},
		{"list shape", "latest", []interface{}{map[string]interface{}{"upgrade_version": "1.2.0"}}, "1.2.0 (latest)"},
		{"nothing named", "latest", map[string]interface{}{"upgrade_version": nil, "latest_version": " "}, "the latest available version (app.upgrade_summary did not name it)"},
		{"not an object", "latest", "odd", "the latest available version (app.upgrade_summary did not name it)"},
	} {
		if got := upgradeTargetDescription(tc.version, tc.summary); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The plan states what is snapshotted before the upgrade, warns when host
// paths are excluded, and never names a snapshot.
func TestUpgradeAppPreviewStatesSnapshotBehaviour(t *testing.T) {
	previewPaths(t, func(t *testing.T, explicit bool) {
		for _, hostpaths := range []bool{true, false} {
			r, _, _ := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
			preview, _ := upgradePreview(t, r, explicit, map[string]interface{}{"snapshot_hostpaths": hostpaths})
			step := plannedStep(t, preview, "snapshot")
			desc := step["description"].(string)
			details := step["details"].(map[string]interface{})
			if !strings.Contains(desc, "ix-volumes") || !strings.Contains(desc, "before the upgrade") || details["snapshot_hostpaths"] != hostpaths {
				t.Errorf("hostpaths=%v: snapshot step = %v", hostpaths, step)
			}
			warnings, _ := preview["warnings"].([]interface{})
			if hostpaths {
				if !strings.Contains(desc, "host paths") || strings.Contains(desc, "not snapshotted") || len(warnings) != 0 {
					t.Errorf("hostpaths=true: %q, warnings %v", desc, warnings)
				}
			} else if !strings.Contains(desc, "not snapshotted") || len(warnings) != 1 || !strings.Contains(warnings[0].(string), "snapshot_hostpaths is disabled") {
				t.Errorf("hostpaths=false: %q, warnings %v", desc, warnings)
			}
			b, _ := json.Marshal(preview)
			if strings.Contains(string(b), "@") {
				t.Errorf("preview names a snapshot: %s", b)
			}
			// The snapshot step comes after stop and before upgrade.
			var ops []string
			for _, a := range preview["planned_actions"].([]interface{}) {
				ops = append(ops, a.(map[string]interface{})["operation"].(string))
			}
			if strings.Join(ops, ",") != "stop,snapshot,upgrade,start,verify" {
				t.Errorf("step order = %v", ops)
			}
		}
	})
}

// The verify step reports the timeout the real run will use, on both preview
// paths, and matches the started upgrade.
func TestUpgradeAppPreviewHealthTimeout(t *testing.T) {
	previewPaths(t, func(t *testing.T, explicit bool) {
		for _, tc := range []struct {
			extra map[string]interface{}
			want  int
		}{
			{nil, 60},
			{map[string]interface{}{"health_timeout_seconds": 300.0}, 300},
			{map[string]interface{}{"health_timeout_seconds": 0.0}, 0},
		} {
			r, _, _ := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
			preview, _ := upgradePreview(t, r, explicit, tc.extra)
			step := plannedStep(t, preview, "verify")
			if got := step["details"].(map[string]interface{})["health_timeout_seconds"]; got != float64(tc.want) {
				t.Errorf("verify details timeout = %v, want %d", got, tc.want)
			}
			desc := step["description"].(string)
			if !strings.Contains(desc, "up to "+strconv.Itoa(tc.want)+"s") {
				t.Errorf("verify step = %q, want %ds", desc, tc.want)
			}
			// The real run with the same arguments waits the same time.
			resp := startUpgrade(t, r, tc.extra)
			if !strings.Contains(resp["message"].(string), "up to "+strconv.Itoa(tc.want)+"s") {
				t.Errorf("real run message = %q", resp["message"])
			}
		}
	})
}

// Previews carry dry_run:true / executed:false on both paths; a real run
// carries dry_run:false / executed:true. Existing fields are unchanged.
func TestUpgradeAppExecutionMarkers(t *testing.T) {
	previewPaths(t, func(t *testing.T, explicit bool) {
		r, _, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
		preview, envelope := upgradePreview(t, r, explicit, nil)
		if preview["dry_run"] != true || preview["executed"] != false || preview["tool"] != "upgrade_app" || preview["planned_actions"] == nil {
			t.Errorf("preview markers: %v", preview)
		}
		if envelope != nil && (envelope["dry_run"] != true || envelope["executed"] != false) {
			t.Errorf("confirmation_required markers: %v", envelope)
		}
		if n := len(f.called("app.upgrade")); n != 0 {
			t.Fatalf("preview called app.upgrade %d times", n)
		}
	})
	r, _, _ := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	resp := startUpgrade(t, r, nil)
	if resp["dry_run"] != false || resp["executed"] != true || resp["task_id"] == nil || resp["upgrade_summary"] == nil {
		t.Errorf("real run markers: %v", resp)
	}
}

// Dry runs of other tools do not gain the markers (they are opt-in).
func TestDryRunMarkersOmittedWhenUnset(t *testing.T) {
	out, err := marshalJSON(&DryRunResult{Tool: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "dry_run") || strings.Contains(out, "executed") {
		t.Errorf("unset markers serialized: %s", out)
	}
}

func TestUpgradeAppDescriptionMentionsSummarySource(t *testing.T) {
	r, _, _ := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	for _, tool := range r.ListTools() {
		if tool.Name == "upgrade_app" {
			if !strings.Contains(tool.Description, "app.upgrade_summary and may be empty") {
				t.Errorf("description: %s", tool.Description)
			}
			return
		}
	}
	t.Fatal("upgrade_app not listed")
}

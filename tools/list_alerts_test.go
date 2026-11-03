package tools

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestListAlertsDismissedFilter(t *testing.T) {
	const (
		active1   = "00000000-0000-0000-0000-000000000001"
		dismissed = "00000000-0000-0000-0000-000000000002"
		active2   = "00000000-0000-0000-0000-000000000003"
		noField   = "00000000-0000-0000-0000-000000000004"
	)
	untagged := fakeAlert(noField, "INFO", false)
	delete(untagged, "dismissed")
	alerts := []interface{}{
		fakeAlert(active1, "WARNING", false),
		fakeAlert(dismissed, "CRITICAL", true),
		fakeAlert(active2, "CRITICAL", false),
		untagged,
	}
	client, _ := newFakeClientWith(t, map[string]interface{}{"alert.list": alerts}, nil)

	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want []string
	}{
		// Default: dismissed alerts are excluded. An alert without a
		// dismissed field is not known to be dismissed, so it is kept.
		{"default", map[string]interface{}{}, []string{active1, active2, noField}},
		{"nil args", nil, []string{active1, active2, noField}},
		{"include_dismissed false", map[string]interface{}{"include_dismissed": false}, []string{active1, active2, noField}},
		{"include_dismissed", map[string]interface{}{"include_dismissed": true}, []string{active1, dismissed, active2, noField}},
		// The existing dismissed filter is unchanged and wins.
		{"dismissed true", map[string]interface{}{"dismissed": true}, []string{dismissed}},
		{"dismissed false", map[string]interface{}{"dismissed": false}, []string{active1, active2}},
		{"dismissed true, include", map[string]interface{}{"dismissed": true, "include_dismissed": true}, []string{dismissed}},
		{"dismissed false, no include", map[string]interface{}{"dismissed": false, "include_dismissed": false}, []string{active1, active2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := handleListAlerts(client, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			var got []map[string]interface{}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("bad JSON %v: %s", err, out)
			}
			var uuids []string
			for _, a := range got {
				uuids = append(uuids, a["uuid"].(string))
			}
			want := append([]string(nil), tc.want...)
			sort.Strings(uuids)
			sort.Strings(want)
			if strings.Join(uuids, ",") != strings.Join(want, ",") {
				t.Errorf("got %v, want %v", uuids, want)
			}
		})
	}

	for _, args := range []map[string]interface{}{
		{"dismissed": true, "include_dismissed": false},
		{"dismissed": false, "include_dismissed": true},
	} {
		if _, err := handleListAlerts(client, args); err == nil || !strings.Contains(err.Error(), "contradicts") {
			t.Errorf("%v: err = %v, want a contradiction error", args, err)
		}
	}
}

func TestListAlertsSchemaDocumentsDefault(t *testing.T) {
	r := NewRegistry(nil, nil)
	def := r.tools["list_alerts"].Definition
	if !strings.Contains(def.Description, "dismissed alerts are excluded") {
		t.Errorf("description = %q", def.Description)
	}
	props := def.InputSchema["properties"].(map[string]interface{})
	inc, ok := props["include_dismissed"].(map[string]interface{})
	if !ok || inc["type"] != "boolean" || inc["default"] != false {
		t.Errorf("include_dismissed schema = %v", props["include_dismissed"])
	}
	// The argument passes schema validation through CallTool.
	seen := stubHandlers(r)
	if _, err := r.CallTool("list_alerts", map[string]interface{}{"include_dismissed": true}); err != nil {
		t.Fatal(err)
	}
	if len(seen["list_alerts"]) != 1 {
		t.Error("handler did not run")
	}
}

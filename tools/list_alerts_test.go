package tools

import (
	"context"
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
			out, err := handleListAlerts(context.Background(), client, tc.args)
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

}

// Invalid arguments are rejected with a fixed message that does not echo the
// values, before alert.list is called.
func TestListAlertsRejectsInvalidArguments(t *testing.T) {
	const (
		badDismissed  = "invalid arguments for tool list_alerts: dismissed must be a boolean (true or false); nothing was executed"
		badInclude    = "invalid arguments for tool list_alerts: include_dismissed must be a boolean (true or false); nothing was executed"
		onlyDismissed = "invalid arguments for tool list_alerts: dismissed=true lists only dismissed alerts and cannot be combined with include_dismissed=false; nothing was executed"
		onlyActive    = "invalid arguments for tool list_alerts: dismissed=false lists only active alerts and cannot be combined with include_dismissed=true; nothing was executed"
	)
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"dismissed string true", map[string]interface{}{"dismissed": "true"}, badDismissed},
		{"dismissed string false", map[string]interface{}{"dismissed": "false"}, badDismissed},
		{"dismissed number 1", map[string]interface{}{"dismissed": float64(1)}, badDismissed},
		{"dismissed number 0", map[string]interface{}{"dismissed": float64(0)}, badDismissed},
		{"dismissed object", map[string]interface{}{"dismissed": map[string]interface{}{}}, badDismissed},
		{"dismissed array", map[string]interface{}{"dismissed": []interface{}{true}}, badDismissed},
		{"include_dismissed string true", map[string]interface{}{"include_dismissed": "true"}, badInclude},
		{"include_dismissed number 1", map[string]interface{}{"include_dismissed": float64(1)}, badInclude},
		{"include_dismissed int 1", map[string]interface{}{"include_dismissed": 1}, badInclude},
		{"include_dismissed empty string", map[string]interface{}{"include_dismissed": ""}, badInclude},
		{"wrong type with a valid dismissed", map[string]interface{}{"dismissed": true, "include_dismissed": "yes"}, badInclude},
		{"dismissed true, include false", map[string]interface{}{"dismissed": true, "include_dismissed": false}, onlyDismissed},
		{"dismissed false, include true", map[string]interface{}{"dismissed": false, "include_dismissed": true}, onlyActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFakeClientWith(t, map[string]interface{}{"alert.list": []interface{}{}}, nil)
			_, err := handleListAlerts(context.Background(), client, tc.args)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v\nwant %s", err, tc.want)
			}
			if calls := fake.called("alert.list"); len(calls) != 0 {
				t.Errorf("alert.list called %d times for rejected arguments", len(calls))
			}
			for _, v := range []string{"yes", "\"true\"", "\"false\""} {
				if strings.Contains(err.Error(), v) {
					t.Errorf("error echoes the value %s: %v", v, err)
				}
			}
		})
	}

	// Through CallTool too: the schema check does not convert the value.
	client, fake := newFakeClientWith(t, map[string]interface{}{"alert.list": []interface{}{}}, nil)
	r := NewRegistry(client, nil)
	if _, err := r.CallTool(t.Context(), "list_alerts", map[string]interface{}{"include_dismissed": "true"}); err == nil || !strings.Contains(err.Error(), badInclude) {
		t.Errorf("CallTool err = %v, want %q", err, badInclude)
	}
	if calls := fake.called("alert.list"); len(calls) != 0 {
		t.Errorf("CallTool: alert.list called %d times for rejected arguments", len(calls))
	}

	// JSON null means the argument is not set.
	client, fake = newFakeClientWith(t, map[string]interface{}{"alert.list": []interface{}{}}, nil)
	if _, err := handleListAlerts(context.Background(), client, map[string]interface{}{"dismissed": nil, "include_dismissed": nil}); err != nil {
		t.Errorf("null arguments: %v", err)
	}
	if len(fake.called("alert.list")) != 1 {
		t.Error("alert.list not called for valid arguments")
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
	if _, err := r.CallTool(t.Context(), "list_alerts", map[string]interface{}{"include_dismissed": true}); err != nil {
		t.Fatal(err)
	}
	if len(seen["list_alerts"]) != 1 {
		t.Error("handler did not run")
	}
}

package tools

import (
	"math"
	"strings"
	"testing"
)

// schemaSample returns a value that schema accepts. With all set, every
// declared property of an object is filled in; otherwise only the required
// ones.
func schemaSample(schema map[string]interface{}, all bool) interface{} {
	if schema == nil {
		return "x"
	}
	if c, ok := schema["const"]; ok {
		return c
	}
	if enum, ok := schema["enum"].([]interface{}); ok && len(enum) > 0 {
		return enum[0]
	}
	if enum, ok := schema["enum"].([]string); ok && len(enum) > 0 {
		return enum[0]
	}
	switch schemaTypes(schema)[0] {
	case "boolean":
		return true
	case "integer", "number":
		if min, ok := testNumber(schema["minimum"]); ok {
			return min
		}
		if max, ok := testNumber(schema["maximum"]); ok && max < 1 {
			return max
		}
		return 1.0
	case "array":
		items, _ := schema["items"].(map[string]interface{})
		if items == nil {
			return []interface{}{}
		}
		return []interface{}{schemaSample(items, all)}
	case "object":
		obj := map[string]interface{}{}
		props, _ := schema["properties"].(map[string]interface{})
		for _, key := range testRequired(schema["required"]) {
			sub, _ := props[key].(map[string]interface{})
			obj[key] = schemaSample(sub, all)
		}
		if all {
			for key, p := range props {
				sub, _ := p.(map[string]interface{})
				obj[key] = schemaSample(sub, all)
			}
		}
		return obj
	}
	// string (the app-name pattern accepts it too).
	return "x"
}

// testRequired and testNumber read the schema in tests independently of
// the validator under test.
func testRequired(v interface{}) []string {
	switch r := v.(type) {
	case []string:
		return r
	case []interface{}:
		var out []string
		for _, s := range r {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func testNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// schemaTypes returns the type names a schema declares ("" when none).
func schemaTypes(schema map[string]interface{}) []string {
	switch t := schema["type"].(type) {
	case string:
		return []string{t}
	case []string:
		return t
	case []interface{}:
		var out []string
		for _, s := range t {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{""}
}

// minimalArgs returns a tool's required arguments with schema-valid values.
func minimalArgs(tool Tool) map[string]interface{} {
	args, _ := schemaSample(tool.Definition.InputSchema, false).(map[string]interface{})
	if args == nil {
		args = map[string]interface{}{}
	}
	return args
}

// wrongTypeValues returns JSON values whose type none of the schema types
// accepts, covering string, number, fraction, boolean, null, object and
// array.
func wrongTypeValues(types []string) map[string]interface{} {
	all := map[string]interface{}{
		"string":   "true",
		"number":   1.0,
		"fraction": 1.5,
		"boolean":  true,
		"null":     nil,
		"object":   map[string]interface{}{"k": "v"},
		"array":    []interface{}{true},
	}
	for _, t := range types {
		switch t {
		case "string":
			delete(all, "string")
		case "boolean":
			delete(all, "boolean")
		case "integer":
			delete(all, "number")
		case "number":
			delete(all, "number")
			delete(all, "fraction")
		case "object":
			delete(all, "object")
		case "array":
			delete(all, "array")
		case "null":
			delete(all, "null")
		}
	}
	return all
}

// TestEveryArgumentRejectsTheWrongType is the regression for issue #105: a
// declared argument with the wrong JSON type (a string "true" for a boolean,
// "10G" for an integer quota, null, an object, ...) was dropped and the
// call ran with the default. For every tool and every declared top-level
// argument, each wrong-typed value must be refused before the handler runs,
// and the error must name the argument without echoing the value.
func TestEveryArgumentRejectsTheWrongType(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	checked := 0
	for name, tool := range r.tools {
		props, _ := tool.Definition.InputSchema["properties"].(map[string]interface{})
		for key, p := range props {
			sub, _ := p.(map[string]interface{})
			types := schemaTypes(sub)
			if types[0] == "" {
				t.Errorf("%s.%s declares no type", name, key)
				continue
			}
			for label, bad := range wrongTypeValues(types) {
				args := minimalArgs(tool)
				args[key] = bad
				if hasParam(tool, "confirm") && key != "confirm" {
					args["confirm"] = true
				}
				_, err := r.CallTool(t.Context(), name, args)
				checked++
				if err == nil {
					t.Errorf("%s accepted %s=%s (%v expected)", name, key, label, types)
					continue
				}
				msg := err.Error()
				if !strings.Contains(msg, key) || !strings.Contains(msg, "nothing was executed") {
					t.Errorf("%s %s=%s: error does not name the argument: %v", name, key, label, err)
				}
				if s, ok := bad.(string); ok && strings.Contains(msg, `"`+s+`"`) {
					t.Errorf("%s %s=%s: error echoes the value: %v", name, key, label, err)
				}
			}
		}
		if len(seen[name]) != 0 {
			t.Errorf("%s: handler ran for a wrong-typed argument", name)
		}
	}
	if checked < 500 {
		t.Fatalf("only %d cases checked", checked)
	}
}

// TestEveryEnumAndRangeEnforced covers enum values and minimum/maximum for
// every tool, including enums inside arrays (graphs, security) and nested
// objects.
func TestEveryEnumAndRangeEnforced(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	for name, tool := range r.tools {
		props, _ := tool.Definition.InputSchema["properties"].(map[string]interface{})
		for key, p := range props {
			sub, _ := p.(map[string]interface{})
			var cases []interface{}
			if _, ok := sub["enum"]; ok {
				cases = append(cases, "not-a-valid-choice")
			}
			if items, ok := sub["items"].(map[string]interface{}); ok {
				if _, ok := items["enum"]; ok {
					cases = append(cases, []interface{}{"not-a-valid-choice"})
				}
			}
			if min, ok := testNumber(sub["minimum"]); ok {
				cases = append(cases, min-1)
			}
			if max, ok := testNumber(sub["maximum"]); ok {
				cases = append(cases, max+1)
			}
			for _, bad := range cases {
				args := minimalArgs(tool)
				args[key] = bad
				if hasParam(tool, "confirm") {
					args["confirm"] = true
				}
				_, err := r.CallTool(t.Context(), name, args)
				if err == nil || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "not-a-valid-choice") {
					t.Errorf("%s %s=%v: err = %v", name, key, bad, err)
				}
			}
		}
		if len(seen[name]) != 0 {
			t.Errorf("%s: handler ran for an invalid value", name)
		}
	}
}

// TestEveryRequiredArgumentEnforced removes each required argument in turn.
func TestEveryRequiredArgumentEnforced(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	n := 0
	for name, tool := range r.tools {
		for _, key := range testRequired(tool.Definition.InputSchema["required"]) {
			args := minimalArgs(tool)
			delete(args, key)
			if hasParam(tool, "confirm") {
				args["confirm"] = true
			}
			_, err := r.CallTool(t.Context(), name, args)
			n++
			if err == nil || !strings.Contains(err.Error(), key+" is required") {
				t.Errorf("%s without %s: err = %v", name, key, err)
			}
		}
		if len(seen[name]) != 0 {
			t.Errorf("%s: handler ran without a required argument", name)
		}
	}
	if n == 0 {
		t.Fatal("no required arguments found")
	}
}

// TestWrongTypedArgumentsSendNothing runs the cases from the issue against
// the fake middleware: each must fail with no middleware call at all.
func TestWrongTypedArgumentsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]interface{}
		want string
	}{
		{"create_dataset", map[string]interface{}{"name": "tank/y", "quota": "10G"}, "quota must be an integer"},
		{"create_dataset", map[string]interface{}{"name": "tank/y", "readonly": "true"}, "readonly must be a boolean"},
		{"create_dataset", map[string]interface{}{"name": "tank/y", "create_ancestors": "false"}, "create_ancestors must be a boolean"},
		{"create_dataset", map[string]interface{}{"name": "tank/y", "create_ancestors": 0.0}, "create_ancestors must be a boolean"},
		{"create_dataset", map[string]interface{}{"name": "tank/y", "create_ancestors": nil}, "create_ancestors must be a boolean"},
		{"create_dataset", map[string]interface{}{"name": "tank/y", "create_ancestors": "false", "dry_run": true}, "create_ancestors must be a boolean"},
		{"create_dataset", map[string]interface{}{"name": "tank/y", "quota": 1.5}, "quota must be an integer"},
		{"create_dataset", map[string]interface{}{"name": "tank/y", "compression": "lz4"}, "compression must be one of"},
		{"create_dataset", map[string]interface{}{"name": "tank/y", "user_properties": []interface{}{map[string]interface{}{"key": "org:a"}}}, "user_properties[0].value is required"},
		{"create_smb_share", map[string]interface{}{"name": "s", "path": "/mnt/tank/s", "browsable": "false"}, "browsable must be a boolean"},
		{"create_smb_share", map[string]interface{}{"name": "s", "path": "/mnt/tank/s", "audit": map[string]interface{}{"enable": "true"}}, "audit.enable must be a boolean"},
		{"create_nfs_share", map[string]interface{}{"path": "/mnt/tank/s", "ro": "true"}, "ro must be a boolean"},
		{"create_nfs_share", map[string]interface{}{"path": "/mnt/tank/s", "security": []interface{}{"sys"}}, "security[0] must be one of"},
		{"query_shares", map[string]interface{}{"share_type": "bogus"}, "share_type must be one of"},
		{"query_datasets", map[string]interface{}{"order_by": "bogus"}, "order_by must be one of"},
		{"query_datasets", map[string]interface{}{"limit": math.NaN()}, "limit must be an integer"},
		{"upgrade_app", map[string]interface{}{"app_name": "plex", "snapshot_hostpaths": "false", "confirm": true}, "snapshot_hostpaths must be a boolean"},
		{"delete_app", map[string]interface{}{"app_name": "plex", "remove_images": "true", "confirm": true}, "remove_images must be a boolean"},
		{"apply_update", map[string]interface{}{"reboot": "false", "confirm": true}, "reboot must be a boolean"},
		{"delete_scrub_schedule", map[string]interface{}{"id": 1.5, "confirm": true}, "id must be an integer"},
		{"system_reboot", map[string]interface{}{"delay": math.Inf(1), "confirm": true}, "delay must be an integer"},
		{"install_app", map[string]interface{}{"app_name": "Bad_Name", "catalog_app": "plex", "values": map[string]interface{}{}}, "app_name does not match"},
		{"leave_directory_service", map[string]interface{}{"credential": map[string]interface{}{"credential_type": "KERBEROS_USER", "username": "u"}, "confirm": true}, "credential.password is required"},
		{"configure_directory_service", map[string]interface{}{"enable": true, "service_type": "LDAP", "configuration": map[string]interface{}{"starttls": "yes"}, "confirm": true}, "configuration.starttls must be a boolean"},
	} {
		client, fake := newFakeClient(t)
		r := NewRegistry(client, nil)
		_, err := r.CallTool(t.Context(), tc.tool, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasSuffix(err.Error(), "nothing was executed") {
			t.Errorf("%s %v: err = %v, want %q", tc.tool, tc.args, err, tc.want)
		}
		fake.mu.Lock()
		if len(fake.calls) != 0 {
			t.Errorf("%s: %d middleware call(s) made", tc.tool, len(fake.calls))
		}
		fake.mu.Unlock()
	}
}

// TestIntegerArgumentsAcceptWholeNumbers checks that JSON numbers (float64)
// with no fraction pass integer checks, including large byte counts.
func TestIntegerArgumentsAcceptWholeNumbers(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	for _, args := range []map[string]interface{}{
		{"name": "tank/y", "quota": 10.0 * (1 << 40)},
		{"name": "tank/y", "quota": 0.0, "create_ancestors": false},
		{"name": "tank/y", "type": "VOLUME", "volsize": 1073741824.0},
	} {
		if _, err := r.CallTool(t.Context(), "create_dataset", args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	if len(seen["create_dataset"]) != 3 {
		t.Errorf("handler ran %d times, want 3", len(seen["create_dataset"]))
	}
}

// TestUnknownArgumentStillReportedFirst: a call with both an undeclared
// argument and a wrong-typed declared one gets the unknown-argument error.
func TestUnknownArgumentStillReportedFirst(t *testing.T) {
	r := NewRegistry(nil, nil)
	stubHandlers(r)
	_, err := r.CallTool(t.Context(), "create_dataset", map[string]interface{}{"name": "tank/y", "quota": "10G", "quotta": 1.0})
	if err == nil || !strings.Contains(err.Error(), `unknown argument "quotta"`) {
		t.Errorf("err = %v", err)
	}
}

// TestNestedFieldNamesAreNotEchoed: a client-supplied nested key with odd
// characters is not echoed in the error.
func TestNestedFieldNamesAreNotEchoed(t *testing.T) {
	r := NewRegistry(nil, nil)
	stubHandlers(r)
	_, err := r.CallTool(t.Context(), "leave_directory_service", map[string]interface{}{"confirm": true, "credential": map[string]interface{}{
		"credential_type": "KERBEROS_USER", "username": "u", "password": "p", "password=" + secretValue: "x",
	}})
	if err == nil || strings.Contains(err.Error(), secretValue) || !strings.Contains(err.Error(), "not an allowed field") {
		t.Errorf("err = %v", err)
	}
}

// TestAppACLEnableMustBeBoolean: host_path_config.acl_enable inside install
// and update values is not covered by the tool schema (values is free-form),
// so the host-path check refuses a non-boolean itself instead of reading it
// as false.
func TestAppACLEnableMustBeBoolean(t *testing.T) {
	for label, v := range map[string]interface{}{"string true": "true", "string false": "false", "number": 1.0, "null": nil} {
		for _, tool := range []string{"install_app", "update_app"} {
			r, fake := hostPathReviewFake(t, nil, nil)
			values := cloneValues(validHostPathValues())
			cfg := hostPathACLConfig("/mnt/tank/apps/jellyfin/config", "/mnt/tank/apps/jellyfin/data")
			cfg["host_path_config"].(map[string]interface{})["acl_enable"] = v
			values["storage"].(map[string]interface{})["config"] = cfg
			args := map[string]interface{}{"app_name": "jellyfin", "values": values}
			if tool == "install_app" {
				args["catalog_app"] = "jellyfin"
				args["train"] = "community"
			}
			out, err := r.CallTool(t.Context(), tool, args)
			if err == nil || !strings.Contains(err.Error(), "acl_enable must be a boolean") {
				t.Errorf("%s acl_enable=%s: err = %v (out=%s)", tool, label, err, out)
			}
			if n := len(fake.called("app.create")) + len(fake.called("app.update")); n != 0 {
				t.Errorf("%s acl_enable=%s: %d write call(s)", tool, label, n)
			}
		}
	}
}

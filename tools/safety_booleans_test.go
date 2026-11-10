package tools

import (
	"strings"
	"testing"
)

// secretDryRunValue is a non-boolean dry_run used to assert that the
// error names the argument but never echoes the value.
const secretDryRunValue = "true-secret-must-not-be-echoed"

func writeishMiddlewareMethod(method string) bool {
	for _, suffix := range []string{".query", ".info", ".status", ".config", ".list", ".login_ex", ".get_instance", ".get_jobs", "upgrade_summary", ".pool_choices"} {
		if strings.HasSuffix(method, suffix) {
			return false
		}
	}
	return true
}

func assertNoWriteCalls(t *testing.T, fake *fakeMiddleware) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, c := range fake.calls {
		if writeishMiddlewareMethod(c.Method) {
			t.Errorf("write method %s was sent to middleware", c.Method)
		}
	}
}

func toolArgsForSafetyFlagTest(name string) map[string]interface{} {
	valid := map[string]map[string]interface{}{
		"system_reboot":           {"delay": 60.0},
		"apply_update":            {"reboot": true},
		"delete_boot_environment": {"id": "25.04.1"},
		"delete_app":              {"app_name": "plex", "remove_images": true},
		"stop_app":                {"app_name": "plex"},
		"start_app":               {"app_name": "plex"},
		"upgrade_app":             {"app_name": "plex", "version": "2.0.0"},
		"install_app":             {"app_name": "plex", "train": "stable"},
		"update_app":              {"app_name": "plex"},
		"leave_directory_service": {"credential": map[string]interface{}{"credential_type": "KERBEROS_USER", "username": "admin", "password": "pw"}},
		"configure_directory_service": {"service_type": "ACTIVEDIRECTORY", "enable": true,
			"credential":    map[string]interface{}{"credential_type": "KERBEROS_USER", "username": "admin", "password": "pw"},
			"configuration": map[string]interface{}{"domain": "example.com", "hostname": "nas"},
		},
		"delete_scrub_schedule":   {"id": 1.0},
		"create_scrub_schedule":   {"pool": "tank", "schedule": map[string]interface{}{"minute": "00", "hour": "00", "dom": "*", "dow": "7", "month": "*"}},
		"run_scrub":               {"pool": "tank"},
		"create_dataset":          {"name": "tank/x"},
		"create_smb_share":        {"name": "share", "path": "/mnt/tank/share"},
		"create_nfs_share":        {"path": "/mnt/tank/share"},
		"download_update":         {},
		"dismiss_alert":           {"uuid": "alert-1"},
		"restore_alert":           {"uuid": "alert-1"},
		"refresh_directory_cache": {},
	}
	args := map[string]interface{}{}
	for k, v := range valid[name] {
		args[k] = v
	}
	return args
}

// TestNonBooleanDryRunAndConfirmRejected is the regression for a string or
// number dry_run (or confirm) falling through to a real write. Every tool
// that declares dry_run is called with "true" and 1 against the fake
// middleware; destructive tools are also called with confirm=true plus a
// bad dry_run, and with confirm="true". Each call must error and send no
// write to the middleware.
func TestNonBooleanDryRunAndConfirmRejected(t *testing.T) {
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"boot.environment.query": []interface{}{
			map[string]interface{}{"id": "25.04.1", "active": false, "activated": false, "keep": false},
		},
		"pool.query": []interface{}{
			map[string]interface{}{"id": 1.0, "name": "tank", "size": 1.0e12, "status": "ONLINE"},
		},
		"pool.scrub.query": []interface{}{},
		"core.get_jobs":    []interface{}{},
	}, nil)
	r := NewRegistry(client, nil)

	badValues := []struct {
		name string
		v    interface{}
	}{
		{`string "true"`, "true"},
		{"number 1", 1},
		{"float 1", 1.0},
		{"secret string", secretDryRunValue},
	}

	coveredDryRun := 0
	for name, tool := range r.tools {
		if !hasParam(tool, "dry_run") {
			continue
		}
		coveredDryRun++
		base := toolArgsForSafetyFlagTest(name)
		for _, bad := range badValues {
			call := map[string]interface{}{}
			for k, v := range base {
				call[k] = v
			}
			call["dry_run"] = bad.v
			out, err := r.CallTool(name, call)
			assertInvalidBooleanArg(t, name, "dry_run", bad.name, out, err)
		}
		if safetyOf(name).Access == Destructive {
			for _, bad := range badValues {
				call := map[string]interface{}{}
				for k, v := range base {
					call[k] = v
				}
				call["confirm"] = true
				call["dry_run"] = bad.v
				out, err := r.CallTool(name, call)
				assertInvalidBooleanArg(t, name, "dry_run", "confirm=true plus "+bad.name, out, err)
			}
			out, err := r.CallTool(name, mergeArgs(base, map[string]interface{}{"confirm": "true"}))
			assertInvalidBooleanArg(t, name, "confirm", `string "true"`, out, err)
		}
	}
	if coveredDryRun == 0 {
		t.Fatal("no tools declared dry_run")
	}
	assertNoWriteCalls(t, fake)
}

func mergeArgs(base, extra map[string]interface{}) map[string]interface{} {
	call := map[string]interface{}{}
	for k, v := range base {
		call[k] = v
	}
	for k, v := range extra {
		call[k] = v
	}
	return call
}

func assertInvalidBooleanArg(t *testing.T, tool, key, caseName, out string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s %s: no error, out=%s", tool, caseName, out)
		return
	}
	msg := err.Error()
	if !strings.Contains(msg, key+" must be a JSON boolean") || !strings.Contains(msg, "nothing was executed") {
		t.Errorf("%s %s: err=%v, want invalid %s", tool, caseName, err, key)
	}
	if strings.Contains(msg, secretDryRunValue) {
		t.Errorf("%s %s: error echoes the argument value: %v", tool, caseName, err)
	}
}

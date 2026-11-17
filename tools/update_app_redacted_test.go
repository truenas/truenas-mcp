package tools

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/redact"
)

// fakeStoredAuthKey is the secret stored in the app's current config. It
// must reach app.update when the caller sends the marker back, and must
// never appear in anything returned to the caller.
const fakeStoredAuthKey = "fake-stored-auth-key-0000"

func redactedAppConfig() map[string]interface{} {
	cfg := issue157IxVolumeConfig()
	cfg["network"] = map[string]interface{}{
		"auth_key":         fakeStoredAuthKey,
		"advertise_routes": []interface{}{"192.0.2.0/24"},
		"hostname":         "app-node",
	}
	return cfg
}

// valuesFromGetAppConfig is what a caller has after get_app_config: the
// current config with the secret replaced by the redaction marker.
func valuesFromGetAppConfig(t *testing.T, current map[string]interface{}) map[string]interface{} {
	t.Helper()
	masked, ok := redact.Value(cloneValues(current)).(map[string]interface{})
	if !ok {
		t.Fatal("redact.Value did not return an object")
	}
	if got := masked["network"].(map[string]interface{})["auth_key"]; got != redact.Marker {
		t.Fatalf("test setup: get_app_config would show auth_key as %v, want the marker", got)
	}
	return masked
}

func networkOf(t *testing.T, values map[string]interface{}) map[string]interface{} {
	t.Helper()
	n, ok := values["network"].(map[string]interface{})
	if !ok {
		t.Fatalf("values.network = %v, want an object", values["network"])
	}
	return n
}

// Regression: on main the marker from get_app_config was sent to
// app.update verbatim, replacing the stored secret with "[REDACTED]".
func TestUpdateAppRedactedMarkerKeepsStoredSecret(t *testing.T) {
	current := redactedAppConfig()
	r, fake := issue157Registry(t, current, false)
	values := valuesFromGetAppConfig(t, current)
	networkOf(t, values)["advertise_routes"] = []interface{}{"192.0.2.0/24", "198.51.100.0/24"}

	if _, err := callUpdateApp(t, r, values, nil); err != nil {
		t.Fatalf("update_app: %v", err)
	}
	sent := networkOf(t, updateAppPayload(t, fake))
	if sent["auth_key"] != fakeStoredAuthKey {
		t.Errorf("app.update network.auth_key = %v, want the stored secret (the marker must not be written)", sent["auth_key"])
	}
	if routes, _ := sent["advertise_routes"].([]interface{}); len(routes) != 2 {
		t.Errorf("app.update network.advertise_routes = %v, want the caller's two routes", sent["advertise_routes"])
	}
	if sent["hostname"] != "app-node" {
		t.Errorf("app.update network.hostname = %v, want app-node", sent["hostname"])
	}
	if networkOf(t, values)["auth_key"] != redact.Marker {
		t.Errorf("caller's values were modified: auth_key = %v", networkOf(t, values)["auth_key"])
	}
}

// A marker with nothing stored at its path is refused, naming the path,
// instead of writing the marker.
func TestUpdateAppRedactedMarkerWithoutStoredValueRefused(t *testing.T) {
	for name, stored := range map[string]interface{}{
		"missing":   nil,
		"empty":     "",
		"not text":  map[string]interface{}{"x": 1},
		"no parent": "drop-parent",
	} {
		t.Run(name, func(t *testing.T) {
			current := redactedAppConfig()
			network := current["network"].(map[string]interface{})
			switch stored {
			case nil:
				delete(network, "auth_key")
			case "drop-parent":
				delete(current, "network")
			default:
				network["auth_key"] = stored
			}
			r, fake := issue157Registry(t, current, false)
			values := cloneValues(redactedAppConfig())
			networkOf(t, values)["auth_key"] = redact.Marker

			for _, extra := range []map[string]interface{}{{"dry_run": true}, nil} {
				out, err := callUpdateApp(t, r, cloneValues(values), extra)
				if err == nil {
					t.Fatalf("update_app (%v) accepted a marker with no stored value: %s", extra, out)
				}
				if !strings.Contains(err.Error(), "network.auth_key") {
					t.Errorf("error %q, want the path network.auth_key", err)
				}
			}
			if n := len(fake.called("app.update")); n != 0 {
				t.Errorf("app.update called %d times", n)
			}
		})
	}
}

// The preview and confirmation prompt name the kept path, never the
// secret; the middleware call carries the secret; nothing returned to the
// caller (preview, prompt, execute reply, tasks_get, logs) contains it.
func TestUpdateAppRedactedMarkerPreviewAndNoLeak(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	current := redactedAppConfig()
	r, fake := issue157Registry(t, current, false)
	values := valuesFromGetAppConfig(t, current)
	networkOf(t, values)["hostname"] = "app-node-2"
	args := map[string]interface{}{"app_name": "jellyfin", "values": values}

	var outputs []string
	check := func(label, out string) {
		t.Helper()
		outputs = append(outputs, out)
		if strings.Contains(out, fakeStoredAuthKey) {
			t.Errorf("%s contains the stored secret:\n%s", label, out)
		}
	}

	dry := cloneValues(args)
	dry["dry_run"] = true
	preview, err := r.CallTool(t.Context(), "update_app", dry)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	check("dry-run preview", preview)
	if !strings.Contains(preview, "network.auth_key") {
		t.Errorf("dry-run preview does not name the kept path network.auth_key:\n%s", preview)
	}

	prompt, err := r.CallTool(t.Context(), "update_app", cloneValues(args))
	if err != nil {
		t.Fatalf("confirmation prompt: %v", err)
	}
	check("confirmation prompt", prompt)
	if !strings.Contains(prompt, "network.auth_key") {
		t.Errorf("confirmation prompt does not name the kept path network.auth_key:\n%s", prompt)
	}
	tok := replyToken(t, prompt)
	if tok == "" {
		t.Fatalf("no confirmation_token in prompt:\n%s", prompt)
	}
	confirmed := cloneValues(args)
	confirmed["confirmation_token"] = tok
	out, err := r.CallTool(t.Context(), "update_app", confirmed)
	if err != nil {
		t.Fatalf("confirmed update_app: %v", err)
	}
	check("execute reply", out)

	var reply map[string]interface{}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("execute reply: %v\n%s", err, out)
	}
	taskID, _ := reply["task_id"].(string)
	if taskID == "" {
		t.Fatalf("no task_id in reply:\n%s", out)
	}
	taskOut, err := r.CallTool(t.Context(), "tasks_get", map[string]interface{}{"task_id": taskID})
	if err != nil {
		t.Fatalf("tasks_get: %v", err)
	}
	check("tasks_get", taskOut)
	check("logs", logs.String())

	if sent := networkOf(t, updateAppPayload(t, fake)); sent["auth_key"] != fakeStoredAuthKey || sent["hostname"] != "app-node-2" {
		t.Errorf("app.update network = %v, want the stored auth_key and the new hostname", sent)
	}
	calls := fake.called("app.update")
	if !strings.Contains(string(calls[0].Params), fakeStoredAuthKey) {
		t.Errorf("app.update params do not carry the stored secret: %s", calls[0].Params)
	}
}

// Lists are matched by index only when the stored list has the same length.
func TestUpdateAppRedactedMarkerInLists(t *testing.T) {
	listConfig := func() map[string]interface{} {
		cfg := issue157IxVolumeConfig()
		cfg["peers"] = []interface{}{
			map[string]interface{}{"name": "a", "preshared_key": "fake-psk-a-0000"},
			map[string]interface{}{"name": "b", "preshared_key": "fake-psk-b-0000"},
		}
		return cfg
	}
	t.Run("same length", func(t *testing.T) {
		current := listConfig()
		r, fake := issue157Registry(t, current, false)
		values := cloneValues(current)
		peers := values["peers"].([]interface{})
		peers[0].(map[string]interface{})["preshared_key"] = redact.Marker
		peers[1].(map[string]interface{})["preshared_key"] = redact.Marker
		peers[1].(map[string]interface{})["name"] = "b2"
		if _, err := callUpdateApp(t, r, values, nil); err != nil {
			t.Fatalf("update_app: %v", err)
		}
		sent := updateAppPayload(t, fake)["peers"].([]interface{})
		if sent[0].(map[string]interface{})["preshared_key"] != "fake-psk-a-0000" ||
			sent[1].(map[string]interface{})["preshared_key"] != "fake-psk-b-0000" ||
			sent[1].(map[string]interface{})["name"] != "b2" {
			t.Errorf("app.update peers = %v, want stored keys kept by index", sent)
		}
	})
	t.Run("different length is refused", func(t *testing.T) {
		current := listConfig()
		r, fake := issue157Registry(t, current, false)
		values := cloneValues(current)
		values["peers"] = []interface{}{map[string]interface{}{"name": "b", "preshared_key": redact.Marker}}
		_, err := callUpdateApp(t, r, values, nil)
		if err == nil || !strings.Contains(err.Error(), "peers[0].preshared_key") {
			t.Fatalf("err = %v, want a refusal naming peers[0].preshared_key", err)
		}
		if n := len(fake.called("app.update")); n != 0 {
			t.Errorf("app.update called %d times", n)
		}
	})
}

// A value with a marker inside it (for example NAME=[REDACTED] in an
// environment list) cannot be restored and is refused unless it is
// unchanged from the stored value.
func TestUpdateAppEmbeddedMarkerRefused(t *testing.T) {
	current := issue157IxVolumeConfig()
	current["env"] = []interface{}{"DB_PASSWORD=fake-db-pw-0000"}
	r, fake := issue157Registry(t, current, false)
	values := cloneValues(current)
	values["env"] = []interface{}{"DB_PASSWORD=" + redact.Marker}
	_, err := callUpdateApp(t, r, values, nil)
	if err == nil || !strings.Contains(err.Error(), "env[0]") {
		t.Fatalf("err = %v, want a refusal naming env[0]", err)
	}
	if strings.Contains(err.Error(), "fake-db-pw-0000") {
		t.Errorf("error leaks the stored value: %v", err)
	}
	if n := len(fake.called("app.update")); n != 0 {
		t.Errorf("app.update called %d times", n)
	}
}

// Custom apps take the same path.
func TestUpdateAppRedactedMarkerCustomApp(t *testing.T) {
	current := redactedAppConfig()
	r, fake := issue157Registry(t, current, true)
	values := valuesFromGetAppConfig(t, current)
	if _, err := callUpdateApp(t, r, values, nil); err != nil {
		t.Fatalf("update_app: %v", err)
	}
	if got := networkOf(t, updateAppPayload(t, fake))["auth_key"]; got != fakeStoredAuthKey {
		t.Errorf("app.update network.auth_key = %v, want the stored secret", got)
	}
}

// The plan itself masks kept paths in everything it shows, so the stored
// secret is hidden even before output redaction runs; only CallParams
// carries it.
func TestUpdateAppPlanMasksKeptSecretBeforeRedaction(t *testing.T) {
	current := redactedAppConfig()
	r, _ := issue157Registry(t, current, false)
	values := valuesFromGetAppConfig(t, current)
	networkOf(t, values)["hostname"] = "app-node-2"
	plan, err := planUpdateApp(t.Context(), r.client, map[string]interface{}{"app_name": "jellyfin", "values": values})
	if err != nil {
		t.Fatalf("planUpdateApp: %v", err)
	}
	shown, err := json.Marshal(map[string]interface{}{
		"current_state": plan.CurrentState,
		"actions":       plan.Actions,
		"warnings":      plan.Warnings,
		"summary":       plan.ConfirmSummary,
		"result":        plan.Result,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(shown), fakeStoredAuthKey) {
		t.Errorf("plan display fields contain the stored secret: %s", shown)
	}
	if !strings.Contains(string(shown), "network.auth_key") {
		t.Errorf("plan display fields do not name the kept path: %s", shown)
	}
	params, err := json.Marshal(plan.CallParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(params), fakeStoredAuthKey) {
		t.Errorf("CallParams do not carry the stored secret: %s", params)
	}
	if strings.Contains(string(params), redact.Marker) {
		t.Errorf("CallParams still carry the marker: %s", params)
	}
	summary := strings.Join(plan.ConfirmSummary.forDisplay().Facts, "\n")
	if !strings.Contains(summary, "network.auth_key") || !strings.Contains(summary, "network: ") {
		t.Errorf("confirmation facts %q, want the changed key and the kept path", summary)
	}
}

func TestRestoreRedactedValuesLeavesUnmarkedValues(t *testing.T) {
	values := map[string]interface{}{"a": "x", "b": []interface{}{1.0, true, nil}, "c": map[string]interface{}{"d": "y"}}
	out, kept, err := restoreRedactedValues(values, nil)
	if err != nil || len(kept) != 0 {
		t.Fatalf("out=%v kept=%v err=%v", out, kept, err)
	}
	if !jsonValuesEqual(out, values) {
		t.Errorf("out = %v, want %v", out, values)
	}
}

const fakeEnvSecret = "fake-env-secret-0000"

func envAppConfig() map[string]interface{} {
	cfg := issue157IxVolumeConfig()
	cfg["TZ"] = "UTC"
	cfg["env"] = []interface{}{
		map[string]interface{}{"name": "DB_PASSWORD", "value": fakeEnvSecret},
		map[string]interface{}{"name": "LOG_LEVEL", "value": "info"},
	}
	return cfg
}

func envEntry(name, value string) map[string]interface{} {
	return map[string]interface{}{"name": name, "value": value}
}

// A value masked only because of a sibling (an env entry's name) is kept
// when the sibling is unchanged.
func TestUpdateAppRedactedEnvValueKeptWhenUnchanged(t *testing.T) {
	current := envAppConfig()
	masked := redact.Value(cloneValues(current)).(map[string]interface{})
	if got := masked["env"].([]interface{})[0].(map[string]interface{})["value"]; got != redact.Marker {
		t.Fatalf("test setup: env[0].value is shown as %v, want the marker", got)
	}
	r, fake := issue157Registry(t, current, false)
	values := cloneValues(masked)
	values["env"].([]interface{})[1].(map[string]interface{})["value"] = "debug"
	if _, err := callUpdateApp(t, r, values, nil); err != nil {
		t.Fatalf("update_app: %v", err)
	}
	env := updateAppPayload(t, fake)["env"].([]interface{})
	if env[0].(map[string]interface{})["value"] != fakeEnvSecret || env[1].(map[string]interface{})["value"] != "debug" {
		t.Errorf("app.update env = %v, want the stored secret kept and LOG_LEVEL changed", env)
	}
}

// Blocker: keeping a stored secret where the outgoing values would no
// longer redact it (a renamed or reordered env entry) must be refused, or
// the next get_app_config shows the secret in clear.
func TestUpdateAppRedactedMarkerRefusedWhenNoLongerRedacted(t *testing.T) {
	for name, env := range map[string][]interface{}{
		"rename":  {envEntry("VISIBLE_NAME", redact.Marker), envEntry("LOG_LEVEL", "info")},
		"reorder": {envEntry("LOG_LEVEL", redact.Marker), envEntry("DB_PASSWORD", redact.Marker)},
	} {
		t.Run(name, func(t *testing.T) {
			current := envAppConfig()
			r, fake := issue157Registry(t, current, false)
			values := cloneValues(current)
			values["env"] = env
			for _, extra := range []map[string]interface{}{{"dry_run": true}, nil} {
				out, err := callUpdateApp(t, r, cloneValues(values), extra)
				if err == nil {
					t.Fatalf("update_app (%v) kept a secret where it is no longer redacted: %s", extra, out)
				}
				if !strings.Contains(err.Error(), "env[0].value") {
					t.Errorf("error %q, want the path env[0].value", err)
				}
				if strings.Contains(err.Error(), fakeEnvSecret) {
					t.Errorf("error leaks the stored value: %v", err)
				}
			}
			if n := len(fake.called("app.update")); n != 0 {
				t.Errorf("app.update called %d times", n)
			}
		})
	}
}

// A marker on a stored value that get_app_config does not redact is not
// "kept": it is refused.
func TestUpdateAppRedactedMarkerOnNonSecretRefused(t *testing.T) {
	current := envAppConfig()
	r, fake := issue157Registry(t, current, false)
	values := cloneValues(current)
	values["TZ"] = redact.Marker
	_, err := callUpdateApp(t, r, values, nil)
	if err == nil || !strings.Contains(err.Error(), "values.TZ") {
		t.Fatalf("err = %v, want a refusal naming values.TZ", err)
	}
	if n := len(fake.called("app.update")); n != 0 {
		t.Errorf("app.update called %d times", n)
	}
}

func cifsAppConfig(server string) map[string]interface{} {
	return issue157ConfigWithAdditional(issue157CIFSEntry(server, "/media", "/mnt/smb"))
}

func cifsValuesWithMarker(server string) map[string]interface{} {
	values := cifsAppConfig(server)
	entry := values["storage"].(map[string]interface{})["additional_storage"].([]interface{})[0].(map[string]interface{})
	entry["cifs_config"].(map[string]interface{})["password"] = redact.Marker
	entry["read_only"] = true
	return values
}

func cifsPasswordSent(t *testing.T, fake *fakeMiddleware) interface{} {
	t.Helper()
	add := storageOf(updateAppPayload(t, fake))["additional_storage"].([]interface{})
	return add[0].(map[string]interface{})["cifs_config"].(map[string]interface{})["password"]
}

// A CIFS password sent back as the marker is kept when the share's
// server and path are unchanged.
func TestUpdateAppRedactedCIFSPasswordKeptOnSameShare(t *testing.T) {
	r, fake := issue157Registry(t, cifsAppConfig("files.example.invalid"), false)
	if _, err := callUpdateApp(t, r, cifsValuesWithMarker("files.example.invalid"), nil); err != nil {
		t.Fatalf("update_app: %v", err)
	}
	if got := cifsPasswordSent(t, fake); got != issue157CIFSPassword {
		t.Errorf("app.update cifs password = %v, want the stored password", got)
	}
}

// Blocker: the stored CIFS password must not be sent to a different
// server, even with allow_storage_change.
func TestUpdateAppRedactedCIFSPasswordRefusedOnServerChange(t *testing.T) {
	r, fake := issue157Registry(t, cifsAppConfig("files.example.invalid"), false)
	for _, extra := range []map[string]interface{}{
		{"allow_storage_change": true, "dry_run": true},
		{"allow_storage_change": true},
	} {
		out, err := callUpdateApp(t, r, cifsValuesWithMarker("other.example.invalid"), extra)
		if err == nil {
			t.Fatalf("update_app (%v) sent the stored CIFS password to a new server: %s", extra, out)
		}
		if !strings.Contains(err.Error(), "storage.additional_storage[0].cifs_config.password") {
			t.Errorf("error %q, want the kept path", err)
		}
		if strings.Contains(err.Error(), issue157CIFSPassword) {
			t.Errorf("error leaks the stored password: %v", err)
		}
	}
	if n := len(fake.called("app.update")); n != 0 {
		t.Errorf("app.update called %d times", n)
	}
}

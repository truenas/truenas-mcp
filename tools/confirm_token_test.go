package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// replyToken returns the confirmation_token of a confirmation_required reply,
// or "" when there is none.
func replyToken(t *testing.T, out string) string {
	t.Helper()
	var reply map[string]interface{}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return ""
	}
	tok, _ := reply["confirmation_token"].(string)
	return tok
}

// realCalls returns the non-dry-run handler calls recorded by stubHandlers.
func realCalls(calls []map[string]interface{}) int {
	n := 0
	for _, c := range calls {
		if c["dry_run"] != true {
			n++
		}
	}
	return n
}

func tokenArgs(base map[string]interface{}, extra map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// The ten destructive tools, update_app included, are gated by a
// confirmation token.
func TestConfirmTokenTenDestructiveTools(t *testing.T) {
	want := []string{"system_reboot", "apply_update", "delete_boot_environment", "delete_app", "stop_app",
		"upgrade_app", "update_app", "configure_directory_service", "leave_directory_service", "delete_scrub_schedule"}
	n := 0
	for _, s := range toolSafety {
		if s.Access == Destructive {
			n++
		}
	}
	if n != len(want) {
		t.Errorf("%d destructive tools, want %d", n, len(want))
	}
	r := NewRegistry(nil, nil)
	for _, name := range want {
		if safetyOf(name).Access != Destructive {
			t.Errorf("%s is %v, want destructive", name, safetyOf(name).Access)
		}
		props := r.tools[name].Definition.InputSchema["properties"].(map[string]interface{})
		if _, ok := props["confirmation_token"]; !ok {
			t.Errorf("%s schema has no confirmation_token", name)
		}
		if _, ok := props["confirm"]; ok {
			t.Errorf("%s schema still declares confirm", name)
		}
	}
}

// Without a token a destructive call previews and returns a token; the
// token executes exactly once; reuse is refused and executes nothing.
func TestConfirmTokenExecutesExactlyOnce(t *testing.T) {
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)
	args := map[string]interface{}{"delay": float64(30)}

	out, err := r.CallTool(t.Context(), "system_reboot", args)
	if err != nil {
		t.Fatal(err)
	}
	tok := replyToken(t, out)
	if tok == "" || !strings.Contains(out, `"status": "confirmation_required"`) || !strings.Contains(out, `"preview"`) {
		t.Fatalf("call without a token: want a preview and a confirmation_token, got:\n%s", out)
	}
	if n := len(fake.called("system.reboot")); n != 0 {
		t.Fatalf("system.reboot sent without a token: %d", n)
	}

	if _, err := r.CallTool(t.Context(), "system_reboot", tokenArgs(args, map[string]interface{}{"confirmation_token": tok})); err != nil {
		t.Fatalf("call with a valid token: %v", err)
	}
	if n := len(fake.called("system.reboot")); n != 1 {
		t.Fatalf("system.reboot calls = %d after a valid token, want 1", n)
	}

	_, err = r.CallTool(t.Context(), "system_reboot", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "already used") || !strings.Contains(err.Error(), "nothing was executed") {
		t.Errorf("reused token: err = %v, want an already-used rejection", err)
	}
	if n := len(fake.called("system.reboot")); n != 1 {
		t.Errorf("system.reboot calls = %d after reuse, want 1", n)
	}
}

// confirm=true no longer executes: it gets the preview, a token and an
// explanation of the new flow.
func TestConfirmTrueAloneDoesNotExecute(t *testing.T) {
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)
	out, err := r.CallTool(t.Context(), "system_reboot", map[string]interface{}{"confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fake.called("system.reboot")); n != 0 {
		t.Fatalf("confirm=true executed system.reboot (%d calls)", n)
	}
	if replyToken(t, out) == "" || !strings.Contains(out, "confirm=true no longer executes") {
		t.Errorf("confirm=true reply should carry a token and explain the new flow:\n%s", out)
	}
}

// A token is bound to the tool and to the exact arguments.
func TestConfirmTokenBoundToToolAndArguments(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	issue := func(name string, args map[string]interface{}) string {
		t.Helper()
		out, err := r.CallTool(t.Context(), name, args)
		if err != nil {
			t.Fatalf("%s preview: %v", name, err)
		}
		tok := replyToken(t, out)
		if tok == "" {
			t.Fatalf("%s preview has no token:\n%s", name, out)
		}
		return tok
	}

	tok := issue("delete_app", map[string]interface{}{"app_name": "plex"})
	_, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{"app_name": "plex", "confirmation_token": tok})
	if err == nil || !strings.Contains(err.Error(), "different tool") || !strings.Contains(err.Error(), "nothing was executed") {
		t.Errorf("token of another tool: err = %v", err)
	}

	tok = issue("delete_app", map[string]interface{}{"app_name": "plex"})
	_, err = r.CallTool(t.Context(), "delete_app", map[string]interface{}{"app_name": "nextcloud", "confirmation_token": tok})
	if err == nil || !strings.Contains(err.Error(), "different arguments") || !strings.Contains(err.Error(), "nothing was executed") {
		t.Errorf("token for other arguments: err = %v", err)
	}
	tok = issue("delete_app", map[string]interface{}{"app_name": "plex"})
	_, err = r.CallTool(t.Context(), "delete_app", map[string]interface{}{"app_name": "plex", "remove_images": true, "confirmation_token": tok})
	if err == nil || !strings.Contains(err.Error(), "different arguments") {
		t.Errorf("token with an added argument: err = %v", err)
	}

	_, err = r.CallTool(t.Context(), "delete_app", map[string]interface{}{"app_name": "plex", "confirmation_token": "not-a-token"})
	if err == nil || !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "nothing was executed") {
		t.Errorf("unknown token: err = %v", err)
	}
	if n := realCalls(seen["delete_app"]) + realCalls(seen["stop_app"]); n != 0 {
		t.Fatalf("a rejected token executed: delete_app %v stop_app %v", seen["delete_app"], seen["stop_app"])
	}

	// The same arguments in another key order, with dry_run=false and the
	// legacy confirm flag, still match.
	tok = issue("delete_app", map[string]interface{}{"remove_images": true, "app_name": "plex"})
	if _, err := r.CallTool(t.Context(), "delete_app", map[string]interface{}{"app_name": "plex", "remove_images": true, "dry_run": false, "confirm": true, "confirmation_token": tok}); err != nil {
		t.Fatalf("matching token refused: %v", err)
	}
	if n := realCalls(seen["delete_app"]); n != 1 {
		t.Fatalf("delete_app ran %d times, want 1", n)
	}
	last := seen["delete_app"][len(seen["delete_app"])-1]
	if _, ok := last["confirmation_token"]; ok {
		t.Error("the handler received confirmation_token")
	}
	if _, ok := last["confirm"]; ok {
		t.Error("the handler received confirm")
	}
}

// dry_run=true previews only and issues no token.
func TestConfirmTokenDryRunIssuesNoToken(t *testing.T) {
	r := NewRegistry(nil, nil)
	stubHandlers(r)
	out, err := r.CallTool(t.Context(), "delete_app", map[string]interface{}{"app_name": "plex", "dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "confirmation_token") {
		t.Errorf("dry run issued a token:\n%s", out)
	}
}

// A preview that says the operation cannot proceed returns an error, issues
// no token and does not invite confirmation.
func TestConfirmTokenBlockedPreviewIssuesNoToken(t *testing.T) {
	bootEnvs := []interface{}{
		map[string]interface{}{"id": "running", "active": true, "activated": false, "keep": false},
		map[string]interface{}{"id": "protected", "active": false, "activated": false, "keep": true},
	}
	client, fake := newFakeClientWith(t, map[string]interface{}{"boot.environment.query": bootEnvs}, nil)
	r := NewRegistry(client, nil)
	for _, id := range []string{"running", "protected"} {
		for _, args := range []map[string]interface{}{{"id": id}, {"id": id, "confirm": true}} {
			out, err := r.CallTool(t.Context(), "delete_boot_environment", args)
			if err == nil {
				t.Errorf("blocked delete of %s (%v) returned no error:\n%s", id, args, out)
				continue
			}
			msg := err.Error()
			if strings.Contains(out, "confirmation_token") || strings.Contains(msg, "confirmation_token=") || !strings.Contains(msg, "cannot proceed") || !strings.Contains(msg, "nothing was executed") {
				t.Errorf("blocked delete of %s (%v): out=%s err=%v", id, args, out, err)
			}
		}
	}
	if calls := fake.called("boot.environment.destroy"); len(calls) != 0 {
		t.Fatalf("a blocked delete sent boot.environment.destroy: %v", calls)
	}
}

// update_app is destructive now: without a token it does not run.
func TestUpdateAppNeedsToken(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	args := minimalArgs(r.tools["update_app"])
	out, err := r.CallTool(t.Context(), "update_app", args)
	if err != nil {
		t.Fatal(err)
	}
	if realCalls(seen["update_app"]) != 0 || replyToken(t, out) == "" {
		t.Errorf("update_app without a token: calls %v, reply:\n%s", seen["update_app"], out)
	}
}

// Write tools that are not destructive run without a token.
func TestNonDestructiveWritesNeedNoToken(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	for name := range r.tools {
		if safetyOf(name).Access != Write {
			continue
		}
		args := minimalArgs(r.tools[name])
		out, err := r.CallTool(t.Context(), name, args)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if realCalls(seen[name]) != 1 || strings.Contains(out, "confirmation") {
			t.Errorf("%s did not run directly: calls %v, out %s", name, seen[name], out)
		}
		props := r.tools[name].Definition.InputSchema["properties"].(map[string]interface{})
		if _, ok := props["confirmation_token"]; ok {
			t.Errorf("write tool %s declares confirmation_token", name)
		}
	}
}

// Read-only mode issues no tokens: upgrade_app stays dry-run only.
func TestReadOnlyIssuesNoTokens(t *testing.T) {
	r, f := readOnlyUpgradeRegistry(t)
	out, err := r.CallTool(t.Context(), "upgrade_app", map[string]interface{}{"app_name": "plex", "dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "confirmation_token") {
		t.Errorf("read-only dry run issued a token:\n%s", out)
	}
	if _, err := r.CallTool(t.Context(), "upgrade_app", map[string]interface{}{"app_name": "plex"}); err == nil || !strings.Contains(err.Error(), "read-only mode") {
		t.Errorf("read-only upgrade without dry_run: err = %v", err)
	}
	for _, tool := range r.ListTools() {
		if props, _ := tool.InputSchema["properties"].(map[string]interface{}); props != nil {
			if _, ok := props["confirmation_token"]; ok {
				t.Errorf("read-only: %s lists confirmation_token", tool.Name)
			}
		}
	}
	assertOnlyReadOnlyCalls(t, f)
}

// approve returns args, without the legacy confirm flag, plus a
// confirmation_token issued for exactly these arguments, as a client has
// after showing the preview to the user and getting their approval. Tests
// that exercise the real handler of a destructive tool use it instead of a
// preview round trip, so their middleware call counts stay unchanged.
func approve(t *testing.T, r *Registry, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	a := withoutKeys(args, "confirm", "confirmation_token")
	h, err := confirmationArgsHash(dropNullOptionals(r.tools[name].Definition.InputSchema, a))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := r.confirmTokens.issue(name, h)
	if err != nil {
		t.Fatal(err)
	}
	a["confirmation_token"] = tok
	return a
}

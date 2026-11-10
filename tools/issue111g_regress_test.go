package tools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
)

// updateStatusResult is a 25.10 update.status result: UpdateStatus in
// middlewared/api/v25_10_0/update.py and the v25.10 API docs
// (https://api.truenas.com/v25.10/api_methods_update.status.html).
// code, error and update_download_progress are at the top level;
// new_version lives at status.new_version, not at the top level.
func updateStatusResult(code string, status, errObj, progress interface{}) map[string]interface{} {
	return map[string]interface{}{
		"code":                     code,
		"status":                   status,
		"error":                    errObj,
		"update_download_progress": progress,
	}
}

func updateStatusDetails(newVersion interface{}) map[string]interface{} {
	return map[string]interface{}{
		"current_version": map[string]interface{}{
			"train":           "25.10",
			"profile":         "GENERAL",
			"matches_profile": true,
		},
		"new_version": newVersion,
	}
}

func updateStatusNewVersion(version string) map[string]interface{} {
	return map[string]interface{}{
		"version":           version,
		"manifest":          map[string]interface{}{"train": "25.10"},
		"release_notes":     nil,
		"release_notes_url": "https://www.truenas.com/docs/scale/25.10/gettingstarted/releasenotes/",
	}
}

// updateStatusAvailable is a NORMAL update.status result with the given
// new_version.version, or a null new_version when version is empty.
func updateStatusAvailable(version string) map[string]interface{} {
	var nv interface{}
	if version != "" {
		nv = updateStatusNewVersion(version)
	}
	return updateStatusResult("NORMAL", updateStatusDetails(nv), nil, nil)
}

// updateStatusWithVersion is the real 25.10 shape. An empty version is a
// null new_version (no update available).
func updateStatusWithVersion(version string) map[string]interface{} {
	return updateStatusAvailable(version)
}

func updateStatusError(errname, reason string) map[string]interface{} {
	return updateStatusResult("ERROR", nil, map[string]interface{}{
		"errname": errname,
		"reason":  reason,
	}, nil)
}

func applyUpdateRegistry(t *testing.T, status map[string]interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	if status == nil {
		status = updateStatusWithVersion("25.10.1")
	}
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"update.status": status,
		"update.run":    99,
		"core.get_jobs": []interface{}{},
	}, nil)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake
}

// The confirmation token for apply_update must bind update.status's
// new_version.version. If that version changes between preview and confirm,
// the token is refused and update.run is not sent.
func TestApplyUpdateNewVersionChangeRefusesConfirm(t *testing.T) {
	status := updateStatusWithVersion("25.10.1")
	r, fake := applyUpdateRegistry(t, status)
	args := map[string]interface{}{"reboot": false}
	tok := previewToken(t, r, "apply_update", args)

	fake.mu.Lock()
	fake.results["update.status"] = updateStatusWithVersion("25.10.2")
	fake.mu.Unlock()

	out, err := r.CallTool(t.Context(), "apply_update", map[string]interface{}{
		"reboot": false, "confirmation_token": tok,
	})
	if err == nil {
		t.Fatalf("confirm must be refused when new_version moved from 25.10.1 to 25.10.2, got %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "different planned operation") {
		t.Errorf("want a plan-mismatch refusal, got %q", msg)
	}
	if !strings.Contains(msg, "nothing was executed") {
		t.Errorf("refusal should say nothing was executed, got %q", msg)
	}
	if n := len(fake.called("update.run")); n != 0 {
		t.Errorf("update.run called %d times after new_version moved, want 0", n)
	}
}

// Confirming an unchanged new_version still sends only {"reboot": ...} to
// update.run; the bound version is not added to the payload.
func TestApplyUpdateConfirmPayloadUnchanged(t *testing.T) {
	r, fake := applyUpdateRegistry(t, updateStatusWithVersion("25.10.1"))
	args := map[string]interface{}{"reboot": false}
	tok := previewToken(t, r, "apply_update", args)
	out, err := r.CallTool(t.Context(), "apply_update", map[string]interface{}{
		"reboot": false, "confirmation_token": tok,
	})
	if err != nil {
		t.Fatalf("confirm of unchanged new_version: %v\n%s", err, out)
	}
	calls := fake.called("update.run")
	if len(calls) != 1 {
		t.Fatalf("update.run calls = %d, want 1", len(calls))
	}
	want, _ := json.Marshal([]interface{}{map[string]interface{}{"reboot": false}})
	if !jsonBytesEqual(t, calls[0].Params, want) {
		t.Errorf("update.run params %s, want %s (payload must not include the bound new_version)", calls[0].Params, want)
	}
}

// When update.status has no new_version, apply_update is blocked: an
// explicit dry_run or a call without a token returns an error with
// status "blocked", issues no token, and does not call update.run.
func TestApplyUpdateNullNewVersionIsBlocked(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]interface{}
	}{
		{"missing new_version", updateStatusResult("NORMAL", map[string]interface{}{
			"current_version": map[string]interface{}{"train": "25.10", "profile": "GENERAL", "matches_profile": true},
		}, nil, nil)},
		{"null new_version", updateStatusAvailable("")},
		{"empty new_version object", updateStatusResult("NORMAL", updateStatusDetails(map[string]interface{}{}), nil, nil)},
		{"blank version", updateStatusResult("NORMAL", updateStatusDetails(updateStatusNewVersion("  ")), nil, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, extra := range []map[string]interface{}{{}, {"dry_run": true}} {
				r, fake := applyUpdateRegistry(t, tc.status)
				call := map[string]interface{}{"reboot": false}
				for k, v := range extra {
					call[k] = v
				}
				out, err := r.CallTool(t.Context(), "apply_update", call)
				if err == nil {
					t.Fatalf("%v returned no error:\n%s", extra, out)
				}
				var reply *mcp.ReplyError
				if !errors.As(err, &reply) {
					t.Fatalf("%v: want a ReplyError, got %T %v", extra, err, err)
				}
				if !strings.Contains(reply.Reply, `"status": "blocked"`) {
					t.Errorf("%v blocked preview lacks status blocked: %s", extra, reply.Reply)
				}
				if strings.Contains(reply.Reply, "confirmation_token") {
					t.Errorf("%v blocked preview issued a token: %s", extra, reply.Reply)
				}
				combined := reply.Message + " " + reply.Reply
				if !strings.Contains(strings.ToLower(combined), "new_version") && !strings.Contains(strings.ToLower(combined), "no update") {
					t.Errorf("%v blocked preview should say there is no update to apply: %s / %s", extra, reply.Message, reply.Reply)
				}
				if n := len(fake.called("update.run")); n != 0 {
					t.Errorf("%v sent update.run %d times", extra, n)
				}
			}
		})
	}
}

// An explicit dry_run (or a call without a token) of apply_update when
// update.status has no new_version is documented as a breaking change.
func TestREADMENotesApplyUpdateNullNewVersionIsBreakingChange(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, note := range readmeBreakingChangeNotes(string(data)) {
		if strings.Contains(note, "apply_update") && strings.Contains(note, "new_version") && strings.Contains(note, "blocked") {
			found = true
			break
		}
	}
	if !found {
		t.Error(`README.md must have a "> **Breaking change:**" note that apply_update with no new_version now returns an error result with status "blocked"`)
	}
}

// A confirmed system_reboot or apply_update re-reads system.info (and, for
// apply_update, update.status) and fails closed if those reads fail.
func TestREADMENotesRebootAndUpdateRereadBeforeWrite(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	hasInfo := strings.Contains(text, "system.info")
	hasStatus := strings.Contains(text, "update.status")
	hasFailClosed := strings.Contains(text, "fails closed") || strings.Contains(text, "fail closed")
	if !hasInfo || !hasStatus || !hasFailClosed {
		t.Error(`README.md must note that a confirmed reboot or update does read-only pre-reads (system.info and update.status) and fails closed if they fail`)
	}
}

// The real 25.10 update.status shape nests new_version under status.
// An available update at 25.10.2 must issue a token, and confirming it
// must call update.run with only {"reboot": ...}.
func TestApplyUpdateAvailableRealShapeIssuesTokenAndConfirms(t *testing.T) {
	r, fake := applyUpdateRegistry(t, updateStatusAvailable("25.10.2"))
	args := map[string]interface{}{"reboot": false}
	out, err := r.CallTool(t.Context(), "apply_update", args)
	if err != nil {
		t.Fatalf("preview of an available update (25.10.2) must succeed, got %v", err)
	}
	tok := replyToken(t, out)
	if tok == "" {
		t.Fatalf("available update at 25.10.2 must issue a token, got:\n%s", out)
	}
	if !strings.Contains(out, "25.10.2") {
		t.Errorf("preview should show the nested new_version 25.10.2:\n%s", out)
	}
	var wrap struct {
		Preview json.RawMessage `json:"preview"`
	}
	if err := json.Unmarshal([]byte(out), &wrap); err != nil {
		t.Fatal(err)
	}
	var preview struct {
		CurrentState   map[string]interface{}   `json:"current_state"`
		PlannedActions []map[string]interface{} `json:"planned_actions"`
	}
	if err := json.Unmarshal(wrap.Preview, &preview); err != nil {
		t.Fatalf("preview: %v\n%s", err, wrap.Preview)
	}
	status, _ := preview.CurrentState["update_status"].(map[string]interface{})
	inner, _ := status["status"].(map[string]interface{})
	nv, _ := inner["new_version"].(map[string]interface{})
	if v, _ := nv["version"].(string); v != "25.10.2" {
		t.Errorf("preview current_state.update_status.status.new_version.version = %v, want 25.10.2 (real 25.10 shape)", nv)
	}

	got, err := r.CallTool(t.Context(), "apply_update", map[string]interface{}{
		"reboot": false, "confirmation_token": tok,
	})
	if err != nil {
		t.Fatalf("confirm of available 25.10.2: %v\n%s", err, got)
	}
	calls := fake.called("update.run")
	if len(calls) != 1 {
		t.Fatalf("update.run calls = %d, want 1", len(calls))
	}
	want, _ := json.Marshal([]interface{}{map[string]interface{}{"reboot": false}})
	if !jsonBytesEqual(t, calls[0].Params, want) {
		t.Errorf("update.run params %s, want %s", calls[0].Params, want)
	}
}

// When update.status code is ERROR, or status is null, apply_update is
// blocked with a clear reason. Nothing is applied and no token is issued.
func TestApplyUpdateErrorOrNullStatusIsBlocked(t *testing.T) {
	cases := []struct {
		name    string
		status  map[string]interface{}
		wantSub string
	}{
		{"code ERROR", updateStatusError("ENOPKG", "No releases match specified update profile."), "ERROR"},
		{"null status", updateStatusResult("NORMAL", nil, nil, nil), "status is null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, extra := range []map[string]interface{}{{}, {"dry_run": true}} {
				r, fake := applyUpdateRegistry(t, tc.status)
				call := map[string]interface{}{"reboot": false}
				for k, v := range extra {
					call[k] = v
				}
				out, err := r.CallTool(t.Context(), "apply_update", call)
				if err == nil {
					t.Fatalf("%v returned no error:\n%s", extra, out)
				}
				var reply *mcp.ReplyError
				if !errors.As(err, &reply) {
					t.Fatalf("%v: want a ReplyError, got %T %v", extra, err, err)
				}
				if !strings.Contains(reply.Reply, `"status": "blocked"`) {
					t.Errorf("%v blocked preview lacks status blocked: %s", extra, reply.Reply)
				}
				if strings.Contains(reply.Reply, "confirmation_token") {
					t.Errorf("%v blocked preview issued a token: %s", extra, reply.Reply)
				}
				// The reason must be in the blocked message itself, not only
				// in the raw update.status object copied into current_state.
				if !strings.Contains(reply.Message, tc.wantSub) {
					t.Errorf("%v blocked message %q does not mention %q", extra, reply.Message, tc.wantSub)
				}
				if tc.name == "code ERROR" && !strings.Contains(reply.Message, "No releases match specified update profile.") {
					t.Errorf("%v blocked message should include the update.status error reason: %s", extra, reply.Message)
				}
				if n := len(fake.called("update.run")); n != 0 {
					t.Errorf("%v sent update.run %d times", extra, n)
				}
			}
		})
	}
}

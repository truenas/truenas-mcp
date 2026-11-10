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

func updateStatusWithVersion(version string) map[string]interface{} {
	st := map[string]interface{}{"status": "AVAILABLE"}
	if version == "" {
		st["new_version"] = nil
		return st
	}
	st["new_version"] = map[string]interface{}{"version": version}
	return st
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
		{"missing new_version", map[string]interface{}{"status": "AVAILABLE"}},
		{"null new_version", updateStatusWithVersion("")},
		{"empty new_version object", map[string]interface{}{"status": "AVAILABLE", "new_version": map[string]interface{}{}}},
		{"blank version", map[string]interface{}{"status": "AVAILABLE", "new_version": map[string]interface{}{"version": "  "}}},
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

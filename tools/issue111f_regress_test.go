package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/redact"
)

// A confirmation token for upgrade_app with version "latest" must bind the
// resolved upgrade_version from the preview. If a newer release is
// published between preview and confirm, the token is refused and nothing
// is upgraded. The app.upgrade payload still sends "latest".
func TestUpgradeAppLatestVersionChangeRefusesConfirm(t *testing.T) {
	r, _, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	args := map[string]interface{}{"app_name": "plex"}
	tok := previewToken(t, r, "upgrade_app", args)

	f.mu.Lock()
	next := upgradeSummaryFixture("x")
	next["upgrade_version"] = "9.9.9"
	next["upgrade_human_version"] = "9.9.9"
	next["latest_version"] = "9.9.9"
	next["latest_human_version"] = "9.9.9"
	f.results["app.upgrade_summary"] = next
	f.mu.Unlock()

	out, err := r.CallTool(t.Context(), "upgrade_app", map[string]interface{}{
		"app_name": "plex", "confirmation_token": tok,
	})
	if err == nil {
		t.Fatalf("confirm must be refused when latest moved from 1.2.0 to 9.9.9, got %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "different planned operation") {
		t.Errorf("want a plan-mismatch refusal, got %q", msg)
	}
	if !strings.Contains(msg, "nothing was executed") {
		t.Errorf("refusal should say nothing was executed, got %q", msg)
	}
	if n := len(f.called("app.upgrade")); n != 0 {
		t.Errorf("app.upgrade called %d times after latest moved, want 0", n)
	}
}

// Confirming an unchanged "latest" still sends the literal "latest" to
// app.upgrade, not the resolved catalog version.
func TestUpgradeAppLatestConfirmSendsLatestLiteral(t *testing.T) {
	r, _, f := newUpgradeRegistry(t, upgradeSummaryFixture("x"))
	args := map[string]interface{}{"app_name": "plex"}
	tok := previewToken(t, r, "upgrade_app", args)
	out, err := r.CallTool(t.Context(), "upgrade_app", map[string]interface{}{
		"app_name": "plex", "confirmation_token": tok,
	})
	if err != nil {
		t.Fatalf("confirm of unchanged latest: %v\n%s", err, out)
	}
	calls := f.called("app.upgrade")
	if len(calls) != 1 {
		t.Fatalf("app.upgrade calls = %d, want 1", len(calls))
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(calls[0].Params, &raw); err != nil || len(raw) != 2 {
		t.Fatalf("app.upgrade params = %s", calls[0].Params)
	}
	var opts map[string]interface{}
	if err := json.Unmarshal(raw[1], &opts); err != nil {
		t.Fatalf("options: %v", err)
	}
	if opts["app_version"] != "latest" {
		t.Errorf("app_version = %v, want the literal \"latest\" (payload must not be replaced with the resolved version)", opts["app_version"])
	}
}

// Custom preview Actions and CurrentState must be masked the same way as
// previewDetails. Server-side output redaction is not a substitute for
// the plan-level guarantee.
func TestWritePlanMasksCustomActionsAndCurrentState(t *testing.T) {
	p := &writePlan{
		CurrentState: map[string]interface{}{
			"name":     "plex",
			"password": "s3cret",
		},
		Actions: []PlannedAction{
			{
				Step:        1,
				Description: "Apply new configuration values to app",
				Operation:   "update",
				Target:      "app.update",
				Details: map[string]interface{}{
					"app_name": "plex",
					"new_values": map[string]interface{}{
						"password": "s3cret",
						"token":    "t0ken",
					},
				},
			},
		},
	}
	result, err := p.dryRunResult()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), "t0ken") {
		t.Errorf("preview leaked a secret: %s", raw)
	}
	state, _ := result.CurrentState.(map[string]interface{})
	if state["name"] != "plex" {
		t.Errorf("current_state.name = %v, want plex", state["name"])
	}
	if state["password"] != redact.Marker {
		t.Errorf("current_state.password = %v, want %s", state["password"], redact.Marker)
	}
	if len(result.PlannedActions) != 1 {
		t.Fatalf("planned_actions = %d, want 1", len(result.PlannedActions))
	}
	details, _ := result.PlannedActions[0].Details.(map[string]interface{})
	values, _ := details["new_values"].(map[string]interface{})
	if values["password"] != redact.Marker || values["token"] != redact.Marker {
		t.Errorf("action details not masked: %v", details)
	}
	// Masking must not mutate the plan used for the real call.
	origState, _ := p.CurrentState.(map[string]interface{})
	if origState["password"] != "s3cret" {
		t.Errorf("masking mutated CurrentState: %v", p.CurrentState)
	}
	origDetails, _ := p.Actions[0].Details.(map[string]interface{})
	origValues, _ := origDetails["new_values"].(map[string]interface{})
	if origValues["password"] != "s3cret" {
		t.Errorf("masking mutated Actions: %v", p.Actions[0].Details)
	}
}

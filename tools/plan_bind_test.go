package tools

import (
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/redact"
)

// A confirmation token bound to a previewed plan is refused when the
// rebuilt plan's method or params differ, and accepted when they match.
func TestConfirmationTokenBindsPlan(t *testing.T) {
	c := newConfirmationTokens()
	argsHash, err := confirmationArgsHash(map[string]interface{}{"app_name": "plex"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := confirmationPlanHash("app.start", []interface{}{"plex"})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := confirmationPlanHash("app.start", []interface{}{"other"})
	if err != nil {
		t.Fatal(err)
	}
	if preview == changed {
		t.Fatal("different params hashed the same")
	}

	tok, err := c.issueWithPlan("stop_app", argsHash, preview)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.redeemWithPlan(tok, "stop_app", argsHash, changed); err == nil {
		t.Fatal("expected a rejection when the planned params changed")
	} else if !strings.Contains(err.Error(), "different planned operation") || !strings.Contains(err.Error(), "nothing was executed") {
		t.Errorf("rejection should name the plan change, got %q", err)
	}

	tok, err = c.issueWithPlan("stop_app", argsHash, preview)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.redeemWithPlan(tok, "stop_app", argsHash, preview); err != nil {
		t.Fatalf("same plan should redeem: %v", err)
	}

	// A token issued without a plan is refused if redeem supplies one.
	tok, err = c.issue("stop_app", argsHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.redeemWithPlan(tok, "stop_app", argsHash, preview); err == nil {
		t.Fatal("expected a rejection when redeem adds a plan binding the issue lacked")
	}
}

func TestConfirmationPlanHashIsCanonical(t *testing.T) {
	a, err := confirmationPlanHash("app.start", []interface{}{map[string]interface{}{"b": 1.0, "a": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := confirmationPlanHash("app.start", []interface{}{map[string]interface{}{"a": "x", "b": 1.0}})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("object key order changed the plan hash")
	}
	c, err := confirmationPlanHash("app.stop", []interface{}{map[string]interface{}{"a": "x", "b": 1.0}})
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Error("different methods hashed the same")
	}
}

func TestPlanPreviewMasksNamedSecretsAndPositionalParams(t *testing.T) {
	named, ok := planPreviewDetails(map[string]interface{}{
		"app_name": "plex",
		"password": "s3cret",
	}, nil).(map[string]interface{})
	if !ok {
		t.Fatalf("named details should be a map, got %T", planPreviewDetails(map[string]interface{}{"app_name": "plex", "password": "s3cret"}, nil))
	}
	if named["app_name"] != "plex" {
		t.Errorf("app_name should stay visible, got %v", named["app_name"])
	}
	if named["password"] != redact.Marker {
		t.Errorf("password should be masked, got %v", named["password"])
	}

	positional, ok := planPreviewDetails(nil, []interface{}{"s3cret", 7}).([]interface{})
	if !ok {
		t.Fatalf("positional details should be a list, got %T", planPreviewDetails(nil, []interface{}{"s3cret", 7}))
	}
	if positional[0] != redact.Marker {
		t.Errorf("bare positional string should be masked, got %v", positional[0])
	}
	if positional[1] != 7 {
		t.Errorf("positional number should stay visible, got %v", positional[1])
	}
}

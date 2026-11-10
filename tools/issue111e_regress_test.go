package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// A length mismatch on planHash is wrapped into the destructive-path
// error, so the caller sees the real ParamNames reason instead of a
// generic "could not be encoded".
func TestDestructivePlanHashErrorIsWrapped(t *testing.T) {
	r, fake := stopAppRegistry(t, "RUNNING")
	tool := r.tools["stop_app"]
	orig := tool.Plan
	tool.Plan = func(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
		p, err := orig(ctx, client, args)
		if err != nil {
			return nil, err
		}
		p.ParamNames = append(append([]string{}, p.ParamNames...), "extra")
		return p, nil
	}
	r.tools["stop_app"] = tool

	out, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{"app_name": "plex"})
	if err == nil {
		t.Fatalf("expected a planHash error, got %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "was NOT executed") || !strings.Contains(msg, "nothing was executed") {
		t.Errorf("error should say the tool was NOT executed, got %q", msg)
	}
	if !strings.Contains(msg, "ParamNames") {
		t.Errorf("error should wrap the ParamNames length mismatch, got %q", msg)
	}
	if n := len(fake.called("app.stop")); n != 0 {
		t.Errorf("planHash failure still called app.stop %d times", n)
	}
}

// A plan-backed tool with no Execute must not issue a confirmation
// token. Checking only after redeem would spend the preview's token
// on the refusal.
func TestPlanWithoutExecuteIssuesNoToken(t *testing.T) {
	r, fake := stopAppRegistry(t, "RUNNING")
	tool := r.tools["stop_app"]
	tool.Execute = nil
	r.tools["stop_app"] = tool

	out, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{"app_name": "plex"})
	if err == nil {
		t.Fatalf("expected an error when Execute is missing, got %s", out)
	}
	msg := err.Error()
	if !strings.Contains(msg, "Execute") && !strings.Contains(msg, "execute") {
		t.Errorf("error should say Execute is missing, got %q", msg)
	}
	if !strings.Contains(msg, "was NOT executed") || !strings.Contains(msg, "nothing was executed") {
		t.Errorf("error should say the tool was not executed, got %q", msg)
	}
	body := out
	var re *mcp.ReplyError
	if errors.As(err, &re) {
		body = re.Reply
	}
	if strings.Contains(body, "confirmation_token") || strings.Contains(out, "confirmation_token") {
		t.Errorf("preview issued a token even though Execute is missing:\n%s", body)
	}
	if n := len(fake.called("app.stop")); n != 0 {
		t.Errorf("missing Execute still called app.stop %d times", n)
	}
}

// NewRegistry must refuse a destructive tool that has a Plan but no
// Execute, so a missing Execute cannot issue a token at all.
func TestRegistryRequiresExecuteWhenPlanIsSet(t *testing.T) {
	r := NewRegistry(nil, nil)
	tool := r.tools["stop_app"]
	tool.Execute = nil
	r.tools["stop_app"] = tool
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected a panic when a destructive Plan is registered without Execute")
		}
		text := ""
		switch v := rec.(type) {
		case string:
			text = v
		case error:
			text = v.Error()
		default:
			text = "non-string panic"
		}
		if !strings.Contains(text, "stop_app") || !strings.Contains(strings.ToLower(text), "execute") {
			t.Errorf("panic should name the tool and Execute, got %q", text)
		}
	}()
	r.checkPlanExecute()
}

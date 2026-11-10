package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

func formElicit() mcp.Elicitation {
	return mcp.Elicitation{SupportsForm: true}
}

func acceptElicit(state string) mcp.Elicitation {
	return mcp.Elicitation{
		SupportsForm: true,
		RequestState: state,
		HasResponse:  true,
		Action:       "accept",
		Approved:     true,
	}
}

func elicitReply(t *testing.T, r *Registry, name string, args map[string]interface{}, e mcp.Elicitation) (string, *mcp.ElicitPrompt, error) {
	t.Helper()
	return r.CallToolElicit(t.Context(), name, args, e)
}

// accept executes exactly once; a second accept of the same prompt does not.
func TestElicitationAcceptExecutesOnce(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}

	out, prompt, err := elicitReply(t, r, "stop_app", args, formElicit())
	if err != nil || out != "" || prompt == nil || prompt.Message == "" || prompt.State == "" {
		t.Fatalf("first call: out=%q prompt=%+v err=%v; want an elicitation prompt and no result", out, prompt, err)
	}
	if strings.Contains(prompt.Message, "confirmation_token") || strings.Contains(out, "confirmation_token") {
		t.Errorf("elicitation prompt must not carry a confirmation_token:\nmessage=%s\nout=%s", prompt.Message, out)
	}
	if !strings.Contains(prompt.Message, "stop_app") || !strings.Contains(prompt.Message, "plex") {
		t.Errorf("prompt message should name the tool and target:\n%s", prompt.Message)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Fatalf("stop_app ran %d times before accept, want 0", n)
	}

	out, prompt2, err := elicitReply(t, r, "stop_app", args, acceptElicit(prompt.State))
	if err != nil || prompt2 != nil || out == "" {
		t.Fatalf("accept: out=%q prompt=%+v err=%v; want a result and no prompt", out, prompt2, err)
	}
	if n := realCalls(seen["stop_app"]); n != 1 {
		t.Fatalf("stop_app ran %d times after accept, want 1", n)
	}

	_, prompt3, err := elicitReply(t, r, "stop_app", args, acceptElicit(prompt.State))
	if err == nil || prompt3 != nil || !strings.Contains(err.Error(), "nothing was executed") {
		t.Errorf("reused prompt: prompt=%+v err=%v; want a refusal", prompt3, err)
	}
	if n := realCalls(seen["stop_app"]); n != 1 {
		t.Errorf("stop_app ran %d times after a reused prompt, want 1", n)
	}
}

func TestElicitationDeclineCancelErrorExecuteNothing(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}

	for _, tc := range []struct {
		name string
		mod  func(mcp.Elicitation) mcp.Elicitation
		want string
	}{
		{"decline", func(e mcp.Elicitation) mcp.Elicitation { e.Action = "decline"; e.Approved = false; return e }, "declined"},
		{"cancel", func(e mcp.Elicitation) mcp.Elicitation { e.Action = "cancel"; e.Approved = false; return e }, "cancelled"},
		{"accept but not approved", func(e mcp.Elicitation) mcp.Elicitation { e.Approved = false; return e }, "not approved"},
		{"elicitation error", func(e mcp.Elicitation) mcp.Elicitation {
			e.Action = ""
			e.ResponseErr = errTokenRejected
			return e
		}, "nothing was executed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, prompt, err := elicitReply(t, r, "stop_app", args, formElicit())
			if err != nil || prompt == nil {
				t.Fatalf("preview: prompt=%+v err=%v", prompt, err)
			}
			e := tc.mod(acceptElicit(prompt.State))
			out, prompt2, err := elicitReply(t, r, "stop_app", args, e)
			if err == nil || prompt2 != nil || out != "" || !strings.Contains(err.Error(), "nothing was executed") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%v prompt=%+v out=%q; want an error containing %q and nothing was executed", err, prompt2, out, tc.want)
			}
			if n := realCalls(seen["stop_app"]); n != 0 {
				t.Errorf("stop_app ran %d times, want 0", n)
			}
		})
	}
}

func TestElicitationTimeoutExecutesNothing(t *testing.T) {
	r, seen, clock := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}

	_, prompt, err := elicitReply(t, r, "stop_app", args, formElicit())
	if err != nil || prompt == nil {
		t.Fatalf("preview: prompt=%+v err=%v", prompt, err)
	}
	clock.advance(confirmationTokenTTL)
	out, prompt2, err := elicitReply(t, r, "stop_app", args, acceptElicit(prompt.State))
	if err == nil || prompt2 != nil || out != "" || !strings.Contains(err.Error(), "nothing was executed") ||
		(!strings.Contains(err.Error(), "expired") && !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "timeout")) {
		t.Errorf("expired prompt: err=%v prompt=%+v out=%q", err, prompt2, out)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Errorf("stop_app ran %d times after a timed-out prompt, want 0", n)
	}
}

// A client that did not declare form elicitation keeps the token flow.
func TestElicitationFallbackToTokenWithoutCapability(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}

	out, prompt, err := elicitReply(t, r, "stop_app", args, mcp.Elicitation{})
	if err != nil || prompt != nil {
		t.Fatalf("no-capability call: prompt=%+v err=%v; want the token flow", prompt, err)
	}
	tok := replyToken(t, out)
	if tok == "" || !strings.Contains(out, `"status": "confirmation_required"`) {
		t.Fatalf("no-capability call: want a preview and a confirmation_token, got:\n%s", out)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Fatalf("stop_app ran %d times before the token, want 0", n)
	}
	if _, err := r.CallTool(t.Context(), "stop_app", tokenArgs(args, map[string]interface{}{"confirmation_token": tok})); err != nil {
		t.Fatalf("token execute: %v", err)
	}
	if n := realCalls(seen["stop_app"]); n != 1 {
		t.Errorf("stop_app ran %d times after the token, want 1", n)
	}
}

// --require-human-confirm refuses destructive tools on clients without
// elicitation instead of issuing a token.
func TestElicitationRequireHumanConfirmRefusesWithoutCapability(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	if err := r.SetPolicy(Policy{RequireHumanConfirm: true}); err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"app_name": "plex"}

	out, prompt, err := elicitReply(t, r, "stop_app", args, mcp.Elicitation{})
	if err == nil || prompt != nil || strings.Contains(out, "confirmation_token") ||
		!strings.Contains(err.Error(), "nothing was executed") ||
		!strings.Contains(err.Error(), "require-human-confirm") {
		t.Errorf("out=%q prompt=%+v err=%v; want a refusal naming --require-human-confirm", out, prompt, err)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Errorf("stop_app ran %d times, want 0", n)
	}

	// A client that does support elicitation still gets the prompt.
	out, prompt, err = elicitReply(t, r, "stop_app", args, formElicit())
	if err != nil || prompt == nil {
		t.Fatalf("with elicitation under the flag: prompt=%+v err=%v out=%q", prompt, err, out)
	}
}

// A valid confirmation_token is executed without elicitation (no double prompt).
func TestElicitationValidTokenSkipsPrompt(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}
	tok := previewToken(t, r, "stop_app", args)

	out, prompt, err := elicitReply(t, r, "stop_app", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}), formElicit())
	if err != nil || prompt != nil || out == "" {
		t.Fatalf("token + elicitation: out=%q prompt=%+v err=%v; want execute and no prompt", out, prompt, err)
	}
	if n := realCalls(seen["stop_app"]); n != 1 {
		t.Errorf("stop_app ran %d times, want 1", n)
	}
}

func TestElicitationMasksSecretsInPrompt(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	const secret = "s3cret-bind-pw"
	tool := r.tools["configure_directory_service"]
	tool.Plan = func(_ context.Context, _ *truenas.Client, args map[string]interface{}) (*writePlan, error) {
		return &writePlan{
			Tool:       "configure_directory_service",
			Method:     stubPlanMethod,
			CallParams: []interface{}{map[string]interface{}{"bindpw": secret, "enable": true}},
			ParamNames: []string{"payload"},
			TargetName: "LDAP",
			Warnings:   []string{"This will replace the LDAP bind credential."},
			CurrentState: map[string]interface{}{
				"bindpw": secret,
			},
		}, nil
	}
	tool.Execute = func(_ context.Context, _ *truenas.Client, args map[string]interface{}, _ *writePlan) (string, error) {
		seen["configure_directory_service"] = append(seen["configure_directory_service"], args)
		return `{"ok":true}`, nil
	}
	// Keep a dry-run handler that would leak if the message used raw args.
	tool.Handler = func(_ context.Context, _ *truenas.Client, args map[string]interface{}) (string, error) {
		seen["configure_directory_service"] = append(seen["configure_directory_service"], args)
		return `{"ok":true,"bindpw":"` + secret + `"}`, nil
	}
	r.tools["configure_directory_service"] = tool

	args := map[string]interface{}{
		"service_type": "LDAP",
		"enable":       true,
		"credential":   map[string]interface{}{"credential_type": "LDAP_PLAIN", "binddn": "cn=admin", "bindpw": secret},
	}
	_, prompt, err := elicitReply(t, r, "configure_directory_service", args, formElicit())
	if err != nil || prompt == nil {
		t.Fatalf("prompt=%+v err=%v", prompt, err)
	}
	if strings.Contains(prompt.Message, secret) {
		t.Errorf("elicitation message leaked the secret:\n%s", prompt.Message)
	}
	if !strings.Contains(prompt.Message, redact.Marker) && !strings.Contains(prompt.Message, "configure_directory_service") {
		t.Errorf("elicitation message looks empty or unmasked:\n%s", prompt.Message)
	}
	if n := realCalls(seen["configure_directory_service"]); n != 0 {
		t.Errorf("configure_directory_service ran %d times before accept, want 0", n)
	}
}

func TestElicitationReadOnlyUnchanged(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	_, prompt, err := elicitReply(t, r, "stop_app", map[string]interface{}{"app_name": "plex"}, formElicit())
	if err == nil || prompt != nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("prompt=%+v err=%v; want a read-only refusal", prompt, err)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Errorf("stop_app ran %d times, want 0", n)
	}

	// upgrade_app stays available for dry runs only, and issues no prompt
	// and no token.
	out, prompt, err := elicitReply(t, r, "upgrade_app", map[string]interface{}{"app_name": "plex", "dry_run": true}, formElicit())
	if err != nil || prompt != nil {
		t.Fatalf("upgrade_app dry_run: prompt=%+v err=%v", prompt, err)
	}
	if strings.Contains(out, "confirmation_token") {
		t.Errorf("read-only dry run issued a token:\n%s", out)
	}
}

func TestElicitationDryRunSkipsPromptAndToken(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	out, prompt, err := elicitReply(t, r, "stop_app", map[string]interface{}{"app_name": "plex", "dry_run": true}, formElicit())
	if err != nil || prompt != nil {
		t.Fatalf("dry_run: prompt=%+v err=%v", prompt, err)
	}
	if strings.Contains(out, "confirmation_token") {
		t.Errorf("dry_run issued a token:\n%s", out)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Errorf("stop_app ran %d real calls, want 0", n)
	}
}

// A real middleware write happens only after accept, and the same plan is used.
func TestElicitationAcceptSendsThePreviewedPlan(t *testing.T) {
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)
	args := map[string]interface{}{"delay": float64(30)}

	_, prompt, err := elicitReply(t, r, "system_reboot", args, formElicit())
	if err != nil || prompt == nil {
		t.Fatalf("prompt=%+v err=%v", prompt, err)
	}
	if n := len(fake.called("system.reboot")); n != 0 {
		t.Fatalf("system.reboot sent before accept: %d", n)
	}

	if _, prompt, err = elicitReply(t, r, "system_reboot", args, acceptElicit(prompt.State)); err != nil || prompt != nil {
		t.Fatalf("accept: prompt=%+v err=%v", prompt, err)
	}
	calls := fake.called("system.reboot")
	if len(calls) != 1 {
		t.Fatalf("system.reboot calls = %d, want 1", len(calls))
	}
}

func TestElicitationDeclineDoesNotFallBackToToken(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}
	_, prompt, err := elicitReply(t, r, "stop_app", args, formElicit())
	if err != nil || prompt == nil {
		t.Fatal(err)
	}
	e := acceptElicit(prompt.State)
	e.Action = "decline"
	e.Approved = false
	out, prompt2, err := elicitReply(t, r, "stop_app", args, e)
	if err == nil || prompt2 != nil || strings.Contains(out, "confirmation_token") || (err != nil && strings.Contains(err.Error(), "confirmation_token")) {
		t.Errorf("decline fell back to the token: out=%q prompt=%+v err=%v", out, prompt2, err)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Errorf("stop_app ran %d times, want 0", n)
	}
}

func TestElicitationBlockedPreviewIssuesNoPrompt(t *testing.T) {
	// delete_boot_environment on the running environment is blocked and
	// must not prompt or issue a token. The fake client used by
	// clockedRegistry's stub plan does not block, so use a real registry
	// only if we can stub a blocked plan.
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	tool := r.tools["delete_boot_environment"]
	tool.Plan = func(context.Context, *truenas.Client, map[string]interface{}) (*writePlan, error) {
		return &writePlan{
			Tool:        "delete_boot_environment",
			Method:      stubPlanMethod,
			CallParams:  []interface{}{"23.10"},
			ParamNames:  []string{"id"},
			TargetName:  "23.10",
			Blocked:     []string{"this is the running boot environment"},
			Description: "Delete boot environment 23.10",
		}, nil
	}
	r.tools["delete_boot_environment"] = tool

	out, prompt, err := elicitReply(t, r, "delete_boot_environment", map[string]interface{}{"id": "23.10"}, formElicit())
	if prompt != nil || err == nil || strings.Contains(out, "confirmation_token") {
		t.Fatalf("blocked preview: out=%q prompt=%+v err=%v; want a blocked error and no prompt", out, prompt, err)
	}
	if !strings.Contains(err.Error(), "nothing was executed") && !strings.Contains(err.Error(), "cannot proceed") && !strings.Contains(err.Error(), "running") {
		t.Errorf("blocked error = %v", err)
	}
	if n := realCalls(seen["delete_boot_environment"]); n != 0 {
		t.Errorf("delete_boot_environment ran %d times, want 0", n)
	}
}

func TestElicitationHandleIsNotAConfirmationToken(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}
	_, prompt, err := elicitReply(t, r, "stop_app", args, formElicit())
	if err != nil || prompt == nil {
		t.Fatalf("prompt=%+v err=%v", prompt, err)
	}
	if r.IsConfirmationToken(prompt.State) {
		t.Error("an elicitation handle must not be recognised as a confirmation_token")
	}
	_, err = r.CallTool(t.Context(), "stop_app", tokenArgs(args, map[string]interface{}{"confirmation_token": prompt.State}))
	if err == nil || !strings.Contains(err.Error(), "nothing was executed") {
		t.Errorf("using the handle as a confirmation_token: err=%v", err)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Errorf("stop_app ran %d times, want 0", n)
	}
}

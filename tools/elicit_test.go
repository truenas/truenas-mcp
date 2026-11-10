package tools

import (
	"context"
	"encoding/json"
	"errors"
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

// issuedToken returns a confirmation_token found in a successful reply or in
// a *mcp.ReplyError body, or "".
func issuedToken(out string, err error) string {
	tokenFrom := func(s string) string {
		var reply map[string]interface{}
		if json.Unmarshal([]byte(s), &reply) != nil {
			return ""
		}
		tok, _ := reply["confirmation_token"].(string)
		return tok
	}
	if tok := tokenFrom(out); tok != "" {
		return tok
	}
	var re *mcp.ReplyError
	if errors.As(err, &re) {
		return tokenFrom(re.Reply)
	}
	return ""
}

// neo's exact repro: with --require-human-confirm, stop_app called with
// confirmation_token "x" must not return a live token, and a follow-up
// redeem must not execute. Clients without elicitation get the flag error.
func TestRequireHumanConfirmBogusTokenDoesNotIssueOne(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	if err := r.SetPolicy(Policy{RequireHumanConfirm: true}); err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"app_name": "plex", "confirmation_token": "x"}

	out, prompt, err := elicitReply(t, r, "stop_app", args, mcp.Elicitation{})
	tok := issuedToken(out, err)
	if prompt != nil || tok != "" || err == nil ||
		!strings.Contains(err.Error(), "require-human-confirm") ||
		!strings.Contains(err.Error(), "nothing was executed") {
		t.Fatalf("bogus token with the flag on: out=%q prompt=%+v tok=%q err=%v; want the require-human-confirm error and no token", out, prompt, tok, err)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Fatalf("stop_app ran %d times after the bogus token, want 0", n)
	}

	// A real-looking token must also be refused, not redeemed, and must
	// not produce a fresh token.
	out, prompt, err = elicitReply(t, r, "stop_app", map[string]interface{}{
		"app_name": "plex", "confirmation_token": "0123456789abcdef0123456789abcdef",
	}, mcp.Elicitation{})
	tok = issuedToken(out, err)
	if prompt != nil || tok != "" || err == nil ||
		!strings.Contains(err.Error(), "require-human-confirm") ||
		!strings.Contains(err.Error(), "nothing was executed") {
		t.Errorf("real-looking token with the flag on: out=%q prompt=%+v tok=%q err=%v", out, prompt, tok, err)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Errorf("stop_app ran %d times, want 0", n)
	}
}

// The same bogus-token call, when the client declared elicitation, must
// prompt rather than issue a token or execute.
func TestRequireHumanConfirmBogusTokenGetsPrompt(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	if err := r.SetPolicy(Policy{RequireHumanConfirm: true}); err != nil {
		t.Fatal(err)
	}
	args := map[string]interface{}{"app_name": "plex", "confirmation_token": "x"}

	out, prompt, err := elicitReply(t, r, "stop_app", args, formElicit())
	tok := issuedToken(out, err)
	if err != nil || prompt == nil || tok != "" {
		t.Fatalf("bogus token with the flag and elicitation: out=%q prompt=%+v tok=%q err=%v; want a prompt and no token", out, prompt, tok, err)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Fatalf("stop_app ran %d times before accept, want 0", n)
	}

	out, prompt2, err := elicitReply(t, r, "stop_app", map[string]interface{}{"app_name": "plex"}, acceptElicit(prompt.State))
	if err != nil || prompt2 != nil || issuedToken(out, err) != "" {
		t.Fatalf("accept after a supplied token: out=%q prompt=%+v err=%v", out, prompt2, err)
	}
	if n := realCalls(seen["stop_app"]); n != 1 {
		t.Errorf("stop_app ran %d times after accept, want 1", n)
	}
}

// With the flag on, a token issued before the flag was set is not redeemed.
func TestRequireHumanConfirmDoesNotRedeemToken(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}
	tok := previewToken(t, r, "stop_app", args)
	if err := r.SetPolicy(Policy{RequireHumanConfirm: true}); err != nil {
		t.Fatal(err)
	}

	out, prompt, err := elicitReply(t, r, "stop_app", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}), mcp.Elicitation{})
	fresh := issuedToken(out, err)
	if prompt != nil || fresh != "" || err == nil || !strings.Contains(err.Error(), "require-human-confirm") {
		t.Errorf("redeem without elicitation: out=%q prompt=%+v fresh=%q err=%v; want the flag error and no new token", out, prompt, fresh, err)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Errorf("stop_app ran %d times without elicitation, want 0", n)
	}

	out, prompt, err = elicitReply(t, r, "stop_app", tokenArgs(args, map[string]interface{}{"confirmation_token": tok}), formElicit())
	fresh = issuedToken(out, err)
	if err != nil || prompt == nil || fresh != "" {
		t.Errorf("redeem with elicitation: out=%q prompt=%+v fresh=%q err=%v; want a prompt and no token", out, prompt, fresh, err)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Errorf("stop_app ran %d times before accept, want 0", n)
	}
}

// With the flag off, a rejected token on an elicitation-capable client
// re-prompts and does not mint a new token.
func TestRejectedTokenRePromptsWhenElicitationSupported(t *testing.T) {
	r, seen, _ := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex", "confirmation_token": "x"}

	out, prompt, err := elicitReply(t, r, "stop_app", args, formElicit())
	tok := issuedToken(out, err)
	if err != nil || prompt == nil || tok != "" {
		t.Fatalf("rejected token with elicitation: out=%q prompt=%+v tok=%q err=%v; want a prompt and no token", out, prompt, tok, err)
	}
	if n := realCalls(seen["stop_app"]); n != 0 {
		t.Fatalf("stop_app ran %d times before accept, want 0", n)
	}

	// The prompt can be accepted; a minted token is not needed and must
	// not have been issued.
	out, prompt2, err := elicitReply(t, r, "stop_app", map[string]interface{}{"app_name": "plex"}, acceptElicit(prompt.State))
	if err != nil || prompt2 != nil || issuedToken(out, err) != "" {
		t.Fatalf("accept after a rejected token: out=%q prompt=%+v err=%v", out, prompt2, err)
	}
	if n := realCalls(seen["stop_app"]); n != 1 {
		t.Errorf("stop_app ran %d times after accept, want 1", n)
	}
}

func TestElicitationRefusalsDoNotMentionConfirmationToken(t *testing.T) {
	r, _, clock := clockedRegistry(t)
	args := map[string]interface{}{"app_name": "plex"}
	_, prompt, err := elicitReply(t, r, "stop_app", args, formElicit())
	if err != nil || prompt == nil {
		t.Fatalf("preview: prompt=%+v err=%v", prompt, err)
	}
	clock.advance(confirmationTokenTTL)
	_, _, err = elicitReply(t, r, "stop_app", args, acceptElicit(prompt.State))
	if err == nil || strings.Contains(err.Error(), "confirmation token") {
		t.Errorf("expired prompt error = %v; must not mention a confirmation token", err)
	}

	_, prompt, err = elicitReply(t, r, "stop_app", args, formElicit())
	if err != nil || prompt == nil {
		t.Fatalf("second preview: %v", err)
	}
	e := acceptElicit(prompt.State)
	e.Action = "decline"
	e.Approved = false
	_, _, err = elicitReply(t, r, "stop_app", args, e)
	if err == nil || strings.Contains(err.Error(), "confirmation token") {
		t.Errorf("decline error = %v; must not mention a confirmation token", err)
	}
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
)

// ElicitConfirmID is the InputRequests map key for the confirmation prompt.
// The client must echo it in InputResponses.
const ElicitConfirmID = "confirm"

// ElicitApprovedField is the required boolean field of the elicitation schema.
// The call executes only on action "accept" with this field true.
const ElicitApprovedField = "approved"

type elicitSinkKey struct{}

// elicitSink carries one tools/call's elicitation state through CallTool.
// It is stored in the context so concurrent calls cannot share it.
type elicitSink struct {
	e      mcp.Elicitation
	prompt *mcp.ElicitPrompt
}

func elicitFrom(ctx context.Context) *elicitSink {
	s, _ := ctx.Value(elicitSinkKey{}).(*elicitSink)
	return s
}

// CallToolElicit runs a tool, using MCP elicitation as the primary
// confirmation path for destructive tools when e.SupportsForm is true.
// Callers of CallTool (no elicitation state) keep the token flow.
func (r *Registry) CallToolElicit(ctx context.Context, name string, args map[string]interface{}, e mcp.Elicitation) (string, *mcp.ElicitPrompt, error) {
	sink := &elicitSink{e: e}
	out, err := r.CallTool(context.WithValue(ctx, elicitSinkKey{}, sink), name, args)
	return out, sink.prompt, err
}

// startElicit runs the preview and, if the operation can proceed, returns
// an elicitation prompt bound to the same plan hash a token would use.
// Nothing is executed. A blocked or failed preview is an error and issues
// no prompt and no token.
func (r *Registry) startElicit(ctx context.Context, name string, tool Tool, args map[string]interface{}, argsHash, planHash [32]byte, plan *writePlan, sink *elicitSink, legacyConfirm bool, rejectedNote string) (string, error) {
	preview, err := r.destructivePreview(ctx, name, tool, args, rejectedNote)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if err := requireConfirmSummary(name, plan); err != nil {
		return "", err
	}
	tok, err := r.confirmTokens.issueElicit(name, argsHash, planHash)
	if err != nil {
		return "", fmt.Errorf("%s%s was NOT executed: %w; nothing was executed", rejectedNote, name, err)
	}
	sink.prompt = &mcp.ElicitPrompt{
		Message: elicitationMessage(name, preview, plan, args, legacyConfirm),
		State:   tok,
	}
	return "", nil
}

// completeElicit finishes a prompt. The pending handle is consumed in every
// case, so a declined, cancelled, failed or expired prompt cannot later
// execute, and cannot fall back to a confirmation token.
func (r *Registry) completeElicit(ctx context.Context, name string, tool Tool, args map[string]interface{}, argsHash, planHash [32]byte, plan *writePlan, sink *elicitSink) (string, error) {
	e := sink.e
	rejected := r.confirmTokens.redeemElicit(e.RequestState, name, argsHash, planHash)
	if rejected != nil {
		return "", fmt.Errorf("%s was NOT executed: %s; nothing was executed", name, rejected)
	}
	if e.ResponseErr != nil {
		return "", fmt.Errorf("%s was NOT executed: the confirmation prompt failed (%v); nothing was executed", name, e.ResponseErr)
	}
	switch {
	case !e.HasResponse:
		return "", fmt.Errorf("%s was NOT executed: the confirmation prompt was incomplete; nothing was executed", name)
	case e.Action == "decline":
		return "", fmt.Errorf("%s was NOT executed: the user declined the confirmation prompt; nothing was executed", name)
	case e.Action == "cancel":
		return "", fmt.Errorf("%s was NOT executed: the confirmation prompt was cancelled; nothing was executed", name)
	case e.Action == "accept" && !e.Approved:
		return "", fmt.Errorf("%s was NOT executed: the confirmation was not approved; nothing was executed", name)
	case e.Action != "accept":
		return "", fmt.Errorf("%s was NOT executed: the confirmation prompt failed (action %q); nothing was executed", name, e.Action)
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	callArgs := withoutKeys(args, "confirmation_token", "dry_run")
	if tool.Plan != nil {
		if tool.Execute == nil {
			return "", fmt.Errorf("%s was NOT executed: it has a plan but no Execute path; nothing was executed", name)
		}
		return tool.Execute(ctx, r.client, callArgs, plan)
	}
	return tool.Handler(ctx, r.client, callArgs)
}

func requireHumanConfirmError(name string) error {
	return fmt.Errorf("%s was NOT executed: this server was started with --require-human-confirm / TRUENAS_MCP_REQUIRE_HUMAN_CONFIRM, which requires a client that supports MCP form elicitation; nothing was executed", name)
}

// elicitationMessage is an already-redacted summary of what is being
// approved: the tool, target, ConfirmSummary headline and key facts, and
// main warnings. Facts come only from ConfirmSummary, never from
// Actions[0] or a parsed preview step. Values shown in facts have already
// been through redact.Value; the finished message is passed through
// redact.Text as a final backstop.
func elicitationMessage(name, preview string, plan *writePlan, args map[string]interface{}, legacyConfirm bool) string {
	var b strings.Builder
	if legacyConfirm {
		b.WriteString("confirm=true no longer executes destructive tools. ")
	}
	b.WriteString("Confirm destructive operation ")
	b.WriteString(name)
	b.WriteString(".\n")

	target, warnings := elicitationTargetAndWarnings(preview, plan, args)
	if target != "" {
		b.WriteString("\nTarget: ")
		b.WriteString(target)
		b.WriteByte('\n')
	}
	sum := (*ConfirmSummary)(nil)
	if plan != nil {
		sum = plan.ConfirmSummary.forDisplay()
	}
	if sum != nil && sum.Headline != "" {
		b.WriteString("\nThis will: ")
		b.WriteString(sum.Headline)
		b.WriteByte('\n')
		if len(sum.Facts) > 0 {
			b.WriteString("\nFacts:\n")
			for _, f := range sum.Facts {
				b.WriteString("- ")
				b.WriteString(f)
				b.WriteByte('\n')
			}
		}
	}
	if len(warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range warnings {
			b.WriteString("- ")
			b.WriteString(w)
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nNothing is executed unless you accept and set approved=true. The prompt is valid for 5 minutes. A decline, cancel, or expiry executes nothing.")
	return strings.TrimSpace(redact.Text(b.String()))
}

// elicitationIdentityKeys are argument names that identify the target of a
// destructive call and are safe to show in the elicitation message. Other
// argument values (passwords, keys, free-text parameters) are omitted.
var elicitationIdentityKeys = []string{"app_name", "id", "name", "service_type", "target"}

func elicitationTargetAndWarnings(preview string, plan *writePlan, args map[string]interface{}) (target string, warnings []string) {
	if plan != nil {
		target = strings.TrimSpace(plan.TargetName)
		warnings = append([]string{}, plan.Warnings...)
	}
	var parsed struct {
		Warnings []string `json:"warnings"`
	}
	if json.Unmarshal([]byte(preview), &parsed) == nil && len(warnings) == 0 {
		warnings = parsed.Warnings
	}
	if target == "" {
		target = identityTarget(args)
	}
	if target == "" && plan != nil {
		target = identityTargetFromPlan(plan)
	}
	return target, warnings
}

func identityTarget(args map[string]interface{}) string {
	for _, k := range elicitationIdentityKeys {
		if s, ok := args[k].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

func identityTargetFromPlan(plan *writePlan) string {
	if plan == nil {
		return ""
	}
	for _, p := range plan.CallParams {
		if m, ok := p.(map[string]interface{}); ok {
			if t := identityTarget(m); t != "" {
				return t
			}
		}
	}
	return ""
}

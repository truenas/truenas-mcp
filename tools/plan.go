package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

// writePlan is the shared plan for a write tool. The dry-run preview is
// this plan, and the real call executes the same plan, so validation
// cannot diverge. Later write-tool migrations should produce one of these.
type writePlan struct {
	Tool        string
	Method      string
	CallParams  []interface{}
	JobMethod   string // expected job method for task identity
	Description string
	Operation   string
	// PreviewDetails are named fields shown in the preview (masked with
	// redact.Value). The real call uses CallParams, which may be positional.
	PreviewDetails map[string]interface{}
	CurrentState   interface{}
	Warnings       []string
	Blocked        []string
	Requirements   *Requirements
	EstimatedTime  *EstimatedTime
	// TargetName is an optional label used in execute responses (for
	// example the app name for start_app).
	TargetName string
}

func (p *writePlan) dryRunResult() *DryRunResult {
	dry, executed := true, false
	return &DryRunResult{
		Tool:         p.Tool,
		CurrentState: p.CurrentState,
		PlannedActions: []PlannedAction{
			{
				Step:        1,
				Description: p.Description,
				Operation:   p.Operation,
				Target:      p.Method,
				Details:     planPreviewDetails(p.PreviewDetails, p.CallParams),
			},
		},
		Warnings:      p.Warnings,
		Blocked:       p.Blocked,
		Requirements:  p.Requirements,
		EstimatedTime: p.EstimatedTime,
		DryRun:        &dry,
		Executed:      &executed,
	}
}

// blockedReply is nil when the plan can proceed. Otherwise it is an error
// whose text is exactly the blocked reasons, and whose JSON reply is the
// preview that lists those same reasons.
func (p *writePlan) blockedReply() error {
	if p == nil || len(p.Blocked) == 0 {
		return nil
	}
	msg := strings.Join(p.Blocked, "; ")
	body, err := marshalJSON(p.dryRunResult())
	if err != nil {
		return fmt.Errorf("%s", msg)
	}
	return &mcp.ReplyError{Message: msg, Reply: body}
}

// planPreviewDetails is the preview view of a plan's parameters. Named
// fields are masked with redact.Value, so secrets under keys such as
// password are hidden. Positional values have no key for the redactor to
// match, so a non-empty string is replaced with redact.Marker; maps and
// nested slices are walked the same way.
func planPreviewDetails(named map[string]interface{}, positional []interface{}) interface{} {
	if named != nil {
		return redact.Value(named)
	}
	return maskPositionalParams(positional)
}

func maskPositionalParams(params []interface{}) []interface{} {
	out := make([]interface{}, len(params))
	for i, p := range params {
		switch v := p.(type) {
		case map[string]interface{}:
			out[i] = redact.Value(v)
		case []interface{}:
			out[i] = maskPositionalParams(v)
		case string:
			if v != "" {
				out[i] = redact.Marker
			} else {
				out[i] = v
			}
		default:
			out[i] = v
		}
	}
	return out
}

// callPlan runs the plan's middleware method with its params. It is a
// method dispatcher, so the contract scanner allows the non-literal
// plan.Method argument.
func callPlan(ctx context.Context, client *truenas.Client, plan *writePlan) (json.RawMessage, error) {
	return client.Call(ctx, plan.Method, plan.CallParams...)
}

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
	// ParamNames, when set, names each positional CallParams entry in the
	// preview (masked with redact.Value). A single map argument is already
	// named and is shown as that map. Either way the preview is derived
	// from CallParams, so the two cannot drift.
	ParamNames    []string
	CurrentState  interface{}
	Warnings      []string
	Blocked       []string
	Requirements  *Requirements
	EstimatedTime *EstimatedTime
	// TargetName is an optional label used in execute responses (for
	// example the app name for start_app).
	TargetName string
	// Actions, when non-nil, is the preview's planned_actions. Use this
	// when the preview describes several steps that a single middleware
	// call will perform (for example upgrade_app). The real call is still
	// Method and CallParams; paramNamesOK still applies.
	Actions []PlannedAction
	// Result is execute-only data produced while planning. It is not
	// shown in the preview.
	Result map[string]interface{}
}

func (p *writePlan) dryRunResult() (*DryRunResult, error) {
	actions, err := p.plannedActions()
	if err != nil {
		return nil, err
	}
	dry, executed := true, false
	result := &DryRunResult{
		Tool:           p.Tool,
		CurrentState:   p.CurrentState,
		PlannedActions: actions,
		Warnings:       p.Warnings,
		Blocked:        p.Blocked,
		Requirements:   p.Requirements,
		EstimatedTime:  p.EstimatedTime,
		DryRun:         &dry,
		Executed:       &executed,
	}
	if len(p.Blocked) > 0 {
		result.Status = "blocked"
	}
	return result, nil
}

func (p *writePlan) marshalDryRun() (string, error) {
	result, err := p.dryRunResult()
	if err != nil {
		return "", err
	}
	return marshalJSON(result)
}

// blockedReply is nil when the plan can proceed. Otherwise it is an error
// whose text is exactly the blocked reasons, and whose JSON reply is the
// preview that lists those same reasons.
func (p *writePlan) blockedReply() error {
	if p == nil || len(p.Blocked) == 0 {
		return nil
	}
	msg := strings.Join(p.Blocked, "; ")
	body, err := p.marshalDryRun()
	if err != nil {
		return fmt.Errorf("%s", msg)
	}
	return &mcp.ReplyError{Message: msg, Reply: body}
}

func (p *writePlan) plannedActions() ([]PlannedAction, error) {
	if p == nil || len(p.Blocked) > 0 {
		return []PlannedAction{}, p.paramNamesOK()
	}
	if err := p.paramNamesOK(); err != nil {
		return nil, err
	}
	if p.Actions != nil {
		return p.Actions, nil
	}
	details, err := p.previewDetails()
	if err != nil {
		return nil, err
	}
	return []PlannedAction{
		{
			Step:        1,
			Description: p.Description,
			Operation:   p.Operation,
			Target:      p.Method,
			Details:     details,
		},
	}, nil
}

// previewDetails is the preview view of the plan's CallParams. Named
// fields come from ParamNames (or a single map argument) and are masked
// with redact.Value. Positional values have no key for the redactor to
// match, so a non-empty string is replaced with redact.Marker.
// paramNamesOK is nil when ParamNames is unused or has one name per
// CallParams entry. A length mismatch must not be previewed: the old
// param_N fallback skipped positional redaction and could show a secret.
func (p *writePlan) paramNamesOK() error {
	if p == nil || len(p.ParamNames) == 0 {
		return nil
	}
	if len(p.ParamNames) != len(p.CallParams) {
		return fmt.Errorf("ParamNames has %d entries, CallParams has %d", len(p.ParamNames), len(p.CallParams))
	}
	return nil
}

func (p *writePlan) previewDetails() (interface{}, error) {
	if p == nil {
		return nil, nil
	}
	if err := p.paramNamesOK(); err != nil {
		return nil, err
	}
	if len(p.ParamNames) > 0 {
		named := make(map[string]interface{}, len(p.ParamNames))
		for i, name := range p.ParamNames {
			named[name] = p.CallParams[i]
		}
		return planPreviewDetails(named, nil), nil
	}
	if len(p.CallParams) == 1 {
		if m, ok := p.CallParams[0].(map[string]interface{}); ok {
			return planPreviewDetails(m, nil), nil
		}
	}
	return planPreviewDetails(nil, p.CallParams), nil
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

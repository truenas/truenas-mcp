package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tools"
)

// elicitReg is a registry that records the elicitation state the server
// passed in and implements the confirmation paths used by the stdio tests.
type elicitReg struct {
	last    mcp.Elicitation
	writes  int
	require bool
}

func (r *elicitReg) ListTools() []mcp.Tool {
	return []mcp.Tool{{
		Name:        "stop_app",
		Description: "Stop an app",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"app_name":           map[string]interface{}{"type": "string"},
				"confirmation_token": map[string]interface{}{"type": "string"},
			},
		},
		Annotations: &mcp.ToolAnnotations{DestructiveHint: true},
	}}
}

func (r *elicitReg) CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	out, _, err := r.CallToolElicit(ctx, name, args, mcp.Elicitation{})
	return out, err
}

func (r *elicitReg) CallToolElicit(_ context.Context, name string, args map[string]interface{}, e mcp.Elicitation) (string, *mcp.ElicitPrompt, error) {
	r.last = e
	if name != "stop_app" {
		return "", nil, fmt.Errorf("unknown tool: %s", name)
	}
	if !e.SupportsForm {
		if r.require {
			return "", nil, fmt.Errorf("%s was NOT executed: this server was started with --require-human-confirm / TRUENAS_MCP_REQUIRE_HUMAN_CONFIRM, which requires a client that supports MCP form elicitation; nothing was executed", name)
		}
		return "{\n  \"confirmation_token\": \"issued-token\",\n  \"status\": \"confirmation_required\"\n}", nil, nil
	}
	if e.HasResponse || e.RequestState != "" {
		if e.ResponseErr != nil || e.Action == "decline" || e.Action == "cancel" || (e.Action == "accept" && !e.Approved) || e.Action != "accept" {
			return "", nil, fmt.Errorf("%s was NOT executed: the confirmation prompt was not accepted; nothing was executed", name)
		}
		if e.RequestState != "elicit-state-1" {
			return "", nil, fmt.Errorf("%s was NOT executed: unexpected prompt state; nothing was executed", name)
		}
		r.writes++
		return `{"executed": true, "app_name": "plex"}`, nil, nil
	}
	return "", &mcp.ElicitPrompt{
		Message: "Confirm destructive operation stop_app.\n\nTarget: plex\n\nNothing is executed unless you accept and set approved=true.",
		State:   "elicit-state-1",
	}, nil
}

func startElicitServer(t *testing.T, reg *elicitReg) (inW *io.PipeWriter, outR *io.PipeReader, done chan error) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := newStdioHandler(reg, false, inR, outW)
	done = make(chan error, 1)
	go func() {
		done <- h.Run(context.Background())
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	return inW, outR, done
}

func connectElicitClient(t *testing.T, ctx context.Context, inW *io.PipeWriter, outR *io.PipeReader, handler func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error), proto string) *sdk.ClientSession {
	t.Helper()
	opts := &sdk.ClientOptions{}
	if handler != nil {
		opts.ElicitationHandler = handler
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1.0"}, opts)
	sessOpts := &sdk.ClientSessionOptions{}
	if proto != "" {
		sessOpts.ProtocolVersion = proto
	}
	cs, err := client.Connect(ctx, &sdk.IOTransport{Reader: outR, Writer: inW}, sessOpts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestElicitationAcceptAndDeclineOverStdio(t *testing.T) {
	versions := sdk.SupportedProtocolVersions()
	if len(versions) == 0 {
		t.Fatal("SDK lists no protocol versions")
	}
	for _, proto := range versions {
		if proto < "2025-06-18" {
			continue
		}
		t.Run(proto+"/accept", func(t *testing.T) {
			reg := &elicitReg{}
			inW, outR, done := startElicitServer(t, reg)
			ctx := t.Context()
			var messages []string
			cs := connectElicitClient(t, ctx, inW, outR, func(_ context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
				messages = append(messages, req.Params.Message)
				return &sdk.ElicitResult{Action: "accept", Content: map[string]any{toolsApproved(): true}}, nil
			}, proto)
			res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "stop_app", Arguments: map[string]interface{}{"app_name": "plex"}})
			if err != nil || res.IsError {
				t.Fatalf("CallTool = %+v, %v", res, err)
			}
			text, _ := toolResultText(t, res)
			if !strings.Contains(text, `"executed": true`) {
				t.Errorf("result = %s; want executed", text)
			}
			if reg.writes != 1 {
				t.Errorf("writes = %d, want 1", reg.writes)
			}
			if !reg.last.SupportsForm || len(messages) != 1 || !strings.Contains(messages[0], "stop_app") || !strings.Contains(messages[0], "plex") {
				t.Errorf("elicitation was not used: supports=%v messages=%q last=%+v", reg.last.SupportsForm, messages, reg.last)
			}
			if err := cs.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			inW.Close()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
		t.Run(proto+"/decline", func(t *testing.T) {
			reg := &elicitReg{}
			inW, outR, done := startElicitServer(t, reg)
			ctx := t.Context()
			cs := connectElicitClient(t, ctx, inW, outR, func(_ context.Context, _ *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
				return &sdk.ElicitResult{Action: "decline"}, nil
			}, proto)
			res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "stop_app", Arguments: map[string]interface{}{"app_name": "plex"}})
			if err != nil {
				t.Fatalf("CallTool err = %v", err)
			}
			text, isErr := toolResultText(t, res)
			if !isErr || !strings.Contains(text, "nothing was executed") {
				t.Errorf("decline result = %q isError=%v; want an error saying nothing was executed", text, isErr)
			}
			if reg.writes != 0 {
				t.Errorf("writes = %d, want 0", reg.writes)
			}
			if err := cs.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			inW.Close()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
}

func toolsApproved() string { return "approved" }

func TestElicitationFallbackWithoutCapabilityOverStdio(t *testing.T) {
	reg := &elicitReg{}
	inW, outR, done := startElicitServer(t, reg)
	ctx := t.Context()
	cs := connectElicitClient(t, ctx, inW, outR, nil, "2025-11-25")
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "stop_app", Arguments: map[string]interface{}{"app_name": "plex"}})
	if err != nil || res.IsError {
		t.Fatalf("CallTool = %+v, %v", res, err)
	}
	text, _ := toolResultText(t, res)
	if !strings.Contains(text, "confirmation_token") || !strings.Contains(text, "confirmation_required") {
		t.Errorf("without elicitation, want the token flow, got %s", text)
	}
	if reg.last.SupportsForm || reg.writes != 0 {
		t.Errorf("supports=%v writes=%d; want token fallback and no write", reg.last.SupportsForm, reg.writes)
	}
	if err := cs.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestElicitationRequireHumanConfirmOverStdio(t *testing.T) {
	reg := &elicitReg{require: true}
	inW, outR, done := startElicitServer(t, reg)
	ctx := t.Context()
	cs := connectElicitClient(t, ctx, inW, outR, nil, "2025-11-25")
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "stop_app", Arguments: map[string]interface{}{"app_name": "plex"}})
	if err != nil {
		t.Fatalf("CallTool err = %v", err)
	}
	text, isErr := toolResultText(t, res)
	if !isErr || !strings.Contains(text, "require-human-confirm") || !strings.Contains(text, "nothing was executed") {
		t.Errorf("result = %q isError=%v; want a refusal naming the flag", text, isErr)
	}
	if reg.writes != 0 {
		t.Errorf("writes = %d, want 0", reg.writes)
	}
	if err := cs.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestElicitationRejectsExtraResponseFields(t *testing.T) {
	versions := sdk.SupportedProtocolVersions()
	if len(versions) == 0 || versions[0] < "2026-07-28" {
		t.Fatal("SDK newest version should be 2026-07-28 or later")
	}
	proto := versions[0]
	if proto < "2026-07-28" {
		t.Fatalf("newest protocol is %s, want 2026-07-28+", proto)
	}

	reg := &elicitReg{}
	inW, outR, done := startElicitServer(t, reg)
	ctx := t.Context()
	cs := connectElicitClient(t, ctx, inW, outR, func(_ context.Context, _ *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		return &sdk.ElicitResult{Action: "accept", Content: map[string]any{
			toolsApproved(): true,
			"extra":         "ignored-or-rejected",
		}}, nil
	}, proto)
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "stop_app", Arguments: map[string]interface{}{"app_name": "plex"}})
	if err != nil {
		// The SDK may reject extra fields against additionalProperties:false
		// before the tool handler runs. That is also a closed refusal.
		if !strings.Contains(err.Error(), "additional properties") && !strings.Contains(err.Error(), "schema") &&
			!strings.Contains(err.Error(), "nothing was executed") {
			t.Fatalf("CallTool err = %v; want a refusal of extra fields", err)
		}
	} else {
		text, isErr := toolResultText(t, res)
		if !isErr || !strings.Contains(text, "nothing was executed") {
			t.Errorf("extra content fields: result = %q isError=%v; want an error saying nothing was executed", text, isErr)
		}
	}
	if reg.writes != 0 {
		t.Errorf("writes = %d, want 0", reg.writes)
	}
	if err := cs.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestApprovedOnlyRejectsExtraFields(t *testing.T) {
	if _, err := approvedOnly(map[string]any{"approved": true, "extra": "nope"}); err == nil {
		t.Error("extra field accepted")
	}
	if _, err := approvedOnly(map[string]any{"approved": true, "approved2": false}); err == nil {
		t.Error("second field accepted")
	}
	if _, err := approvedOnly(map[string]any{"approved": "true"}); err == nil {
		t.Error("string approved accepted")
	}
	if _, err := approvedOnly(nil); err == nil {
		t.Error("nil content accepted")
	}
	if _, err := approvedOnly(map[string]any{}); err == nil {
		t.Error("empty content accepted")
	}
	ok, err := approvedOnly(map[string]any{"approved": true})
	if err != nil || !ok {
		t.Errorf("approved true: ok=%v err=%v", ok, err)
	}
	ok, err = approvedOnly(map[string]any{"approved": false})
	if err != nil || ok {
		t.Errorf("approved false: ok=%v err=%v", ok, err)
	}
}

// MCP elicitation requestedSchema is a restricted JSON Schema object. The
// specification allows only these top-level keys; a strict client may reject
// a prompt that carries any other key (for example additionalProperties).
var elicitRequestedSchemaKeys = map[string]bool{
	"type":       true,
	"properties": true,
	"required":   true,
}

func TestAdvertisedElicitSchemaHasOnlySpecKeys(t *testing.T) {
	res := elicitCallResult(&mcp.ElicitPrompt{Message: "confirm this", State: "s"})
	req, ok := res.InputRequests[tools.ElicitConfirmID].(*sdk.ElicitParams)
	if !ok || req == nil {
		t.Fatal("elicitCallResult did not advertise a confirm input request")
	}
	schema, ok := req.RequestedSchema.(map[string]interface{})
	if !ok || schema == nil {
		t.Fatalf("requestedSchema type %T, want map[string]interface{}", req.RequestedSchema)
	}
	for k := range schema {
		if !elicitRequestedSchemaKeys[k] {
			t.Errorf("requestedSchema has %q, which is not part of the MCP elicitation requestedSchema shape", k)
		}
	}
	if schema["type"] != "object" {
		t.Errorf("requestedSchema type = %v, want object", schema["type"])
	}
	if _, ok := schema["properties"]; !ok {
		t.Error("requestedSchema is missing properties")
	}
	if _, ok := schema["required"]; !ok {
		t.Error("requestedSchema is missing required")
	}
}

// protocol20260728Meta is the per-request _meta a 2026-07-28 client sends
// instead of the initialize handshake (SEP-2575).
func protocol20260728Meta() map[string]interface{} {
	return map[string]interface{}{
		"io.modelcontextprotocol/protocolVersion": "2026-07-28",
		"io.modelcontextprotocol/clientInfo":      map[string]interface{}{"name": "test-client", "version": "1.0"},
		"io.modelcontextprotocol/clientCapabilities": map[string]interface{}{
			"elicitation": map[string]interface{}{"form": map[string]interface{}{}},
		},
	}
}

func TestElicitationRejectsExtraFieldsViaRawInputResponses(t *testing.T) {
	// A raw 2026-07-28 tools/call with inputResponses bypasses the go-sdk
	// client's schema check, so this covers approvedOnly end to end.
	reg := &elicitReg{}
	c, run := session(t, newTestHandler(reg, false))
	first := c.call(float64(1), "tools/call", map[string]interface{}{
		"_meta":     protocol20260728Meta(),
		"name":      "stop_app",
		"arguments": map[string]interface{}{"app_name": "plex"},
	})
	result, _ := first["result"].(map[string]interface{})
	state, _ := result["requestState"].(string)
	if state == "" {
		t.Fatalf("first tools/call = %v; want inputRequests and requestState", first)
	}
	reqs, _ := result["inputRequests"].(map[string]interface{})
	if _, ok := reqs[tools.ElicitConfirmID]; !ok {
		t.Fatalf("first tools/call missing confirm input request: %v", first)
	}

	second := c.call(float64(2), "tools/call", map[string]interface{}{
		"_meta":        protocol20260728Meta(),
		"name":         "stop_app",
		"arguments":    map[string]interface{}{"app_name": "plex"},
		"requestState": state,
		"inputResponses": map[string]interface{}{
			tools.ElicitConfirmID: map[string]interface{}{
				"action": "accept",
				"content": map[string]interface{}{
					"approved": true,
					"extra":    "should-be-rejected",
				},
			},
		},
	})
	text, isErr := toolText(t, second)
	if !isErr || !strings.Contains(text, "nothing was executed") {
		t.Errorf("raw extra fields: result = %q isError=%v; want an error saying nothing was executed", text, isErr)
	}
	if reg.writes != 0 {
		t.Errorf("writes = %d, want 0", reg.writes)
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

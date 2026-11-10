package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/truenas/truenas-mcp/mcp"
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

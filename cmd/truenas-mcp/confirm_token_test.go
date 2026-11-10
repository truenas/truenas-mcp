package main

import (
	"context"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
)

const (
	liveToken    = "0123456789abcdef0123456789abcdef"
	spoofedToken = "fedcba9876543210fedcba9876543210"
)

// tokenRegistry answers every call with a confirmation reply that carries
// token, plus a secret in the preview. Only liveToken counts as issued.
type tokenRegistry struct{ token string }

func (r tokenRegistry) ListTools() []mcp.Tool {
	return []mcp.Tool{{Name: "query_directory_services", Description: "fake", InputSchema: map[string]interface{}{"type": "object"}}}
}

func (r tokenRegistry) CallTool(context.Context, string, map[string]interface{}) (string, error) {
	return "{\n  \"confirmation_token\": \"" + r.token + "\",\n  \"preview\": {\n    \"bindpw\": \"fake-bind-pw\",\n    \"confirmation_token\": \"" + r.token + "\"\n  },\n  \"status\": \"confirmation_required\"\n}", nil
}

func (r tokenRegistry) IsConfirmationToken(tok string) bool { return tok == liveToken }

// The confirmation token reaches the client (redaction would otherwise hide
// a value under a key containing "token"), while everything else is still
// redacted, and the token never appears in the debug logs.
func TestConfirmationTokenReachesClientButNotLogs(t *testing.T) {
	logs := captureDebugLogs(t)
	c, run := session(t, newTestHandler(tokenRegistry{token: liveToken}, true))
	c.initialize()
	text, isError := toolText(t, c.call(float64(1), "tools/call", toolArgs("query_directory_services", nil)))
	// The next call presents the token: [STDIN] must hide it.
	c.call(float64(2), "tools/call", toolArgs("query_directory_services", map[string]interface{}{"app_name": "plex", "confirmation_token": liveToken}))
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if isError || !strings.Contains(text, `"confirmation_token": "`+liveToken+`"`) {
		t.Errorf("the client did not get the top-level confirmation token:\n%s", text)
	}
	if strings.Contains(text, "fake-bind-pw") || strings.Count(text, liveToken) != 1 {
		t.Errorf("the rest of the reply was not redacted (only the top-level token may pass):\n%s", text)
	}
	assertDebugHidesSecret(t, logs.String(), liveToken, "[STDIN]", "[STDOUT]")
}

// A confirmation_token the registry did not issue (for example one that a
// TrueNAS result happens to contain) is redacted as before.
func TestUnissuedConfirmationTokenIsRedacted(t *testing.T) {
	text, _ := callText(t, tokenRegistry{token: spoofedToken})
	if strings.Contains(text, spoofedToken) || !strings.Contains(text, redact.Marker) {
		t.Errorf("an unissued token was not redacted:\n%s", text)
	}
}

// replyErrorRegistry refuses a call with a *mcp.ReplyError whose reply
// carries a fresh token, as a rejected or blocked destructive call does.
type replyErrorRegistry struct{ tokenRegistry }

func (r replyErrorRegistry) CallTool(context.Context, string, map[string]interface{}) (string, error) {
	return "", &mcp.ReplyError{
		Message: "delete_app: confirmation token rejected: it was already used; nothing was executed",
		Reply:   `{"confirmation_token":"` + liveToken + `","error":"confirmation token rejected: it was already used; nothing was executed","preview":{"bindpw":"fake-bind-pw"},"status":"confirmation_required"}`,
	}
}

// A ReplyError is an error result whose content is the JSON reply, with the
// fresh token visible and everything else redacted.
func TestReplyErrorCarriesTokenAsErrorResult(t *testing.T) {
	text, isError := callText(t, replyErrorRegistry{})
	if !isError || !strings.Contains(text, `"confirmation_token":"`+liveToken+`"`) || !strings.Contains(text, "already used") {
		t.Errorf("reply error result (isError=%v):\n%s", isError, text)
	}
	if strings.Contains(text, "fake-bind-pw") {
		t.Errorf("reply error result not redacted:\n%s", text)
	}
}

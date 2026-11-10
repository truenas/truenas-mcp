package main

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/truenas/truenas-mcp/redact"
)

// wantToolOutputCap is the global cap on tool results and errors sent to
// the MCP client (100 KiB). It is written as a literal so this test
// compiles against main before the cap exists.
const wantToolOutputCap = 100 * 1024

func toolResultText(t *testing.T, res *sdk.CallToolResult) (string, bool) {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("result = %+v", res)
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("content = %T", res.Content[0])
	}
	return tc.Text, res.IsError
}

func TestToolResultLeavesSmallOutputUncut(t *testing.T) {
	const body = "pool: tank\nstatus: ONLINE"
	text, isError := toolResultText(t, toolResult(body, nil))
	if isError || text != body {
		t.Fatalf("small result = %q, isError=%v", text, isError)
	}
}

func TestToolResultCapsOversizedOutputWithNotice(t *testing.T) {
	body := strings.Repeat("a", wantToolOutputCap+4096)
	text, isError := toolResultText(t, toolResult(body, nil))
	if isError {
		t.Fatal("oversized result was returned as an error")
	}
	if len(text) > wantToolOutputCap {
		t.Errorf("capped result is %d bytes, want at most %d", len(text), wantToolOutputCap)
	}
	if !strings.Contains(text, "truncated") {
		t.Errorf("capped result has no truncation notice:\n%.400s", text)
	}
	for _, hint := range []string{"limit", "select", "filters"} {
		if !strings.Contains(strings.ToLower(text), hint) {
			t.Errorf("truncation notice does not tell the model to use %s:\n%.400s", hint, text)
		}
	}
	if !utf8.ValidString(text) {
		t.Error("capped result is not valid UTF-8")
	}
	if len(text) >= len(body) || strings.HasSuffix(text, body[len(body)-32:]) {
		t.Error("capped result still contains the full body")
	}
}

func TestToolResultCapsOversizedMultibyteOutput(t *testing.T) {
	// "é" is two bytes. A byte-wise cut near the cap would land mid-rune.
	body := strings.Repeat("é", wantToolOutputCap/2+256)
	text, isError := toolResultText(t, toolResult(body, nil))
	if isError {
		t.Fatal("oversized result was returned as an error")
	}
	if len(text) > wantToolOutputCap {
		t.Errorf("capped result is %d bytes, want at most %d", len(text), wantToolOutputCap)
	}
	if !utf8.ValidString(text) {
		t.Error("capped result is not valid UTF-8")
	}
	if !strings.Contains(text, "truncated") {
		t.Errorf("capped result has no truncation notice:\n%.200s", text[max(0, len(text)-200):])
	}
}

func TestToolResultCapsOversizedErrorWithNotice(t *testing.T) {
	err := errors.New(strings.Repeat("e", wantToolOutputCap+4096))
	text, isError := toolResultText(t, toolResult("", err))
	if !isError {
		t.Fatal("oversized error was not marked IsError")
	}
	if !strings.HasPrefix(text, "Error: ") {
		t.Errorf("error text = %.200q", text)
	}
	if len(text) > wantToolOutputCap {
		t.Errorf("capped error is %d bytes, want at most %d", len(text), wantToolOutputCap)
	}
	if !strings.Contains(text, "truncated") {
		t.Errorf("capped error has no truncation notice:\n%.400s", text)
	}
}

func TestToolResultRedactsSecretBeforeCut(t *testing.T) {
	// A unique credential long enough that any cut near the 100 KiB cap
	// lands inside the value. After a correct redact-then-cut, no 4-byte
	// window of it may remain.
	var secretB strings.Builder
	for i := 0; secretB.Len() < 400; i++ {
		secretB.WriteString("Qz8J")
		if i < 10 {
			secretB.WriteByte(byte('0' + i))
		} else {
			secretB.WriteByte(byte('A' + (i - 10)))
		}
		secretB.WriteString("Xw")
	}
	secret := secretB.String()[:400]
	const key = "{'bindpw': '"
	prefix := strings.Repeat("x", wantToolOutputCap-300-len(key))
	body := prefix + key + secret + "'}" + strings.Repeat("y", 8192)

	text, isError := toolResultText(t, toolResult(body, nil))
	if isError {
		t.Fatal("result was returned as an error")
	}
	if len(text) > wantToolOutputCap {
		t.Errorf("capped result is %d bytes, want at most %d", len(text), wantToolOutputCap)
	}
	if !strings.Contains(text, "truncated") {
		t.Fatalf("result was not truncated; cannot check a secret at the cut")
	}
	for i := 0; i+4 <= len(secret); i++ {
		if w := secret[i : i+4]; strings.Contains(text, w) {
			t.Fatalf("capped result leaks part of the secret (%q)", w)
		}
	}
	if !strings.Contains(text, redact.Marker) {
		t.Error("expected the bindpw value to be replaced with the redaction marker")
	}
}

func TestToolResultCapsOutputOnTheWire(t *testing.T) {
	body := strings.Repeat("n", wantToolOutputCap+1024)
	text, isError := callText(t, secretRegistry{result: body})
	if isError {
		t.Fatal("oversized result was a tool error")
	}
	if len(text) > wantToolOutputCap {
		t.Errorf("wire result is %d bytes, want at most %d", len(text), wantToolOutputCap)
	}
	if !strings.Contains(text, "truncated") {
		t.Errorf("wire result has no truncation notice:\n%.200s", text[max(0, len(text)-200):])
	}
}

package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
)

const (
	livePassword     = "Gen-Pass-7xQ2-w9Lm-4Rt8"
	unissuedPassword = "Not-Generated-3vB6-k8Pz"
	suppliedPassword = "Supplied-Pass-5hJ3-n2Xc"
)

// passwordRegistry answers every call with a result that carries password
// as generated_password, plus a password in a nested entry. Only
// livePassword counts as generated, and only once.
type passwordRegistry struct {
	password string
	asError  bool
	mu       *sync.Mutex
	issued   map[string]bool
}

func newPasswordRegistry(pw string, asError bool) passwordRegistry {
	return passwordRegistry{password: pw, asError: asError, mu: &sync.Mutex{}, issued: map[string]bool{livePassword: true}}
}

func (r passwordRegistry) ListTools() []mcp.Tool {
	return []mcp.Tool{{Name: "query_directory_services", Description: "fake", InputSchema: map[string]interface{}{"type": "object"}}}
}

func (r passwordRegistry) CallTool(context.Context, string, map[string]interface{}) (string, error) {
	out := "{\n  \"created\": true,\n  \"generated_password\": \"" + r.password + "\",\n  \"user\": {\n    \"password\": \"" + r.password + "\"\n  }\n}"
	if r.asError {
		return "", &mcp.ReplyError{Message: "failed", Reply: out}
	}
	return out, nil
}

func (r passwordRegistry) TakeOneTimeSecret(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ok := r.issued[s]
	delete(r.issued, s)
	return ok
}

// A generated password reaches the client once, only as the top-level
// generated_password of a successful result, and never the debug logs.
func TestGeneratedPasswordReachesClientOnce(t *testing.T) {
	logs := captureDebugLogs(t)
	c, run := session(t, newTestHandler(newPasswordRegistry(livePassword, false), true))
	c.initialize()
	first, isError := toolText(t, c.call(float64(1), "tools/call", toolArgs("query_directory_services", nil)))
	second, _ := toolText(t, c.call(float64(2), "tools/call", toolArgs("query_directory_services",
		map[string]interface{}{"password_mode": "set", "password": suppliedPassword})))
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if isError || !strings.Contains(first, `"generated_password": "`+livePassword+`"`) || strings.Count(first, livePassword) != 1 {
		t.Errorf("the client did not get the password exactly once:\n%s", first)
	}
	if strings.Contains(second, livePassword) || !strings.Contains(second, redact.Marker) {
		t.Errorf("the password was returned a second time:\n%s", second)
	}
	assertDebugHidesSecret(t, logs.String(), livePassword, "[STDOUT]")
	assertDebugHidesSecret(t, logs.String(), suppliedPassword, "[STDIN]")
}

// A generated_password the registry did not generate, or one in an error
// reply, is redacted as any other password.
func TestUnissuedGeneratedPasswordIsRedacted(t *testing.T) {
	for name, reg := range map[string]passwordRegistry{
		"unissued":    newPasswordRegistry(unissuedPassword, false),
		"error reply": newPasswordRegistry(livePassword, true),
	} {
		text, _ := callText(t, reg)
		if strings.Contains(text, unissuedPassword) || strings.Contains(text, livePassword) || !strings.Contains(text, redact.Marker) {
			t.Errorf("%s: password not redacted:\n%s", name, text)
		}
	}
}

package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
)

func TestAllowVMConsoleFlagAndEnv(t *testing.T) {
	if f := flag.Lookup("allow-vm-console"); f == nil {
		t.Fatal("flag --allow-vm-console is not registered")
	}

	lookup := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			v, ok := m[k]
			return v, ok
		}
	}

	env := lookup(map[string]string{
		"TRUENAS_MCP_ALLOW_VM_CONSOLE": " debian , 7 , , alpine ",
	})
	got := allowedVMConsoles(nil, env)
	if strings.Join(got, ",") != "debian,7,alpine" {
		t.Fatalf("from env = %q, want debian,7,alpine", got)
	}

	// Flag overrides env.
	got = allowedVMConsoles(map[string]string{"allow-vm-console": "ubuntu, 3"}, env)
	if strings.Join(got, ",") != "ubuntu,3" {
		t.Fatalf("from flag = %q, want ubuntu,3", got)
	}

	// Explicit empty flag disables env allowlist.
	got = allowedVMConsoles(map[string]string{"allow-vm-console": ""}, env)
	if len(got) != 0 {
		t.Fatalf("empty flag should override env, got %q", got)
	}

	// Unset returns empty.
	got = allowedVMConsoles(nil, lookup(nil))
	if len(got) != 0 {
		t.Fatalf("unset should be empty, got %q", got)
	}
}

type stubConsoleRegistry struct{}

func (stubConsoleRegistry) ListTools() []mcp.Tool {
	return []mcp.Tool{{
		Name:        "send_vm_console_keys",
		Description: "stub",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"session_id": map[string]interface{}{"type": "string"},
				"text":       map[string]interface{}{"type": "string"},
				"keys":       map[string]interface{}{"type": "array"},
				"sensitive":  map[string]interface{}{"type": "boolean"},
			},
		},
	}}
}

func (stubConsoleRegistry) CallTool(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
	return `{"status":"sent","sensitive":true}`, nil
}

func TestDebugStdinLogsRedactVMConsoleKeys(t *testing.T) {
	logs := captureDebugLogs(t)
	c, run := session(t, newTestHandler(stubConsoleRegistry{}, true))
	c.initialize()

	const secretText = "GuestRootPass!9988"
	const secretKey = "Ctrl+S"

	c.call(float64(1), "tools/call", toolArgs("send_vm_console_keys", map[string]interface{}{
		"session_id": "vmcon_abc123",
		"text":       secretText,
		"keys":       []interface{}{secretKey, "Enter"},
		"sensitive":  true,
	}))
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := logs.String()
	if strings.Contains(out, secretText) {
		t.Errorf("send_vm_console_keys text leaked into debug stderr:\n%s", out)
	}
	if strings.Contains(out, secretKey) {
		t.Errorf("send_vm_console_keys keys leaked into debug stderr:\n%s", out)
	}
	if !strings.Contains(out, "[STDIN]") || !strings.Contains(out, redact.Marker) {
		t.Errorf("expected redacted [STDIN] log line:\n%s", out)
	}
}

func TestRedactStdinForLogConsoleKeysShapes(t *testing.T) {
	const secret = "plainword42"
	call := func(id int, args string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"send_vm_console_keys","arguments":%s}}`, id, args)
	}
	args := `{"session_id":"vmcon_abc","text":"` + secret + `","keys":["` + secret + `"]}`
	// Without session_id nothing else on the line is redacted, so the line
	// reaches the console masking exactly as the client sent it.
	bare := `{"text":"` + secret + `","keys":["` + secret + `"]}`
	escaped := strings.Replace(call(1, bare), "send_vm", `send\u005fvm`, 1)
	other := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"read_vm_console","arguments":{"wait_for":"login:"}}}`
	cases := map[string]string{
		"single":                     call(1, args),
		"batch":                      "[" + call(1, args) + "," + call(2, args) + "]",
		"several per line":           call(1, args) + " " + call(2, args) + "\n" + call(3, args),
		"console call after another": other + " " + call(1, bare),
		"escaped name":               escaped,
		"arguments string":           call(1, `"`+secret+`"`),
		"unparsable":                 call(1, args) + `{"broken":`,
		"escaped unparsable":         escaped + "}",
	}
	for name, line := range cases {
		out := string(redactStdinForLog([]byte(line)))
		if strings.Contains(out, secret) {
			t.Errorf("%s: keystrokes leaked: %s", name, out)
		}
		if !strings.Contains(out, redact.Marker) {
			t.Errorf("%s: no redaction marker: %s", name, out)
		}
	}
	for name, want := range map[string]int{"batch": 2, "several per line": 3} {
		if got := string(redactStdinForLog([]byte(cases[name]))); strings.Count(got, "send_vm_console_keys") != want {
			t.Errorf("%s: want all %d messages kept after masking: %s", name, want, got)
		}
	}

	// Other tools are left alone.
	if got := string(redactStdinForLog([]byte(other))); got != other {
		t.Errorf("unrelated call changed: %s", got)
	}
}

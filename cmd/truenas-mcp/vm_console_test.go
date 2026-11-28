package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/tools"
	"github.com/truenas/truenas-mcp/truenas"
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
				"console_id": map[string]interface{}{"type": "string"},
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
		"console_id": "vmcon_abc123",
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
	args := `{"console_id":"vmcon_abc","text":"` + secret + `","keys":["` + secret + `"]}`
	// Without any sensitive keys on the line, RedactJSONForLog leaves the raw
	// JSON untouched so escaped tool names reach console masking as sent.
	bare := `{"text":"` + secret + `","keys":["` + secret + `"]}`
	escaped := strings.Replace(call(1, bare), "send_vm", `send\u005fvm`, 1)
	other := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"read_vm_console","arguments":{"console_id":"vmcon_abc","wait_for":"login:"}}}`
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

	// Other tools (including console_id) are left alone.
	if got := string(redactStdinForLog([]byte(other))); got != other {
		t.Errorf("unrelated call changed: %s", got)
	}
}

// TestVMConsoleRoundTripThroughStdioRedaction drives open_vm_console,
// read_vm_console, send_vm_console_keys and close_vm_console through the real
// MCP StdioHandler output-redaction path (callResult -> redact.ToolOutputKeepingKeys).
// Regression test for #251: the console handle must survive output redaction
// intact and work in follow-up calls, while actual secrets in tool output are
// still redacted.
func TestVMConsoleRoundTripThroughStdioRedaction(t *testing.T) {
	const (
		shellToken  = "tok-vmconsole-stdio-test-9911"
		guestSecret = "guest-secret-value-8822"
	)

	var (
		mu       sync.Mutex
		received [][]byte
	)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/current", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				ID     interface{}   `json:"id"`
				Method string        `json:"method"`
				Params []interface{} `json:"params"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				return
			}
			var res interface{}
			switch req.Method {
			case "auth.login_ex":
				res = map[string]interface{}{"response_type": "SUCCESS"}
			case "system.info":
				res = map[string]interface{}{"hostname": "nas", "version": "25.10.0"}
			case "vm.query":
				res = []map[string]interface{}{{
					"id": 2.0, "name": "debian", "uuid": "22222222-2222-2222-2222-222222222222",
					"memory": 2048.0, "autostart": true,
					"status": map[string]interface{}{"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"},
				}}
			case "vm.status":
				res = map[string]interface{}{"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"}
			case "auth.me":
				res = map[string]interface{}{
					"pw_name": "truenas_admin",
					"privilege": map[string]interface{}{
						"web_shell": true,
						"roles":     map[string]interface{}{"$set": []interface{}{"VM_WRITE"}},
					},
				}
			case "auth.generate_token":
				res = shellToken
			case "core.resize_shell":
				res = nil
			default:
				b, _ := json.Marshal(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"error":   map[string]interface{}{"code": -32001, "message": "unexpected method " + req.Method},
				})
				_ = conn.WriteMessage(websocket.TextMessage, b)
				continue
			}
			b, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": res})
			_ = conn.WriteMessage(websocket.TextMessage, b)
		}
	})
	mux.HandleFunc("/websocket/shell", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		ack, _ := json.Marshal(map[string]interface{}{"msg": "connected", "id": "mw-shell-stdio-1"})
		_ = conn.WriteMessage(websocket.TextMessage, ack)
		// Include a secret-shaped line in guest output so output redaction
		// actively modifies and re-encodes the response JSON.
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("Connected to domain\r\nlogin: \r\npassword="+guestSecret+"\r\n"))
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			mu.Lock()
			received = append(received, append([]byte(nil), data...))
			mu.Unlock()
		}
	})

	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	endpoint := "wss://" + strings.TrimPrefix(srv.URL, "https://") + "/api/current"
	client, err := truenas.NewClientWithOptions(endpoint, "1-fakeapikey", &tls.Config{InsecureSkipVerify: true}, truenas.Options{APIUser: "truenas_admin"})
	if err != nil {
		t.Fatalf("NewClientWithOptions: %v", err)
	}
	if err := client.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	reg := tools.NewRegistry(client, nil)
	reg.SetAllowedVMConsoles([]string{"debian"})
	t.Cleanup(func() {
		reg.Close()
		client.Close()
	})

	c, run := session(t, newTestHandler(reg, false))
	c.initialize()

	// Step 1: preview open_vm_console to obtain confirmation_token.
	previewText, isError := toolText(t, c.call(float64(1), "tools/call", toolArgs("open_vm_console", map[string]interface{}{"name": "debian"})))
	if isError {
		t.Fatalf("open_vm_console preview failed: %s", previewText)
	}
	var preview map[string]interface{}
	if err := json.Unmarshal([]byte(previewText), &preview); err != nil {
		t.Fatalf("unmarshal preview: %v\n%s", err, previewText)
	}
	confirmTok, _ := preview["confirmation_token"].(string)
	if confirmTok == "" || confirmTok == redact.Marker {
		t.Fatalf("missing confirmation_token in preview: %s", previewText)
	}

	// Step 2: execute open_vm_console through stdio redaction.
	openText, isError := toolText(t, c.call(float64(2), "tools/call", toolArgs("open_vm_console", map[string]interface{}{
		"name":               "debian",
		"confirmation_token": confirmTok,
	})))
	if isError {
		t.Fatalf("open_vm_console failed: %s", openText)
	}
	if strings.Contains(openText, shellToken) {
		t.Errorf("open_vm_console leaked shell auth token")
	}
	if strings.Contains(openText, guestSecret) {
		t.Errorf("open_vm_console did not redact secret in screen output:\n%s", openText)
	}
	var opened map[string]interface{}
	if err := json.Unmarshal([]byte(openText), &opened); err != nil {
		t.Fatalf("unmarshal open_vm_console result: %v\n%s", err, openText)
	}
	if _, hasOldKey := opened["session_id"]; hasOldKey {
		t.Errorf("open_vm_console still returned session_id key: %s", openText)
	}
	consoleID, _ := opened["console_id"].(string)
	if consoleID == "" || consoleID == redact.Marker || strings.Contains(consoleID, redact.Marker) || !strings.HasPrefix(consoleID, "vmcon_") {
		t.Fatalf("open_vm_console returned bad/redacted console_id = %q\n%s", consoleID, openText)
	}

	// Step 3: read_vm_console using the redacted-output console_id.
	readText, isError := toolText(t, c.call(float64(3), "tools/call", toolArgs("read_vm_console", map[string]interface{}{
		"console_id": consoleID,
	})))
	if isError {
		t.Fatalf("read_vm_console failed: %s", readText)
	}
	if strings.Contains(readText, guestSecret) {
		t.Errorf("read_vm_console did not redact secret in screen output:\n%s", readText)
	}
	var readRes map[string]interface{}
	if err := json.Unmarshal([]byte(readText), &readRes); err != nil {
		t.Fatalf("unmarshal read_vm_console result: %v\n%s", err, readText)
	}
	if readRes["console_id"] != consoleID {
		t.Errorf("read_vm_console console_id = %v, want %q", readRes["console_id"], consoleID)
	}
	if screen, _ := readRes["screen"].(string); !strings.Contains(screen, "login:") {
		t.Errorf("read_vm_console screen missing login prompt: %q", screen)
	}

	// Step 4: send_vm_console_keys using the same console_id.
	sendText, isError := toolText(t, c.call(float64(4), "tools/call", toolArgs("send_vm_console_keys", map[string]interface{}{
		"console_id": consoleID,
		"text":       "root",
		"keys":       []interface{}{"Enter"},
	})))
	if isError {
		t.Fatalf("send_vm_console_keys failed: %s", sendText)
	}
	var sendRes map[string]interface{}
	if err := json.Unmarshal([]byte(sendText), &sendRes); err != nil {
		t.Fatalf("unmarshal send_vm_console_keys result: %v\n%s", err, sendText)
	}
	if sendRes["console_id"] != consoleID || sendRes["status"] != "sent" {
		t.Errorf("send_vm_console_keys result = %v, want status=sent console_id=%q", sendRes, consoleID)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	if len(received) != 1 || string(received[0]) != "root\r" {
		t.Errorf("shell received %q, want [\"root\\r\"]", received)
	}
	mu.Unlock()

	// Step 5: close_vm_console using the same console_id.
	closeText, isError := toolText(t, c.call(float64(5), "tools/call", toolArgs("close_vm_console", map[string]interface{}{
		"console_id": consoleID,
	})))
	if isError {
		t.Fatalf("close_vm_console failed: %s", closeText)
	}
	var closeRes map[string]interface{}
	if err := json.Unmarshal([]byte(closeText), &closeRes); err != nil {
		t.Fatalf("unmarshal close_vm_console result: %v\n%s", err, closeText)
	}
	if closeRes["console_id"] != consoleID || closeRes["status"] != "closed" {
		t.Errorf("close_vm_console result = %v, want status=closed console_id=%q", closeRes, consoleID)
	}

	// Step 6: verify closed session is gone.
	afterClose, isError := toolText(t, c.call(float64(6), "tools/call", toolArgs("read_vm_console", map[string]interface{}{
		"console_id": consoleID,
	})))
	if !isError {
		t.Errorf("read_vm_console after close should fail, got: %s", afterClose)
	}

	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

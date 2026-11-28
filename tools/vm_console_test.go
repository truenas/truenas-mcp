package tools

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/truenas/truenas-mcp/truenas"
)

const testVMConsoleToken = "tok-vmconsole-secret-never-log-5519"

// vmConsoleFakeServer serves both TrueNAS /api/current JSON-RPC and
// /websocket/shell over TLS so tests exercise the full middleware + WebSocket
// handshake for both 25.10 (stable/goldeye) and 26.0+/master.
type vmConsoleFakeServer struct {
	srv *httptest.Server

	mu            sync.Mutex
	version       string
	vms           []map[string]interface{}
	statuses      map[int]map[string]interface{}
	authMe        map[string]interface{}
	authMeErr     string
	sysInfoErr    string
	tokenParams   []interface{}
	resizeCalls   [][]interface{}
	handshakes    []map[string]interface{}
	receivedInput [][]byte
	receivedTypes []int
	shellHandler  func(conn *websocket.Conn, hs map[string]interface{}, pushInput func(int, []byte))
}

func newVMConsoleFakeServer(t *testing.T, version string) *vmConsoleFakeServer {
	t.Helper()
	fs := &vmConsoleFakeServer{
		version: version,
		vms: []map[string]interface{}{
			{
				"id": 1.0, "name": "ubuntu", "uuid": "11111111-1111-1111-1111-111111111111",
				"memory": 4096.0, "autostart": false,
				"status": map[string]interface{}{"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"},
			},
			{
				"id": 2.0, "name": "debian", "uuid": "22222222-2222-2222-2222-222222222222",
				"memory": 2048.0, "autostart": true,
				"status": map[string]interface{}{"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"},
			},
			{
				"id": 3.0, "name": "alpine", "uuid": "33333333-3333-3333-3333-333333333333",
				"memory": 1024.0, "autostart": false,
				"status": map[string]interface{}{"state": "RUNNING", "pid": 4322.0, "domain_state": "RUNNING"},
			},
			{
				"id": 4.0, "name": "freebsd", "uuid": "44444444-4444-4444-4444-444444444444",
				"memory": 1024.0, "autostart": false,
				"status": map[string]interface{}{"state": "RUNNING", "pid": 4323.0, "domain_state": "RUNNING"},
			},
		},
		statuses: map[int]map[string]interface{}{
			1: {"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"},
			2: {"state": "RUNNING", "pid": 4321.0, "domain_state": "RUNNING"},
			3: {"state": "RUNNING", "pid": 4322.0, "domain_state": "RUNNING"},
			4: {"state": "RUNNING", "pid": 4323.0, "domain_state": "RUNNING"},
		},
		authMe: map[string]interface{}{
			"pw_name": "truenas_admin",
			"privilege": map[string]interface{}{
				"web_shell": true,
				"roles":     []interface{}{"VM_WRITE"},
			},
		},
	}

	fs.shellHandler = func(conn *websocket.Conn, hs map[string]interface{}, pushInput func(int, []byte)) {
		ack, _ := json.Marshal(map[string]interface{}{"msg": "connected", "id": "mw-shell-uuid-9001"})
		_ = conn.WriteMessage(websocket.TextMessage, ack)
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("Connected to domain\r\nlogin: "))
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			pushInput(mt, data)
		}
	}

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
			res, errMsg := fs.handleRPC(req.Method, req.Params)
			var resp map[string]interface{}
			if errMsg != "" {
				resp = map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"error":   map[string]interface{}{"code": -32001, "message": errMsg},
				}
			} else {
				resp = map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  res,
				}
			}
			b, _ := json.Marshal(resp)
			_ = conn.WriteMessage(websocket.TextMessage, b)
		}
	})
	mux.HandleFunc("/websocket/shell", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		mt, raw, err := conn.ReadMessage()
		if err != nil || mt != websocket.TextMessage {
			return
		}
		var hs map[string]interface{}
		if err := json.Unmarshal(raw, &hs); err != nil {
			return
		}
		fs.mu.Lock()
		fs.handshakes = append(fs.handshakes, hs)
		handler := fs.shellHandler
		fs.mu.Unlock()

		pushInput := func(msgType int, data []byte) {
			fs.mu.Lock()
			cp := make([]byte, len(data))
			copy(cp, data)
			fs.receivedInput = append(fs.receivedInput, cp)
			fs.receivedTypes = append(fs.receivedTypes, msgType)
			fs.mu.Unlock()
		}
		handler(conn, hs, pushInput)
	})

	fs.srv = httptest.NewTLSServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *vmConsoleFakeServer) handleRPC(method string, params []interface{}) (interface{}, string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	switch method {
	case "auth.login_ex":
		return map[string]interface{}{"response_type": "SUCCESS"}, ""
	case "system.info":
		if fs.sysInfoErr != "" {
			return nil, fs.sysInfoErr
		}
		if fs.version == "" {
			return map[string]interface{}{"hostname": "nas"}, ""
		}
		return map[string]interface{}{"hostname": "nas", "version": fs.version}, ""
	case "vm.query":
		return fs.filterVMsLocked(params), ""
	case "vm.status":
		if len(params) > 0 {
			if idf, ok := params[0].(float64); ok {
				if st, exists := fs.statuses[int(idf)]; exists {
					return st, ""
				}
			}
		}
		return nil, "VM not found"
	case "auth.me":
		if fs.authMeErr != "" {
			return nil, fs.authMeErr
		}
		return fs.authMe, ""
	case "auth.generate_token":
		fs.tokenParams = params
		return testVMConsoleToken, ""
	case "core.resize_shell":
		fs.resizeCalls = append(fs.resizeCalls, params)
		return nil, ""
	default:
		return nil, fmt.Sprintf("unexpected method %s", method)
	}
}

func (fs *vmConsoleFakeServer) filterVMsLocked(params []interface{}) []map[string]interface{} {
	if len(params) == 0 {
		return fs.vms
	}
	filters, _ := params[0].([]interface{})
	if len(filters) == 0 {
		return fs.vms
	}
	var out []map[string]interface{}
	for _, vm := range fs.vms {
		match := true
		for _, rawF := range filters {
			f, ok := rawF.([]interface{})
			if !ok || len(f) < 3 {
				continue
			}
			field, _ := f[0].(string)
			op, _ := f[1].(string)
			if op != "=" {
				continue
			}
			if vm[field] != f[2] {
				match = false
				break
			}
		}
		if match {
			out = append(out, vm)
		}
	}
	return out
}

func (fs *vmConsoleFakeServer) newRegistry(t *testing.T, allowed []string) *Registry {
	t.Helper()
	endpoint := "wss://" + strings.TrimPrefix(fs.srv.URL, "https://") + "/api/current"
	client, err := truenas.NewClientWithOptions(endpoint, "1-fakeapikey", &tls.Config{InsecureSkipVerify: true}, truenas.Options{APIUser: "truenas_admin"})
	if err != nil {
		t.Fatalf("NewClientWithOptions: %v", err)
	}
	if err := client.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	r := NewRegistry(client, nil)
	r.SetAllowedVMConsoles(allowed)
	t.Cleanup(func() {
		r.Close()
		client.Close()
	})
	return r
}

func openVMConsoleForTest(t *testing.T, r *Registry, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	approved := approve(t, r, "open_vm_console", args)
	out, err := r.CallTool(context.Background(), "open_vm_console", approved)
	if err != nil {
		t.Fatalf("open_vm_console failed: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid open_vm_console JSON: %v\n%s", err, out)
	}
	return parsed
}

func TestVMConsoleHandshakeBothBranches(t *testing.T) {
	cases := []struct {
		name           string
		version        string
		roles          []interface{}
		wantDomain     string
		wantResizeCols float64
		wantResizeRows float64
	}{
		{
			name:           "25.10 stable/goldeye uses id_name and web_shell only",
			version:        "25.10.0",
			roles:          []interface{}{"READONLY_ADMIN"}, // 25.10 checks web_shell only, not VM_WRITE
			wantDomain:     "2_debian",
			wantResizeCols: 100,
			wantResizeRows: 30,
		},
		{
			name:           "26.0+ uses uuid and requires VM_WRITE",
			version:        "26.0.0",
			roles:          []interface{}{"VM_WRITE"},
			wantDomain:     "22222222-2222-2222-2222-222222222222",
			wantResizeCols: 80,
			wantResizeRows: 24,
		},
		{
			name:           "master (28.0) uses uuid and requires VM_WRITE",
			version:        "28.0.0-MASTER",
			roles:          []interface{}{"VM_WRITE"},
			wantDomain:     "22222222-2222-2222-2222-222222222222",
			wantResizeCols: 80,
			wantResizeRows: 24,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newVMConsoleFakeServer(t, tc.version)
			fs.authMe["privilege"].(map[string]interface{})["roles"] = tc.roles
			r := fs.newRegistry(t, []string{"debian"})

			args := map[string]interface{}{"name": "debian"}
			if tc.wantResizeCols != 80 {
				args["cols"] = tc.wantResizeCols
				args["rows"] = tc.wantResizeRows
			}
			opened := openVMConsoleForTest(t, r, args)

			consoleID, _ := opened["console_id"].(string)
			if !strings.HasPrefix(consoleID, "vmcon_") {
				t.Fatalf("console_id = %q, want opaque vmcon_ prefix", consoleID)
			}
			if strings.Contains(consoleID, "mw-shell-uuid") || strings.Contains(consoleID, testVMConsoleToken) {
				t.Fatalf("console_id leaked internal shell id or token: %q", consoleID)
			}
			if opened["libvirt_domain"] != tc.wantDomain {
				t.Errorf("libvirt_domain = %v, want %q", opened["libvirt_domain"], tc.wantDomain)
			}
			if screen, _ := opened["screen"].(string); !strings.Contains(screen, "login:") {
				t.Errorf("initial screen missing banner: %q", screen)
			}

			// Verify auth.generate_token args: [60, {}, true, true]
			fs.mu.Lock()
			tokParams := fs.tokenParams
			handshakes := fs.handshakes
			resizes := fs.resizeCalls
			fs.mu.Unlock()

			if len(tokParams) != 4 || tokParams[0] != 60.0 || tokParams[2] != true || tokParams[3] != true {
				t.Errorf("auth.generate_token params = %#v, want [60, {}, true, true]", tokParams)
			}
			attrs, _ := tokParams[1].(map[string]interface{})
			if attrs == nil || len(attrs) != 0 {
				t.Errorf("auth.generate_token attrs = %#v, want empty map", tokParams[1])
			}
			if len(handshakes) != 1 {
				t.Fatalf("expected 1 websocket handshake, got %d", len(handshakes))
			}
			if handshakes[0]["token"] != testVMConsoleToken {
				t.Errorf("handshake did not present generated token")
			}
			opts, _ := handshakes[0]["options"].(map[string]interface{})
			if opts == nil || opts["vm_id"] != 2.0 {
				t.Errorf("handshake options = %#v, want vm_id=2", opts)
			}
			if len(resizes) != 1 || resizes[0][0] != "mw-shell-uuid-9001" || resizes[0][1] != tc.wantResizeCols || resizes[0][2] != tc.wantResizeRows {
				t.Errorf("core.resize_shell calls = %#v, want [mw-shell-uuid-9001 %v %v]", resizes, tc.wantResizeCols, tc.wantResizeRows)
			}

			// Send keys and verify binary frame on wire.
			sendOut, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
				"console_id": consoleID,
				"text":       "root",
				"keys":       []interface{}{"Enter"},
			})
			if err != nil {
				t.Fatalf("send_vm_console_keys: %v", err)
			}
			if strings.Contains(sendOut, "root") {
				t.Errorf("send_vm_console_keys echoed input: %s", sendOut)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				fs.mu.Lock()
				n := len(fs.receivedInput)
				fs.mu.Unlock()
				if n > 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			fs.mu.Lock()
			if len(fs.receivedInput) != 1 || fs.receivedTypes[0] != websocket.BinaryMessage || string(fs.receivedInput[0]) != "root\r" {
				t.Errorf("wire input = types:%v data:%q, want Binary %q", fs.receivedTypes, fs.receivedInput, "root\r")
			}
			fs.mu.Unlock()

			// Close session.
			if _, err := r.CallTool(context.Background(), "close_vm_console", map[string]interface{}{
				"console_id": consoleID,
			}); err != nil {
				t.Fatalf("close_vm_console: %v", err)
			}
			if _, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{
				"console_id": consoleID,
			}); err == nil {
				t.Errorf("expected read_vm_console to fail after close_vm_console")
			}
		})
	}
}

func TestVMConsoleRefusals(t *testing.T) {
	t.Run("read-only hides and refuses open, send, close; allows read_vm_console", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, []string{"debian"})
		// Open a session first, then enable read-only policy to prove an
		// already-opened session can still be read while write/destructive
		// console tools are hidden and refused.
		opened := openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
		consoleID := opened["console_id"].(string)

		if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
			t.Fatal(err)
		}
		listed := map[string]bool{}
		for _, tool := range r.ListTools() {
			listed[tool.Name] = true
		}
		for _, hidden := range []string{"open_vm_console", "send_vm_console_keys", "close_vm_console"} {
			if listed[hidden] {
				t.Errorf("%s should be hidden under --read-only", hidden)
			}
			if _, err := r.CallTool(context.Background(), hidden, map[string]interface{}{"console_id": consoleID, "id": 2.0}); err == nil || !strings.Contains(err.Error(), "read-only") {
				t.Errorf("%s under --read-only: got err=%v, want read-only refusal", hidden, err)
			}
		}
		if !listed["read_vm_console"] {
			t.Errorf("read_vm_console should remain visible under --read-only")
		}
		out, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{"console_id": consoleID})
		if err != nil || !strings.Contains(out, "login:") {
			t.Errorf("read_vm_console under --read-only failed: out=%s err=%v", out, err)
		}
	})

	t.Run("unset allowlist refuses every VM with instructions", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, nil)
		_, err := r.CallTool(context.Background(), "open_vm_console", map[string]interface{}{"id": 2.0})
		if err == nil || !strings.Contains(err.Error(), "--allow-vm-console") || !strings.Contains(err.Error(), "TRUENAS_MCP_ALLOW_VM_CONSOLE") {
			t.Fatalf("expected allowlist error naming flag and env, got: %v", err)
		}
	})

	t.Run("VM not in allowlist is refused with instructions", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, []string{"alpine", "99"})
		_, err := r.CallTool(context.Background(), "open_vm_console", map[string]interface{}{"id": 2.0})
		if err == nil || !strings.Contains(err.Error(), "not allowed") || !strings.Contains(err.Error(), "--allow-vm-console") {
			t.Fatalf("expected not-allowed error, got: %v", err)
		}
	})

	t.Run("allowlist matches by numeric ID", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, []string{"2"})
		opened := openVMConsoleForTest(t, r, map[string]interface{}{"name": "debian"})
		if opened["vm_name"] != "debian" {
			t.Fatalf("unexpected open response: %#v", opened)
		}
	})

	t.Run("STOPPED VM is refused", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, []string{"ubuntu", "1"})
		_, err := r.CallTool(context.Background(), "open_vm_console", map[string]interface{}{"id": 1.0})
		if err == nil || !strings.Contains(err.Error(), "STOPPED") || !strings.Contains(err.Error(), "RUNNING") {
			t.Fatalf("expected STOPPED/RUNNING refusal, got: %v", err)
		}
	})

	t.Run("unknown and unsupported TrueNAS versions are refused strictly", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			version string
			sysErr  string
		}{
			{"empty version", "", ""},
			{"unparseable version", "custom-build", ""},
			{"system.info error", "25.10.0", "system.info failed"},
			{"too old 25.04", "25.04.1", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fs := newVMConsoleFakeServer(t, tc.version)
				fs.sysInfoErr = tc.sysErr
				r := fs.newRegistry(t, []string{"debian"})
				_, err := r.CallTool(context.Background(), "open_vm_console", map[string]interface{}{"id": 2.0})
				if err == nil || !strings.Contains(err.Error(), "unknown or unsupported") || !strings.Contains(err.Error(), "libvirt domain format") {
					t.Fatalf("%s: expected strict early unknown/unsupported version refusal, got: %v", tc.name, err)
				}
			})
		}
	})

	t.Run("missing web_shell privilege is refused on 25.10 and master", func(t *testing.T) {
		for _, ver := range []string{"25.10.0", "28.0.0-MASTER"} {
			t.Run(ver, func(t *testing.T) {
				fs := newVMConsoleFakeServer(t, ver)
				fs.authMe["privilege"].(map[string]interface{})["web_shell"] = false
				r := fs.newRegistry(t, []string{"debian"})
				_, err := r.CallTool(context.Background(), "open_vm_console", map[string]interface{}{"id": 2.0})
				if err == nil || !strings.Contains(err.Error(), "web_shell") {
					t.Fatalf("%s: expected web_shell refusal, got: %v", ver, err)
				}
			})
		}
	})

	t.Run("missing VM_WRITE role is refused on 26.0+/master", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "28.0.0-MASTER")
		fs.authMe["privilege"].(map[string]interface{})["roles"] = []interface{}{"VM_READ"}
		r := fs.newRegistry(t, []string{"debian"})
		_, err := r.CallTool(context.Background(), "open_vm_console", map[string]interface{}{"id": 2.0})
		if err == nil || !strings.Contains(err.Error(), "VM_WRITE") {
			t.Fatalf("expected VM_WRITE refusal on master, got: %v", err)
		}
	})

	t.Run("empty VM UUID is refused on 26.0+/master", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "28.0.0-MASTER")
		fs.vms[1]["uuid"] = ""
		r := fs.newRegistry(t, []string{"debian"})
		_, err := r.CallTool(context.Background(), "open_vm_console", map[string]interface{}{"id": 2.0})
		if err == nil || !strings.Contains(err.Error(), "UUID") {
			t.Fatalf("expected empty UUID refusal on master, got: %v", err)
		}
	})

	t.Run("concurrent session cap and duplicate VM session are refused", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, []string{"debian", "alpine", "freebsd"})
		r.vmConsoles.maxSessions = 2

		s1 := openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
		// Duplicate VM is refused.
		if _, err := r.CallTool(context.Background(), "open_vm_console", map[string]interface{}{"id": 2.0}); err == nil || !strings.Contains(err.Error(), "already has an active serial console session") {
			t.Fatalf("expected duplicate VM session refusal, got: %v", err)
		}

		// Second distinct VM succeeds.
		_ = openVMConsoleForTest(t, r, map[string]interface{}{"id": 3.0})

		// Third exceeds cap=2.
		if _, err := r.CallTool(context.Background(), "open_vm_console", map[string]interface{}{"id": 4.0}); err == nil || !strings.Contains(err.Error(), "maximum concurrent") {
			t.Fatalf("expected max concurrent sessions refusal, got: %v", err)
		}

		// Closing one frees a slot.
		if _, err := r.CallTool(context.Background(), "close_vm_console", map[string]interface{}{"console_id": s1["console_id"]}); err != nil {
			t.Fatalf("close_vm_console: %v", err)
		}
		_ = openVMConsoleForTest(t, r, map[string]interface{}{"id": 4.0})
	})

	t.Run("VM stopping between plan and execute is refused", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, []string{"debian"})
		plan, err := r.planOpenVMConsole(context.Background(), r.client, map[string]interface{}{"id": 2.0})
		if err != nil {
			t.Fatalf("planOpenVMConsole: %v", err)
		}
		fs.mu.Lock()
		fs.statuses[2] = map[string]interface{}{"state": "STOPPED", "pid": nil, "domain_state": "SHUTOFF"}
		fs.mu.Unlock()
		if _, err := r.executeOpenVMConsole(context.Background(), r.client, nil, plan); err == nil || !strings.Contains(err.Error(), "STOPPED") {
			t.Fatalf("expected executeOpenVMConsole refusal when VM stopped after plan, got: %v", err)
		}
	})

	t.Run("immediate virsh disconnect surfaces stderr and frees slot", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		fs.shellHandler = func(conn *websocket.Conn, _ map[string]interface{}, _ func(int, []byte)) {
			ack, _ := json.Marshal(map[string]interface{}{"msg": "connected", "id": "mw-shell-busy"})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte("error: Active console session exists for this domain\r\n"))
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		}
		r := fs.newRegistry(t, []string{"debian"})
		approved := approve(t, r, "open_vm_console", map[string]interface{}{"id": 2.0})
		_, err := r.CallTool(context.Background(), "open_vm_console", approved)
		if err == nil || !strings.Contains(err.Error(), "Active console session exists") {
			t.Fatalf("expected immediate disconnect error with virsh text, got: %v", err)
		}
		r.vmConsoles.mu.Lock()
		remaining := len(r.vmConsoles.sessions)
		inFlight := len(r.vmConsoles.vmInFlight)
		r.vmConsoles.mu.Unlock()
		if remaining != 0 || inFlight != 0 {
			t.Errorf("failed open leaked session state: sessions=%d inFlight=%d", remaining, inFlight)
		}
	})

	t.Run("server auth rejection includes branch guidance and scrubs token", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "28.0.0-MASTER")
		fs.shellHandler = func(conn *websocket.Conn, _ map[string]interface{}, _ func(int, []byte)) {
			fail, _ := json.Marshal(map[string]interface{}{
				"msg":   "failed",
				"error": map[string]interface{}{"error": 13, "reason": "Missing required role for VM shell: VM_WRITE " + testVMConsoleToken},
			})
			_ = conn.WriteMessage(websocket.TextMessage, fail)
		}
		r := fs.newRegistry(t, []string{"debian"})
		approved := approve(t, r, "open_vm_console", map[string]interface{}{"id": 2.0})
		_, err := r.CallTool(context.Background(), "open_vm_console", approved)
		if err == nil || !strings.Contains(err.Error(), "VM_WRITE") {
			t.Fatalf("expected VM_WRITE auth failure, got: %v", err)
		}
		if strings.Contains(err.Error(), testVMConsoleToken) {
			t.Fatalf("auth token leaked into error: %v", err)
		}
	})

	t.Run("payload too long and invalid key inputs are refused without echoing", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, []string{"debian"})
		opened := openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
		sid := opened["console_id"].(string)

		// > 4096 characters text (rejected by schema validation, never echoed)
		bigSecret := "SECRET_" + strings.Repeat("A", vmConsoleMaxPayloadBytes)
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id": sid,
			"text":       bigSecret,
		}); err == nil || !strings.Contains(err.Error(), "4096") {
			t.Fatalf("expected payload size refusal, got: %v", err)
		} else if strings.Contains(err.Error(), "SECRET_") {
			t.Fatalf("error echoed secret payload: %v", err)
		}

		// Combined text + keys > 4096 bytes (reaches encoder cap)
		nearLimit := "SECRET_" + strings.Repeat("B", vmConsoleMaxPayloadBytes-10)
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id": sid,
			"text":       nearLimit,
			"keys":       []interface{}{"F12", "F12"},
		}); err == nil || !strings.Contains(err.Error(), "maximum payload size") {
			t.Fatalf("expected combined payload size refusal, got: %v", err)
		} else if strings.Contains(err.Error(), "SECRET_") {
			t.Fatalf("error echoed secret payload: %v", err)
		}

		// > 256 keys (rejected by schema validation)
		manyKeys := make([]interface{}, vmConsoleMaxKeys+1)
		for i := range manyKeys {
			manyKeys[i] = "Enter"
		}
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id": sid,
			"keys":       manyKeys,
		}); err == nil || !strings.Contains(err.Error(), "256") {
			t.Fatalf("expected too-many-keys refusal, got: %v", err)
		}

		// Unknown key must not echo key value (in case caller put a secret there)
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id": sid,
			"keys":       []interface{}{"SecretPasswordNotAKey99"},
		}); err == nil || !strings.Contains(err.Error(), "keys[0]: unsupported key name") {
			t.Fatalf("expected unsupported key error, got: %v", err)
		} else if strings.Contains(err.Error(), "SecretPasswordNotAKey99") {
			t.Fatalf("unsupported key error echoed caller input: %v", err)
		}

		// Empty input refused
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id": sid,
		}); err == nil || !strings.Contains(err.Error(), "provide text, keys, or both") {
			t.Fatalf("expected empty input error, got: %v", err)
		}

		// Legacy session_id argument is rejected (no backward-compat alias)
		// and whitespace-only console_id returns "console_id is required".
		for _, tc := range []struct {
			tool      string
			blankArgs map[string]interface{}
		}{
			{"read_vm_console", map[string]interface{}{"console_id": "   "}},
			{"send_vm_console_keys", map[string]interface{}{"console_id": "   ", "text": "x"}},
			{"close_vm_console", map[string]interface{}{"console_id": "   "}},
		} {
			if _, err := r.CallTool(context.Background(), tc.tool, map[string]interface{}{
				"session_id": sid,
			}); err == nil || !strings.Contains(err.Error(), `unknown argument "session_id"`) {
				t.Errorf("%s with session_id: got err=%v, want unknown argument error", tc.tool, err)
			}
			if _, err := r.CallTool(context.Background(), tc.tool, tc.blankArgs); err == nil || !strings.Contains(err.Error(), "console_id is required") {
				t.Errorf("%s with blank console_id: got err=%v, want console_id is required", tc.tool, err)
			}
		}
	})
}

func TestVMConsoleVT100EmulationWaitAndDSR(t *testing.T) {
	fs := newVMConsoleFakeServer(t, "25.10.0")
	sendFromGuest := make(chan []byte, 16)
	dsrReplyCh := make(chan []byte, 4)

	fs.shellHandler = func(conn *websocket.Conn, _ map[string]interface{}, pushInput func(int, []byte)) {
		ack, _ := json.Marshal(map[string]interface{}{"msg": "connected", "id": "mw-shell-vt"})
		_ = conn.WriteMessage(websocket.TextMessage, ack)
		// Send styled VT100 screen with cursor positioning and a DSR cursor query.
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("\x1b[2J\x1b[1;1H\x1b[32mDebian Installer\x1b[0m\x1b[3;5HSelect language\x1b[6n"))

		go func() {
			for chunk := range sendFromGuest {
				if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
					return
				}
			}
		}()

		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			pushInput(mt, data)
			if bytes.Contains(data, []byte("\x1b[")) && bytes.HasSuffix(data, []byte("R")) {
				dsrReplyCh <- data
			}
		}
	}

	r := fs.newRegistry(t, []string{"debian"})
	opened := openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
	sid := opened["console_id"].(string)

	// Verify DSR query reply was forwarded back over WebSocket without deadlocking.
	select {
	case reply := <-dsrReplyCh:
		if string(reply) != "\x1b[3;20R" {
			t.Errorf("DSR reply = %q, want %q", reply, "\x1b[3;20R")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DSR cursor position reply")
	}

	// Verify rendered plain text stripped ANSI colors and cursor position is accurate.
	readOut, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{"console_id": sid})
	if err != nil {
		t.Fatalf("read_vm_console: %v", err)
	}
	var snap map[string]interface{}
	if err := json.Unmarshal([]byte(readOut), &snap); err != nil {
		t.Fatal(err)
	}
	screen, _ := snap["screen"].(string)
	if !strings.Contains(screen, "Debian Installer") || !strings.Contains(screen, "Select language") {
		t.Errorf("rendered screen missing expected text:\n%s", screen)
	}
	if strings.Contains(screen, "\x1b[32m") {
		t.Errorf("rendered screen leaked raw ANSI color codes:\n%q", screen)
	}
	cursor := snap["cursor"].(map[string]interface{})
	if cursor["row"] != 2.0 || cursor["col"] != 19.0 {
		t.Errorf("cursor = %#v, want row=2 col=19 (0-indexed)", cursor)
	}

	// Test wait_for waking up promptly when guest emits the prompt.
	go func() {
		time.Sleep(40 * time.Millisecond)
		sendFromGuest <- []byte("\r\nPartition disks [y/N]: ")
	}()
	start := time.Now()
	waitOut, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{
		"console_id":      sid,
		"wait_for":        "Partition disks [y/N]:",
		"timeout_seconds": 3.0,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("read_vm_console wait_for: %v", err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("wait_for took %s, expected immediate wakeup", elapsed)
	}
	var waitSnap map[string]interface{}
	_ = json.Unmarshal([]byte(waitOut), &waitSnap)
	if waitSnap["matched"] != true || waitSnap["timed_out"] != false {
		t.Errorf("wait_for result = matched:%v timed_out:%v, want true/false", waitSnap["matched"], waitSnap["timed_out"])
	}

	// Test wait_for_regex matching dynamically arriving output.
	go func() {
		time.Sleep(40 * time.Millisecond)
		sendFromGuest <- []byte("\r\nroot@guest-01:~# ")
	}()
	regexOut, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{
		"console_id":      sid,
		"wait_for_regex":  `root@guest-\d+:~#`,
		"timeout_seconds": 3.0,
	})
	if err != nil {
		t.Fatalf("read_vm_console wait_for_regex: %v", err)
	}
	var regexSnap map[string]interface{}
	_ = json.Unmarshal([]byte(regexOut), &regexSnap)
	if regexSnap["matched"] != true || regexSnap["timed_out"] != false {
		t.Errorf("wait_for_regex result = matched:%v timed_out:%v, want true/false", regexSnap["matched"], regexSnap["timed_out"])
	}

	// Test timeout when pattern never arrives.
	toOut, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{
		"console_id":      sid,
		"wait_for":        "never-appears-xyz",
		"timeout_seconds": 0.08,
	})
	if err != nil {
		t.Fatalf("read_vm_console timeout: %v", err)
	}
	var toSnap map[string]interface{}
	_ = json.Unmarshal([]byte(toOut), &toSnap)
	if toSnap["matched"] != false || toSnap["timed_out"] != true {
		t.Errorf("timeout result = matched:%v timed_out:%v, want false/true", toSnap["matched"], toSnap["timed_out"])
	}

	// Test DECCKM application cursor keys mode toggled by guest.
	sendFromGuest <- []byte("\x1b[?1h")
	// Wait briefly for the emulator to process DECCKM enable.
	time.Sleep(40 * time.Millisecond)
	if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
		"console_id": sid,
		"keys":       []interface{}{"Up", "Home"},
	}); err != nil {
		t.Fatalf("send_vm_console_keys DECCKM: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	fs.mu.Lock()
	var lastInput []byte
	if len(fs.receivedInput) > 0 {
		lastInput = fs.receivedInput[len(fs.receivedInput)-1]
	}
	fs.mu.Unlock()
	if string(lastInput) != "\x1bOA\x1bOH" {
		t.Errorf("DECCKM Up+Home = %q, want %q", lastInput, "\x1bOA\x1bOH")
	}

	close(sendFromGuest)
}

func TestVMConsoleIdleTimeoutAndNonCancellableCleanup(t *testing.T) {
	t.Run("idle timeout closes session even with periodic guest output", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		wsClosed := make(chan struct{}, 1)
		fs.shellHandler = func(conn *websocket.Conn, _ map[string]interface{}, _ func(int, []byte)) {
			ack, _ := json.Marshal(map[string]interface{}{"msg": "connected", "id": "mw-shell-idle"})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte("booting...\r\n"))

			stopWrite := make(chan struct{})
			go func() {
				ticker := time.NewTicker(15 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopWrite:
						return
					case <-ticker.C:
						if err := conn.WriteMessage(websocket.BinaryMessage, []byte(".")); err != nil {
							return
						}
					}
				}
			}()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					close(stopWrite)
					wsClosed <- struct{}{}
					return
				}
			}
		}

		r := fs.newRegistry(t, []string{"debian"})
		r.vmConsoles.settleTimeout = 20 * time.Millisecond
		r.vmConsoles.idleTimeout = 80 * time.Millisecond
		opened := openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
		sid := opened["console_id"].(string)

		select {
		case <-wsClosed:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for idle sweeper to close console WebSocket")
		}

		if _, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{"console_id": sid}); err == nil {
			t.Errorf("expected idle-expired session to be removed")
		}
	})

	t.Run("close_vm_console works with already-cancelled context", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		wsClosed := make(chan struct{}, 1)
		fs.shellHandler = func(conn *websocket.Conn, _ map[string]interface{}, _ func(int, []byte)) {
			ack, _ := json.Marshal(map[string]interface{}{"msg": "connected", "id": "mw-shell-cancel"})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte("ready\r\n"))
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					wsClosed <- struct{}{}
					return
				}
			}
		}

		r := fs.newRegistry(t, []string{"debian"})
		opened := openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
		sid := opened["console_id"].(string)

		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		// Invoke handler directly to simulate request cancellation mid-cleanup.
		if _, err := r.handleCloseVMConsole(cancelledCtx, nil, map[string]interface{}{"console_id": sid}); err != nil {
			t.Fatalf("handleCloseVMConsole with cancelled ctx failed: %v", err)
		}
		select {
		case <-wsClosed:
		case <-time.After(2 * time.Second):
			t.Fatal("close_vm_console did not close underlying WebSocket")
		}
	})

	t.Run("registry.Close shuts down all open sessions", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		wsClosed := make(chan struct{}, 2)
		fs.shellHandler = func(conn *websocket.Conn, _ map[string]interface{}, _ func(int, []byte)) {
			ack, _ := json.Marshal(map[string]interface{}{"msg": "connected", "id": "mw-shell-shutdown"})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte("ready\r\n"))
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					wsClosed <- struct{}{}
					return
				}
			}
		}

		r := fs.newRegistry(t, []string{"debian", "alpine"})
		_ = openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
		_ = openVMConsoleForTest(t, r, map[string]interface{}{"id": 3.0})

		r.Close()
		for i := 0; i < 2; i++ {
			select {
			case <-wsClosed:
			case <-time.After(2 * time.Second):
				t.Fatalf("shutdown closed only %d of 2 sessions", i)
			}
		}
	})
}

func TestVMConsoleNoSecretLeakageInResultsOrLogs(t *testing.T) {
	truenas.SetDebugLogging(true)
	defer truenas.SetDebugLogging(false)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	fs := newVMConsoleFakeServer(t, "25.10.0")
	r := fs.newRegistry(t, []string{"debian"})
	opened := openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
	sid := opened["console_id"].(string)

	const secretPlain = "MyTopSecretRootPass!99"
	const secretSens = "UltraSecretLuksPassphrase#77"

	// Default (sensitive=false): never echoes or logs input.
	out1, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
		"console_id": sid,
		"text":       secretPlain,
		"keys":       []interface{}{"Enter"},
	})
	if err != nil {
		t.Fatalf("send_vm_console_keys (default): %v", err)
	}
	if strings.Contains(out1, secretPlain) {
		t.Errorf("default response leaked sent text: %s", out1)
	}
	if !strings.Contains(out1, `"bytes_sent"`) {
		t.Errorf("expected bytes_sent in non-sensitive response: %s", out1)
	}

	// sensitive=true: never echoes, never logs, and omits byte/key counts.
	out2, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
		"console_id": sid,
		"text":       secretSens,
		"keys":       []interface{}{"Enter"},
		"sensitive":  true,
	})
	if err != nil {
		t.Fatalf("send_vm_console_keys (sensitive): %v", err)
	}
	if strings.Contains(out2, secretSens) {
		t.Errorf("sensitive response leaked sent text: %s", out2)
	}
	if strings.Contains(out2, "bytes_sent") || strings.Contains(out2, "keys_sent") {
		t.Errorf("sensitive response should omit byte and key counts: %s", out2)
	}

	logs := logBuf.String()
	for _, secret := range []string{secretPlain, secretSens, testVMConsoleToken} {
		if strings.Contains(logs, secret) {
			t.Errorf("secret %q leaked into debug logs:\n%s", secret, logs)
		}
	}
	if strings.Contains(fmt.Sprint(opened), testVMConsoleToken) {
		t.Errorf("auth token leaked into open_vm_console output")
	}
}

func TestEncodeVMConsoleInputNamedKeys(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		keys      []string
		appCursor bool
		want      string
	}{
		{"text + Enter", "ls -la", []string{"Enter"}, false, "ls -la\r"},
		{"Return and CR aliases", "", []string{"Return", "CR"}, false, "\r\r"},
		{"Tab and Shift+Tab", "", []string{"Tab", "Shift+Tab", "BackTab"}, false, "\t\x1b[Z\x1b[Z"},
		{"Backspace, Esc, Space", "", []string{"Backspace", "Esc", "Space"}, false, "\x7f\x1b "},
		{"arrows and Home/End normal mode", "", []string{"Up", "Down", "Right", "Left", "Home", "End"}, false, "\x1b[A\x1b[B\x1b[C\x1b[D\x1b[H\x1b[F"},
		{"arrows and Home/End DECCKM app mode", "", []string{"Up", "Down", "Right", "Left", "Home", "End"}, true, "\x1bOA\x1bOB\x1bOC\x1bOD\x1bOH\x1bOF"},
		{"editing and paging keys", "", []string{"Insert", "Delete", "PageUp", "PageDown"}, false, "\x1b[2~\x1b[3~\x1b[5~\x1b[6~"},
		{"function keys F1-F12", "", []string{"F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10", "F11", "F12"}, false,
			"\x1bOP\x1bOQ\x1bOR\x1bOS\x1b[15~\x1b[17~\x1b[18~\x1b[19~\x1b[20~\x1b[21~\x1b[23~\x1b[24~"},
		{"Ctrl combinations", "", []string{"Ctrl+A", "Ctrl+C", "Ctrl+D", "Ctrl+Z", "Control+L", "C-u", "Ctrl+Space", "Ctrl+\\", "Ctrl+]"}, false,
			"\x01\x03\x04\x1a\x0c\x15\x00\x1c\x1d"},
		{"Alt combinations", "", []string{"Alt+x", "Meta+2", "Alt+F2", "Alt+Enter"}, false, "\x1bx\x1b2\x1b\x1bOQ\x1b\r"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeVMConsoleInput(tc.text, tc.keys, tc.appCursor)
			if err != nil {
				t.Fatalf("encodeVMConsoleInput: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVMConsoleCursorKeysModeEncoding(t *testing.T) {
	keyCases := []struct {
		key    string
		normal string
		app    string
	}{
		{"Up", "\x1b[A", "\x1bOA"},
		{"Down", "\x1b[B", "\x1bOB"},
		{"Right", "\x1b[C", "\x1bOC"},
		{"Left", "\x1b[D", "\x1bOD"},
		{"Home", "\x1b[H", "\x1bOH"},
		{"End", "\x1b[F", "\x1bOF"},
	}

	for _, tc := range keyCases {
		t.Run(tc.key+"/normal", func(t *testing.T) {
			appCursor, err := resolveVMConsoleAppCursorKeys(vmConsoleCursorKeysNormal, true)
			if err != nil {
				t.Fatalf("resolveVMConsoleAppCursorKeys: %v", err)
			}
			got, err := encodeVMConsoleInput("", []string{tc.key}, appCursor)
			if err != nil {
				t.Fatalf("encodeVMConsoleInput: %v", err)
			}
			if string(got) != tc.normal {
				t.Errorf("%s normal = %q, want %q", tc.key, got, tc.normal)
			}
		})
		t.Run(tc.key+"/application", func(t *testing.T) {
			appCursor, err := resolveVMConsoleAppCursorKeys(vmConsoleCursorKeysApplication, false)
			if err != nil {
				t.Fatalf("resolveVMConsoleAppCursorKeys: %v", err)
			}
			got, err := encodeVMConsoleInput("", []string{tc.key}, appCursor)
			if err != nil {
				t.Fatalf("encodeVMConsoleInput: %v", err)
			}
			if string(got) != tc.app {
				t.Errorf("%s application = %q, want %q", tc.key, got, tc.app)
			}
		})
	}

	resolveCases := []struct {
		mode    string
		tracked bool
		want    bool
	}{
		{"", false, false},
		{"", true, true},
		{"auto", false, false},
		{"auto", true, true},
		{"normal", false, false},
		{"normal", true, false},
		{"application", false, true},
		{"application", true, true},
	}
	for _, tc := range resolveCases {
		got, err := resolveVMConsoleAppCursorKeys(tc.mode, tc.tracked)
		if err != nil {
			t.Errorf("resolveVMConsoleAppCursorKeys(%q, %v) unexpected err: %v", tc.mode, tc.tracked, err)
			continue
		}
		if got != tc.want {
			t.Errorf("resolveVMConsoleAppCursorKeys(%q, %v) = %v, want %v", tc.mode, tc.tracked, got, tc.want)
		}
	}

	if _, err := resolveVMConsoleAppCursorKeys("SecretBadMode", false); err == nil {
		t.Errorf("expected error for invalid cursor_keys mode")
	} else if strings.Contains(err.Error(), "SecretBadMode") {
		t.Errorf("invalid cursor_keys error echoed caller input: %v", err)
	}

	if got := vmConsoleCursorKeysModeName(false); got != "normal" {
		t.Errorf("vmConsoleCursorKeysModeName(false) = %q, want normal", got)
	}
	if got := vmConsoleCursorKeysModeName(true); got != "application" {
		t.Errorf("vmConsoleCursorKeysModeName(true) = %q, want application", got)
	}
}

func TestVMConsoleCursorKeysOverrideAndModeReporting(t *testing.T) {
	readSnap := func(t *testing.T, r *Registry, args map[string]interface{}) map[string]interface{} {
		t.Helper()
		out, err := r.CallTool(context.Background(), "read_vm_console", args)
		if err != nil {
			t.Fatalf("read_vm_console: %v", err)
		}
		var snap map[string]interface{}
		if err := json.Unmarshal([]byte(out), &snap); err != nil {
			t.Fatalf("unmarshal read_vm_console: %v\n%s", err, out)
		}
		return snap
	}

	t.Run("mid-menu session without DECCKM sends SS3 with cursor_keys=application for that call only", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, []string{"debian"})
		shell := newFakeVMShell()
		// Guest draws a dialog menu without emitting DECCKM (as when attaching mid-menu).
		shell.frames <- []byte("Console Setup\r\n1) Install/Upgrade\r\n2) Shell\r\n3) Reboot\r\n")

		r.vmConsoles.opener = func(context.Context, *truenas.Client, int) (vmShellConn, error) {
			return shell, nil
		}
		r.vmConsoles.settleTimeout = 10 * time.Millisecond
		opened := openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
		sid := opened["console_id"].(string)

		if opened["cursor_keys_mode"] != "normal" {
			t.Errorf("open_vm_console cursor_keys_mode = %v, want normal", opened["cursor_keys_mode"])
		}
		snap := readSnap(t, r, map[string]interface{}{
			"console_id":      sid,
			"wait_for":        "Console Setup",
			"timeout_seconds": 2.0,
		})
		if snap["cursor_keys_mode"] != "normal" {
			t.Errorf("read_vm_console cursor_keys_mode = %v, want normal", snap["cursor_keys_mode"])
		}

		allCursorKeys := []interface{}{"Up", "Down", "Left", "Right", "Home", "End"}

		// Default (auto) sends CSI because no DECCKM sequence was observed.
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id": sid,
			"keys":       allCursorKeys,
		}); err != nil {
			t.Fatalf("send_vm_console_keys default: %v", err)
		}
		// Override with application sends SS3 for this call only.
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id":  sid,
			"keys":        allCursorKeys,
			"cursor_keys": "application",
		}); err != nil {
			t.Fatalf("send_vm_console_keys application: %v", err)
		}
		// Subsequent auto call reverts to tracked normal mode (CSI).
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id":  sid,
			"keys":        []interface{}{"Home", "Down"},
			"cursor_keys": "auto",
		}); err != nil {
			t.Fatalf("send_vm_console_keys auto: %v", err)
		}

		shell.mu.Lock()
		writes := append([][]byte(nil), shell.writes...)
		shell.mu.Unlock()
		want := []string{
			"\x1b[A\x1b[B\x1b[D\x1b[C\x1b[H\x1b[F",
			"\x1bOA\x1bOB\x1bOD\x1bOC\x1bOH\x1bOF",
			"\x1b[H\x1b[B",
		}
		if len(writes) != len(want) {
			t.Fatalf("got %d writes (%q), want %d (%q)", len(writes), writes, len(want), want)
		}
		for i, w := range want {
			if string(writes[i]) != w {
				t.Errorf("write[%d] = %q, want %q", i, writes[i], w)
			}
		}

		// Tracked mode stays normal after the one-shot override.
		snapAfter := readSnap(t, r, map[string]interface{}{"console_id": sid})
		if snapAfter["cursor_keys_mode"] != "normal" {
			t.Errorf("cursor_keys_mode after override = %v, want normal", snapAfter["cursor_keys_mode"])
		}
	})

	t.Run("DECCKM mode changes reported by read_vm_console and overridden by cursor_keys=normal", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, []string{"debian"})
		shell := newFakeVMShell()
		sid := openFakeVMConsole(t, r, shell)

		if snap := readSnap(t, r, map[string]interface{}{"console_id": sid}); snap["cursor_keys_mode"] != "normal" {
			t.Errorf("initial cursor_keys_mode = %v, want normal", snap["cursor_keys_mode"])
		}

		// Guest enables DECCKM.
		shell.frames <- []byte("\x1b[?1hmenu-ready")
		snap := readSnap(t, r, map[string]interface{}{
			"console_id":      sid,
			"wait_for":        "menu-ready",
			"timeout_seconds": 2.0,
		})
		if snap["cursor_keys_mode"] != "application" {
			t.Errorf("after DECCKM enable cursor_keys_mode = %v, want application", snap["cursor_keys_mode"])
		}

		// Auto sends SS3, normal overrides to CSI for that call only, next auto sends SS3 again.
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id": sid,
			"keys":       []interface{}{"Up", "Home", "End"},
		}); err != nil {
			t.Fatalf("send_vm_console_keys auto (app): %v", err)
		}
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id":  sid,
			"keys":        []interface{}{"Up", "Home", "End"},
			"cursor_keys": "normal",
		}); err != nil {
			t.Fatalf("send_vm_console_keys normal override: %v", err)
		}
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id": sid,
			"keys":       []interface{}{"Down"},
		}); err != nil {
			t.Fatalf("send_vm_console_keys after normal override: %v", err)
		}

		// Guest disables DECCKM.
		shell.frames <- []byte("\x1b[?1lshell-ready")
		snap = readSnap(t, r, map[string]interface{}{
			"console_id":      sid,
			"wait_for":        "shell-ready",
			"timeout_seconds": 2.0,
		})
		if snap["cursor_keys_mode"] != "normal" {
			t.Errorf("after DECCKM disable cursor_keys_mode = %v, want normal", snap["cursor_keys_mode"])
		}
		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id": sid,
			"keys":       []interface{}{"Down"},
		}); err != nil {
			t.Fatalf("send_vm_console_keys after DECCKM disable: %v", err)
		}

		shell.mu.Lock()
		writes := append([][]byte(nil), shell.writes...)
		shell.mu.Unlock()
		want := []string{
			"\x1bOA\x1bOH\x1bOF",
			"\x1b[A\x1b[H\x1b[F",
			"\x1bOB",
			"\x1b[B",
		}
		if len(writes) != len(want) {
			t.Fatalf("got %d writes (%q), want %d (%q)", len(writes), writes, len(want), want)
		}
		for i, w := range want {
			if string(writes[i]) != w {
				t.Errorf("write[%d] = %q, want %q", i, writes[i], w)
			}
		}
	})

	t.Run("invalid cursor_keys is rejected without echoing", func(t *testing.T) {
		fs := newVMConsoleFakeServer(t, "25.10.0")
		r := fs.newRegistry(t, []string{"debian"})
		shell := newFakeVMShell()
		sid := openFakeVMConsole(t, r, shell)

		if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{
			"console_id":  sid,
			"keys":        []interface{}{"Down"},
			"cursor_keys": "SecretModeValue99",
		}); err == nil || !strings.Contains(err.Error(), "cursor_keys") {
			t.Fatalf("expected invalid cursor_keys error, got: %v", err)
		} else if strings.Contains(err.Error(), "SecretModeValue99") {
			t.Fatalf("invalid cursor_keys error echoed caller input: %v", err)
		}

		if _, err := r.handleSendVMConsoleKeys(context.Background(), nil, map[string]interface{}{
			"console_id":  sid,
			"keys":        []interface{}{"Down"},
			"cursor_keys": "SecretModeValue99",
		}); err == nil || !strings.Contains(err.Error(), "cursor_keys") {
			t.Fatalf("handler: expected invalid cursor_keys error, got: %v", err)
		} else if strings.Contains(err.Error(), "SecretModeValue99") {
			t.Fatalf("handler: invalid cursor_keys error echoed caller input: %v", err)
		}
	})
}

func TestTruncateUTF8BytesPreservesRunes(t *testing.T) {
	in := "hello 世界!"
	got, truncated := truncateUTF8Bytes(in, 8) // "hello " is 6 bytes, "世" is 3 bytes
	if !truncated || got != "hello " {
		t.Errorf("got (%q, %v), want (%q, true)", got, truncated, "hello ")
	}
}

func TestAuthMeRolesUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:  "list of strings",
			input: `{"roles": ["VM_WRITE", "FULL_ADMIN"]}`,
			want:  []string{"VM_WRITE", "FULL_ADMIN"},
		},
		{
			name:  "empty list",
			input: `{"roles": []}`,
			want:  nil,
		},
		{
			name:  "set object",
			input: `{"roles": {"$set": ["FULL_ADMIN", "VM_WRITE"]}}`,
			want:  []string{"FULL_ADMIN", "VM_WRITE"},
		},
		{
			name:  "empty set object",
			input: `{"roles": {"$set": []}}`,
			want:  nil,
		},
		{
			name:  "set object with null",
			input: `{"roles": {"$set": null}}`,
			want:  nil,
		},
		{
			name:  "null roles",
			input: `{"roles": null}`,
			want:  nil,
		},
		{
			name:  "missing roles",
			input: `{}`,
			want:  nil,
		},
		{
			name:    "invalid string",
			input:   `{"roles": "FULL_ADMIN"}`,
			wantErr: true,
		},
		{
			name:    "invalid number",
			input:   `{"roles": 42}`,
			wantErr: true,
		},
		{
			name:    "invalid boolean",
			input:   `{"roles": true}`,
			wantErr: true,
		},
		{
			name:    "invalid array element",
			input:   `{"roles": [123]}`,
			wantErr: true,
		},
		{
			name:    "invalid empty object",
			input:   `{"roles": {}}`,
			wantErr: true,
		},
		{
			name:    "invalid object wrong key",
			input:   `{"roles": {"other": ["FULL_ADMIN"]}}`,
			wantErr: true,
		},
		{
			name:    "invalid object extra key",
			input:   `{"roles": {"$set": ["FULL_ADMIN"], "extra": true}}`,
			wantErr: true,
		},
		{
			name:    "invalid set non-array value",
			input:   `{"roles": {"$set": "FULL_ADMIN"}}`,
			wantErr: true,
		},
		{
			name:    "invalid set element",
			input:   `{"roles": {"$set": [123]}}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Roles authMeRoles `json:"roles"`
			}
			err := json.Unmarshal([]byte(tc.input), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s, got roles=%v", tc.input, got.Roles)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.input, err)
			}
			if !slices.Equal(got.Roles, tc.want) {
				t.Errorf("roles = %v, want %v", got.Roles, tc.want)
			}
		})
	}
}

func TestFetchAuthMePrivilegeSetRoles(t *testing.T) {
	cases := []struct {
		name           string
		roles          map[string]interface{}
		wantVMWrite    bool
		wantFullAdmin  bool
		wantBlockedStr string
	}{
		{
			name:          "FULL_ADMIN in $set satisfies console role check",
			roles:         map[string]interface{}{"$set": []interface{}{"FULL_ADMIN"}},
			wantFullAdmin: true,
		},
		{
			name:        "VM_WRITE in $set satisfies console role check",
			roles:       map[string]interface{}{"$set": []interface{}{"VM_WRITE"}},
			wantVMWrite: true,
		},
		{
			name:           "VM_READ in $set is blocked on 27.1.0-MASTER",
			roles:          map[string]interface{}{"$set": []interface{}{"VM_READ"}},
			wantBlockedStr: "VM_WRITE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newVMConsoleFakeServer(t, "27.1.0-MASTER")
			fs.authMe["privilege"].(map[string]interface{})["roles"] = tc.roles
			r := fs.newRegistry(t, []string{"debian"})

			priv, err := fetchAuthMePrivilege(context.Background(), r.client)
			if err != nil {
				t.Fatalf("fetchAuthMePrivilege: %v", err)
			}
			if !priv.Present || !priv.WebShell || priv.Username != "truenas_admin" {
				t.Fatalf("unexpected privilege metadata: %+v", priv)
			}
			if got := hasRole(priv.Roles, "VM_WRITE"); got != tc.wantVMWrite {
				t.Errorf("hasRole(VM_WRITE) = %v, want %v (roles=%v)", got, tc.wantVMWrite, priv.Roles)
			}
			if got := hasRole(priv.Roles, "FULL_ADMIN"); got != tc.wantFullAdmin {
				t.Errorf("hasRole(FULL_ADMIN) = %v, want %v (roles=%v)", got, tc.wantFullAdmin, priv.Roles)
			}

			plan, err := r.planOpenVMConsole(context.Background(), r.client, map[string]interface{}{"id": 2.0})
			if err != nil {
				t.Fatalf("planOpenVMConsole: %v", err)
			}
			if tc.wantBlockedStr == "" {
				if len(plan.Blocked) != 0 {
					t.Errorf("expected no blocked reasons, got %v", plan.Blocked)
				}
			} else {
				if len(plan.Blocked) == 0 || !strings.Contains(strings.Join(plan.Blocked, "; "), tc.wantBlockedStr) {
					t.Errorf("expected blocked reason containing %q, got %v", tc.wantBlockedStr, plan.Blocked)
				}
			}
		})
	}
}

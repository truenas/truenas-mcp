package truenas

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testShellToken = "tok-supersecret-shell-token-9876"

type fakeShellServer struct {
	srv         *httptest.Server
	mu          sync.Mutex
	tokenParams []interface{}
	tokenDelay  time.Duration
	shellDials  int
	handshake   map[string]interface{}
	inputFrames []struct {
		msgType int
		data    []byte
	}
}

func startFakeShellServer(t *testing.T, shellHandler func(*websocket.Conn)) *fakeShellServer {
	t.Helper()
	fs := &fakeShellServer{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	mux := http.NewServeMux()
	mux.HandleFunc(apiPath, func(w http.ResponseWriter, r *http.Request) {
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
			var result interface{}
			switch req.Method {
			case "auth.login_ex":
				result = map[string]interface{}{"response_type": "SUCCESS"}
			case "auth.generate_token":
				fs.mu.Lock()
				fs.tokenParams = req.Params
				delay := fs.tokenDelay
				fs.mu.Unlock()
				time.Sleep(delay)
				result = testShellToken
			default:
				result = map[string]interface{}{}
			}
			resp, _ := json.Marshal(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  result,
			})
			_ = conn.WriteMessage(websocket.TextMessage, resp)
		}
	})
	mux.HandleFunc(shellPath, func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		fs.mu.Lock()
		fs.shellDials++
		fs.mu.Unlock()
		shellHandler(conn)
	})

	fs.srv = httptest.NewTLSServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

func newTestShellClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	endpoint := "wss://" + strings.TrimPrefix(srv.URL, "https://") + apiPath
	client, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser})
	if err != nil {
		t.Fatalf("NewClientWithOptions: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	if err := client.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	return client
}

func TestOpenVMShellHandshakeAndBinaryIO(t *testing.T) {
	SetDebugLogging(true)
	defer SetDebugLogging(false)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	inputReceived := make(chan struct{}, 1)
	var fs *fakeShellServer
	fs = startFakeShellServer(t, func(conn *websocket.Conn) {
		mt, raw, err := conn.ReadMessage()
		if err != nil || mt != websocket.TextMessage {
			return
		}
		var hs map[string]interface{}
		if err := json.Unmarshal(raw, &hs); err != nil {
			return
		}
		fs.mu.Lock()
		fs.handshake = hs
		fs.mu.Unlock()

		ack, _ := json.Marshal(map[string]interface{}{
			"msg": "connected",
			"id":  "shell-uuid-1234",
		})
		_ = conn.WriteMessage(websocket.TextMessage, ack)
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("login: "))

		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		fs.mu.Lock()
		fs.inputFrames = append(fs.inputFrames, struct {
			msgType int
			data    []byte
		}{msgType: mt, data: data})
		fs.mu.Unlock()
		inputReceived <- struct{}{}

		// Wait until client closes.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	client := newTestShellClient(t, fs.srv)
	sc, err := client.OpenVMShell(context.Background(), 7)
	if err != nil {
		t.Fatalf("OpenVMShell: %v", err)
	}
	defer func() { _ = sc.Close() }()

	if sc.ID() != "shell-uuid-1234" {
		t.Fatalf("ID() = %q, want %q", sc.ID(), "shell-uuid-1234")
	}

	// Verify auth.generate_token arguments: [60, {}, true, true]
	fs.mu.Lock()
	params := fs.tokenParams
	hs := fs.handshake
	fs.mu.Unlock()

	if len(params) != 4 {
		t.Fatalf("auth.generate_token params = %#v, want 4 args", params)
	}
	if params[0] != float64(shellTokenTTLSeconds) {
		t.Errorf("ttl = %#v, want %d", params[0], shellTokenTTLSeconds)
	}
	attrs, ok := params[1].(map[string]interface{})
	if !ok || len(attrs) != 0 {
		t.Errorf("attrs = %#v, want empty map", params[1])
	}
	if params[2] != true || params[3] != true {
		t.Errorf("match_origin=%v single_use=%v, want true, true", params[2], params[3])
	}

	// Verify handshake payload.
	if hs["token"] != testShellToken {
		t.Errorf("handshake token mismatch")
	}
	opts, _ := hs["options"].(map[string]interface{})
	if opts == nil || opts["vm_id"] != float64(7) {
		t.Errorf("handshake options = %#v, want vm_id=7", opts)
	}

	// Verify PTY read and binary input write.
	mt, frame, err := sc.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if mt != websocket.BinaryMessage || string(frame) != "login: " {
		t.Errorf("ReadFrame = (%d, %q), want (Binary, %q)", mt, frame, "login: ")
	}
	if err := sc.WriteInput([]byte("root\r")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	select {
	case <-inputReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shell server to receive input")
	}
	fs.mu.Lock()
	frames := fs.inputFrames
	fs.mu.Unlock()
	if len(frames) != 1 || frames[0].msgType != websocket.BinaryMessage || string(frames[0].data) != "root\r" {
		t.Errorf("server received frames = %#v, want 1 binary frame %q", frames, "root\r")
	}

	// Verify the auth token never appeared in debug logs!
	logs := logBuf.String()
	if strings.Contains(logs, testShellToken) {
		t.Errorf("shell auth token leaked into debug logs:\n%s", logs)
	}
	if !strings.Contains(logs, "[REDACTED]") {
		t.Errorf("expected redacted response marker in debug logs:\n%s", logs)
	}
}

func TestOpenVMShellAuthFailureScrubsToken(t *testing.T) {
	fs := startFakeShellServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
		reply, _ := json.Marshal(map[string]interface{}{
			"msg": "failed",
			"error": map[string]interface{}{
				"error":  13,
				"reason": "Missing required role for VM shell: VM_WRITE (token=" + testShellToken + ")",
			},
		})
		_ = conn.WriteMessage(websocket.TextMessage, reply)
	})

	client := newTestShellClient(t, fs.srv)
	_, err := client.OpenVMShell(context.Background(), 3)
	if err == nil {
		t.Fatal("expected OpenVMShell to fail")
	}
	var authErr *ShellAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *ShellAuthError, got %T: %v", err, err)
	}
	if authErr.Code != 13 {
		t.Errorf("Code = %d, want 13", authErr.Code)
	}
	if !strings.Contains(authErr.Reason, "VM_WRITE") {
		t.Errorf("Reason = %q, want VM_WRITE", authErr.Reason)
	}
	if strings.Contains(err.Error(), testShellToken) {
		t.Errorf("auth token leaked into error: %v", err)
	}
}

func TestOpenVMShellAbruptCloseDuringHandshake(t *testing.T) {
	fs := startFakeShellServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, ""),
			time.Now().Add(time.Second),
		)
	})

	client := newTestShellClient(t, fs.srv)
	_, err := client.OpenVMShell(context.Background(), 3)
	if err == nil {
		t.Fatal("expected error when server closes during handshake")
	}
	if !strings.Contains(err.Error(), "closed the VM serial console connection") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOpenVMShellRejectsInvalidVMID(t *testing.T) {
	// The server would accept the handshake: without vm_id TrueNAS opens a
	// host login shell, so the check must refuse before minting a token or
	// dialing.
	fs := startFakeShellServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
		ack, _ := json.Marshal(map[string]interface{}{"msg": "connected", "id": "host-shell"})
		_ = conn.WriteMessage(websocket.TextMessage, ack)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	client := newTestShellClient(t, fs.srv)
	for _, id := range []int{0, -1} {
		sc, err := client.OpenVMShell(context.Background(), id)
		if err == nil {
			_ = sc.Close()
			t.Fatalf("expected error for vmID=%d", id)
		}
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.tokenParams != nil || fs.shellDials != 0 {
		t.Errorf("invalid vmID minted a token (%v) or dialed the shell (%d times)", fs.tokenParams != nil, fs.shellDials)
	}
}

func TestOpenVMShellRejectsEmptyShellID(t *testing.T) {
	for _, id := range []string{"", "   "} {
		fs := startFakeShellServer(t, func(conn *websocket.Conn) {
			_, _, _ = conn.ReadMessage()
			ack, _ := json.Marshal(map[string]interface{}{"msg": "connected", "id": id})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
		})
		client := newTestShellClient(t, fs.srv)
		sc, err := client.OpenVMShell(context.Background(), 3)
		if err == nil {
			_ = sc.Close()
			t.Fatalf("expected error for shell id %q", id)
		}
		if !strings.Contains(err.Error(), "without a shell id") {
			t.Errorf("shell id %q: unexpected error: %v", id, err)
		}
	}
}

func TestFormatShellDialErrorScrubsSecrets(t *testing.T) {
	c := &Client{apiKey: testAPIKey}
	target := &url.URL{Scheme: "wss", Host: "192.0.2.10", Path: shellPath}
	scrub := func(s string) string {
		return c.scrubSecrets(strings.ReplaceAll(s, testShellToken, "[REDACTED]"))
	}
	raw := errors.New("dial tcp 192.0.2.10:443: refused (token=" + testShellToken + " key=" + testAPIKey + ")")
	err := c.formatShellDialError(target, nil, nil, raw, scrub)
	for _, secret := range []string{testShellToken, testAPIKey} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("dial error leaked %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("dial error lost its cause: %v", err)
	}
}

func TestLateShellTokenResponseIsMaskedInDebugLogs(t *testing.T) {
	SetDebugLogging(true)
	defer SetDebugLogging(false)
	logs := &syncBuffer{}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)

	fs := startFakeShellServer(t, func(*websocket.Conn) {})
	client := newTestShellClient(t, fs.srv)
	fs.mu.Lock()
	fs.tokenDelay = 300 * time.Millisecond
	fs.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.OpenVMShell(ctx, 3); err == nil {
		t.Fatal("expected the token request to time out")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), "unknown request ID") {
		if time.Now().After(deadline) {
			t.Fatalf("late token response never arrived:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(logs.String(), testShellToken) {
		t.Errorf("late shell token response logged unmasked:\n%s", logs.String())
	}
}

func TestAbandonPendingBoundsLateSensitive(t *testing.T) {
	c := &Client{pending: make(map[string]*pendingCall)}
	for i := 0; i < maxLateSensitive+6; i++ {
		key := strconv.Itoa(i)
		c.pending[key] = &pendingCall{sensitiveResult: true}
		c.abandonPending(key)
	}
	c.pending["plain"] = &pendingCall{}
	c.abandonPending("plain")
	if len(c.lateSensitive) != maxLateSensitive {
		t.Fatalf("lateSensitive holds %d entries, want %d", len(c.lateSensitive), maxLateSensitive)
	}
	if c.lateSensitive[len(c.lateSensitive)-1] != strconv.Itoa(maxLateSensitive+5) || slices.Contains(c.lateSensitive, "0") || slices.Contains(c.lateSensitive, "plain") {
		t.Errorf("lateSensitive should keep the newest sensitive keys only: %v", c.lateSensitive)
	}
	if len(c.pending) != 0 {
		t.Errorf("abandoned calls still pending: %d", len(c.pending))
	}
}

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/truenas/truenas-mcp/truenas"
)

// fakeVMShell is an in-memory vmShellConn. Guest output is fed through
// frames; closing frames (or calling drop) ends the stream as the remote side
// would.
type fakeVMShell struct {
	frames     chan []byte
	closed     chan struct{}
	closeOnce  sync.Once
	closeCalls atomic.Int32
	failWrites atomic.Bool

	mu     sync.Mutex
	writes [][]byte
}

func newFakeVMShell() *fakeVMShell {
	return &fakeVMShell{frames: make(chan []byte, 16), closed: make(chan struct{})}
}

func (f *fakeVMShell) ID() string { return "mw-shell-fake" }

func (f *fakeVMShell) ReadFrame() (int, []byte, error) {
	select {
	case b, ok := <-f.frames:
		if !ok {
			return 0, nil, io.EOF
		}
		return websocket.BinaryMessage, b, nil
	case <-f.closed:
		return 0, nil, errors.New("use of closed network connection")
	}
}

func (f *fakeVMShell) WriteInput(data []byte) error {
	if f.failWrites.Load() {
		return errors.New("write deadline exceeded")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), data...))
	return nil
}

func (f *fakeVMShell) Close() error {
	f.closeCalls.Add(1)
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeVMShell) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-f.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("console shell was never closed locally (session leaked)")
	}
}

// openFakeVMConsole opens a console on VM 2 ("debian") through the tool layer
// with the WebSocket replaced by shell, and returns the session id.
func openFakeVMConsole(t *testing.T, r *Registry, shell vmShellConn) string {
	t.Helper()
	r.vmConsoles.opener = func(context.Context, *truenas.Client, int) (vmShellConn, error) {
		return shell, nil
	}
	r.vmConsoles.settleTimeout = 10 * time.Millisecond
	opened := openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
	sid, _ := opened["session_id"].(string)
	if sid == "" {
		t.Fatalf("open_vm_console returned no session_id: %v", opened)
	}
	return sid
}

func managedSession(t *testing.T, r *Registry, sid string) *vmConsoleSession {
	t.Helper()
	r.vmConsoles.mu.Lock()
	defer r.vmConsoles.mu.Unlock()
	s := r.vmConsoles.sessions[sid]
	if s == nil {
		t.Fatalf("session %s is not listed by the manager", sid)
	}
	return s
}

func waitChan(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// readEnded polls read_vm_console until it reports connected=false and
// returns the parsed result.
func readEnded(t *testing.T, r *Registry, sid string) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		out, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{"session_id": sid})
		if err != nil {
			t.Fatalf("read_vm_console after the remote end: %v (want connected=false and a close_reason)", err)
		}
		var snap map[string]interface{}
		if err := json.Unmarshal([]byte(out), &snap); err != nil {
			t.Fatal(err)
		}
		if snap["connected"] == false {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("session still reports connected after the remote end: %s", out)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func callWithin(t *testing.T, d time.Duration, what string, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(d):
		t.Fatalf("%s did not return within %s (deadlock)", what, d)
	}
}

func TestVMConsoleRemoteEndTearsDownSession(t *testing.T) {
	fs := newVMConsoleFakeServer(t, "25.10.0")
	r := fs.newRegistry(t, []string{"debian"})
	shell := newFakeVMShell()
	sid := openFakeVMConsole(t, r, shell)
	s := managedSession(t, r, sid)

	shell.frames <- []byte("Debian GNU/Linux\r\nlogin: ")
	close(shell.frames) // virsh exited / VM powered off / Ctrl+]

	shell.waitClosed(t)
	waitChan(t, s.readerDone, "reader exit")
	waitChan(t, s.drainerDone, "reply drainer exit")

	snap := readEnded(t, r, sid)
	if snap["close_reason"] != "console stream closed by remote" {
		t.Errorf("close_reason = %v, want console stream closed by remote", snap["close_reason"])
	}
	if !strings.Contains(fmt.Sprint(snap["screen"]), "login:") {
		t.Errorf("ended session lost its last screen: %v", snap["screen"])
	}

	// The VM's slot is free again, so a new console can be opened.
	if err := r.vmConsoles.checkCapacity(2, "debian"); err != nil {
		t.Errorf("slot still held after the remote end: %v", err)
	}
	if _, err := r.CallTool(context.Background(), "send_vm_console_keys", map[string]interface{}{"session_id": sid, "text": "x"}); err == nil || !strings.Contains(err.Error(), "is closed (console stream closed by remote)") {
		t.Errorf("send on an ended session: err = %v, want closed with the reason", err)
	}

	if _, err := r.CallTool(context.Background(), "close_vm_console", map[string]interface{}{"session_id": sid}); err != nil {
		t.Fatalf("close_vm_console on an ended session: %v", err)
	}
	if _, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{"session_id": sid}); err == nil {
		t.Error("closed session is still listed")
	}
	if n := shell.closeCalls.Load(); n < 1 {
		t.Errorf("shell Close calls = %d", n)
	}
}

func TestVMConsoleRemoteEndOverWebSocket(t *testing.T) {
	cases := []struct {
		name string
		end  func(conn *websocket.Conn)
	}{
		{"shell exits with a close frame", func(conn *websocket.Conn) {
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		}},
		{"connection dropped", func(conn *websocket.Conn) {
			_ = conn.NetConn().Close()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newVMConsoleFakeServer(t, "25.10.0")
			endShell := make(chan struct{})
			fs.shellHandler = func(conn *websocket.Conn, _ map[string]interface{}, _ func(int, []byte)) {
				ack, _ := json.Marshal(map[string]interface{}{"msg": "connected", "id": "mw-shell-end"})
				_ = conn.WriteMessage(websocket.TextMessage, ack)
				_ = conn.WriteMessage(websocket.BinaryMessage, []byte("login: "))
				<-endShell
				tc.end(conn)
			}
			r := fs.newRegistry(t, []string{"debian"})
			r.vmConsoles.settleTimeout = 10 * time.Millisecond
			opened := openVMConsoleForTest(t, r, map[string]interface{}{"id": 2.0})
			sid := opened["session_id"].(string)
			s := managedSession(t, r, sid)

			close(endShell)
			snap := readEnded(t, r, sid)
			if snap["close_reason"] != "console stream closed by remote" {
				t.Errorf("close_reason = %v", snap["close_reason"])
			}
			waitChan(t, s.readerDone, "reader exit")
			waitChan(t, s.drainerDone, "reply drainer exit")
			callWithin(t, 2*time.Second, "registry shutdown", func() error { r.vmConsoles.Close(); return nil })
		})
	}
}

func TestVMConsoleFailedReplyWriteDoesNotHang(t *testing.T) {
	fs := newVMConsoleFakeServer(t, "25.10.0")
	r := fs.newRegistry(t, []string{"debian"})
	shell := newFakeVMShell()
	sid := openFakeVMConsole(t, r, shell)
	s := managedSession(t, r, sid)

	shell.failWrites.Store(true)
	// Two cursor position queries: the first reply write fails, the second
	// used to block emu.Write forever with the session lock held.
	shell.frames <- []byte("\x1b[6n")
	shell.frames <- []byte("\x1b[6n")
	shell.frames <- []byte("\x1b[c")

	callWithin(t, 2*time.Second, "read_vm_console", func() error {
		_, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{"session_id": sid})
		return err
	})
	callWithin(t, 2*time.Second, "close_vm_console", func() error {
		_, err := r.CallTool(context.Background(), "close_vm_console", map[string]interface{}{"session_id": sid})
		if err != nil && strings.Contains(err.Error(), "not found") {
			return nil // already ended and dropped is fine; hanging is not
		}
		return err
	})
	shell.waitClosed(t)
	waitChan(t, s.readerDone, "reader exit")
	waitChan(t, s.drainerDone, "reply drainer exit")
	callWithin(t, 2*time.Second, "idle sweep", func() error { r.vmConsoles.sweepIdle(time.Now().Add(time.Hour)); return nil })
}

func TestVMConsoleFailedReplyWriteEndsSession(t *testing.T) {
	m := newVMConsoleManager()
	t.Cleanup(m.Close)
	shell := newFakeVMShell()
	shell.failWrites.Store(true)
	s := registerTestSession(t, m, 2, shell)

	shell.frames <- []byte("\x1b[6n")
	shell.waitClosed(t)
	waitChan(t, s.forwarderDone, "reply forwarder exit")
	snap := s.snapshotLockedCopy(false, false)
	if snap.Connected || snap.CloseReason != "write failed" {
		t.Errorf("after a failed reply write: connected=%v reason=%q, want false/write failed", snap.Connected, snap.CloseReason)
	}
	if _, err := m.getSession(s.id); err != nil {
		t.Errorf("ended session should stay readable: %v", err)
	}
}

func registerTestSession(t *testing.T, m *vmConsoleManager, vmID int, shell vmShellConn) *vmConsoleSession {
	t.Helper()
	if err := m.reserveSlot(vmID, "vm"); err != nil {
		t.Fatal(err)
	}
	id, err := generateVMConsoleSessionID()
	if err != nil {
		t.Fatal(err)
	}
	s := newVMConsoleSession(id, vmID, "vm", "dom", 80, 24, shell, m.sessionEnded)
	if err := m.registerSession(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVMConsoleEndedSessionsAreBounded(t *testing.T) {
	m := newVMConsoleManager()
	m.maxSessions = vmConsoleMaxEndedSessions + 4
	t.Cleanup(m.Close)

	var first *vmConsoleSession
	for i := 0; i < vmConsoleMaxEndedSessions+3; i++ {
		shell := newFakeVMShell()
		s := registerTestSession(t, m, 100+i, shell)
		if first == nil {
			first = s
		}
		close(shell.frames)
		shell.waitClosed(t)
		// Wait until the manager has recorded the end, so ages are ordered.
		deadline := time.Now().Add(2 * time.Second)
		for {
			m.mu.Lock()
			_, ok := m.ended[s.id]
			m.mu.Unlock()
			if ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("session end was never recorded")
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(2 * time.Millisecond)
	}
	m.mu.Lock()
	listed, ended, inFlight := len(m.sessions), len(m.ended), len(m.vmInFlight)
	m.mu.Unlock()
	if listed != vmConsoleMaxEndedSessions || ended != vmConsoleMaxEndedSessions || inFlight != 0 {
		t.Errorf("listed=%d ended=%d inFlight=%d, want %d/%d/0", listed, ended, inFlight, vmConsoleMaxEndedSessions, vmConsoleMaxEndedSessions)
	}
	if _, err := m.getSession(first.id); err == nil {
		t.Error("oldest ended session was not dropped")
	}
}

func TestVMConsoleIdleSweepDropsEndedSessions(t *testing.T) {
	m := newVMConsoleManager()
	m.idleTimeout = time.Hour // keep the background sweeper out of the way
	t.Cleanup(m.Close)
	shell := newFakeVMShell()
	s := registerTestSession(t, m, 2, shell)
	close(shell.frames)
	shell.waitClosed(t)
	waitChan(t, s.readerDone, "reader exit")

	m.sweepIdle(time.Now())
	if _, err := m.getSession(s.id); err != nil {
		t.Fatalf("ended session dropped before its retention time: %v", err)
	}
	m.sweepIdle(time.Now().Add(2 * time.Hour))
	if _, err := m.getSession(s.id); err == nil {
		t.Fatal("idle sweep did not drop the ended session")
	}
}

func TestVMConsoleShutdownReachesEndedSessions(t *testing.T) {
	m := newVMConsoleManager()
	active, ended := newFakeVMShell(), newFakeVMShell()
	registerTestSession(t, m, 2, active)
	s := registerTestSession(t, m, 3, ended)
	close(ended.frames)
	waitChan(t, s.readerDone, "reader exit")

	callWithin(t, 2*time.Second, "manager shutdown", func() error { m.Close(); return nil })
	active.waitClosed(t)
	ended.waitClosed(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) != 0 || len(m.ended) != 0 || len(m.vmInFlight) != 0 {
		t.Errorf("shutdown left sessions=%d ended=%d inFlight=%d", len(m.sessions), len(m.ended), len(m.vmInFlight))
	}
}

func TestVMConsoleCloseIsIdempotentAndConcurrent(t *testing.T) {
	m := newVMConsoleManager()
	t.Cleanup(m.Close)
	shell := newFakeVMShell()
	s := registerTestSession(t, m, 2, shell)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Close("closed by client")
		}()
	}
	wg.Wait()
	waitChan(t, s.drainerDone, "reply drainer exit")
	waitChan(t, s.forwarderDone, "reply forwarder exit")
	if n := shell.closeCalls.Load(); n != 1 {
		t.Errorf("shell Close calls = %d, want exactly 1", n)
	}
	if snap := s.snapshotLockedCopy(false, false); snap.CloseReason != "closed by client" {
		t.Errorf("close_reason = %q, want the local reason", snap.CloseReason)
	}
}

func TestVMConsoleReserveSlotGuards(t *testing.T) {
	m := newVMConsoleManager()
	m.maxSessions = 2
	t.Cleanup(m.Close)

	if err := m.reserveSlot(2, "debian"); err != nil {
		t.Fatal(err)
	}
	// Two confirms racing for the same VM: the second must lose.
	if err := m.reserveSlot(2, "debian"); err == nil || !strings.Contains(err.Error(), "already opening") {
		t.Errorf("second reserve for the same VM: err = %v, want already opening", err)
	}
	m.mu.Lock()
	m.vmInFlight[2] = "vmcon_x"
	m.mu.Unlock()
	if err := m.reserveSlot(2, "debian"); err == nil || !strings.Contains(err.Error(), "already has an active") {
		t.Errorf("reserve for a VM with a session: err = %v", err)
	}
	if err := m.reserveSlot(3, "alpine"); err != nil {
		t.Fatal(err)
	}
	if err := m.reserveSlot(4, "freebsd"); err == nil || !strings.Contains(err.Error(), "maximum concurrent") {
		t.Errorf("reserve over the cap: err = %v, want maximum concurrent", err)
	}
	m.releaseReservation(3)
	if err := m.reserveSlot(4, "freebsd"); err != nil {
		t.Errorf("reserve after a release: %v", err)
	}
	m.Close()
	if err := m.reserveSlot(5, "x"); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Errorf("reserve after shutdown: err = %v", err)
	}
}

func TestVMConsoleReadResetsIdleTimer(t *testing.T) {
	m := newVMConsoleManager()
	t.Cleanup(m.Close)
	s := registerTestSession(t, m, 2, newFakeVMShell())
	const timeout = time.Minute

	old := time.Now().Add(-2 * timeout)
	s.mu.Lock()
	s.lastActivity = old
	s.mu.Unlock()
	if !s.isIdle(time.Now(), timeout) {
		t.Fatal("precondition: session should be idle")
	}
	if _, err := s.readScreen(context.Background(), "", nil, 0); err != nil {
		t.Fatal(err)
	}
	if s.isIdle(time.Now(), timeout) {
		t.Error("read_vm_console did not reset the idle timer")
	}

	// A wait that times out counts as activity when it ends, not only when
	// it starts.
	start := time.Now()
	snap, err := s.readScreen(context.Background(), "never-appears", nil, 60*time.Millisecond)
	if err != nil || !snap.TimedOut {
		t.Fatalf("wait: snap=%+v err=%v", snap, err)
	}
	s.mu.Lock()
	last := s.lastActivity
	s.mu.Unlock()
	if last.Sub(start) < 50*time.Millisecond {
		t.Errorf("timed-out wait did not refresh activity at its end (last activity %s after start)", last.Sub(start))
	}
}

func TestEncodeVMConsoleInputLimits(t *testing.T) {
	if _, err := encodeVMConsoleInput(strings.Repeat("a", vmConsoleMaxPayloadBytes), nil, false); err != nil {
		t.Errorf("4096-byte text rejected: %v", err)
	}
	if _, err := encodeVMConsoleInput(strings.Repeat("a", vmConsoleMaxPayloadBytes+1), nil, false); err == nil {
		t.Error("4097-byte text without keys was accepted")
	}
	if _, err := encodeVMConsoleInput(strings.Repeat("a", vmConsoleMaxPayloadBytes), []string{"Enter"}, false); err == nil {
		t.Error("4096-byte text plus a key was accepted")
	}
	keys := make([]string, vmConsoleMaxKeys+1)
	for i := range keys {
		keys[i] = "Enter"
	}
	if _, err := encodeVMConsoleInput("", keys[:vmConsoleMaxKeys], false); err != nil {
		t.Errorf("256 keys rejected: %v", err)
	}
	if _, err := encodeVMConsoleInput("", keys, false); err == nil {
		t.Error("257 keys were accepted")
	}
}

func TestVMConsoleSendOnClosedSession(t *testing.T) {
	m := newVMConsoleManager()
	t.Cleanup(m.Close)
	shell := newFakeVMShell()
	s := registerTestSession(t, m, 2, shell)
	s.Close("closed by client")

	if _, _, err := s.sendInput("secret", []string{"Enter"}); err == nil || !strings.Contains(err.Error(), "is closed (closed by client)") {
		t.Errorf("send on a closed session: err = %v", err)
	}
	shell.mu.Lock()
	defer shell.mu.Unlock()
	if len(shell.writes) != 0 {
		t.Errorf("send on a closed session wrote %d frame(s) to the shell", len(shell.writes))
	}
}

// blockingVMShell's WriteInput blocks until the shell is closed, like a
// write to a peer that stopped reading.
type blockingVMShell struct{ *fakeVMShell }

func (b blockingVMShell) WriteInput([]byte) error {
	<-b.closed
	return errors.New("use of closed network connection")
}

func TestVMConsoleSlowReplyWritesDoNotStallReads(t *testing.T) {
	fs := newVMConsoleFakeServer(t, "25.10.0")
	r := fs.newRegistry(t, []string{"debian"})
	shell := blockingVMShell{newFakeVMShell()}
	sid := openFakeVMConsole(t, r, shell)

	// Far more queries than the reply queue holds while the first reply
	// write is stuck.
	go func() {
		for i := 0; i < 4*vmConsoleReplyQueue; i++ {
			select {
			case shell.frames <- []byte("\x1b[6n"):
			case <-shell.closed:
				return
			}
		}
		select {
		case shell.frames <- []byte("ready> "):
		case <-shell.closed:
		}
	}()
	callWithin(t, 3*time.Second, "read_vm_console wait_for", func() error {
		out, err := r.CallTool(context.Background(), "read_vm_console", map[string]interface{}{
			"session_id": sid, "wait_for": "ready>", "timeout_seconds": 2.0,
		})
		if err == nil && !strings.Contains(out, `"matched": true`) {
			return fmt.Errorf("guest output after the queries never rendered: %s", out)
		}
		return err
	})
	callWithin(t, 3*time.Second, "close_vm_console", func() error {
		_, err := r.CallTool(context.Background(), "close_vm_console", map[string]interface{}{"session_id": sid})
		return err
	})
}

func TestVMConsoleFailedSendEndsSession(t *testing.T) {
	m := newVMConsoleManager()
	t.Cleanup(m.Close)
	shell := newFakeVMShell()
	s := registerTestSession(t, m, 2, shell)
	shell.failWrites.Store(true)

	if _, _, err := s.sendInput("x", nil); err == nil {
		t.Fatal("expected the failed write to be reported")
	}
	shell.waitClosed(t)
	waitChan(t, s.readerDone, "reader exit")
	if snap := s.snapshotLockedCopy(false, false); snap.Connected || snap.CloseReason != "write failed" {
		t.Errorf("connected=%v reason=%q, want false/write failed", snap.Connected, snap.CloseReason)
	}
	if err := m.checkCapacity(2, "vm"); err != nil {
		t.Errorf("slot still held after a failed send: %v", err)
	}
}

func TestVMConsoleEndSessionMarksClosedOnce(t *testing.T) {
	var ended atomic.Int32
	shell := newFakeVMShell()
	s := newVMConsoleSession("vmcon_test", 2, "vm", "dom", 80, 24, shell, func(string) { ended.Add(1) })
	s.endSession("console stream closed by remote")
	if snap := s.snapshotLockedCopy(false, false); snap.Connected || snap.CloseReason != "console stream closed by remote" {
		t.Errorf("right after endSession: connected=%v reason=%q", snap.Connected, snap.CloseReason)
	}
	s.endSession("write failed")
	shell.waitClosed(t)
	waitChan(t, s.readerDone, "reader exit")
	if n := ended.Load(); n != 1 {
		t.Errorf("onEnded called %d times, want 1", n)
	}
	if snap := s.snapshotLockedCopy(false, false); snap.CloseReason != "console stream closed by remote" {
		t.Errorf("close_reason = %q, want the first reason", snap.CloseReason)
	}
}

package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/truenas/truenas-mcp/internal/safego"
	"github.com/truenas/truenas-mcp/truenas"
)

const (
	defaultVMConsoleCols          = 80
	defaultVMConsoleRows          = 24
	minVMConsoleCols              = 40
	maxVMConsoleCols              = 240
	minVMConsoleRows              = 10
	maxVMConsoleRows              = 100
	defaultVMConsoleMaxSessions   = 4
	defaultVMConsoleIdleTimeout   = 10 * time.Minute
	defaultVMConsoleScrollback    = 200
	vmConsoleMaxOutputBytes       = 32 * 1024
	vmConsoleMaxWaitPatternBytes  = 512
	defaultVMConsoleWaitTimeout   = 10 * time.Second
	maxVMConsoleWaitTimeout       = 60 * time.Second
	defaultVMConsoleSettleTimeout = 150 * time.Millisecond
	// vmConsoleMaxEndedSessions bounds how many sessions that ended on the
	// TrueNAS side stay listed (readable with connected=false and a
	// close_reason) until close_vm_console, the idle sweep or shutdown
	// drops them. The oldest is dropped first.
	vmConsoleMaxEndedSessions = 16
	// vmConsoleReplyQueue bounds terminal query replies waiting to be sent
	// back to the guest. Replies beyond it are dropped so the emulator never
	// stalls on a slow or broken connection.
	vmConsoleReplyQueue = 16
)

// vmShellConn is the subset of *truenas.ShellConn used by vmConsoleSession.
type vmShellConn interface {
	ID() string
	ReadFrame() (int, []byte, error)
	WriteInput(data []byte) error
	Close() error
}

// vmShellOpener dials and authenticates a VM serial console WebSocket.
type vmShellOpener func(ctx context.Context, client *truenas.Client, vmID int) (vmShellConn, error)

func defaultVMShellOpener(ctx context.Context, client *truenas.Client, vmID int) (vmShellConn, error) {
	return client.OpenVMShell(ctx, vmID)
}

// vmConsoleSnapshot is a point-in-time view of a VM console's rendered screen.
type vmConsoleSnapshot struct {
	SessionID    string
	VMID         int
	VMName       string
	Cols         int
	Rows         int
	CursorRow    int
	CursorCol    int
	CursorHidden bool
	AltScreen    bool
	Screen       string
	Truncated    bool
	Matched      bool
	TimedOut     bool
	Connected    bool
	CloseReason  string
}

type vmConsoleSession struct {
	id       string
	vmID     int
	vmName   string
	domain   string
	cols     int
	rows     int
	shell    vmShellConn
	emu      *vt.Emulator
	openedAt time.Time
	// onEnded is called once when the session ends on its own (the remote
	// side closed it, a write failed or a goroutine panicked), so the
	// manager can release the VM's slot while keeping the session readable.
	onEnded func(sessionID string)

	mu            sync.Mutex
	lastActivity  time.Time
	appCursorKeys bool
	changed       chan struct{}
	closed        bool
	closeReason   string

	closeOnce     sync.Once
	replies       chan []byte
	readerDone    chan struct{}
	drainerDone   chan struct{}
	forwarderDone chan struct{}
}

func newVMConsoleSession(id string, vmID int, vmName, domain string, cols, rows int, shell vmShellConn, onEnded func(string)) *vmConsoleSession {
	emu := vt.NewEmulator(cols, rows)
	// Bound scrollback memory; vt's default is 10,000 lines per session.
	emu.SetScrollbackSize(defaultVMConsoleScrollback)

	now := time.Now()
	s := &vmConsoleSession{
		id:            id,
		vmID:          vmID,
		vmName:        vmName,
		domain:        domain,
		cols:          cols,
		rows:          rows,
		shell:         shell,
		emu:           emu,
		openedAt:      now,
		lastActivity:  now,
		onEnded:       onEnded,
		changed:       make(chan struct{}),
		replies:       make(chan []byte, vmConsoleReplyQueue),
		readerDone:    make(chan struct{}),
		drainerDone:   make(chan struct{}),
		forwarderDone: make(chan struct{}),
	}

	// Track DECCKM (Application Cursor Keys) so arrow/Home/End encoding
	// matches what full-screen installers and TUI menus expect.
	emu.SetCallbacks(vt.Callbacks{
		EnableMode: func(mode ansi.Mode) {
			if mode == ansi.ModeCursorKeys {
				s.appCursorKeys = true
			}
		},
		DisableMode: func(mode ansi.Mode) {
			if mode == ansi.ModeCursorKeys {
				s.appCursorKeys = false
			}
		},
	})

	go s.forwardReplies()
	go s.drainEmulatorReplies()
	go s.readLoop()
	return s
}

// drainEmulatorReplies reads terminal query responses (such as DSR cursor
// position reports) from vt.Emulator's unbuffered internal pipe and queues
// them for forwardReplies. It never blocks on the network, so emu.Write
// (called by readLoop under s.mu) cannot stall; a reply that does not fit in
// the queue is dropped. It returns once the pipe is closed.
func (s *vmConsoleSession) drainEmulatorReplies() {
	defer close(s.drainerDone)
	defer close(s.replies)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("vm console reply drainer panicked (session=%s): %s\n%s", s.id, safego.Describe(r), debug.Stack())
			// Nothing reads the pipe any more: close it so emu.Write fails
			// instead of blocking.
			s.closeEmulatorInput()
			s.endSession("internal reply drainer error")
		}
	}()
	buf := make([]byte, 1024)
	for {
		n, err := s.emu.Read(buf)
		if n > 0 {
			reply := make([]byte, n)
			copy(reply, buf[:n])
			select {
			case s.replies <- reply:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

// forwardReplies writes queued terminal query replies to the guest PTY. After
// a failed write it ends the session and discards the rest of the queue until
// drainEmulatorReplies closes it.
func (s *vmConsoleSession) forwardReplies() {
	defer close(s.forwarderDone)
	failed := false
	defer func() {
		if r := recover(); r != nil {
			log.Printf("vm console reply forwarder panicked (session=%s): %s\n%s", s.id, safego.Describe(r), debug.Stack())
			s.endSession("internal reply forwarder error")
		}
	}()
	for reply := range s.replies {
		if failed {
			continue
		}
		if err := s.shell.WriteInput(reply); err != nil {
			failed = true
			s.endSession("write failed")
		}
	}
}

func (s *vmConsoleSession) readLoop() {
	reason := "console stream closed by remote"
	defer func() {
		if r := recover(); r != nil {
			log.Printf("vm console reader panicked (session=%s): %s\n%s", s.id, safego.Describe(r), debug.Stack())
			reason = "internal reader error"
		}
		close(s.readerDone)
		s.endSession(reason)
	}()
	for {
		_, data, err := s.shell.ReadFrame()
		if err != nil {
			return
		}
		if len(data) == 0 {
			continue
		}
		s.mu.Lock()
		if !s.closed {
			_, _ = s.emu.Write(data)
			s.notifyLocked()
		}
		s.mu.Unlock()
	}
}

// endSession records that the console ended without a local Close (the
// remote side closed it, the connection dropped, a write failed or a
// goroutine panicked), tells the manager so it can free the VM's slot, and
// tears the session down in the background. Only the first call has any
// effect, and none after a local Close.
func (s *vmConsoleSession) endSession(reason string) {
	s.mu.Lock()
	first := !s.closed
	if first {
		s.closed = true
		if s.closeReason == "" {
			s.closeReason = reason
		}
		s.notifyLocked()
	}
	s.mu.Unlock()
	if !first {
		return
	}
	if s.onEnded != nil {
		s.onEnded(s.id)
	}
	// Close waits for readLoop and the reply goroutines, any of which may be
	// the caller, so it runs in its own goroutine.
	go s.Close(reason)
}

func (s *vmConsoleSession) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// closeEmulatorInput closes vt.Emulator's reply pipe (a concurrency-safe
// *io.PipeWriter), which unblocks emu.Read and makes any emu.Write that
// produces a reply fail instead of blocking.
func (s *vmConsoleSession) closeEmulatorInput() {
	if closer, ok := s.emu.InputPipe().(io.Closer); ok {
		_ = closer.Close()
	}
}

// Close shuts down the shell WebSocket (and with it the keepalive), the reply
// goroutines and the terminal emulator, exactly once. It takes no context,
// so cleanup cannot be cancelled. Safe to call concurrently and repeatedly;
// a later call waits for the first to finish. The rendered screen stays
// readable afterwards.
func (s *vmConsoleSession) Close(reason string) {
	s.closeOnce.Do(func() {
		// Neither step takes s.mu, so they run even while readLoop is
		// inside emu.Write.
		s.closeEmulatorInput()
		_ = s.shell.Close()

		s.mu.Lock()
		s.closed = true
		if s.closeReason == "" {
			s.closeReason = reason
		}
		s.notifyLocked()
		s.mu.Unlock()

		// Wait for every goroutine to exit before emu.Close so -race never
		// observes concurrent Read/Write vs Close on vt.Emulator.
		<-s.readerDone
		<-s.drainerDone
		<-s.forwarderDone
		s.mu.Lock()
		_ = s.emu.Close()
		s.mu.Unlock()
	})
}

func (s *vmConsoleSession) isIdle(now time.Time, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && now.Sub(s.lastActivity) >= timeout
}

// waitInitialSettle waits briefly after opening a session so any immediate
// remote exit (for example, virsh console printing an error frame and then
// closing the socket because another console is already attached) is observed
// before returning a session_id.
func (s *vmConsoleSession) waitInitialSettle(ctx context.Context, d time.Duration) vmConsoleSnapshot {
	if d <= 0 {
		return s.snapshotLockedCopy(false, false)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		s.mu.Lock()
		if s.closed {
			snap := s.renderLocked()
			s.mu.Unlock()
			return snap
		}
		ch := s.changed
		s.mu.Unlock()

		select {
		case <-ch:
		case <-timer.C:
			return s.snapshotLockedCopy(false, false)
		case <-ctx.Done():
			return s.snapshotLockedCopy(false, false)
		}
	}
}

func (s *vmConsoleSession) snapshotLockedCopy(matched, timedOut bool) vmConsoleSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.renderLocked()
	snap.Matched = matched
	snap.TimedOut = timedOut
	return snap
}

func (s *vmConsoleSession) renderLocked() vmConsoleSnapshot {
	raw := strings.ReplaceAll(s.emu.String(), "\r\n", "\n")
	screen, truncated := truncateUTF8Bytes(raw, vmConsoleMaxOutputBytes)
	pos := s.emu.CursorPosition()
	return vmConsoleSnapshot{
		SessionID:    s.id,
		VMID:         s.vmID,
		VMName:       s.vmName,
		Cols:         s.cols,
		Rows:         s.rows,
		CursorRow:    pos.Y,
		CursorCol:    pos.X,
		CursorHidden: s.emu.CursorHidden(),
		AltScreen:    s.emu.IsAltScreen(),
		Screen:       screen,
		Truncated:    truncated,
		Connected:    !s.closed,
		CloseReason:  s.closeReason,
	}
}

func (s *vmConsoleSession) readScreen(ctx context.Context, waitFor string, waitRe *regexp.Regexp, timeout time.Duration) (vmConsoleSnapshot, error) {
	waiting := waitFor != "" || waitRe != nil
	var timer *time.Timer
	var timerCh <-chan time.Time
	if waiting && timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timerCh = timer.C
	}

	for {
		s.mu.Lock()
		s.lastActivity = time.Now()
		snap := s.renderLocked()
		matched := false
		if waitFor != "" {
			matched = strings.Contains(snap.Screen, waitFor)
		} else if waitRe != nil {
			matched = waitRe.MatchString(snap.Screen)
		}
		closed := s.closed
		ch := s.changed
		s.mu.Unlock()

		if !waiting {
			return snap, nil
		}
		if matched {
			snap.Matched = true
			return snap, nil
		}
		if closed {
			return snap, nil
		}
		if timeout <= 0 {
			snap.TimedOut = true
			return snap, nil
		}

		select {
		case <-ch:
		case <-timerCh:
			s.mu.Lock()
			s.lastActivity = time.Now()
			finalSnap := s.renderLocked()
			if waitFor != "" {
				finalSnap.Matched = strings.Contains(finalSnap.Screen, waitFor)
			} else if waitRe != nil {
				finalSnap.Matched = waitRe.MatchString(finalSnap.Screen)
			}
			finalSnap.TimedOut = !finalSnap.Matched
			s.mu.Unlock()
			return finalSnap, nil
		case <-ctx.Done():
			return vmConsoleSnapshot{}, ctx.Err()
		}
	}
}

func (s *vmConsoleSession) sendInput(text string, keys []string) (int, int, error) {
	s.mu.Lock()
	if s.closed {
		reason := s.closeReason
		s.mu.Unlock()
		if reason == "" {
			reason = "disconnected"
		}
		return 0, 0, fmt.Errorf("VM serial console session %s is closed (%s)", s.id, reason)
	}
	appCursor := s.appCursorKeys
	s.lastActivity = time.Now()
	s.mu.Unlock()

	payload, err := encodeVMConsoleInput(text, keys, appCursor)
	if err != nil {
		return 0, 0, err
	}
	if err := s.shell.WriteInput(payload); err != nil {
		s.endSession("write failed")
		return 0, 0, fmt.Errorf("failed to write to VM serial console session %s: console connection closed", s.id)
	}
	return len(payload), len(keys), nil
}

func truncateUTF8Bytes(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// vmConsoleManager owns all active server-side VM serial console sessions,
// enforcing the VM allowlist, concurrent session cap, per-VM uniqueness,
// idle timeout sweeping, and non-cancellable shutdown cleanup.
type vmConsoleManager struct {
	mu            sync.Mutex
	allowlist     []string
	maxSessions   int
	idleTimeout   time.Duration
	settleTimeout time.Duration
	opener        vmShellOpener

	sessions      map[string]*vmConsoleSession
	ended         map[string]time.Time // sessionID -> when it ended remotely
	vmInFlight    map[int]string       // vmID -> sessionID (or "opening")
	closed        bool
	sweeperStop   chan struct{}
	sweeperDone   chan struct{}
	sweeperActive bool
}

func newVMConsoleManager() *vmConsoleManager {
	return &vmConsoleManager{
		maxSessions:   defaultVMConsoleMaxSessions,
		idleTimeout:   defaultVMConsoleIdleTimeout,
		settleTimeout: defaultVMConsoleSettleTimeout,
		opener:        defaultVMShellOpener,
		sessions:      make(map[string]*vmConsoleSession),
		ended:         make(map[string]time.Time),
		vmInFlight:    make(map[int]string),
	}
}

func (m *vmConsoleManager) setAllowlist(entries []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cleaned := make([]string, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e != "" {
			cleaned = append(cleaned, e)
		}
	}
	m.allowlist = cleaned
}

func (m *vmConsoleManager) checkAllowed(vmID int, vmName string) error {
	m.mu.Lock()
	list := make([]string, len(m.allowlist))
	copy(list, m.allowlist)
	m.mu.Unlock()

	if len(list) == 0 {
		return fmt.Errorf("VM serial console access is disabled: no VMs are allowed. Restart the server with --allow-vm-console=<vm-name-or-id> (or set TRUENAS_MCP_ALLOW_VM_CONSOLE) to explicitly allow specific VMs")
	}
	idStr := strconv.Itoa(vmID)
	for _, entry := range list {
		if entry == idStr || (vmName != "" && entry == vmName) {
			return nil
		}
	}
	return fmt.Errorf("VM %q (id %d) is not allowed for serial console access. Add %q or %q to --allow-vm-console (or TRUENAS_MCP_ALLOW_VM_CONSOLE) to allow it", vmName, vmID, vmName, idStr)
}

func (m *vmConsoleManager) checkCapacity(vmID int, vmName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capacityErrorLocked(vmID, vmName)
}

func (m *vmConsoleManager) reserveSlot(vmID int, vmName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.capacityErrorLocked(vmID, vmName); err != nil {
		return err
	}
	m.vmInFlight[vmID] = "opening"
	return nil
}

// capacityErrorLocked refuses a new session while shutting down, when vmID
// already has a session open or opening, or when the session cap is reached.
func (m *vmConsoleManager) capacityErrorLocked(vmID int, vmName string) error {
	if m.closed {
		return fmt.Errorf("VM serial console manager is shutting down")
	}
	if existing, ok := m.vmInFlight[vmID]; ok {
		if existing == "opening" {
			return fmt.Errorf("a serial console session for VM %q (id %d) is already opening", vmName, vmID)
		}
		return fmt.Errorf("VM %q (id %d) already has an active serial console session (%s); close it with close_vm_console first", vmName, vmID, existing)
	}
	if len(m.vmInFlight) >= m.maxSessions {
		return fmt.Errorf("maximum concurrent VM serial console sessions (%d) reached; close an existing session with close_vm_console first", m.maxSessions)
	}
	return nil
}

func (m *vmConsoleManager) releaseReservation(vmID int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vmInFlight[vmID] == "opening" {
		delete(m.vmInFlight, vmID)
	}
}

func (m *vmConsoleManager) registerSession(s *vmConsoleSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		delete(m.vmInFlight, s.vmID)
		return fmt.Errorf("VM serial console manager is shutting down")
	}
	m.sessions[s.id] = s
	m.vmInFlight[s.vmID] = s.id
	m.ensureSweeperLocked()
	return nil
}

// sessionEnded is a session's onEnded callback. It frees the VM's slot but
// keeps the session listed, so read_vm_console reports connected=false and
// the close_reason, until close_vm_console, the idle sweep or shutdown drops
// it. At most vmConsoleMaxEndedSessions ended sessions are kept.
func (m *vmConsoleManager) sessionEnded(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return
	}
	if m.vmInFlight[s.vmID] == sessionID {
		delete(m.vmInFlight, s.vmID)
	}
	m.ended[sessionID] = time.Now()
	for len(m.ended) > vmConsoleMaxEndedSessions {
		// Ended sessions are already being torn down; just unlist it.
		m.dropLocked(m.oldestEndedLocked())
	}
}

func (m *vmConsoleManager) oldestEndedLocked() string {
	var oldestID string
	var oldest time.Time
	for id, at := range m.ended {
		if oldestID == "" || at.Before(oldest) {
			oldestID, oldest = id, at
		}
	}
	return oldestID
}

// dropLocked removes sessionID from the manager and frees its VM slot. The
// caller closes the returned session (nil if it was not listed) after
// releasing m.mu.
func (m *vmConsoleManager) dropLocked(sessionID string) *vmConsoleSession {
	delete(m.ended, sessionID)
	s, ok := m.sessions[sessionID]
	if !ok {
		return nil
	}
	delete(m.sessions, sessionID)
	if m.vmInFlight[s.vmID] == sessionID {
		delete(m.vmInFlight, s.vmID)
	}
	return s
}

func (m *vmConsoleManager) getSession(sessionID string) (*vmConsoleSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("VM serial console session %q not found (it may have closed or timed out)", sessionID)
	}
	return s, nil
}

// closeSession removes sessionID from the manager and closes it. Cleanup
// takes no context, so a cancelled request cannot interrupt it.
func (m *vmConsoleManager) closeSession(sessionID, reason string) (*vmConsoleSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	m.mu.Lock()
	s := m.dropLocked(sessionID)
	m.mu.Unlock()

	if s == nil {
		return nil, fmt.Errorf("VM serial console session %q not found (it may already be closed or timed out)", sessionID)
	}
	s.Close(reason)
	return s, nil
}

// Close shuts down the idle sweeper and closes every active console session.
// Cleanup uses a non-cancellable path so server shutdown never leaks sockets.
func (m *vmConsoleManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	sessions := make([]*vmConsoleSession, 0, len(m.sessions))
	for id, s := range m.sessions {
		sessions = append(sessions, s)
		delete(m.sessions, id)
	}
	clear(m.ended)
	clear(m.vmInFlight)
	stopCh := m.sweeperStop
	doneCh := m.sweeperDone
	active := m.sweeperActive
	m.sweeperActive = false
	m.mu.Unlock()

	if active {
		close(stopCh)
		<-doneCh
	}
	for _, s := range sessions {
		s.Close("server shutdown")
	}
}

func (m *vmConsoleManager) ensureSweeperLocked() {
	if m.sweeperActive || m.idleTimeout <= 0 {
		return
	}
	m.sweeperStop = make(chan struct{})
	m.sweeperDone = make(chan struct{})
	m.sweeperActive = true
	interval := m.idleTimeout / 4
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	go m.sweepLoop(interval, m.sweeperStop, m.sweeperDone)
}

func (m *vmConsoleManager) sweepLoop(interval time.Duration, stop <-chan struct{}, done chan struct{}) {
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("vm console idle sweeper panicked: %s\n%s", safego.Describe(r), debug.Stack())
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			m.sweepIdle(now)
		}
	}
}

func (m *vmConsoleManager) sweepIdle(now time.Time) {
	m.mu.Lock()
	timeout := m.idleTimeout
	var expired, ended []*vmConsoleSession
	for id, s := range m.sessions {
		if endedAt, ok := m.ended[id]; ok {
			if now.Sub(endedAt) >= timeout {
				ended = append(ended, m.dropLocked(id))
			}
		} else if s.isIdle(now, timeout) {
			expired = append(expired, m.dropLocked(id))
		}
	}
	m.mu.Unlock()

	for _, s := range expired {
		log.Printf("Closing idle VM serial console session %s (vm_id=%d)", s.id, s.vmID)
		s.Close("idle timeout")
	}
	for _, s := range ended {
		s.Close("")
	}
}

func generateVMConsoleSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("failed to generate console session id: %w", err)
	}
	return "vmcon_" + hex.EncodeToString(raw[:]), nil
}

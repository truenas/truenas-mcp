package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
)

// blockingRegistry has tools that block until the test releases them:
//
//	fast        returns at once
//	slow        blocks until release is closed, ignoring ctx
//	wait_ctx    blocks until its ctx is done, and reports ctx's error
type blockingRegistry struct {
	started chan string
	release chan struct{}
	ctxErr  chan error
}

func newBlockingRegistry(t *testing.T) *blockingRegistry {
	r := &blockingRegistry{started: make(chan string, 16), release: make(chan struct{}), ctxErr: make(chan error, 16)}
	t.Cleanup(func() {
		select {
		case <-r.release:
		default:
			close(r.release)
		}
	})
	return r
}

func (r *blockingRegistry) ListTools() []mcp.Tool {
	var out []mcp.Tool
	for _, n := range []string{"fast", "slow", "wait_ctx"} {
		out = append(out, mcp.Tool{Name: n, Description: n, InputSchema: map[string]interface{}{"type": "object"}})
	}
	return out
}

func (r *blockingRegistry) CallTool(ctx context.Context, name string, _ map[string]interface{}) (string, error) {
	switch name {
	case "slow":
		r.started <- name
		<-r.release
	case "wait_ctx":
		r.started <- name
		<-ctx.Done()
		r.ctxErr <- ctx.Err()
		return "", ctx.Err()
	}
	return name + " done", nil
}

func (r *blockingRegistry) waitStarted(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-r.started:
		if got != want {
			t.Fatalf("started %s, want %s", got, want)
		}
	case <-time.After(replyTimeout):
		t.Fatalf("%s did not start", want)
	}
}

func (r *blockingRegistry) notStarted(t *testing.T) {
	t.Helper()
	select {
	case got := <-r.started:
		t.Fatalf("%s started, want it to wait", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// A slow tool call does not hold up other calls or ping: they are answered
// while it is still running, and its own reply comes when it finishes.
func TestSlowToolCallDoesNotBlockOtherRequests(t *testing.T) {
	reg := newBlockingRegistry(t)
	c, run := session(t, newTestHandler(reg, false))
	c.initialize()
	c.send(map[string]interface{}{"id": 1, "method": "tools/call", "params": toolArgs("slow", nil)})
	reg.waitStarted(t, "slow")

	if text, _ := toolText(t, c.call(float64(2), "tools/call", toolArgs("fast", nil))); text != "fast done" {
		t.Errorf("fast call = %q", text)
	}
	c.call(float64(3), "ping", nil)
	c.call(float64(4), "tools/list", nil)

	close(reg.release)
	m := c.recv()
	if text, _ := toolText(t, m); m["id"] != float64(1) || text != "slow done" {
		t.Errorf("slow call reply = %v", m)
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.noMoreOutput()
}

// notifications/cancelled for an in-flight call cancels the context the
// tool runs with, and the call gets no reply; the session goes on.
func TestCancelledNotificationCancelsToolContext(t *testing.T) {
	reg := newBlockingRegistry(t)
	c, run := session(t, newTestHandler(reg, false))
	c.initialize()
	c.send(map[string]interface{}{"id": 1, "method": "tools/call", "params": toolArgs("wait_ctx", nil)})
	reg.waitStarted(t, "wait_ctx")
	c.send(map[string]interface{}{"method": "notifications/cancelled", "params": map[string]interface{}{"requestId": 1, "reason": "user cancelled"}})
	select {
	case err := <-reg.ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("tool ctx error = %v, want context.Canceled", err)
		}
	case <-time.After(replyTimeout):
		t.Fatal("the tool's context was not cancelled")
	}
	// The next reply is the ping's: the cancelled call got none.
	c.call(float64(2), "ping", nil)
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.noMoreOutput()
}

// A cancelled call returns at once even if its tool does not observe ctx
// (tool handlers do not take a context yet); the tool finishes in the
// background and its result is dropped.
func TestCancelledCallReturnsWhileToolStillRuns(t *testing.T) {
	reg := newBlockingRegistry(t)
	c, run := session(t, newTestHandler(reg, false))
	c.initialize()
	c.send(map[string]interface{}{"id": 1, "method": "tools/call", "params": toolArgs("slow", nil)})
	reg.waitStarted(t, "slow")
	c.send(map[string]interface{}{"method": "notifications/cancelled", "params": map[string]interface{}{"requestId": 1}})
	c.call(float64(2), "ping", nil)
	close(reg.release)
	c.call(float64(3), "ping", nil)
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	c.noMoreOutput()
}

// When the client closes stdin (the end of the session), calls in flight
// are cancelled and Run returns, even if a tool ignores its context.
func TestInputEOFCancelsInFlightCalls(t *testing.T) {
	reg := newBlockingRegistry(t)
	c, run := session(t, newTestHandler(reg, false))
	c.initialize()
	c.send(map[string]interface{}{"id": 1, "method": "tools/call", "params": toolArgs("wait_ctx", nil)})
	reg.waitStarted(t, "wait_ctx")
	c.send(map[string]interface{}{"id": 2, "method": "tools/call", "params": toolArgs("slow", nil)})
	reg.waitStarted(t, "slow")
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case err := <-reg.ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("tool ctx error = %v, want context.Canceled", err)
		}
	case <-time.After(replyTimeout):
		t.Fatal("the tool's context was not cancelled")
	}
	c.noMoreOutput()
}

// At most callSlots tool calls run at once; the next one starts when one
// finishes, and other requests are not held up meanwhile.
func TestToolCallConcurrencyIsBounded(t *testing.T) {
	reg := newBlockingRegistry(t)
	h := newTestHandler(reg, false)
	h.callSlots = make(chan struct{}, 2)
	c, run := session(t, h)
	c.initialize()
	for id := 1; id <= 3; id++ {
		c.send(map[string]interface{}{"id": id, "method": "tools/call", "params": toolArgs("slow", nil)})
	}
	reg.waitStarted(t, "slow")
	reg.waitStarted(t, "slow")
	reg.notStarted(t)
	c.call(float64(9), "ping", nil)
	close(reg.release)
	reg.waitStarted(t, "slow")
	seen := map[float64]bool{}
	for range 3 {
		m := c.recv()
		id, _ := m["id"].(float64)
		seen[id] = true
	}
	if len(seen) != 3 || !seen[1] || !seen[2] || !seen[3] {
		t.Errorf("replies = %v, want ids 1, 2 and 3", seen)
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// A call past its overall deadline gets a tool error, which says whether
// the tool had started (and so may still change the system).
func TestToolCallDeadline(t *testing.T) {
	reg := newBlockingRegistry(t)
	h := newTestHandler(reg, false)
	h.callTimeout = 200 * time.Millisecond
	h.callSlots = make(chan struct{}, 1)
	c, run := session(t, h)
	c.initialize()
	c.send(map[string]interface{}{"id": 1, "method": "tools/call", "params": toolArgs("slow", nil)})
	reg.waitStarted(t, "slow")
	// The slot is held by the slow call, so this one never starts.
	c.send(map[string]interface{}{"id": 2, "method": "tools/call", "params": toolArgs("fast", nil)})
	replies := map[interface{}]map[string]interface{}{}
	for range 2 {
		m := c.recv()
		replies[m["id"]] = m
	}
	text, isError := toolText(t, replies[float64(1)])
	if !isError || !strings.Contains(text, "did not finish within 200ms") || !strings.Contains(text, "check the system state") {
		t.Errorf("running call past its deadline = %q (isError %v)", text, isError)
	}
	text, isError = toolText(t, replies[float64(2)])
	if !isError || !strings.Contains(text, "tool fast was not run") {
		t.Errorf("queued call past its deadline = %q (isError %v)", text, isError)
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

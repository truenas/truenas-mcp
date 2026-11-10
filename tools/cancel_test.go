package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A cancelled tool call must stop waiting for its in-flight middleware
// request and return promptly. Until handlers and Client.Call observe
// ctx, CallTool stays blocked in the 120s middleware timeout.
func TestCancelledToolCallReleasesPendingMiddlewareRequest(t *testing.T) {
	hold := make(chan struct{})
	held := make(chan struct{})
	client, fake := newFakeClientFrom(t, &fakeMiddleware{
		holdMethod: "system.info",
		hold:       hold,
		held:       held,
	})
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	r := NewRegistry(client, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.CallTool(ctx, "system_info", nil)
		done <- err
	}()

	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("middleware never received system.info")
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CallTool = %v; want an error wrapping context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CallTool did not return within 2s of cancellation; the middleware request is still held")
	}

	// A late reply to the cancelled request must not break the session:
	// the next call should succeed once the held reply is released.
	close(hold)
	// Give the late reply a moment to arrive and be discarded.
	time.Sleep(20 * time.Millisecond)
	out, err := r.CallTool(context.Background(), "system_info", nil)
	if err != nil {
		t.Fatalf("call after a cancelled in-flight request: %v", err)
	}
	if !strings.Contains(out, `"hostname": "nas"`) {
		t.Fatalf("unexpected result after recovery: %q", out)
	}
	if n := len(fake.called("system.info")); n < 2 {
		t.Errorf("system.info was called %d times, want at least 2 (cancelled + retry)", n)
	}
}

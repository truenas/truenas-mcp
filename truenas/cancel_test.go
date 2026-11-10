package truenas

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A cancelled Call must stop waiting for the middleware response and
// return promptly, without dropping the session. A late reply to the
// cancelled request is discarded.
func TestCallCancellationReleasesPendingRequest(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.holdMethod = "system.info"
	})
	c := insecureClient(t, f, Options{})
	if err := c.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, "system.info")
		done <- err
	}()
	// auth.login_ex plus the held system.info
	f.waitFrames(t, 2)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call = %v; want an error wrapping context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return within 2s of cancellation; the middleware request is still held")
	}

	close(f.release)
	time.Sleep(20 * time.Millisecond)
	if _, err := c.Call(context.Background(), "core.get_jobs"); err != nil {
		t.Fatalf("call after a cancelled in-flight request: %v", err)
	}
}

// A Call whose context is already done must not be sent.
func TestCallWithDoneContextIsNotSent(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	c := insecureClient(t, f, Options{})
	if err := c.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	frames, _, _ := f.snapshot()
	before := len(frames)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Call(ctx, "system.info")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call = %v; want context.Canceled", err)
	}
	frames, _, _ = f.snapshot()
	if len(frames) != before {
		t.Fatalf("Call sent %d extra frames; want none", len(frames)-before)
	}
}

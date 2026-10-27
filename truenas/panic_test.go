package truenas

import (
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// captureLog sends the standard logger to w for the rest of the test.
func captureLog(t *testing.T, w io.Writer) {
	t.Helper()
	log.SetOutput(w)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

// A panic in the read loop must not crash the process. It is logged, the
// connection is dropped, the request in flight fails, and the next call
// reconnects and logs in again.
func TestReadLoopPanicIsRecovered(t *testing.T) {
	logs := &syncBuffer{}
	captureLog(t, logs)

	f := newFakeTrueNAS(t, nil)
	c := insecureClient(t, f, Options{})
	if err := c.Authenticate(); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	var armed atomic.Bool
	c.frameHook = func(*rpcResponse) {
		if armed.CompareAndSwap(true, false) {
			nilMapWrite(nil) // a runtime panic
		}
	}
	armed.Store(true)

	done := make(chan error, 1)
	go func() { _, err := c.Call("system.info"); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "read loop") {
			t.Fatalf("Call error = %v; want a read loop failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Call hung after the read loop panicked")
	}
	if !strings.Contains(logs.String(), "read loop panicked") {
		t.Errorf("panic was not logged: %s", logs.String())
	}
	// The client recovers by reconnecting and logging in again.
	if _, err := c.Call("system.info"); err != nil {
		t.Fatalf("Call after the panic: %v", err)
	}
	if n := f.logins(t); n != 2 {
		t.Errorf("logins = %d; want 2 (one per connection)", n)
	}
}

// A panic in the keepalive goroutine is logged and ends only that goroutine.
func TestKeepalivePanicIsRecovered(t *testing.T) {
	logs := &syncBuffer{}
	captureLog(t, logs)

	c := &Client{pingInterval: 5 * time.Millisecond}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		c.keepalive(nil, make(chan struct{})) // a nil conn panics on the first ping
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("keepalive did not return after panicking")
	}
	if !strings.Contains(logs.String(), "keepalive panicked") {
		t.Errorf("panic was not logged: %s", logs.String())
	}
}

// nilMapWrite panics with a runtime error. The map is a parameter so that
// static analysis does not flag the deliberate nil write.
func nilMapWrite(m map[string]int) { m["boom"] = 1 }

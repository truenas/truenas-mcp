package truenas

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func checkOversizedResponseError(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: got no error", name)
	}
	msg := err.Error()
	for _, want := range []string{
		"the TrueNAS response was larger than 10 MiB",
		"narrow the query",
		"filters or a limit",
		"re-established on the next request",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("%s: got %q, want it to contain %q", name, msg, want)
		}
	}
	if strings.Contains(msg, "failed to read response") {
		t.Errorf("%s: got %q, want the oversize explained rather than reported as a read failure", name, msg)
	}
	if !errors.Is(err, websocket.ErrReadLimit) {
		t.Errorf("%s: got %#v, want it to wrap websocket.ErrReadLimit", name, err)
	}
}

func TestReadErrorOversizedResponse(t *testing.T) {
	checkOversizedResponseError(t, "readError", readError(websocket.ErrReadLimit))
}

// An oversized TrueNAS response is reported as such, not as a lost
// connection. Other in-flight calls on the same connection fail with the
// same wording, and the next call reconnects.
func TestOversizedResponseIsExplained(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.holdMethod = "system.info"
		f.oversizeResultFor = "pool.dataset.query"
		// Leave oversizeResultBytes unset so the fake default is one
		// byte over maxResponseBytes (not a hardcoded 10 MiB+1).
	})
	defer close(f.release)
	c := insecureClient(t, f, Options{})
	if err := c.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}

	held := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "system.info")
		held <- err
	}()
	f.waitFrames(t, 2) // login + held system.info

	oversize := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "pool.dataset.query")
		oversize <- err
	}()

	select {
	case err := <-oversize:
		checkOversizedResponseError(t, "oversize call", err)
	case <-time.After(15 * time.Second):
		t.Fatal("oversize call still waiting")
	}
	select {
	case err := <-held:
		checkOversizedResponseError(t, "pending call", err)
	case <-time.After(5 * time.Second):
		t.Fatal("pending call still waiting: the dropped connection did not fail it")
	}

	waitDisconnected(t, c)
	if _, err := c.Call(context.Background(), "core.get_jobs"); err != nil {
		t.Fatalf("Call after the oversized response: %v", err)
	}
	if _, _, conns := f.snapshot(); conns != 2 {
		t.Errorf("conns = %d, want 2 (a reconnect)", conns)
	}
}

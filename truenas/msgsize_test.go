package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// A request over the TrueNAS message limit is refused before it is sent,
// with a message that says so, and the connection stays usable.
func TestOversizedRequestNotSent(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.maxMessage = maxRequestBytes })
	c := insecureClient(t, f, Options{})
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	before, _, _ := f.snapshot()

	big := strings.Repeat("x", 70*1024)
	_, err := c.Call(context.Background(), "filesystem.mkdir", map[string]interface{}{"path": big})
	if err == nil {
		t.Fatal("70 KiB request: got no error")
	}
	for _, want := range []string{
		"request too large for the TrueNAS API",
		"the filesystem.mkdir request is 70.",
		"over the 64 KiB limit",
		"nothing was sent to TrueNAS and nothing was executed",
		"Reduce the size of the input",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("got %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), big[:100]) {
		t.Errorf("error echoes the input: %.200s", err)
	}

	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatalf("Call after the oversized request: %v", err)
	}
	after, _, conns := f.snapshot()
	if len(after) != len(before)+1 {
		t.Errorf("fake received %d frames after the oversized request, want 1 (only the follow-up call)", len(after)-len(before))
	}
	if conns != 1 {
		t.Errorf("conns = %d, want 1: an oversized request must not drop the connection", conns)
	}
}

// A request of exactly maxRequestBytes is still sent; one byte more is not.
func TestRequestSizeLimitBoundary(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.maxMessage = maxRequestBytes })
	c := insecureClient(t, f, Options{})
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// The next request id; ids here stay in the same number of digits.
	base, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: c.requestID.Load() + 1, Method: "system.info", Params: []interface{}{""}})
	if err != nil {
		t.Fatal(err)
	}
	pad := maxRequestBytes - len(base)
	if _, err := c.Call(context.Background(), "system.info", strings.Repeat("x", pad)); err != nil {
		t.Errorf("request of exactly %d bytes: %v", maxRequestBytes, err)
	}
	_, err = c.Call(context.Background(), "system.info", strings.Repeat("x", pad+1))
	if err == nil || !strings.Contains(err.Error(), "request too large") {
		t.Errorf("request of %d bytes: got %v, want a request too large error", maxRequestBytes+1, err)
	}
	if _, _, conns := f.snapshot(); conns != 1 {
		t.Errorf("conns = %d, want 1", conns)
	}
}

// If TrueNAS closes the connection with 1009 (message too big), say what
// that means instead of passing on the raw close frame, and reconnect on
// the next call.
func TestMessageTooBigCloseIsExplained(t *testing.T) {
	// A server limit below ours, so the request is sent and refused there.
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.maxMessage = 1024 })
	c := insecureClient(t, f, Options{})
	_, err := c.Call(context.Background(), "filesystem.mkdir", map[string]interface{}{"path": strings.Repeat("x", 2048)})
	if err == nil {
		t.Fatal("got no error")
	}
	for _, want := range []string{
		"TrueNAS closed the connection because a request was too large for its API (limit 64 KiB per message)",
		"the oversized request was not executed",
		"Reduce the size of the input",
		"re-established on the next request",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("got %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "failed to read response") {
		t.Errorf("got %q, want the close explained rather than reported as a read failure", err)
	}
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.CloseMessageTooBig {
		t.Errorf("got %#v, want it to wrap the 1009 close error", err)
	}

	waitDisconnected(t, c)
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatalf("Call after the 1009 close: %v", err)
	}
	if _, _, conns := f.snapshot(); conns != 2 {
		t.Errorf("conns = %d, want 2 (a reconnect)", conns)
	}
}

func TestReadErrorWording(t *testing.T) {
	if got := readError(errors.New("EOF")).Error(); got != "failed to read response: EOF" {
		t.Errorf("other read errors: got %q", got)
	}
	other := &websocket.CloseError{Code: websocket.CloseGoingAway, Text: "bye"}
	if got := readError(other).Error(); !strings.HasPrefix(got, "failed to read response: ") {
		t.Errorf("close 1001: got %q", got)
	}
}

package truenas

import (
	"bytes"
	"crypto/tls"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitDisconnected waits until the client has noticed that its socket closed.
func waitDisconnected(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.connMu.Lock()
		gone := c.conn == nil
		c.connMu.Unlock()
		if gone {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("client did not notice the closed connection")
}

func TestAPIUserRequired(t *testing.T) {
	_, err := NewClientWithOptions("truenas.example", testAPIKey, &tls.Config{}, Options{})
	if err == nil || !strings.Contains(err.Error(), "API user") {
		t.Fatalf("empty APIUser: got %v, want an error naming the API user", err)
	}
}

func TestConcurrentFirstCallsLogInOnce(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	c := insecureClient(t, f, Options{})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Call("system.info"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Call: %v", err)
	}
	if n := f.logins(t); n != 1 {
		t.Errorf("auth.login_ex sent %d times for 20 concurrent first calls, want 1", n)
	}
	if n := f.unauthedCalls(); n != 0 {
		t.Errorf("%d calls reached the server before login", n)
	}
}

func TestDropDuringLogin(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.dropLogins = 1 })
	c := insecureClient(t, f, Options{})
	if _, err := c.Call("system.info"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	raws, _, conns := f.snapshot()
	if conns != 2 {
		t.Errorf("conns = %d, want 2", conns)
	}
	var methods []string
	for _, fr := range parseFrames(t, raws) {
		methods = append(methods, fr.Method)
	}
	if got := strings.Join(methods, ","); got != "auth.login_ex,auth.login_ex,system.info" {
		t.Errorf("frames = %s; want a second login before the call", got)
	}
	if n := f.unauthedCalls(); n != 0 {
		t.Errorf("%d calls sent on a connection that was not logged in", n)
	}
}

func TestDropDuringLoginTwiceFails(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.dropLogins = 2 })
	c := insecureClient(t, f, Options{})
	if _, err := c.Call("system.info"); err == nil {
		t.Fatal("Call succeeded although every login was dropped")
	}
	if c.authedSession() != nil {
		t.Error("a connection is marked authenticated after failed logins")
	}
	if n := f.unauthedCalls(); n != 0 {
		t.Errorf("%d calls sent without a login", n)
	}
}

func TestNotAuthenticatedReauthenticatesOnce(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.expireOnce = true })
	c := insecureClient(t, f, Options{})
	for i := 0; i < 2; i++ {
		if _, err := c.Call("system.info"); err != nil {
			t.Fatalf("Call %d: %v", i, err)
		}
	}
	raws, _, conns := f.snapshot()
	if conns != 1 {
		t.Errorf("conns = %d, want 1 (re-login on the same socket)", conns)
	}
	var methods []string
	for _, fr := range parseFrames(t, raws) {
		methods = append(methods, fr.Method)
	}
	want := "auth.login_ex,system.info,system.info,auth.login_ex,system.info"
	if got := strings.Join(methods, ","); got != want {
		t.Errorf("frames = %s, want %s", got, want)
	}
}

func TestNotAuthenticatedRetriedOnlyOnce(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.errors["system.info"] = map[string]interface{}{
			"code": -32001, "message": "Method call error",
			"data": map[string]interface{}{"errname": "ENOTAUTHENTICATED", "reason": "Not authenticated"},
		}
	})
	c := insecureClient(t, f, Options{})
	_, err := c.Call("system.info")
	if err == nil || !strings.Contains(err.Error(), "ENOTAUTHENTICATED") {
		t.Fatalf("got %v, want the ENOTAUTHENTICATED error after one retry", err)
	}
	if n := f.logins(t); n != 2 {
		t.Errorf("auth.login_ex sent %d times, want 2", n)
	}
}

func TestNoRetryAfterWrite(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.dropOn = "pool.scrub.run" })
	c := insecureClient(t, f, Options{})
	if _, err := c.Call("pool.scrub.run", "tank", 35); err == nil {
		t.Fatal("Call succeeded although the server dropped the connection")
	}
	raws, _, conns := f.snapshot()
	sent := 0
	for _, fr := range parseFrames(t, raws) {
		if fr.Method == "pool.scrub.run" {
			sent++
		}
	}
	if sent != 1 || conns != 1 {
		t.Errorf("pool.scrub.run sent %d times over %d connections; a written request must not be retried", sent, conns)
	}
	// The client recovers for the next call.
	waitDisconnected(t, c)
	if _, err := c.Call("system.info"); err != nil {
		t.Errorf("next Call: %v", err)
	}
}

func TestOldTrueNASGets404Hint(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	endpoint := "wss://" + strings.TrimPrefix(srv.URL, "https://") + apiPath
	c, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Authenticate()
	if err == nil || !strings.Contains(err.Error(), "TrueNAS 25.04 or later required") {
		t.Errorf("got %v, want the TrueNAS 25.04 hint", err)
	}
}

func TestNullIDErrorReachesCaller(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.nullIDFor = "system.info" })
	c := insecureClient(t, f, Options{})
	done := make(chan error, 1)
	go func() {
		_, err := c.Call("system.info")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Invalid Request") {
			t.Errorf("got %v, want the null-id error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller still waiting: null-id error was dropped")
	}
}

func TestURLLoggingRedactedAndLegacyWarningOnce(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	endpoint := strings.Replace(f.wsURL("127.0.0.1"), "wss://", "wss://user:hunter2@", 1)
	endpoint = strings.TrimSuffix(endpoint, apiPath) + legacyPath

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	c, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The dialer refuses userinfo, but the URL is logged first, and each
	// attempt resolves the endpoint again.
	for i := 0; i < 2; i++ {
		_ = c.Authenticate()
	}
	out := buf.String()
	if n := strings.Count(out, "Connecting to"); n != 2 {
		t.Fatalf("connected %d times, want 2:\n%s", n, out)
	}
	if n := strings.Count(out, "legacy protocol"); n != 1 {
		t.Errorf("legacy warning logged %d times, want 1", n)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("password from the URL was logged:\n%s", out)
	}
	if strings.Contains(out, "26.04") {
		t.Errorf("stale version claim in log:\n%s", out)
	}
}

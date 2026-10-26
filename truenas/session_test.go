package truenas

import (
	"bytes"
	"crypto/tls"
	"errors"
	"log"
	"math"
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
	if conn, _ := c.authedSession(); conn != nil {
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

func TestOtherPath404Hint(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	endpoint := "wss://" + strings.TrimPrefix(srv.URL, "https://") + "/api/v99"
	c, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Authenticate()
	if err == nil || !strings.Contains(err.Error(), "check the path") || !strings.Contains(err.Error(), apiPath) {
		t.Errorf("got %v, want a hint about the path", err)
	}
}

func TestNullIDErrorReachesOnlyPendingCaller(t *testing.T) {
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

func TestNullIDErrorDoesNotFailOtherCalls(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.nullIDFor = "bad.method"
		f.holdMethod = "system.info"
	})
	c := insecureClient(t, f, Options{})
	if err := c.Authenticate(); err != nil {
		t.Fatal(err)
	}
	good := make(chan error, 1)
	go func() {
		_, err := c.Call("system.info")
		good <- err
	}()
	f.waitFrames(t, 2)
	bad := make(chan error, 1)
	go func() {
		_, err := c.Call("bad.method")
		bad <- err
	}()
	f.waitFrames(t, 3) // the null-id reply is sent right away, two calls pending
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-bad:
		t.Fatalf("ambiguous null-id error failed a call: %v", err)
	case err := <-good:
		t.Fatalf("ambiguous null-id error failed a call: %v", err)
	default:
	}
	close(f.release)
	if err := <-good; err != nil {
		t.Errorf("unrelated call failed: %v", err)
	}
	// The unmatched call is released when the connection closes.
	c.Close()
	select {
	case err := <-bad:
		if err == nil {
			t.Error("unanswered call succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unanswered call still waiting after Close")
	}
}

func TestUnencodableRequestIsNotSent(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	c := insecureClient(t, f, Options{})
	if err := c.Authenticate(); err != nil {
		t.Fatal(err)
	}
	_, err := c.Call("system.info", math.NaN())
	if err == nil || !strings.Contains(err.Error(), "failed to encode request") || errors.Is(err, errNotSent) {
		t.Fatalf("got %v, want an encode error", err)
	}
	if _, err := c.Call("system.info"); err != nil {
		t.Fatalf("next Call: %v", err)
	}
	raws, _, conns := f.snapshot()
	if len(raws) != 2 || conns != 1 {
		t.Errorf("%d frames over %d connections; the bad request must not be sent or reset the connection", len(raws), conns)
	}
}

func TestStaleNotAuthenticatedKeepsNewLogin(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	c := insecureClient(t, f, Options{})
	if err := c.Authenticate(); err != nil {
		t.Fatal(err)
	}
	conn, gen := c.authedSession()
	if err := c.Authenticate(); err != nil { // a newer login on the same connection
		t.Fatal(err)
	}
	c.forgetSession(conn, gen) // stale ENOTAUTHENTICATED for the old login
	if now, _ := c.authedSession(); now != conn {
		t.Fatal("stale ENOTAUTHENTICATED cleared a newer login")
	}
	_, gen = c.authedSession()
	c.forgetSession(conn, gen)
	if now, _ := c.authedSession(); now != nil {
		t.Fatal("ENOTAUTHENTICATED for the current login did not clear it")
	}
}

func TestKeepaliveDropsSilentConnection(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.noPong = true
		f.holdMethod = "system.info"
	})
	c := insecureClient(t, f, Options{})
	c.pingInterval, c.pongWait = 20*time.Millisecond, 200*time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := c.Call("system.info") // never answered
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("call on a silent connection succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call on a silent connection still waiting: no keepalive")
	}
	waitDisconnected(t, c)
}

func TestKeepaliveKeepsIdleConnection(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	c := insecureClient(t, f, Options{})
	c.pingInterval, c.pongWait = 20*time.Millisecond, 100*time.Millisecond
	if err := c.Authenticate(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // five pongWaits without traffic
	if conn, _ := c.authedSession(); conn == nil {
		t.Fatal("idle connection with pongs was dropped")
	}
	if _, err := c.Call("system.info"); err != nil {
		t.Fatal(err)
	}
	if _, _, conns := f.snapshot(); conns != 1 {
		t.Errorf("%d connections, want 1", conns)
	}
}

func TestURLWithUserinfoRefused(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	host := strings.TrimPrefix(f.wsURL("127.0.0.1"), "wss://")
	host = strings.TrimSuffix(host, apiPath)
	endpoints := []string{
		"wss://alice:hunter2@" + host + legacyPath,
		"wss://alice@" + host,
		" WSS://alice:hunter2@" + host + "/api/current/ ",
		"wss://alice:hun%zzter2@" + host, // unparseable userinfo
		"alice:hunter2@" + host,
		"alice@" + host,
	}
	for _, endpoint := range endpoints {
		var buf bytes.Buffer
		log.SetOutput(&buf)
		c, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser})
		if err != nil {
			t.Fatal(err)
		}
		err = c.Authenticate()
		c.Close()
		log.SetOutput(os.Stderr)
		if !errors.Is(err, errUserinfo) {
			t.Errorf("%q: got %v, want the userinfo refusal", endpoint, err)
			continue
		}
		for _, secret := range []string{"alice", "hunter2", "hun%zzter2"} {
			if strings.Contains(err.Error(), secret) || strings.Contains(buf.String(), secret) {
				t.Errorf("%q: %q leaked into the error or logs:\n%v\n%s", endpoint, secret, err, buf.String())
			}
		}
	}
	if _, _, conns := f.snapshot(); conns != 0 {
		t.Errorf("%d connections made with userinfo URLs", conns)
	}
}

func TestURLLoggingAndLegacyWarningOnce(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	endpoint := strings.TrimSuffix(f.wsURL("127.0.0.1"), apiPath) + legacyPath

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	c, err := NewClientWithOptions(endpoint, testAPIKey, &tls.Config{InsecureSkipVerify: true}, Options{APIUser: testAPIUser})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Each connection resolves the endpoint again.
	for i := 0; i < 2; i++ {
		if err := c.Authenticate(); err != nil {
			t.Fatal(err)
		}
		c.Close()
		waitDisconnected(t, c)
	}
	out := buf.String()
	if n := strings.Count(out, "Connecting to"); n != 2 {
		t.Fatalf("connected %d times, want 2:\n%s", n, out)
	}
	if n := strings.Count(out, "legacy protocol"); n != 1 {
		t.Errorf("legacy warning logged %d times, want 1", n)
	}
	if strings.Contains(out, "26.04") {
		t.Errorf("stale version claim in log:\n%s", out)
	}
}

// staleConnection simulates a writer that has noticed a broken socket: the
// client drops conn 1 and reconnects, while conn 1's readLoop is still
// running and only sees its read error later.
func staleConnection(t *testing.T, c *Client) {
	t.Helper()
	if err := c.Authenticate(); err != nil {
		t.Fatal(err)
	}
	c.connMu.Lock()
	c.conn, c.authedConn = nil, nil
	c.connMu.Unlock()
}

func TestOldConnectionDoesNotFailNewRequests(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.holdConn, f.holdMethod = 2, "system.info" })
	c := insecureClient(t, f, Options{})
	staleConnection(t, c) // frame 1: login on conn 1

	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, err := c.Call("system.info")
			errs <- err
		}()
	}
	f.waitFrames(t, 5) // login on conn 2, then three calls held on conn 2
	f.closeSocket(1)
	time.Sleep(100 * time.Millisecond) // let conn 1's readLoop see its error
	close(f.release)
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			t.Errorf("call in flight on the new connection failed: %v", err)
		}
	}
}

func TestOldConnectionDoesNotFailNewLogin(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.holdConn, f.holdMethod = 2, "auth.login_ex" })
	c := insecureClient(t, f, Options{})
	staleConnection(t, c)

	errs := make(chan error, 1)
	go func() {
		_, err := c.Call("system.info")
		errs <- err
	}()
	f.waitFrames(t, 2) // login on conn 2, held
	f.closeSocket(1)
	time.Sleep(100 * time.Millisecond)
	close(f.release)
	if err := <-errs; err != nil {
		t.Fatalf("login in flight on the new connection failed: %v", err)
	}
	if n := f.logins(t); n != 2 {
		t.Errorf("auth.login_ex sent %d times, want 2", n)
	}
}

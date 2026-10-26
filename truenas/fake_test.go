package truenas

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeTrueNAS is a TLS JSON-RPC 2.0 server that records every inbound frame,
// so tests can assert exactly what the client sent -- above all, whether the
// API key ever left the client.
type fakeTrueNAS struct {
	*httptest.Server

	loginType  string                            // auth.login_ex response_type; default SUCCESS
	echoKey    bool                              // auth.login_ex fails with an error echoing the key
	notify     bool                              // send an id-less notification before every reply
	stringIDs  bool                              // echo ids back as JSON strings
	closeAfter int                               // close the socket after N non-auth replies (0 = never)
	errors     map[string]map[string]interface{} // method -> JSON-RPC error object
	dropLogins int                               // close the socket instead of answering the first N logins
	dropOn     string                            // close the socket instead of answering this method (once)
	expireOnce bool                              // forget the login after the first non-auth reply (once)
	nullIDFor  string                            // answer this method with a null-id error
	holdConn   int                               // with holdMethod: connection (1-based) whose replies wait for release; 0 = all
	holdMethod string                            // hold replies to this method ("*" = every method) until release is closed
	release    chan struct{}
	noPong     bool // ignore websocket pings, like a half-open connection

	mu         sync.Mutex
	frames     []string
	paths      []string
	conns      int
	sockets    []*websocket.Conn
	unauthed   int // non-auth calls received on a connection that was not logged in
	dropped    bool
	expired    bool
	loginsSeen int
}

func newFakeTrueNAS(t *testing.T, configure func(*fakeTrueNAS)) *fakeTrueNAS {
	t.Helper()
	f := &fakeTrueNAS{loginType: "SUCCESS", errors: map[string]map[string]interface{}{}, release: make(chan struct{})}
	if configure != nil {
		configure(f)
	}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// startFakeTrueNAS keeps the pre-fork helper signature for the logging and TLS tests.
func startFakeTrueNAS(t *testing.T, echoKeyInError bool) *httptest.Server {
	return newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.echoKey = echoKeyInError }).Server
}

func (f *fakeTrueNAS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.conns++
	index := f.conns
	f.mu.Unlock()

	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	f.mu.Lock()
	f.sockets = append(f.sockets, conn)
	f.mu.Unlock()
	if f.noPong {
		conn.SetPingHandler(func(string) error { return nil })
	}
	var writeMu sync.Mutex // held replies are written from other goroutines
	send := func(v interface{}) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(v)
	}

	calls := 0
	authed := false
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.frames = append(f.frames, string(raw))
		f.mu.Unlock()

		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(raw, &req) != nil {
			return
		}
		var id interface{} = req.ID
		if f.stringIDs {
			id = string(req.ID)
		}
		if f.notify {
			send(map[string]interface{}{
				"jsonrpc": "2.0", "method": "collection_update",
				"params": map[string]interface{}{"msg": "added", "collection": "core.get_jobs"},
			})
		}

		f.mu.Lock()
		dropLogin := false
		if req.Method == "auth.login_ex" {
			f.loginsSeen++
			dropLogin = f.loginsSeen <= f.dropLogins
		}
		dropCall := req.Method == f.dropOn && !f.dropped
		if dropCall {
			f.dropped = true
		}
		if req.Method != "auth.login_ex" && !authed {
			f.unauthed++
		}
		f.mu.Unlock()
		if dropLogin || dropCall {
			return
		}

		reply := map[string]interface{}{"jsonrpc": "2.0", "id": id}
		switch {
		case req.Method == "auth.login_ex" && f.echoKey:
			reply["error"] = map[string]interface{}{
				"code": -32001, "message": "Method call error",
				"data": map[string]interface{}{
					"error": 13, "errname": "EACCES",
					"reason": "invalid API key: " + testAPIKey,
					"trace":  map[string]interface{}{"formatted": "Traceback: ValidationError('" + testAPIKey + "')"},
				},
			}
		case req.Method == "auth.login_ex":
			reply["result"] = map[string]interface{}{"response_type": f.loginType}
			authed = f.loginType == "SUCCESS"
		case !authed:
			reply["error"] = map[string]interface{}{
				"code": -32001, "message": "Method call error",
				"data": map[string]interface{}{"errname": "ENOTAUTHENTICATED", "reason": "Not authenticated"},
			}
		case req.Method == f.nullIDFor:
			reply["id"] = nil
			reply["error"] = map[string]interface{}{"code": -32600, "message": "Invalid Request"}
		case f.errors[req.Method] != nil:
			reply["error"] = f.errors[req.Method]
		default:
			calls++
			// Secret-bearing result, like core.get_jobs job arguments
			reply["result"] = map[string]interface{}{"bindpw": "responsesecret9", "domain": "example.com"}
		}
		if f.holdMethod != "" && (f.holdMethod == "*" || f.holdMethod == req.Method) && (f.holdConn == 0 || f.holdConn == index) {
			go func() {
				<-f.release
				send(reply)
			}()
			continue
		}
		if err := send(reply); err != nil {
			return
		}
		f.mu.Lock()
		if f.expireOnce && !f.expired && req.Method != "auth.login_ex" && authed {
			f.expired, authed = true, false
		}
		f.mu.Unlock()
		if f.closeAfter > 0 && req.Method != "auth.login_ex" && calls >= f.closeAfter {
			return
		}
	}
}

// wsURL is the fake's JSON-RPC endpoint under the given host name.
func (f *fakeTrueNAS) wsURL(host string) string {
	port := f.Listener.Addr().(*net.TCPAddr).Port
	return "wss://" + net.JoinHostPort(host, strconv.Itoa(port)) + apiPath
}

// logins counts the auth.login_ex frames received.
func (f *fakeTrueNAS) logins(t *testing.T) int {
	t.Helper()
	raws, _, _ := f.snapshot()
	n := 0
	for _, fr := range parseFrames(t, raws) {
		if fr.Method == "auth.login_ex" {
			n++
		}
	}
	return n
}

func (f *fakeTrueNAS) unauthedCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unauthed
}

// closeSocket closes the server side of connection i (1-based).
func (f *fakeTrueNAS) closeSocket(i int) {
	f.mu.Lock()
	conn := f.sockets[i-1]
	f.mu.Unlock()
	conn.Close()
}

// waitFrames waits until the fake has received at least n frames.
func (f *fakeTrueNAS) waitFrames(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raws, _, _ := f.snapshot(); len(raws) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fake did not receive %d frames", n)
}

func (f *fakeTrueNAS) snapshot() ([]string, []string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.frames...), append([]string(nil), f.paths...), f.conns
}

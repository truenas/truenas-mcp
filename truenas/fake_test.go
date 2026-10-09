package truenas

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

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

	mu     sync.Mutex
	frames []string
	paths  []string
	conns  int
}

func newFakeTrueNAS(t *testing.T, configure func(*fakeTrueNAS)) *fakeTrueNAS {
	t.Helper()
	f := &fakeTrueNAS{loginType: "SUCCESS", errors: map[string]map[string]interface{}{}}
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
	f.mu.Unlock()

	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	calls := 0
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
			conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0", "method": "collection_update",
				"params": map[string]interface{}{"msg": "added", "collection": "core.get_jobs"},
			})
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
		case f.errors[req.Method] != nil:
			reply["error"] = f.errors[req.Method]
		default:
			calls++
			// Secret-bearing result, like core.get_jobs job arguments
			reply["result"] = map[string]interface{}{"bindpw": "responsesecret9", "domain": "example.com"}
		}
		if err := conn.WriteJSON(reply); err != nil {
			return
		}
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

func (f *fakeTrueNAS) sawKey() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fr := range f.frames {
		if strings.Contains(fr, testAPIKey) {
			return true
		}
	}
	return false
}

func (f *fakeTrueNAS) snapshot() ([]string, []string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.frames...), append([]string(nil), f.paths...), f.conns
}

// trustingTLS verifies the fake's certificate properly (it is valid for
// 127.0.0.1 and example.com), unlike InsecureSkipVerify.
func (f *fakeTrueNAS) trustingTLS() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(f.Certificate())
	return &tls.Config{RootCAs: pool}
}

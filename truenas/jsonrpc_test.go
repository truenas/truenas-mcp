package truenas

import (
	"crypto/tls"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func insecureClient(t *testing.T, f *fakeTrueNAS, opts Options) *Client {
	t.Helper()
	c, err := NewClientWithOptions(f.wsURL("127.0.0.1"), testAPIKey, &tls.Config{InsecureSkipVerify: true}, opts)
	if err != nil {
		t.Fatalf("NewClientWithOptions: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

type frame struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
	Raw     map[string]json.RawMessage
}

func parseFrames(t *testing.T, raws []string) []frame {
	t.Helper()
	out := make([]frame, 0, len(raws))
	for _, r := range raws {
		var fr frame
		if err := json.Unmarshal([]byte(r), &fr); err != nil {
			t.Fatalf("frame is not JSON: %s", r)
		}
		json.Unmarshal([]byte(r), &fr.Raw)
		out = append(out, fr)
	}
	return out
}

func TestEndpointURL(t *testing.T) {
	ok := map[string]string{
		"truenas.example":                        "wss://truenas.example:443/api/current",
		"192.168.0.31":                           "wss://192.168.0.31:443/api/current",
		"truenas.example:8443":                   "wss://truenas.example:443/api/current", // port ignored, as before
		"wss://truenas.example:443/api/current":  "wss://truenas.example:443/api/current",
		"wss://truenas.example/api/current":      "wss://truenas.example/api/current",
		"wss://truenas.example:8443/api/current": "wss://truenas.example:8443/api/current",
		"wss://truenas.example:443/websocket":    "wss://truenas.example:443/api/current", // legacy path upgraded
		"wss://truenas.example":                  "wss://truenas.example/api/current",
		"wss://truenas.example:443/":             "wss://truenas.example:443/api/current",
	}
	for in, want := range ok {
		c := &Client{endpoint: in}
		u, err := c.endpointURL()
		if err != nil || u.String() != want {
			t.Errorf("endpointURL(%q) = %v, %v; want %s", in, u, err, want)
		}
	}
	refused := map[string]string{
		"ws://truenas.example/api/current": "ws://",
		"ws://truenas.example/websocket":   "ws://",
		"https://truenas.example":          "scheme",
		"wss:///api/current":               "no host",
	}
	for in, mention := range refused {
		c := &Client{endpoint: in}
		if u, err := c.endpointURL(); err == nil {
			t.Errorf("endpointURL(%q) = %v; want refusal", in, u)
		} else if !strings.Contains(err.Error(), mention) {
			t.Errorf("endpointURL(%q) error %q should mention %q", in, err, mention)
		}
	}
}

func TestFramesAreJSONRPC(t *testing.T) {
	f := newFakeTrueNAS(t, nil)
	c := insecureClient(t, f, Options{})
	if _, err := c.Call("system.info"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if _, err := c.Call("pool.query", []interface{}{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	raws, paths, _ := f.snapshot()
	if paths[0] != apiPath {
		t.Errorf("connected to %q, want %q", paths[0], apiPath)
	}
	frames := parseFrames(t, raws)
	if frames[0].Method != "auth.login_ex" {
		t.Errorf("first frame is %q, want auth.login_ex", frames[0].Method)
	}
	seen := map[string]bool{}
	for _, fr := range frames {
		if fr.JSONRPC != "2.0" {
			t.Errorf("frame without jsonrpc 2.0: %+v", fr.Raw)
		}
		if _, legacy := fr.Raw["msg"]; legacy {
			t.Errorf("legacy msg field sent: %+v", fr.Raw)
		}
		if !strings.HasPrefix(strings.TrimSpace(string(fr.Raw["params"])), "[") {
			t.Errorf("params is not an array: %s", fr.Raw["params"])
		}
		var n uint64
		if err := json.Unmarshal(fr.ID, &n); err != nil {
			t.Errorf("id is not a number: %s", fr.ID)
		}
		if seen[string(fr.ID)] {
			t.Errorf("duplicate id %s", fr.ID)
		}
		seen[string(fr.ID)] = true
	}
	if string(frames[1].Raw["params"]) != "[]" {
		t.Errorf("call without params sent %s, want []", frames[1].Raw["params"])
	}
}

func TestLoginExPayload(t *testing.T) {
	for _, tc := range []struct{ user, want string }{{"", "root"}, {"svc-mcp", "svc-mcp"}} {
		f := newFakeTrueNAS(t, nil)
		c := insecureClient(t, f, Options{APIUser: tc.user})
		if err := c.Authenticate(); err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		raws, _, _ := f.snapshot()
		fr := parseFrames(t, raws)[0]
		if len(fr.Params) != 1 {
			t.Fatalf("auth.login_ex params = %d entries, want 1", len(fr.Params))
		}
		var got map[string]string
		json.Unmarshal(fr.Params[0], &got)
		want := map[string]string{"mechanism": "API_KEY_PLAIN", "username": tc.want, "api_key": testAPIKey}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("login_ex %s = %q, want %q", k, got[k], v)
			}
		}
	}
}

func TestLoginRejectsNonSuccess(t *testing.T) {
	for _, rt := range []string{"AUTH_ERR", "EXPIRED", "OTP_REQUIRED", "REDIRECT", ""} {
		f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.loginType = rt })
		c := insecureClient(t, f, Options{})
		err := c.Authenticate()
		if err == nil {
			t.Errorf("response_type %q accepted", rt)
			continue
		}
		if !strings.Contains(err.Error(), "response_type="+rt) {
			t.Errorf("error %q does not name response_type %q", err, rt)
		}
		if strings.Contains(err.Error(), testAPIKey) {
			t.Errorf("API key in auth error: %s", err)
		}
		if rt == "AUTH_ERR" && !strings.Contains(err.Error(), "--api-user") {
			t.Errorf("AUTH_ERR error lacks the --api-user hint: %s", err)
		}
	}
}

func TestErrorDataReachesToolText(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.errors["pool.dataset.create"] = map[string]interface{}{
			"code": -32001, "message": "Method call error",
			"data": map[string]interface{}{
				"error": 22, "errname": "EINVAL", "reason": "[EINVAL] pool.dataset.create.name: bad name",
				"extra": []interface{}{[]interface{}{"pool.dataset.create.name", "must not contain spaces", 22}},
				"trace": map[string]interface{}{"formatted": "Traceback ..."},
			},
		}
	})
	c := insecureClient(t, f, Options{})
	_, err := c.Call("pool.dataset.create", map[string]interface{}{"name": "tank/x"})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"EINVAL", "bad name", "(code 22)", "pool.dataset.create.name", "Method: pool.dataset.create", "Traceback", "Details:", "must not contain spaces"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text missing %q:\n%s", want, err)
		}
	}
}

func TestErrnamePrefixedWhenMissingFromReason(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.errors["pool.get"] = map[string]interface{}{
			"code": -32001, "message": "Method call error",
			"data": map[string]interface{}{"error": 2, "errname": "ENOENT", "reason": "pool tank not found"},
		}
	})
	c := insecureClient(t, f, Options{})
	_, err := c.Call("pool.get")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"[ENOENT] pool tank not found", "(code 2)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text missing %q:\n%s", want, err)
		}
	}
}

func TestErrorWithoutData(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.errors["no.such"] = map[string]interface{}{"code": -32601, "message": "Method not found"}
	})
	c := insecureClient(t, f, Options{})
	_, err := c.Call("no.such")
	if err == nil || !strings.Contains(err.Error(), "Method not found") || !strings.Contains(err.Error(), "(code -32601)") {
		t.Errorf("got %v", err)
	}
}

func TestNotificationsAndStringIDsRouted(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.notify = true; f.stringIDs = true })
	c := insecureClient(t, f, Options{})
	res, err := c.Call("system.info")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(res), "example.com") {
		t.Errorf("unexpected result %s", res)
	}
}

func TestConcurrentCallsWithNotifications(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.notify = true })
	c := insecureClient(t, f, Options{})
	if err := c.Authenticate(); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
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
		t.Errorf("concurrent Call: %v", err)
	}
}

func TestReconnectReauthenticates(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) { f.closeAfter = 1 })
	c := insecureClient(t, f, Options{})
	for i := 0; i < 2; i++ {
		if _, err := c.Call("system.info"); err != nil {
			t.Fatalf("Call %d: %v", i, err)
		}
	}
	raws, _, conns := f.snapshot()
	if conns != 2 {
		t.Errorf("conns = %d, want 2", conns)
	}
	logins := 0
	for _, fr := range parseFrames(t, raws) {
		if fr.Method == "auth.login_ex" {
			logins++
		}
	}
	if logins != 2 {
		t.Errorf("auth.login_ex sent %d times, want 2 (one per connection)", logins)
	}
}

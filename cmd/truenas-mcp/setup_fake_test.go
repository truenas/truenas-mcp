package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// fakeSetupNAS is a TLS JSON-RPC 2.0 server that answers the calls setup
// makes, following the 25.10 auth and api_key plugins, and records every
// inbound frame.
type fakeSetupNAS struct {
	*httptest.Server

	user, password string
	otp            string // required second factor; empty = no 2FA
	roles          []string
	users          map[string][]string // other users: name -> roles
	version        string

	loginType   string                 // force this PASSWORD_PLAIN response_type
	loginErr    map[string]interface{} // auth.login_ex PASSWORD_PLAIN error object
	otpErr      map[string]interface{} // auth.login_ex_continue error object
	createErr   map[string]interface{} // api_key.create error object
	queryDenied bool                   // api_key.query fails with EACCES
	keyLoginBad bool                   // API_KEY_PLAIN logins fail

	mu      sync.Mutex
	frames  []string
	keys    []*fakeKey
	nextID  int
	deleted []int
	creates []map[string]interface{}
	updates []map[string]interface{}
}

type fakeKey struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	secret   string
}

const (
	fakeUser     = "mcpadmin"
	fakePassword = "PW-MARKER-7f3a9c"
	fakeOTP      = "OTP-MARKER-424242"
)

func newFakeSetupNAS(t *testing.T, configure func(*fakeSetupNAS)) *fakeSetupNAS {
	t.Helper()
	f := &fakeSetupNAS{
		user: fakeUser, password: fakePassword,
		roles:   []string{"READONLY_ADMIN"},
		users:   map[string][]string{},
		version: "25.10.1",
		nextID:  10,
	}
	if configure != nil {
		configure(f)
	}
	f.Server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.Config.ErrorLog = log.New(io.Discard, "", 0) // expected handshake failures in TLS tests
	f.StartTLS()
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSetupNAS) host() string { return strings.TrimPrefix(f.URL, "https://") }

func (f *fakeSetupNAS) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.frames...)
}

// methods lists the methods received, in order.
func (f *fakeSetupNAS) methods() []string {
	var out []string
	for _, fr := range f.snapshot() {
		var req struct{ Method string }
		_ = json.Unmarshal([]byte(fr), &req)
		out = append(out, req.Method)
	}
	return out
}

func (f *fakeSetupNAS) count(method string) int {
	n := 0
	for _, m := range f.methods() {
		if m == method {
			n++
		}
	}
	return n
}

func (f *fakeSetupNAS) keyByName(name string) *fakeKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k.Name == name {
			return k
		}
	}
	return nil
}

func (f *fakeSetupNAS) addKey(name, owner string) *fakeKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	k := &fakeKey{ID: f.nextID, Name: name, Username: owner, secret: fmt.Sprintf("%d-KEY-MARKER-%d", f.nextID, f.nextID)}
	f.keys = append(f.keys, k)
	return k
}

func callErr(errno int, errname, reason string) map[string]interface{} {
	return map[string]interface{}{
		"code": -32001, "message": "Method call error",
		"data": map[string]interface{}{"error": errno, "errname": errname, "reason": reason},
	}
}

type fakeConnState struct {
	authed     bool
	user       string
	otpPending bool
	otpTries   int
}

func (f *fakeSetupNAS) userInfo() map[string]interface{} {
	return map[string]interface{}{
		"pw_name":            f.user,
		"privilege":          map[string]interface{}{"roles": f.roles},
		"account_attributes": []string{"LOCAL"},
	}
}

func (f *fakeSetupNAS) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	st := &fakeConnState{}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.frames = append(f.frames, string(raw))
		f.mu.Unlock()
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &req) != nil {
			return
		}
		result, errObj := f.handle(st, req.Method, req.Params)
		reply := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID}
		if errObj != nil {
			reply["error"] = errObj
		} else {
			reply["result"] = result
		}
		if err := conn.WriteJSON(reply); err != nil {
			return
		}
	}
}

func (f *fakeSetupNAS) handle(st *fakeConnState, method string, params []json.RawMessage) (interface{}, map[string]interface{}) {
	arg := func(i int, v interface{}) {
		if i < len(params) {
			_ = json.Unmarshal(params[i], v)
		}
	}
	switch method {
	case "auth.login_ex":
		var d map[string]interface{}
		arg(0, &d)
		switch d["mechanism"] {
		case "PASSWORD_PLAIN":
			if f.loginErr != nil {
				return nil, f.loginErr
			}
			if f.loginType != "" {
				res := map[string]interface{}{"response_type": f.loginType}
				if f.loginType == "REDIRECT" {
					res["urls"] = []string{"192.0.2.11"}
				}
				return res, nil
			}
			if d["username"] != f.user || d["password"] != f.password {
				return map[string]interface{}{"response_type": "AUTH_ERR"}, nil
			}
			if f.otp != "" {
				st.otpPending, st.otpTries = true, 0
				return map[string]interface{}{"response_type": "OTP_REQUIRED", "username": f.user}, nil
			}
			st.authed, st.user = true, f.user
			return map[string]interface{}{"response_type": "SUCCESS", "user_info": f.userInfo(), "authenticator": "LEVEL_1"}, nil
		case "API_KEY_PLAIN":
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, k := range f.keys {
				if k.secret == d["api_key"] && k.Username == d["username"] && !f.keyLoginBad {
					st.authed, st.user = true, k.Username
					return map[string]interface{}{"response_type": "SUCCESS", "user_info": nil, "authenticator": "LEVEL_1"}, nil
				}
			}
			return map[string]interface{}{"response_type": "AUTH_ERR"}, nil
		}
		return nil, callErr(22, "EINVAL", "unexpected mechanism")
	case "auth.login_ex_continue":
		if f.otpErr != nil {
			return nil, f.otpErr
		}
		var d map[string]interface{}
		arg(0, &d)
		if !st.otpPending || d["mechanism"] != "OTP_TOKEN" {
			return nil, callErr(22, "EINVAL", "OTP_TOKEN: no authentication in progress")
		}
		if d["otp_token"] == f.otp {
			st.otpPending, st.authed, st.user = false, true, f.user
			return map[string]interface{}{"response_type": "SUCCESS", "user_info": f.userInfo(), "authenticator": "LEVEL_2"}, nil
		}
		st.otpTries++
		if st.otpTries >= 3 {
			st.otpPending = false
			return map[string]interface{}{"response_type": "AUTH_ERR"}, nil
		}
		return map[string]interface{}{"response_type": "OTP_REQUIRED", "username": f.user}, nil
	}
	if !st.authed {
		return nil, callErr(13, "ENOTAUTHENTICATED", "Not authenticated")
	}
	switch method {
	case "auth.logout":
		st.authed = false
		return true, nil
	case "system.info":
		return map[string]interface{}{"version": f.version, "hostname": "nas"}, nil
	case "user.query":
		var filters [][]interface{}
		arg(0, &filters)
		name, _ := filters[0][2].(string)
		if name == f.user {
			return []interface{}{map[string]interface{}{"username": name, "roles": f.roles}}, nil
		}
		if roles, ok := f.users[name]; ok {
			return []interface{}{map[string]interface{}{"username": name, "roles": roles}}, nil
		}
		return []interface{}{}, nil
	case "api_key.query", "api_key.my_keys":
		if method == "api_key.query" && f.queryDenied {
			return nil, callErr(13, "EACCES", "Not authorized")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []interface{}{}
		for _, k := range f.keys {
			if method == "api_key.my_keys" && k.Username != st.user {
				continue
			}
			out = append(out, map[string]interface{}{"id": k.ID, "name": k.Name, "username": k.Username, "keyhash": "********"})
		}
		return out, nil
	case "api_key.create":
		if f.createErr != nil {
			return nil, f.createErr
		}
		var d map[string]interface{}
		arg(0, &d)
		f.mu.Lock()
		f.creates = append(f.creates, d)
		for _, k := range f.keys {
			if k.Name == d["name"] {
				f.mu.Unlock()
				e := callErr(22, "EINVAL", "api_key_create: name must be unique")
				return nil, e
			}
		}
		f.mu.Unlock()
		name, _ := d["name"].(string)
		owner, _ := d["username"].(string)
		k := f.addKey(name, owner)
		return map[string]interface{}{"id": k.ID, "name": k.Name, "username": k.Username, "key": k.secret}, nil
	case "api_key.update":
		var id int
		var d map[string]interface{}
		arg(0, &id)
		arg(1, &d)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.updates = append(f.updates, d)
		for _, k := range f.keys {
			if k.ID == id {
				if d["reset"] == true {
					k.secret = fmt.Sprintf("%d-ROTATED-KEY-MARKER", id)
					return map[string]interface{}{"id": k.ID, "name": k.Name, "username": k.Username, "key": k.secret}, nil
				}
				return map[string]interface{}{"id": k.ID, "name": k.Name, "username": k.Username}, nil
			}
		}
		return nil, callErr(22, "EINVAL", "no such key")
	case "api_key.delete":
		var id int
		arg(0, &id)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deleted = append(f.deleted, id)
		for i, k := range f.keys {
			if k.ID == id {
				f.keys = append(f.keys[:i], f.keys[i+1:]...)
				return true, nil
			}
		}
		return nil, callErr(22, "EINVAL", "no such key")
	}
	return nil, callErr(22, "EINVAL", "method not implemented by the fake: "+method)
}

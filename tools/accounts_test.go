package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/truenas/truenas-mcp/redact"
)

// Secret sentinels a hostile or future middleware might return on a user
// row. None may appear in query_users output.
var userSecretSentinels = map[string]interface{}{
	"unixhash":         "$6$sentinel-unixhash",
	"smbhash":          "SENTINEL0SMBHASH0000000000000000",
	"password_history": []interface{}{"$6$sentinel-history"},
	"password":         "sentinel-plain-password",
	"sshpubkey":        "-----BEGIN OPENSSH PRIVATE KEY-----\nsentinel-ssh-private\n-----END OPENSSH PRIVATE KEY-----",
	"api_keys":         []interface{}{4242.0},
	"twofactor_config": map[string]interface{}{
		"secret":           "SENTINEL2FASECRET",
		"provisioning_uri": "otpauth://totp/x?secret=SENTINEL2FAURI",
	},
	"twofactor_secret": "SENTINEL2FASECRET2",
}

func userRow(id, uid float64, username string, extra map[string]interface{}) map[string]interface{} {
	row := map[string]interface{}{
		"id": id, "uid": uid, "username": username, "full_name": "Example " + username,
		"builtin": false, "immutable": false, "local": true, "locked": false,
		"password_disabled": false, "smb": true, "twofactor_auth_configured": true,
		"home": "/var/empty", "shell": "/usr/sbin/nologin",
		"group":         map[string]interface{}{"id": 40.0, "bsdgrp_gid": 3000.0, "bsdgrp_group": username},
		"groups":        []interface{}{41.0, 42.0},
		"sudo_commands": []interface{}{}, "sudo_commands_nopasswd": []interface{}{"/usr/bin/true"},
		"roles": []interface{}{"READONLY_ADMIN"},
	}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

type accountFake struct {
	mu      sync.Mutex
	calls   map[string][]json.RawMessage
	users   []interface{}
	groups  []interface{}
	failFor map[string]bool
	count   float64
}

func (a *accountFake) middleware() *fakeMiddleware {
	return &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "user.query" && method != "group.query" {
			return nil, nil, false
		}
		a.mu.Lock()
		if a.calls == nil {
			a.calls = map[string][]json.RawMessage{}
		}
		a.calls[method] = append(a.calls[method], params)
		a.mu.Unlock()
		var raw []interface{}
		_ = json.Unmarshal(params, &raw)
		var filters []interface{}
		options := map[string]interface{}{}
		if len(raw) > 0 {
			filters, _ = raw[0].([]interface{})
		}
		if len(raw) > 1 {
			options, _ = raw[1].(map[string]interface{})
		}
		if options["count"] == true {
			return a.count, nil, true
		}
		lookup := len(filters) == 1 && fmt.Sprint(filters[0].([]interface{})[0]) == "id"
		if lookup && a.failFor[method] {
			return nil, map[string]interface{}{"code": -32001, "message": "lookup failed"}, true
		}
		if method == "user.query" {
			if lookup {
				return idNameRows(a.users, filters, "username"), nil, true
			}
			return a.users, nil, true
		}
		if lookup {
			return idNameRows(a.groups, filters, "name"), nil, true
		}
		return a.groups, nil, true
	}}
}

// idNameRows answers an [id, in, [...]] lookup with select [id, field].
func idNameRows(rows []interface{}, filters []interface{}, field string) []interface{} {
	want := map[float64]bool{}
	for _, v := range filters[0].([]interface{})[2].([]interface{}) {
		want[v.(float64)] = true
	}
	out := []interface{}{}
	for _, r := range rows {
		m := r.(map[string]interface{})
		if want[m["id"].(float64)] {
			out = append(out, map[string]interface{}{"id": m["id"], field: m[field]})
		}
	}
	return out
}

func (a *accountFake) params(t *testing.T, method string, i int) ([]interface{}, map[string]interface{}) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls[method]) <= i {
		t.Fatalf("%s call %d not made (calls: %d)", method, i, len(a.calls[method]))
	}
	return parseQueryParams(t, a.calls[method][i])
}

func TestQueryUsersNeverReturnsSecrets(t *testing.T) {
	a := &accountFake{
		users: []interface{}{userRow(70, 3000, "alice-example", userSecretSentinels)},
		groups: []interface{}{
			map[string]interface{}{"id": 41.0, "name": "media-example"},
			map[string]interface{}{"id": 42.0, "name": "backup-example"},
		},
	}
	r := queryFilterRegistry(t, a.middleware())
	out, err := r.CallTool(t.Context(), "query_users", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	// The handler drops secrets itself; the server's redact.ToolOutput pass
	// is a second layer and must find nothing left to change.
	if redact.ToolOutput(out) != out {
		t.Errorf("redact.ToolOutput changed query_users output, so a secret-looking field got through:\n%s", out)
	}
	for _, s := range []string{
		"sentinel", "SENTINEL", "PRIVATE KEY", "otpauth", "4242",
		"unixhash", "smbhash", "password_history", "sshpubkey", "api_keys", "twofactor_config", `"password"`,
	} {
		if strings.Contains(out, s) {
			t.Errorf("query_users output contains %q:\n%s", s, out)
		}
	}

	_, options := a.params(t, "user.query", 0)
	sel, _ := options["select"].([]interface{})
	for _, f := range sel {
		switch f {
		case "unixhash", "smbhash", "password_history", "sshpubkey", "api_keys":
			t.Errorf("user.query select requests %v", f)
		}
	}

	users := decode(t, out)["users"].([]interface{})
	u := users[0].(map[string]interface{})
	want := map[string]interface{}{
		"username": "alice-example", "uid": 3000.0, "full_name": "Example alice-example",
		"builtin": false, "immutable": false, "locked": false, "password_disabled": false,
		"smb": true, "two_factor_enabled": true, "sudo": true, "sudo_nopasswd": true,
		"home": "/var/empty", "shell": "/usr/sbin/nologin",
		"primary_group": "alice-example", "primary_gid": 3000.0,
	}
	for k, v := range want {
		if u[k] != v {
			t.Errorf("%s = %v, want %v", k, u[k], v)
		}
	}
	if got := u["groups"]; !reflect.DeepEqual(got, []interface{}{"backup-example", "media-example"}) {
		t.Errorf("groups = %v", got)
	}
	if got := u["roles"]; !reflect.DeepEqual(got, []interface{}{"READONLY_ADMIN"}) {
		t.Errorf("roles = %v", got)
	}
}

func TestQueryUsersFilters(t *testing.T) {
	cases := []struct {
		args map[string]interface{}
		want []interface{}
	}{
		{map[string]interface{}{}, []interface{}{[]interface{}{"local", "=", true}}},
		{map[string]interface{}{"include_directory_users": true}, []interface{}{}},
		{
			map[string]interface{}{"username": "svc-example", "builtin": false, "locked": true},
			[]interface{}{
				[]interface{}{"local", "=", true},
				[]interface{}{"username", "=", "svc-example"},
				[]interface{}{"builtin", "=", false},
				[]interface{}{"locked", "=", true},
			},
		},
	}
	for _, tc := range cases {
		a := &accountFake{users: []interface{}{}}
		r := queryFilterRegistry(t, a.middleware())
		out, err := r.CallTool(t.Context(), "query_users", tc.args)
		if err != nil {
			t.Fatal(err)
		}
		filters, options := a.params(t, "user.query", 0)
		if filters == nil {
			filters = []interface{}{}
		}
		if !reflect.DeepEqual(filters, tc.want) {
			t.Errorf("args %v: filters = %v, want %v", tc.args, filters, tc.want)
		}
		if options["limit"] != 50.0 {
			t.Errorf("limit = %v, want 50", options["limit"])
		}
		if resp := decode(t, out); resp["total_users"] != 0.0 || resp["user_count"] != 0.0 {
			t.Errorf("empty result: %s", out)
		}
		// No secondary groups: no group lookup.
		if len(a.calls["group.query"]) != 0 {
			t.Errorf("group.query called with no groups to resolve")
		}
	}
}

func TestQueryUsersLimitedAndGroupLookupFailure(t *testing.T) {
	a := &accountFake{
		users: []interface{}{
			userRow(70, 3000, "alice-example", nil),
			userRow(71, 3001, "bob-example", map[string]interface{}{"sudo_commands_nopasswd": []interface{}{}}),
		},
		failFor: map[string]bool{"group.query": true},
		count:   5,
	}
	r := queryFilterRegistry(t, a.middleware())
	out, err := r.CallTool(t.Context(), "query_users", map[string]interface{}{"limit": 2.0})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	if resp["total_users"] != 5.0 || !strings.Contains(fmt.Sprint(resp["note"]), "Showing 2 of 5 users") {
		t.Errorf("limited: %s", out)
	}
	if !strings.Contains(fmt.Sprint(resp["note"]), "unresolved_group_ids") {
		t.Errorf("note does not mention the failed group lookup: %s", out)
	}
	bob := resp["users"].([]interface{})[1].(map[string]interface{})
	if bob["sudo"] != false || bob["sudo_nopasswd"] != false {
		t.Errorf("bob sudo flags = %v / %v", bob["sudo"], bob["sudo_nopasswd"])
	}
	if !reflect.DeepEqual(bob["unresolved_group_ids"], []interface{}{41.0, 42.0}) || len(bob["groups"].([]interface{})) != 0 {
		t.Errorf("bob groups = %v, unresolved = %v", bob["groups"], bob["unresolved_group_ids"])
	}
}

func TestQueryGroupsMembers(t *testing.T) {
	var users []interface{}
	var members []interface{}
	for i := 0; i < maxGroupMembersListed+5; i++ {
		id := float64(100 + i)
		users = append(users, map[string]interface{}{"id": id, "username": fmt.Sprintf("user%03d-example", i)})
		members = append(members, id)
	}
	a := &accountFake{
		users: users,
		groups: []interface{}{
			map[string]interface{}{
				"id": 41.0, "gid": 3100.0, "name": "media-example", "builtin": false, "immutable": false,
				"local": true, "smb": true, "users": []interface{}{101.0, 100.0, 999.0},
				"roles": []interface{}{}, "sudo_commands": []interface{}{"/usr/bin/true"}, "sudo_commands_nopasswd": []interface{}{},
			},
			map[string]interface{}{
				"id": 42.0, "gid": 545.0, "name": "builtin_users", "builtin": true, "immutable": false,
				"local": true, "smb": true, "users": members, "roles": []interface{}{"SHARING_ADMIN", "READONLY_ADMIN"},
			},
		},
	}
	r := queryFilterRegistry(t, a.middleware())
	out, err := r.CallTool(t.Context(), "query_groups", map[string]interface{}{"name": "media-example", "builtin": false})
	if err != nil {
		t.Fatal(err)
	}
	filters, _ := a.params(t, "group.query", 0)
	want := []interface{}{
		[]interface{}{"local", "=", true},
		[]interface{}{"name", "=", "media-example"},
		[]interface{}{"builtin", "=", false},
	}
	if !reflect.DeepEqual(filters, want) {
		t.Errorf("filters = %v", filters)
	}
	if n := len(a.calls["user.query"]); n != 1 {
		t.Errorf("user.query called %d times, want one lookup for every group", n)
	}

	groups := decode(t, out)["groups"].([]interface{})
	media := groups[0].(map[string]interface{})
	if !reflect.DeepEqual(media["members"], []interface{}{"user000-example", "user001-example"}) ||
		!reflect.DeepEqual(media["unresolved_member_ids"], []interface{}{999.0}) || media["member_count"] != 3.0 {
		t.Errorf("media members = %v", media)
	}
	if media["sudo"] != true || media["sudo_nopasswd"] != false || media["gid"] != 3100.0 || media["smb"] != true {
		t.Errorf("media flags = %v", media)
	}
	if _, ok := media["members_truncated"]; ok {
		t.Errorf("media marked truncated")
	}
	bu := groups[1].(map[string]interface{})
	if len(bu["members"].([]interface{})) != maxGroupMembersListed || bu["members_truncated"] != true ||
		bu["member_count"] != float64(maxGroupMembersListed+5) {
		t.Errorf("builtin_users: count=%v truncated=%v listed=%d", bu["member_count"], bu["members_truncated"], len(bu["members"].([]interface{})))
	}
	if !reflect.DeepEqual(bu["roles"], []interface{}{"READONLY_ADMIN", "SHARING_ADMIN"}) {
		t.Errorf("roles = %v", bu["roles"])
	}
	if redact.ToolOutput(out) != out {
		t.Errorf("redact.ToolOutput changed query_groups output:\n%s", out)
	}
}

func TestAccountToolsUnderPolicy(t *testing.T) {
	a := &accountFake{users: []interface{}{}, groups: []interface{}{}}
	r := queryFilterRegistry(t, a.middleware())
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, def := range r.ListTools() {
		listed[def.Name] = true
	}
	for _, name := range []string{"query_users", "query_groups"} {
		if !listed[name] {
			t.Errorf("%s hidden in read-only mode", name)
		}
		if _, err := r.CallTool(t.Context(), name, map[string]interface{}{}); err != nil {
			t.Errorf("%s in read-only mode: %v", name, err)
		}
	}

	if err := r.SetPolicy(Policy{Deny: []string{"query_users"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CallTool(t.Context(), "query_users", map[string]interface{}{}); err == nil {
		t.Error("query_users ran although denied")
	}
	if err := r.SetPolicy(Policy{Allow: []string{"query_users"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CallTool(t.Context(), "query_groups", map[string]interface{}{}); err == nil {
		t.Error("query_groups ran although not allowed")
	}
}

package tools

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// accountWriteFake serves local users and groups for the account write tools.
// user.create, user.update, group.create and group.update change the
// fixture, so a call reads back what it wrote.
type accountWriteFake struct {
	mu         sync.Mutex
	users      []map[string]interface{}
	groups     []map[string]interface{}
	paths      map[string]string // filesystem.stat type by path
	fail       map[string]map[string]interface{}
	onQuery    func(method string) // runs before every user.query / group.query
	nextUser   int
	smbGuest   string // smb.config guest; "" serves nobody
	smbNoGuest bool   // smb.config reports no guest field
}

func accountUser(id, uid int, name string, group int, groups []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "uid": uid, "username": name, "full_name": strings.ToUpper(name[:1]) + name[1:],
		"builtin": false, "immutable": false, "local": true, "locked": false,
		"password_disabled": true, "group": map[string]interface{}{"id": group, "bsdgrp_group": name},
		"groups": groups, "home": defaultHomePath, "shell": "/usr/bin/bash", "smb": false,
		"twofactor_auth_configured": false, "sudo_commands": []interface{}{},
		"sudo_commands_nopasswd": []interface{}{}, "roles": []interface{}{},
		"ssh_password_enabled": false, "email": nil, "last_password_change": nil,
	}
}

func accountGroup(id, gid int, name string, users []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "gid": gid, "name": name, "builtin": false, "immutable": false, "local": true,
		"smb": false, "users": users, "roles": []interface{}{},
		"sudo_commands": []interface{}{}, "sudo_commands_nopasswd": []interface{}{},
	}
}

func accountUserRows() []map[string]interface{} {
	root := accountUser(1, 0, "root", 1, []interface{}{})
	root["builtin"], root["immutable"], root["home"] = true, true, "/root"
	admin := accountUser(2, adminUID, "truenas_admin", 2, []interface{}{40})
	admin["builtin"], admin["roles"] = true, []interface{}{"FULL_ADMIN"}
	svc := accountUser(3, 3001, "svc", 22, []interface{}{})
	alice := accountUser(10, 3000, "alice", 20, []interface{}{})
	bob := accountUser(11, 3002, "bob", 21, []interface{}{42})
	bob["home"], bob["password_disabled"], bob["smb"] = "/mnt/tank/home/bob", false, true
	dave := accountUser(12, 3003, "dave", 23, []interface{}{44})
	eve := accountUser(13, 3004, "eve", 24, []interface{}{})
	eve["sudo_commands"] = []interface{}{"ALL"}
	daemon := accountUser(14, 1, "daemon", 25, []interface{}{})
	daemon["builtin"] = true
	remote := accountUser(15, 200000, "remote", 26, []interface{}{})
	remote["local"] = false
	return []map[string]interface{}{root, admin, svc, alice, bob, dave, eve, daemon, remote}
}

func accountGroupRows() []map[string]interface{} {
	root := accountGroup(1, 0, "root", []interface{}{1})
	root["builtin"] = true
	adminGrp := accountGroup(2, adminUID, "truenas_admin", []interface{}{2})
	admins := accountGroup(40, 544, "builtin_administrators", []interface{}{2})
	admins["builtin"], admins["roles"] = true, []interface{}{"FULL_ADMIN"}
	users := accountGroup(43, 545, "builtin_users", []interface{}{})
	users["builtin"] = true
	ops := accountGroup(44, 3300, "ops", []interface{}{12})
	ops["sudo_commands_nopasswd"] = []interface{}{"ALL"}
	return []map[string]interface{}{
		root, adminGrp, admins, users, ops,
		accountGroup(20, 3000, "alice", []interface{}{}),
		accountGroup(21, 3002, "bob", []interface{}{}),
		accountGroup(22, 3001, "svc", []interface{}{}),
		accountGroup(23, 3003, "dave", []interface{}{}),
		accountGroup(24, 3004, "eve", []interface{}{}),
		accountGroup(25, 1, "daemon", []interface{}{}),
		accountGroup(42, 3100, "staff", []interface{}{11}),
	}
}

func newAccountWriteFake() *accountWriteFake {
	return &accountWriteFake{
		users:    accountUserRows(),
		groups:   accountGroupRows(),
		paths:    map[string]string{"/mnt/tank/home": "DIRECTORY", "/mnt/tank/home/bob": "DIRECTORY", "/mnt/tank/file": "FILE"},
		fail:     map[string]map[string]interface{}{},
		nextUser: 100,
	}
}

func (f *accountWriteFake) handle(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
	if f.onQuery != nil && (method == "user.query" || method == "group.query") {
		f.onQuery(method)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.fail[method]; ok {
		return nil, e, true
	}
	var p []interface{}
	_ = json.Unmarshal(params, &p)
	switch method {
	case "user.query":
		return f.users, nil, true
	case "group.query":
		return f.groups, nil, true
	case "filesystem.stat":
		path, _ := p[0].(string)
		if typ, ok := f.paths[path]; ok {
			return map[string]interface{}{"type": typ}, nil, true
		}
		return nil, rpcErr("[ENOENT] Path " + path + " not found"), true
	case "user.create":
		payload, _ := p[0].(map[string]interface{})
		f.nextUser++
		name, _ := payload["username"].(string)
		row := accountUser(f.nextUser, 4000+f.nextUser, name, 20, []interface{}{})
		row["password_disabled"] = payload["password_disabled"]
		row["password"] = payload["password"]
		f.users = append(f.users, row)
		return row, nil, true
	case "user.update":
		id, _ := intID(p[0])
		payload, _ := p[1].(map[string]interface{})
		for _, u := range f.users {
			if n, _ := intID(u["id"]); n == id {
				for k, v := range payload {
					if k != "password" {
						u[k] = v
					}
				}
				return u, nil, true
			}
		}
		return nil, rpcErr("no such user"), true
	case "group.create":
		payload, _ := p[0].(map[string]interface{})
		name, _ := payload["name"].(string)
		users, _ := payload["users"].([]interface{})
		f.groups = append(f.groups, accountGroup(300, 5000, name, users))
		return 300, nil, true
	case "group.update":
		id, _ := intID(p[0])
		payload, _ := p[1].(map[string]interface{})
		for _, g := range f.groups {
			if n, _ := intID(g["id"]); n == id {
				for k, v := range payload {
					g[k] = v
				}
				return id, nil, true
			}
		}
		return nil, rpcErr("no such group"), true
	case "user.delete", "group.delete":
		id, _ := intID(p[0])
		rows := &f.groups
		if method == "user.delete" {
			rows = &f.users
		}
		for i, row := range *rows {
			if n, _ := intID(row["id"]); n == id {
				*rows = append((*rows)[:i], (*rows)[i+1:]...)
				return id, nil, true
			}
		}
		return nil, rpcErr("no such account"), true
	case "smb.config":
		if f.smbNoGuest {
			return map[string]interface{}{"enable_smb1": false}, nil, true
		}
		guest := f.smbGuest
		if guest == "" {
			guest = "nobody"
		}
		return map[string]interface{}{"guest": guest}, nil, true
	}
	return nil, nil, false
}

// set changes field of the named user (or group, when user is false).
func (f *accountWriteFake) set(user bool, name, field string, v interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows, key := f.groups, "name"
	if user {
		rows, key = f.users, "username"
	}
	for _, row := range rows {
		if row[key] == name {
			row[field] = v
		}
	}
}

func accountRegistry(t *testing.T) (*Registry, *fakeMiddleware, *accountWriteFake) {
	t.Helper()
	af := newAccountWriteFake()
	fake := &fakeMiddleware{handle: af.handle}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake, af
}

var accountWriteMethods = []string{userCreateMethod, userUpdateMethod, groupCreateMethod, groupUpdateMethod, userDeleteMethod, groupDeleteMethod}

func assertNoAccountWrites(t *testing.T, fake *fakeMiddleware) {
	t.Helper()
	for _, m := range accountWriteMethods {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("%s sent %d time(s)", m, n)
		}
	}
}

// writePayload returns the payload map of the only call to method.
func writePayload(t *testing.T, fake *fakeMiddleware, method string, index int) map[string]interface{} {
	t.Helper()
	calls := fake.called(method)
	if len(calls) != 1 {
		t.Fatalf("%s sent %d time(s), want 1", method, len(calls))
	}
	var p []interface{}
	if err := json.Unmarshal(calls[0].Params, &p); err != nil {
		t.Fatal(err)
	}
	payload, _ := p[index].(map[string]interface{})
	if payload == nil {
		t.Fatalf("%s params: %s", method, calls[0].Params)
	}
	return payload
}

// ---- classification and read-only ----

func TestAccountWriteToolsNeedConfirmation(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	for name, args := range map[string]map[string]interface{}{
		"create_user":  {"username": "carol", "full_name": "Carol"},
		"update_user":  {"username": "alice", "full_name": "Alice B"},
		"create_group": {"name": "media"},
		"update_group": {"name": "staff", "smb": true},
	} {
		if safetyOf(name).Access != Destructive {
			t.Errorf("%s is %v, want destructive", name, safetyOf(name).Access)
		}
		out, err := r.CallTool(t.Context(), name, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if replyToken(t, out) == "" {
			t.Errorf("%s: no confirmation token:\n%s", name, out)
		}
		out, err = r.CallTool(t.Context(), name, mergeArgs(args, map[string]interface{}{"confirm": true}))
		if err == nil && replyToken(t, out) == "" {
			t.Errorf("%s: confirm=true without a token did not stop at the preview:\n%s", name, out)
		}
	}
	assertNoAccountWrites(t, fake)
}

func TestAccountWriteToolsReadOnly(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	for name, args := range map[string]map[string]interface{}{
		"create_user":  {"username": "carol", "full_name": "Carol", "password_mode": "random"},
		"update_user":  {"username": "alice", "password_mode": "random"},
		"create_group": {"name": "media"},
		"update_group": {"name": "staff", "smb": true},
	} {
		if names[name] {
			t.Errorf("%s listed in read-only mode", name)
		}
		for _, extra := range []map[string]interface{}{{}, {"dry_run": true}, {"confirmation_token": "x"}} {
			_, err := r.CallTool(t.Context(), name, mergeArgs(args, extra))
			if err == nil || !strings.Contains(err.Error(), "read-only") {
				t.Errorf("%s %v in read-only mode: err = %v", name, extra, err)
			}
		}
	}
	if len(fake.called("user.query"))+len(fake.called("group.query")) != 0 {
		t.Error("read-only mode read accounts for a refused write tool")
	}
	assertNoAccountWrites(t, fake)
}

// ---- argument refusals ----

func TestAccountRefusedArguments(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	cases := []struct {
		tool string
		args map[string]interface{}
		want string
	}{
		{"create_user", map[string]interface{}{"username": "carol", "full_name": "C", "sudo_commands": []interface{}{"ALL"}}, "sudo_commands is refused"},
		{"create_user", map[string]interface{}{"username": "carol", "full_name": "C", "sudo_commands_nopasswd": []interface{}{"ALL"}}, "sudo_commands_nopasswd is refused"},
		{"create_user", map[string]interface{}{"username": "carol", "full_name": "C", "roles": []interface{}{"FULL_ADMIN"}}, "roles is refused"},
		{"create_user", map[string]interface{}{"username": "carol", "full_name": "C", "random_password": true}, "random_password is refused"},
		{"create_user", map[string]interface{}{"username": "carol", "full_name": "C", "groups": []interface{}{40.0}}, "groups is refused"},
		{"create_user", map[string]interface{}{"username": "carol", "full_name": "C", "group_create": true}, "group_create is refused"},
		{"create_user", map[string]interface{}{"username": "carol", "full_name": "C", "home_mode": "777"}, "home_mode is refused"},
		{"update_user", map[string]interface{}{"username": "alice", "uid": 4000.0}, "uid is refused"},
		{"update_user", map[string]interface{}{"username": "alice", "sudo_commands": []interface{}{"ALL"}}, "sudo_commands is refused"},
		{"update_user", map[string]interface{}{"username": "alice", "privileges": []interface{}{"x"}}, "privileges is refused"},
		{"update_user", map[string]interface{}{"username": "alice", "new_username": "alicia"}, "new_username is refused"},
		{"update_user", map[string]interface{}{"username": "alice", "password_disabled": false}, "password_disabled is refused"},
		{"create_group", map[string]interface{}{"name": "media", "sudo_commands": []interface{}{"ALL"}}, "sudo_commands is refused"},
		{"create_group", map[string]interface{}{"name": "media", "roles": []interface{}{"READONLY_ADMIN"}}, "roles is refused"},
		{"update_group", map[string]interface{}{"name": "staff", "gid": 4000.0}, "gid is refused"},
		{"update_group", map[string]interface{}{"name": "staff", "sudo_commands_nopasswd": []interface{}{"ALL"}}, "sudo_commands_nopasswd is refused"},
		{"update_group", map[string]interface{}{"name": "staff", "users": []interface{}{10.0}}, "users is refused"},
	}
	for _, tc := range cases {
		_, err := r.CallTool(t.Context(), tc.tool, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was executed") {
			t.Errorf("%s %v: err = %v, want %q", tc.tool, tc.args, err, tc.want)
		}
	}
	// A plain typo is still an unknown argument.
	_, err := r.CallTool(t.Context(), "create_user", map[string]interface{}{"username": "carol", "full_name": "C", "usrname": "x"})
	if err == nil || !strings.Contains(err.Error(), "unknown argument") {
		t.Errorf("typo: err = %v", err)
	}
	if len(fake.called("user.query"))+len(fake.called("group.query")) != 0 {
		t.Error("refused arguments reached the middleware")
	}
	assertNoAccountWrites(t, fake)
}

// ---- target refusals ----

func TestCreateUserRefusals(t *testing.T) {
	r, fake, af := accountRegistry(t)
	base := func(extra map[string]interface{}) map[string]interface{} {
		return mergeArgs(map[string]interface{}{"username": "carol", "full_name": "Carol"}, extra)
	}
	af.paths["/mnt/tank/home/carol"] = "DIRECTORY"
	for _, tc := range []struct {
		args map[string]interface{}
		want string
	}{
		{map[string]interface{}{"username": "root", "full_name": "R"}, "root account"},
		{map[string]interface{}{"username": "admin", "full_name": "A"}, "built-in administrator account"},
		{map[string]interface{}{"username": "truenas_admin", "full_name": "A"}, "built-in administrator account"},
		{map[string]interface{}{"username": "svc", "full_name": "S"}, "account this server signs in as"},
		{map[string]interface{}{"username": "alice", "full_name": "A"}, "already exists"},
		{map[string]interface{}{"username": "staff", "full_name": "S"}, "a group named 'staff' already exists"},
		{base(map[string]interface{}{"uid": 3000.0}), "uid 3000 is already used"},
		{base(map[string]interface{}{"group": "builtin_administrators"}), "is an administrative group"},
		{base(map[string]interface{}{"group": "truenas_admin"}), "is an administrative group"},
		{base(map[string]interface{}{"add_groups": []interface{}{"root"}}), "has gid 0"},
		{base(map[string]interface{}{"add_groups": []interface{}{"builtin_users"}}), "is a built-in group"},
		{base(map[string]interface{}{"add_groups": []interface{}{"ops"}}), "grants sudo"},
		{base(map[string]interface{}{"add_groups": []interface{}{"wheel"}}), "not found as local groups: wheel"},
		{base(map[string]interface{}{"home": "/mnt/tank/home"}), "already exists; these tools only create a new home directory"},
		{base(map[string]interface{}{"home": "/mnt/tank/missing"}), "does not exist"},
		{base(map[string]interface{}{"home": "/mnt/tank/file"}), "not a directory"},
		{base(map[string]interface{}{"home": "/mnt/tank/home/../x"}), "home must be an absolute path"},
		{base(map[string]interface{}{"home": "/home"}), "home must be an absolute path under /mnt"},
		{base(map[string]interface{}{"smb": true}), "smb needs a password"},
		{base(map[string]interface{}{"uid": float64(nobodyID)}), "belongs to nobody"},
	} {
		assertRefused(t, r, fake, "create_user", userCreateMethod, tc.args, tc.want)
	}
}

func TestUpdateUserRefusals(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	for _, tc := range []struct {
		args map[string]interface{}
		want string
	}{
		{map[string]interface{}{"username": "root", "full_name": "R"}, "root account"},
		{map[string]interface{}{"username": "truenas_admin", "full_name": "A"}, "built-in administrator account"},
		{map[string]interface{}{"username": "admin", "full_name": "A"}, "built-in administrator account"},
		{map[string]interface{}{"username": "svc", "full_name": "S"}, "account this server signs in as"},
		{map[string]interface{}{"username": "daemon", "full_name": "D"}, "is a built-in account"},
		{map[string]interface{}{"username": "remote", "full_name": "R"}, "not found"},
		{map[string]interface{}{"username": "dave", "full_name": "D"}, "member of group 'ops', which grants sudo"},
		{map[string]interface{}{"username": "eve", "full_name": "E"}, "has sudo commands"},
		{map[string]interface{}{"username": "alice", "add_groups": []interface{}{"builtin_administrators"}}, "is an administrative group"},
		{map[string]interface{}{"username": "alice", "add_groups": []interface{}{"truenas_admin"}}, "is an administrative group"},
		{map[string]interface{}{"username": "alice", "add_groups": []interface{}{"root"}}, "has gid 0"},
		{map[string]interface{}{"username": "alice", "group": "builtin_users"}, "is a built-in group"},
		{map[string]interface{}{"username": "bob", "remove_groups": []interface{}{"ops"}}, "grants sudo"},
		{map[string]interface{}{"username": "bob", "home": "/mnt/tank/home"}, "already has home directory"},
		{map[string]interface{}{"username": "alice", "full_name": "Alice"}, "nothing to change"},
		{map[string]interface{}{"username": "alice", "smb": true}, "SMB user with password login disabled"},
		{map[string]interface{}{"username": "bob", "password_mode": "disabled"}, "SMB user with password login disabled"},
		{map[string]interface{}{"username": "alice", "add_groups": []interface{}{"staff"}, "remove_groups": []interface{}{"staff"}}, "in both add_groups and remove_groups"},
	} {
		assertRefused(t, r, fake, "update_user", userUpdateMethod, tc.args, tc.want)
	}
}

func TestCreateGroupRefusals(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	for _, tc := range []struct {
		args map[string]interface{}
		want string
	}{
		{map[string]interface{}{"name": "wheel"}, "administrative group name"},
		{map[string]interface{}{"name": "builtin_administrators"}, "administrative group name"},
		{map[string]interface{}{"name": "staff"}, "already exists"},
		{map[string]interface{}{"name": "media", "gid": 3100.0}, "gid 3100 is already used"},
		{map[string]interface{}{"name": "media", "members": []interface{}{"root"}}, "root account"},
		{map[string]interface{}{"name": "media", "members": []interface{}{"truenas_admin"}}, "built-in administrator account"},
		{map[string]interface{}{"name": "media", "members": []interface{}{"svc"}}, "account this server signs in as"},
		{map[string]interface{}{"name": "media", "members": []interface{}{"dave"}}, "grants sudo"},
		{map[string]interface{}{"name": "media", "members": []interface{}{"nobody2"}}, "not found as local users"},
	} {
		assertRefused(t, r, fake, "create_group", groupCreateMethod, tc.args, tc.want)
	}
}

func TestUpdateGroupRefusals(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	for _, tc := range []struct {
		args map[string]interface{}
		want string
	}{
		{map[string]interface{}{"name": "builtin_administrators", "smb": true}, "administrative group"},
		{map[string]interface{}{"name": "wheel", "smb": true}, "administrative group"},
		{map[string]interface{}{"name": "root", "smb": true}, "administrative group"},
		{map[string]interface{}{"name": "builtin_users", "add_members": []interface{}{"alice"}}, "is a built-in group"},
		{map[string]interface{}{"name": "ops", "add_members": []interface{}{"alice"}}, "grants sudo"},
		{map[string]interface{}{"name": "staff", "add_members": []interface{}{"root"}}, "root account"},
		{map[string]interface{}{"name": "staff", "add_members": []interface{}{"svc"}}, "account this server signs in as"},
		{map[string]interface{}{"name": "staff", "add_members": []interface{}{"daemon"}}, "is a built-in account"},
		{map[string]interface{}{"name": "staff", "add_members": []interface{}{"eve"}}, "has sudo commands"},
		{map[string]interface{}{"name": "staff", "add_members": []interface{}{"bob"}}, "nothing to change"},
	} {
		assertRefused(t, r, fake, "update_group", groupUpdateMethod, tc.args, tc.want)
	}
}

// ---- password modes ----

const userSuppliedPassword = "Correct-Horse-7-Battery"

func TestCreateUserPasswordDisabled(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	preview, out := confirmAndRun(t, r, fake, "create_user", userCreateMethod,
		map[string]interface{}{"username": "carol", "full_name": "Carol", "add_groups": []interface{}{"staff"}})
	if !strings.Contains(previewText(t, preview), "password login disabled") {
		t.Errorf("preview does not say password login is disabled:\n%s", previewText(t, preview))
	}
	payload := writePayload(t, fake, userCreateMethod, 0)
	if payload["password_disabled"] != true || payload["password"] != nil || payload["random_password"] != nil {
		t.Errorf("payload = %v", payload)
	}
	if payload["group_create"] != true || len(payload["sudo_commands"].([]interface{})) != 0 ||
		len(payload["sudo_commands_nopasswd"].([]interface{})) != 0 {
		t.Errorf("payload = %v", payload)
	}
	if groups, _ := payload["groups"].([]interface{}); len(groups) != 1 || groups[0] != 42.0 {
		t.Errorf("groups = %v", payload["groups"])
	}
	if _, ok := payload["home"]; ok {
		t.Errorf("home sent without home argument: %v", payload)
	}
	resp := decodeObj(t, out)
	if resp["created"] != true || resp["generated_password"] != nil {
		t.Errorf("result = %s", out)
	}
	if strings.Contains(out, `"password":`) {
		t.Errorf("result has a password field: %s", out)
	}
}

func TestCreateUserRandomPasswordShownOnce(t *testing.T) {
	r, fake, af := accountRegistry(t)
	args := map[string]interface{}{"username": "carol", "full_name": "Carol", "password_mode": "random", "smb": true, "home": "/mnt/tank/home"}
	dry, err := r.CallTool(t.Context(), "create_user", mergeArgs(args, map[string]interface{}{"dry_run": true}))
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.CallTool(t.Context(), "create_user", args)
	if err != nil {
		t.Fatal(err)
	}
	tok := replyToken(t, first)
	if tok == "" {
		t.Fatalf("no token:\n%s", first)
	}
	assertNoAccountWrites(t, fake)
	out, err := r.CallTool(t.Context(), "create_user", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeObj(t, out)
	pw, _ := resp["generated_password"].(string)
	if len(pw) < 20 {
		t.Fatalf("generated_password = %q in %s", pw, out)
	}
	if n := strings.Count(out, pw); n != 1 {
		t.Errorf("password appears %d times in the result", n)
	}
	for what, s := range map[string]string{"dry run": dry, "preview": first, "token": tok} {
		if strings.Contains(s, pw) {
			t.Errorf("%s contains the generated password", what)
		}
	}
	payload := writePayload(t, fake, userCreateMethod, 0)
	if payload["password"] != pw || payload["password_disabled"] != false || payload["home"] != "/mnt/tank/home" || payload["home_create"] != true {
		t.Errorf("payload = %v", payload)
	}
	if _, ok := payload["random_password"]; ok {
		t.Error("random_password sent: the password is generated here, not by the server")
	}
	// The server's own echo of the password is not passed through.
	if user, _ := resp["user"].(map[string]interface{}); user["password"] != nil {
		t.Errorf("user entry carries the password: %v", user)
	}
	if !r.TakeOneTimeSecret(pw) {
		t.Error("generated password not registered for one-time output")
	}
	if r.TakeOneTimeSecret(pw) {
		t.Error("generated password could be output twice")
	}

	// A second user gets a different password; the token is single use.
	if _, err := r.CallTool(t.Context(), "create_user", mergeArgs(args, map[string]interface{}{"confirmation_token": tok})); err == nil {
		t.Error("token redeemed twice")
	}
	af.mu.Lock()
	n := len(af.users)
	af.mu.Unlock()
	_, out2 := confirmAndRun(t, r, fake, "create_user", "none",
		map[string]interface{}{"username": "dan", "full_name": "Dan", "password_mode": "random"})
	if pw2, _ := decodeObj(t, out2)["generated_password"].(string); pw2 == "" || pw2 == pw {
		t.Errorf("second password %q", pw2)
	}
	af.mu.Lock()
	if len(af.users) != n+1 {
		t.Errorf("users = %d, want %d", len(af.users), n+1)
	}
	af.mu.Unlock()
}

func TestCreateUserSuppliedPasswordNeverEchoed(t *testing.T) {
	r, fake, af := accountRegistry(t)
	args := map[string]interface{}{"username": "carol", "full_name": "Carol", "password_mode": "set", "password": userSuppliedPassword}
	dry, err := r.CallTool(t.Context(), "create_user", mergeArgs(args, map[string]interface{}{"dry_run": true}))
	if err != nil {
		t.Fatal(err)
	}
	preview, out := confirmAndRun(t, r, fake, "create_user", userCreateMethod, args)
	for what, s := range map[string]string{"dry run": dry, "preview": previewText(t, preview), "result": out} {
		if strings.Contains(s, userSuppliedPassword) {
			t.Errorf("%s echoes the password: %s", what, s)
		}
	}
	if payload := writePayload(t, fake, userCreateMethod, 0); payload["password"] != userSuppliedPassword || payload["password_disabled"] != false {
		t.Errorf("payload = %v", payload)
	}
	if decodeObj(t, out)["generated_password"] != nil {
		t.Error("supplied password returned as generated_password")
	}

	// A middleware error that quotes the password does not leak it.
	af.fail[userCreateMethod] = map[string]interface{}{
		"code": -32001, "message": "[EINVAL] user_create.password: '" + userSuppliedPassword + "' is too weak",
		"data": map[string]interface{}{"extra": []interface{}{[]interface{}{"user_create.password", userSuppliedPassword}}},
	}
	args["username"] = "dan"
	tok := destroyPreviewToken(t, r, "create_user", args)
	_, err = r.CallTool(t.Context(), "create_user", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil {
		t.Fatal("failed create reported success")
	}
	if strings.Contains(err.Error(), userSuppliedPassword) {
		t.Errorf("error leaks the password: %v", err)
	}

	// Mode and password must agree, and the error never echoes the value.
	for _, bad := range []map[string]interface{}{
		{"username": "erin", "full_name": "E", "password_mode": "set"},
		{"username": "erin", "full_name": "E", "password": userSuppliedPassword},
		{"username": "erin", "full_name": "E", "password_mode": "random", "password": userSuppliedPassword},
		{"username": "erin", "full_name": "E", "password_mode": "set", "password": "short"},
	} {
		_, err := r.CallTool(t.Context(), "create_user", bad)
		if err == nil || strings.Contains(err.Error(), userSuppliedPassword) {
			t.Errorf("%v: err = %v", bad["password_mode"], err)
		}
	}
}

func TestUpdateUserPasswordModes(t *testing.T) {
	r, fake, _ := accountRegistry(t)

	// keep (the default) sends no password field.
	confirmAndRun(t, r, fake, "update_user", userUpdateMethod, map[string]interface{}{"username": "bob", "full_name": "Robert"})
	payload := writePayload(t, fake, userUpdateMethod, 1)
	if _, ok := payload["password"]; ok || len(payload) != 1 || payload["full_name"] != "Robert" {
		t.Errorf("keep: payload = %v", payload)
	}

	// random on a user with password login disabled enables it.
	r2, fake2, _ := accountRegistry(t)
	preview, out := confirmAndRun(t, r2, fake2, "update_user", userUpdateMethod, map[string]interface{}{"username": "alice", "password_mode": "random"})
	pw, _ := decodeObj(t, out)["generated_password"].(string)
	if pw == "" || strings.Count(out, pw) != 1 || strings.Contains(previewText(t, preview), pw) {
		t.Errorf("random: result %s", out)
	}
	payload = writePayload(t, fake2, userUpdateMethod, 1)
	if payload["password"] != pw || payload["password_disabled"] != false {
		t.Errorf("random: payload = %v", payload)
	}

	// set sends the caller's password, never shown.
	r3, fake3, _ := accountRegistry(t)
	preview, out = confirmAndRun(t, r3, fake3, "update_user", userUpdateMethod,
		map[string]interface{}{"username": "bob", "password_mode": "set", "password": userSuppliedPassword})
	if strings.Contains(previewText(t, preview)+out, userSuppliedPassword) {
		t.Error("set: password echoed")
	}
	if payload = writePayload(t, fake3, userUpdateMethod, 1); payload["password"] != userSuppliedPassword {
		t.Errorf("set: payload = %v", payload)
	}

	// disabled turns password login off.
	r4, fake4, af4 := accountRegistry(t)
	af4.set(true, "bob", "smb", false)
	confirmAndRun(t, r4, fake4, "update_user", userUpdateMethod, map[string]interface{}{"username": "bob", "password_mode": "disabled"})
	if payload = writePayload(t, fake4, userUpdateMethod, 1); payload["password_disabled"] != true || payload["password"] != nil {
		t.Errorf("disabled: payload = %v", payload)
	}
}

// ---- token binding ----

// assertExecuteRefused redeems tok after the state changed: the call must
// fail without sending a write.
func assertExecuteRefused(t *testing.T, r *Registry, fake *fakeMiddleware, tool string, args map[string]interface{}, tok string) {
	t.Helper()
	if _, err := r.CallTool(t.Context(), tool, mergeArgs(args, map[string]interface{}{"confirmation_token": tok})); err == nil {
		t.Errorf("%s ran after the state changed", tool)
	}
	assertNoAccountWrites(t, fake)
}

func TestUpdateUserTokenBoundToUser(t *testing.T) {
	for _, change := range []struct {
		field string
		value interface{}
	}{
		{"full_name", "Someone Else"},
		{"shell", "/usr/bin/zsh"},
		{"locked", true},
		{"email", "other@example.com"},
		{"last_password_change", map[string]interface{}{"$date": 1}},
	} {
		r, fake, af := accountRegistry(t)
		args := map[string]interface{}{"username": "alice", "full_name": "Alice B"}
		tok := destroyPreviewToken(t, r, "update_user", args)
		af.set(true, "alice", change.field, change.value)
		assertTokenRejected(t, r, fake, "update_user", userUpdateMethod, args, tok)
	}
	// A user that became privileged in between is refused outright.
	for _, change := range []struct {
		field string
		value interface{}
	}{
		{"groups", []interface{}{40}},
		{"roles", []interface{}{"FULL_ADMIN"}},
		{"sudo_commands", []interface{}{"ALL"}},
		{"builtin", true},
	} {
		r, fake, af := accountRegistry(t)
		args := map[string]interface{}{"username": "alice", "full_name": "Alice B"}
		tok := destroyPreviewToken(t, r, "update_user", args)
		af.set(true, "alice", change.field, change.value)
		assertExecuteRefused(t, r, fake, "update_user", args, tok)
	}
}

func TestUpdateUserTokenBoundToGroups(t *testing.T) {
	r, fake, af := accountRegistry(t)
	args := map[string]interface{}{"username": "alice", "add_groups": []interface{}{"staff"}}
	tok := destroyPreviewToken(t, r, "update_user", args)
	af.set(false, "staff", "smb", true)
	assertTokenRejected(t, r, fake, "update_user", userUpdateMethod, args, tok)

	r, fake, af = accountRegistry(t)
	tok = destroyPreviewToken(t, r, "update_user", args)
	af.set(false, "staff", "roles", []interface{}{"SHARING_ADMIN"})
	assertExecuteRefused(t, r, fake, "update_user", args, tok)
}

func TestCreateUserTokenBoundToState(t *testing.T) {
	r, fake, af := accountRegistry(t)
	args := map[string]interface{}{"username": "carol", "full_name": "Carol", "home": "/mnt/tank/home", "add_groups": []interface{}{"staff"}}
	tok := destroyPreviewToken(t, r, "create_user", args)
	af.set(false, "staff", "sudo_commands", []interface{}{"ALL"})
	assertExecuteRefused(t, r, fake, "create_user", args, tok)

	r, fake, af = accountRegistry(t)
	tok = destroyPreviewToken(t, r, "create_user", args)
	af.mu.Lock()
	af.paths["/mnt/tank/home/carol"] = "DIRECTORY"
	af.mu.Unlock()
	_, err := r.CallTool(t.Context(), "create_user", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("home created between preview and execute: err = %v", err)
	}
	assertNoAccountWrites(t, fake)
}

func TestUpdateGroupTokenBoundToGroup(t *testing.T) {
	r, fake, af := accountRegistry(t)
	args := map[string]interface{}{"name": "staff", "add_members": []interface{}{"alice"}}
	tok := destroyPreviewToken(t, r, "update_group", args)
	af.set(false, "staff", "users", []interface{}{11, 13})
	assertTokenRejected(t, r, fake, "update_group", groupUpdateMethod, args, tok)

	r, fake, af = accountRegistry(t)
	tok = destroyPreviewToken(t, r, "update_group", args)
	af.set(true, "alice", "groups", []interface{}{44})
	assertExecuteRefused(t, r, fake, "update_group", args, tok)
}

func TestCreateGroupTokenBoundToMembers(t *testing.T) {
	r, fake, af := accountRegistry(t)
	args := map[string]interface{}{"name": "media", "members": []interface{}{"alice"}}
	tok := destroyPreviewToken(t, r, "create_group", args)
	af.set(true, "alice", "uid", 3999)
	assertTokenRejected(t, r, fake, "create_group", groupCreateMethod, args, tok)
}

// ---- successful changes ----

func TestUpdateUserChanges(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	args := map[string]interface{}{
		"username": "alice", "full_name": "Alice B", "email": "alice@example.com", "shell": "/usr/bin/zsh",
		"add_groups": []interface{}{"staff"}, "home": "/mnt/tank/home",
	}
	preview, out := confirmAndRun(t, r, fake, "update_user", userUpdateMethod, args)
	text := previewText(t, preview)
	for _, want := range []string{"Alice B", "alice@example.com", "/usr/bin/zsh", "add to group: staff", "/mnt/tank/home/alice"} {
		if !strings.Contains(text, want) {
			t.Errorf("preview lacks %q:\n%s", want, text)
		}
	}
	payload := writePayload(t, fake, userUpdateMethod, 1)
	for k, v := range map[string]interface{}{"full_name": "Alice B", "email": "alice@example.com", "shell": "/usr/bin/zsh", "home": "/mnt/tank/home", "home_create": true} {
		if payload[k] != v {
			t.Errorf("payload[%s] = %v, want %v", k, payload[k], v)
		}
	}
	if groups, _ := payload["groups"].([]interface{}); len(groups) != 1 || groups[0] != 42.0 {
		t.Errorf("groups = %v", payload["groups"])
	}
	for _, k := range []string{"uid", "username", "sudo_commands", "sudo_commands_nopasswd", "roles", "password"} {
		if _, ok := payload[k]; ok {
			t.Errorf("payload sends %s: %v", k, payload)
		}
	}
	var p []interface{}
	_ = json.Unmarshal(fake.called(userUpdateMethod)[0].Params, &p)
	if p[0] != 10.0 {
		t.Errorf("user.update id = %v", p[0])
	}
	if resp := decodeObj(t, out); resp["updated"] != true {
		t.Errorf("result = %s", out)
	}
}

func TestCreateAndUpdateGroup(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	_, out := confirmAndRun(t, r, fake, "create_group", groupCreateMethod,
		map[string]interface{}{"name": "media", "members": []interface{}{"alice", "bob"}})
	payload := writePayload(t, fake, groupCreateMethod, 0)
	if users, _ := payload["users"].([]interface{}); len(users) != 2 || payload["name"] != "media" ||
		len(payload["sudo_commands"].([]interface{})) != 0 {
		t.Errorf("payload = %v", payload)
	}
	if decodeObj(t, out)["created"] != true || !strings.Contains(out, "alice") {
		t.Errorf("result = %s", out)
	}

	_, out = confirmAndRun(t, r, fake, "update_group", groupUpdateMethod,
		map[string]interface{}{"name": "staff", "add_members": []interface{}{"alice"}, "remove_members": []interface{}{"bob"}, "smb": true})
	payload = writePayload(t, fake, groupUpdateMethod, 1)
	if users, _ := payload["users"].([]interface{}); len(users) != 1 || users[0] != 10.0 || payload["smb"] != true {
		t.Errorf("payload = %v", payload)
	}
	for _, k := range []string{"gid", "name", "sudo_commands", "roles"} {
		if _, ok := payload[k]; ok {
			t.Errorf("payload sends %s", k)
		}
	}
	if !strings.Contains(out, "add member: alice") || !strings.Contains(out, "remove member: bob") {
		t.Errorf("result = %s", out)
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pw, err := generatePassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != generatedPasswordLen || seen[pw] {
			t.Fatalf("password %d: len %d, repeated %v", i, len(pw), seen[pw])
		}
		seen[pw] = true
		for _, class := range passwordClasses {
			if !strings.ContainsAny(pw, class) {
				t.Errorf("password lacks a character from %q", class)
			}
		}
	}
}

func TestOneTimeSecretsExpireAndAreConsumed(t *testing.T) {
	s := newOneTimeSecrets()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	s.add("a")
	s.add("b")
	if s.take("") || s.take("c") {
		t.Error("unknown value accepted")
	}
	if !s.take("a") || s.take("a") {
		t.Error("value not accepted exactly once")
	}
	now = now.Add(oneTimeSecretTTL)
	if s.take("b") {
		t.Error("expired value accepted")
	}
	for k := range s.entries {
		t.Errorf("entry %x kept", k)
	}
}

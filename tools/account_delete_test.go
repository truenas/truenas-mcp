package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// setGuest sets the SMB guest account smb.config reports.
func (f *accountWriteFake) setGuest(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.smbGuest = name
}

// addPath makes filesystem.stat report p with type typ.
func (f *accountWriteFake) addPath(p, typ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths[p] = typ
}

// failMethod makes method fail with e.
func (f *accountWriteFake) failMethod(method string, e map[string]interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[method] = e
}

// deleteParams returns the (id, options) params of the only call to method.
func deleteParams(t *testing.T, fake *fakeMiddleware, method string) (float64, map[string]interface{}) {
	t.Helper()
	calls := fake.called(method)
	if len(calls) != 1 {
		t.Fatalf("%s sent %d time(s), want 1", method, len(calls))
	}
	var p []interface{}
	if err := json.Unmarshal(calls[0].Params, &p); err != nil || len(p) != 2 {
		t.Fatalf("%s params = %s, want [id, options]", method, calls[0].Params)
	}
	id, _ := p[0].(float64)
	opts, _ := p[1].(map[string]interface{})
	return id, opts
}

func TestAccountDeleteToolsClassifiedAndReadOnly(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	for _, name := range []string{"delete_user", "delete_group"} {
		if safetyOf(name).Access != Destructive {
			t.Errorf("%s is not destructive", name)
		}
		if !hasParam(r.tools[name], "confirmation_token") {
			t.Errorf("%s has no confirmation_token", name)
		}
	}
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	for name, args := range map[string]map[string]interface{}{
		"delete_user":  {"username": "alice"},
		"delete_group": {"name": "staff"},
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
	for _, m := range []string{"user.query", "group.query", "smb.config", "filesystem.stat"} {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("read-only mode sent %s %d time(s)", m, n)
		}
	}
	assertNoAccountWrites(t, fake)
}

func TestAccountDeleteToolsNeedConfirmation(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	for name, args := range map[string]map[string]interface{}{
		"delete_user":  {"username": "alice"},
		"delete_group": {"name": "staff"},
	} {
		out, err := r.CallTool(t.Context(), name, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if replyToken(t, out) == "" {
			t.Errorf("%s ran without confirmation:\n%s", name, out)
		}
		if _, err := r.CallTool(t.Context(), name, mergeArgs(args, map[string]interface{}{"confirmation_token": "forged"})); err == nil {
			t.Errorf("%s accepted a forged token", name)
		}
	}
	assertNoAccountWrites(t, fake)
}

// ---- delete_user ----

func TestDeleteUserKeepsPrimaryGroup(t *testing.T) {
	r, fake, af := accountRegistry(t)
	preview, out := confirmAndRun(t, r, fake, "delete_user", userDeleteMethod, map[string]interface{}{"username": "alice"})
	id, opts := deleteParams(t, fake, userDeleteMethod)
	if id != 10 || len(opts) != 1 || opts["delete_group"] != false {
		t.Errorf("user.delete params = %v %v, want 10 {delete_group: false}", id, opts)
	}
	body := previewBody(t, preview)
	assertContainsAll(t, "preview", body,
		`"delete_group":false`, `"home_deleted":false`, "primary group 'alice' (delete_group is false)",
		"primary group (kept): alice", "home: none", "keep uid 3000")
	resp := decodeObj(t, out)
	if resp["deleted"] != true || resp["username"] != "alice" || resp["primary_group_kept"] != "alice" || resp["home_deleted"] != false || resp["note"] != nil {
		t.Errorf("response = %v", resp)
	}
	for _, g := range af.groups {
		if g["name"] == "alice" {
			return
		}
	}
	t.Error("the primary group was removed from the fixture")
}

func TestDeleteUserKeepsHomeDirectory(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	preview, out := confirmAndRun(t, r, fake, "delete_user", userDeleteMethod, map[string]interface{}{"username": "bob"})
	if _, opts := deleteParams(t, fake, userDeleteMethod); opts["delete_group"] != false {
		t.Errorf("options = %v", opts)
	}
	assertContainsAll(t, "preview", previewBody(t, preview),
		"home directory /mnt/tank/home/bob and its files", "/mnt/tank/home/bob/.ssh",
		"membership of staff", "SMB password entry", "SMB share ACL entries", "home: /mnt/tank/home/bob (kept)")
	var checked bool
	for _, c := range fake.called("filesystem.stat") {
		checked = checked || strings.Contains(string(c.Params), "/mnt/tank/home/bob/.ssh")
	}
	if !checked {
		t.Error("the .ssh directory was not checked")
	}
	if resp := decodeObj(t, out); resp["home_kept"] != "/mnt/tank/home/bob" || resp["home_deleted"] != false {
		t.Errorf("response = %v", resp)
	}
}

func TestDeleteUserRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		user  string
		setup func(af *accountWriteFake)
		want  string
	}{
		{"root", "root", nil, "root account"},
		{"admin", "admin", nil, "built-in administrator account"},
		{"truenas_admin", "truenas_admin", nil, "built-in administrator account"},
		{"api user", "svc", nil, "account this server signs in as"},
		{"directory service", "remote", nil, "directory service"},
		{"built-in", "daemon", nil, "built-in account"},
		{"sudo", "eve", nil, "sudo commands"},
		{"privileged group member", "dave", nil, "member of group 'ops', which grants sudo"},
		{"uid 0", "alice", func(af *accountWriteFake) { af.set(true, "alice", "uid", 0) }, "uid 0"},
		{"admin uid", "alice", func(af *accountWriteFake) { af.set(true, "alice", "uid", adminUID) }, "built-in administrator uid"},
		{"roles", "alice", func(af *accountWriteFake) { af.set(true, "alice", "roles", []interface{}{"READONLY_ADMIN"}) }, "TrueNAS roles"},
		{"immutable", "alice", func(af *accountWriteFake) { af.set(true, "alice", "immutable", true) }, "built-in account"},
		{"unreadable group", "alice", func(af *accountWriteFake) { af.set(true, "alice", "groups", []interface{}{99}) }, "could not read group id 99"},
		{"missing", "nobody2", nil, "local user 'nobody2' not found"},
		{"smb guest", "alice", func(af *accountWriteFake) { af.setGuest("alice") }, "SMB guest account"},
		{"ssh directory", "bob", func(af *accountWriteFake) { af.addPath("/mnt/tank/home/bob/.ssh", "DIRECTORY") }, "always removes the .ssh directory"},
		{"smb.config fails", "alice", func(af *accountWriteFake) { af.failMethod("smb.config", rpcErr("boom")) }, "could not read the SMB guest account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fake, af := accountRegistry(t)
			if tc.setup != nil {
				tc.setup(af)
			}
			assertRefused(t, r, fake, "delete_user", userDeleteMethod, map[string]interface{}{"username": tc.user}, tc.want)
		})
	}
}

func TestDeleteUserRefusedArguments(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	for key, want := range map[string]string{
		"delete_group": "primary group is always kept",
		"delete_home":  "home directory is never deleted",
		"uid":          "by username",
	} {
		_, err := r.CallTool(t.Context(), "delete_user", map[string]interface{}{"username": "alice", key: true})
		if err == nil || !strings.Contains(err.Error(), key+" is refused: ") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", key, err)
		}
	}
	assertNoAccountWrites(t, fake)
}

func TestDeleteUserTokenBoundToUser(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(af *accountWriteFake)
	}{
		{"full name", func(af *accountWriteFake) { af.set(true, "alice", "full_name", "Someone Else") }},
		{"home", func(af *accountWriteFake) { af.set(true, "alice", "home", "/mnt/tank/home/alice") }},
		{"smb", func(af *accountWriteFake) { af.set(true, "alice", "smb", true) }},
		{"groups", func(af *accountWriteFake) { af.set(true, "alice", "groups", []interface{}{42}) }},
		{"primary group renamed", func(af *accountWriteFake) { af.set(false, "alice", "name", "alice2") }},
		{"smb guest", func(af *accountWriteFake) { af.setGuest("guestuser") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			r, fake, af := accountRegistry(t)
			args := map[string]interface{}{"username": "alice"}
			tok := destroyPreviewToken(t, r, "delete_user", args)
			change.mutate(af)
			assertTokenRejected(t, r, fake, "delete_user", userDeleteMethod, args, tok)
		})
	}
	// A state that is now refused is refused on confirm too.
	for _, change := range []struct {
		name   string
		mutate func(af *accountWriteFake)
	}{
		{"sudo granted", func(af *accountWriteFake) { af.set(true, "bob", "sudo_commands", []interface{}{"ALL"}) }},
		{"group gains sudo", func(af *accountWriteFake) { af.set(false, "staff", "sudo_commands", []interface{}{"ALL"}) }},
		{"ssh directory created", func(af *accountWriteFake) { af.addPath("/mnt/tank/home/bob/.ssh", "DIRECTORY") }},
		{"became smb guest", func(af *accountWriteFake) { af.setGuest("bob") }},
		{"deleted meanwhile", func(af *accountWriteFake) { af.set(true, "bob", "username", "bob-gone") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			r, fake, af := accountRegistry(t)
			args := map[string]interface{}{"username": "bob"}
			tok := destroyPreviewToken(t, r, "delete_user", args)
			change.mutate(af)
			assertExecuteRefused(t, r, fake, "delete_user", args, tok)
		})
	}
}

func TestDeleteUserMiddlewareErrors(t *testing.T) {
	r, fake, af := accountRegistry(t)
	args := map[string]interface{}{"username": "alice"}
	tok := destroyPreviewToken(t, r, "delete_user", args)
	af.failMethod(userDeleteMethod, rpcErr("[EINVAL] user_delete: boom"))
	_, err := r.CallTool(t.Context(), "delete_user", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "failed to delete user alice") || !strings.Contains(err.Error(), "query_users") {
		t.Errorf("err = %v", err)
	}
	if n := len(fake.called(userDeleteMethod)); n != 1 {
		t.Errorf("user.delete sent %d time(s)", n)
	}
}

// ---- delete_group ----

func TestDeleteGroupKeepsUsers(t *testing.T) {
	r, fake, af := accountRegistry(t)
	preview, out := confirmAndRun(t, r, fake, "delete_group", groupDeleteMethod, map[string]interface{}{"name": "staff"})
	id, opts := deleteParams(t, fake, groupDeleteMethod)
	if id != 42 || len(opts) != 1 || opts["delete_users"] != false {
		t.Errorf("group.delete params = %v %v, want 42 {delete_users: false}", id, opts)
	}
	assertContainsAll(t, "preview", previewBody(t, preview),
		`"delete_users":false`, "members losing the group: bob", "keep gid 3100", "no user is deleted")
	resp := decodeObj(t, out)
	if resp["deleted"] != true || resp["name"] != "staff" || resp["users_deleted"] != false || resp["note"] != nil {
		t.Errorf("response = %v", resp)
	}
	if len(af.users) != len(accountUserRows()) {
		t.Error("a user was removed from the fixture")
	}
}

func TestDeleteGroupRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		group string
		setup func(af *accountWriteFake)
		want  string
	}{
		{"root", "root", nil, "administrative group"},
		{"truenas_admin", "truenas_admin", nil, "administrative group"},
		{"builtin_administrators", "builtin_administrators", nil, "administrative group"},
		{"wheel by name", "wheel", nil, "administrative group"},
		{"built-in", "builtin_users", nil, "built-in group"},
		{"sudo", "ops", nil, "grants sudo"},
		{"primary group", "alice", nil, "primary group of user(s) alice"},
		{"gid 0", "staff", func(af *accountWriteFake) { af.set(false, "staff", "gid", 0) }, "has gid 0"},
		{"admin gid", "staff", func(af *accountWriteFake) { af.set(false, "staff", "gid", adminUID) }, "built-in administrator gid"},
		{"roles", "staff", func(af *accountWriteFake) { af.set(false, "staff", "roles", []interface{}{"SHARING_ADMIN"}) }, "grants TrueNAS roles"},
		{"immutable", "staff", func(af *accountWriteFake) { af.set(false, "staff", "immutable", true) }, "built-in group"},
		{"directory service", "staff", func(af *accountWriteFake) { af.set(false, "staff", "local", false) }, "directory service"},
		{"api user member", "staff", func(af *accountWriteFake) { af.set(false, "staff", "users", []interface{}{3, 11}) }, "account this server signs in as ('svc')"},
		{"missing", "nogroup2", nil, "local group 'nogroup2' not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fake, af := accountRegistry(t)
			if tc.setup != nil {
				tc.setup(af)
			}
			assertRefused(t, r, fake, "delete_group", groupDeleteMethod, map[string]interface{}{"name": tc.group}, tc.want)
		})
	}
}

func TestDeleteGroupRefusedArguments(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	for key, want := range map[string]string{"delete_users": "never deleted", "gid": "by name"} {
		_, err := r.CallTool(t.Context(), "delete_group", map[string]interface{}{"name": "staff", key: true})
		if err == nil || !strings.Contains(err.Error(), key+" is refused: ") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", key, err)
		}
	}
	assertNoAccountWrites(t, fake)
}

func TestDeleteGroupTokenBoundToGroup(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(af *accountWriteFake)
	}{
		{"member added", func(af *accountWriteFake) { af.set(false, "staff", "users", []interface{}{10, 11}) }},
		{"smb", func(af *accountWriteFake) { af.set(false, "staff", "smb", true) }},
		{"member renamed", func(af *accountWriteFake) { af.set(true, "bob", "username", "robert") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			r, fake, af := accountRegistry(t)
			args := map[string]interface{}{"name": "staff"}
			tok := destroyPreviewToken(t, r, "delete_group", args)
			change.mutate(af)
			assertTokenRejected(t, r, fake, "delete_group", groupDeleteMethod, args, tok)
		})
	}
	for _, change := range []struct {
		name   string
		mutate func(af *accountWriteFake)
	}{
		{"became a primary group", func(af *accountWriteFake) {
			af.set(true, "alice", "group", map[string]interface{}{"id": 42, "bsdgrp_group": "staff"})
		}},
		{"gained sudo", func(af *accountWriteFake) { af.set(false, "staff", "sudo_commands", []interface{}{"ALL"}) }},
		{"api user joined", func(af *accountWriteFake) { af.set(false, "staff", "users", []interface{}{3}) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			r, fake, af := accountRegistry(t)
			args := map[string]interface{}{"name": "staff"}
			tok := destroyPreviewToken(t, r, "delete_group", args)
			change.mutate(af)
			assertExecuteRefused(t, r, fake, "delete_group", args, tok)
		})
	}
}

func TestDeleteGroupMiddlewareError(t *testing.T) {
	r, _, af := accountRegistry(t)
	args := map[string]interface{}{"name": "staff"}
	tok := destroyPreviewToken(t, r, "delete_group", args)
	af.failMethod(groupDeleteMethod, rpcErr("[EINVAL] group_delete: boom"))
	_, err := r.CallTool(t.Context(), "delete_group", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "failed to delete group staff") || !strings.Contains(err.Error(), "query_groups") {
		t.Errorf("err = %v", err)
	}
}

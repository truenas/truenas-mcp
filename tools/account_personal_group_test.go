package tools

import (
	"strings"
	"testing"
)

// addCustomAdmin adds carl, a custom administrator whose own primary group
// grants nothing by itself.
func addCustomAdmin(af *accountWriteFake) {
	af.mu.Lock()
	defer af.mu.Unlock()
	carl := accountUser(17, 3006, "carl", 28, []interface{}{})
	carl["roles"] = []interface{}{"FULL_ADMIN"}
	af.users = append(af.users, carl)
	af.groups = append(af.groups, accountGroup(28, 3006, "carl", []interface{}{}))
}

func TestPersonalGroupMembershipRefused(t *testing.T) {
	r, fake, af := accountRegistry(t)
	addCustomAdmin(af)
	for _, tc := range []struct {
		tool, method string
		args         map[string]interface{}
		want         string
	}{
		{"create_user", userCreateMethod, map[string]interface{}{"username": "carol", "full_name": "Carol", "add_groups": []interface{}{"alice"}}, "group 'alice' is the primary group of user 'alice'"},
		{"create_user", userCreateMethod, map[string]interface{}{"username": "carol", "full_name": "Carol", "group": "alice"}, "group 'alice' is the primary group of user 'alice'"},
		{"create_user", userCreateMethod, map[string]interface{}{"username": "carol", "full_name": "Carol", "add_groups": []interface{}{"carl"}}, "group 'carl' is the primary group of user 'carl'"},
		{"update_user", userUpdateMethod, map[string]interface{}{"username": "bob", "add_groups": []interface{}{"alice"}}, "group 'alice' is the primary group of user 'alice'"},
		{"update_user", userUpdateMethod, map[string]interface{}{"username": "bob", "group": "alice"}, "group 'alice' is the primary group of user 'alice'"},
		{"update_user", userUpdateMethod, map[string]interface{}{"username": "alice", "add_groups": []interface{}{"carl"}}, "group 'carl' is the primary group of user 'carl'"},
		{"update_group", groupUpdateMethod, map[string]interface{}{"name": "alice", "add_members": []interface{}{"bob"}}, "group 'alice' is the primary group of user 'alice'"},
		{"update_group", groupUpdateMethod, map[string]interface{}{"name": "carl", "add_members": []interface{}{"alice"}}, "group 'carl' is the primary group of user 'carl'"},
	} {
		assertRefused(t, r, fake, tc.tool, tc.method, tc.args, tc.want)
	}
	assertNoAccountWrites(t, fake)
}

// A user's own primary group, a group nobody has as primary group, and
// leaving another user's personal group are all allowed.
func TestPersonalGroupRuleAllows(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	confirmAndRun(t, r, fake, "update_group", groupUpdateMethod,
		map[string]interface{}{"name": "alice", "add_members": []interface{}{"alice"}})

	r, fake, _ = accountRegistry(t)
	confirmAndRun(t, r, fake, "create_user", userCreateMethod,
		map[string]interface{}{"username": "carol", "full_name": "Carol", "add_groups": []interface{}{"staff"}})
	if payload := writePayload(t, fake, userCreateMethod, 0); payload["group_create"] != true {
		t.Errorf("payload = %v", payload)
	}

	r, fake, af := accountRegistry(t)
	af.set(true, "bob", "groups", []interface{}{20, 42})
	confirmAndRun(t, r, fake, "update_user", userUpdateMethod,
		map[string]interface{}{"username": "bob", "remove_groups": []interface{}{"alice"}})
	if groups, _ := writePayload(t, fake, userUpdateMethod, 1)["groups"].([]interface{}); len(groups) != 1 || groups[0] != 42.0 {
		t.Errorf("groups = %v", groups)
	}

	r, fake, af = accountRegistry(t)
	af.set(false, "alice", "users", []interface{}{11})
	confirmAndRun(t, r, fake, "update_group", groupUpdateMethod,
		map[string]interface{}{"name": "alice", "remove_members": []interface{}{"bob"}})
}

// A group that becomes someone's primary group between the preview and the
// confirmation is refused when the confirmation re-plans.
func TestPersonalGroupRefusedAtConfirm(t *testing.T) {
	staffPrimary := map[string]interface{}{"id": 42, "bsdgrp_group": "staff"}
	for _, tc := range []struct {
		tool string
		args map[string]interface{}
	}{
		{"create_user", map[string]interface{}{"username": "carol", "full_name": "Carol", "add_groups": []interface{}{"staff"}}},
		{"update_user", map[string]interface{}{"username": "alice", "add_groups": []interface{}{"staff"}}},
		{"update_user", map[string]interface{}{"username": "alice", "group": "staff"}},
		{"update_group", map[string]interface{}{"name": "staff", "add_members": []interface{}{"alice"}}},
	} {
		r, fake, af := accountRegistry(t)
		tok := destroyPreviewToken(t, r, tc.tool, tc.args)
		af.set(true, "dave", "group", staffPrimary)
		_, err := r.CallTool(t.Context(), tc.tool, mergeArgs(tc.args, map[string]interface{}{"confirmation_token": tok}))
		if err == nil || !strings.Contains(err.Error(), "group 'staff' is the primary group of user 'dave'") {
			t.Errorf("%s %v: err = %v", tc.tool, tc.args, err)
		}
		assertNoAccountWrites(t, fake)
	}
}

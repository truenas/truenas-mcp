package tools

import (
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/redact"
)

// addRoleGroup adds a custom local group that grants a TrueNAS role. It is
// neither built-in nor named like an administrative group, so only its
// roles make it privileged. frank belongs to it as a secondary group.
func addRoleGroup(af *accountWriteFake) {
	af.mu.Lock()
	defer af.mu.Unlock()
	auditors := accountGroup(45, 3400, "auditors", []interface{}{16})
	auditors["roles"] = []interface{}{"READONLY_ADMIN"}
	af.groups = append(af.groups, auditors, accountGroup(27, 3005, "frank", []interface{}{}))
	af.users = append(af.users, accountUser(16, 3005, "frank", 27, []interface{}{45}))
}

func TestCustomRoleGroupIsPrivileged(t *testing.T) {
	r, fake, af := accountRegistry(t)
	addRoleGroup(af)
	const grants = "grants TrueNAS roles (READONLY_ADMIN)"
	for _, tc := range []struct {
		tool, method string
		args         map[string]interface{}
		want         string
	}{
		// Membership of the group is refused.
		{"create_user", userCreateMethod, map[string]interface{}{"username": "carol", "full_name": "Carol", "add_groups": []interface{}{"auditors"}}, grants},
		{"create_user", userCreateMethod, map[string]interface{}{"username": "carol", "full_name": "Carol", "group": "auditors"}, grants},
		{"update_user", userUpdateMethod, map[string]interface{}{"username": "alice", "add_groups": []interface{}{"auditors"}}, grants},
		{"update_user", userUpdateMethod, map[string]interface{}{"username": "alice", "group": "auditors"}, grants},
		{"update_group", groupUpdateMethod, map[string]interface{}{"name": "auditors", "add_members": []interface{}{"alice"}}, grants},
		{"update_group", groupUpdateMethod, map[string]interface{}{"name": "auditors", "smb": true}, grants},
		// Its members are privileged users.
		{"update_user", userUpdateMethod, map[string]interface{}{"username": "frank", "full_name": "Frank B"}, "member of group 'auditors', which " + grants},
		{"create_group", groupCreateMethod, map[string]interface{}{"name": "media", "members": []interface{}{"frank"}}, "member of group 'auditors', which " + grants},
		{"update_group", groupUpdateMethod, map[string]interface{}{"name": "staff", "add_members": []interface{}{"frank"}}, "member of group 'auditors', which " + grants},
	} {
		assertRefused(t, r, fake, tc.tool, tc.method, tc.args, tc.want)
	}
	assertNoAccountWrites(t, fake)
}

func TestUpdateUserPrivilegedThroughPrimaryGroupOnly(t *testing.T) {
	for name, change := range map[string]struct {
		field string
		value interface{}
		want  string
	}{
		"roles": {"roles", []interface{}{"SHARING_ADMIN"}, "grants TrueNAS roles (SHARING_ADMIN)"},
		"sudo":  {"sudo_commands", []interface{}{"ALL"}, "grants sudo"},
	} {
		t.Run(name, func(t *testing.T) {
			r, fake, af := accountRegistry(t)
			// alice has no secondary groups; only her primary group changes.
			af.set(false, "alice", change.field, change.value)
			assertRefused(t, r, fake, "update_user", userUpdateMethod,
				map[string]interface{}{"username": "alice", "full_name": "Alice B"}, "member of group 'alice', which "+change.want)
			assertRefused(t, r, fake, "update_group", groupUpdateMethod,
				map[string]interface{}{"name": "staff", "add_members": []interface{}{"alice"}}, "member of group 'alice', which "+change.want)
			assertNoAccountWrites(t, fake)
		})
	}
}

func TestUpdateGroupRemovingPrivilegedUserRefused(t *testing.T) {
	r, fake, af := accountRegistry(t)
	// eve (sudo) and dave (member of ops, which grants sudo) are in staff.
	af.set(false, "staff", "users", []interface{}{11, 12, 13})
	for _, tc := range []struct {
		member, want string
	}{
		{"eve", "has sudo commands"},
		{"dave", "member of group 'ops', which grants sudo"},
	} {
		assertRefused(t, r, fake, "update_group", groupUpdateMethod,
			map[string]interface{}{"name": "staff", "remove_members": []interface{}{tc.member}}, tc.want)
	}
	assertNoAccountWrites(t, fake)
}

func TestHomeInManagedDatasetRefused(t *testing.T) {
	r, fake, af := accountRegistry(t)
	for _, tc := range []struct {
		home, want string
	}{
		{"/mnt/boot-pool/home", "inside 'boot-pool'"},
		{"/mnt/freenas-boot", "inside 'freenas-boot'"},
		{"/mnt/tank/ix-apps/home", "inside 'ix-apps'"},
		{"/mnt/tank/ix-applications", "inside 'ix-applications'"},
		// Matched case-insensitively, as share paths are.
		{"/mnt/tank/IX-Apps/home", "inside 'ix-apps'"},
		{"/mnt/tank/IX-APPLICATIONS", "inside 'ix-applications'"},
		{"/mnt/Boot-Pool/home", "inside 'boot-pool'"},
		{"/mnt/tank/.system/home", "inside '.system'"},
		{"/mnt/tank/.ix-virt", "inside '.ix-virt'"},
		{"/mnt/tank/data/.hidden/home", "inside '.hidden'"},
		{"/mnt/.home", "under a pool's /mnt/<pool> path"},
		{"/mnt/", "home must be"},
	} {
		// The parent exists, so only the managed-dataset rule refuses it.
		af.mu.Lock()
		af.paths[tc.home] = "DIRECTORY"
		af.mu.Unlock()
		assertRefused(t, r, fake, "create_user", userCreateMethod,
			map[string]interface{}{"username": "carol", "full_name": "Carol", "home": tc.home}, tc.want)
		assertRefused(t, r, fake, "update_user", userUpdateMethod,
			map[string]interface{}{"username": "alice", "home": tc.home}, tc.want)
	}
	assertNoAccountWrites(t, fake)
}

func TestUpdateUserHomeChecks(t *testing.T) {
	r, fake, af := accountRegistry(t)
	af.mu.Lock()
	af.paths["/mnt/tank/home/alice"] = "DIRECTORY"
	af.mu.Unlock()
	for _, tc := range []struct {
		home, want string
	}{
		{"/mnt/tank/home", "'/mnt/tank/home/alice' already exists"},
		{"/mnt/tank/missing", "does not exist"},
		{"/mnt/tank/file", "not a directory"},
	} {
		assertRefused(t, r, fake, "update_user", userUpdateMethod,
			map[string]interface{}{"username": "alice", "home": tc.home}, tc.want)
	}

	// A user whose home is empty (rather than /var/empty) has no home
	// directory either, so a new one may be set.
	r, fake, af = accountRegistry(t)
	af.set(true, "alice", "home", "")
	confirmAndRun(t, r, fake, "update_user", userUpdateMethod,
		map[string]interface{}{"username": "alice", "home": "/mnt/tank/home"})
	payload := writePayload(t, fake, userUpdateMethod, 1)
	if payload["home"] != "/mnt/tank/home" || payload["home_create"] != true {
		t.Errorf("payload = %v", payload)
	}
}

func TestScrubSecretTextIgnoresCaseAndEscaping(t *testing.T) {
	for _, tc := range []struct {
		msg, secret string
	}{
		{"[EINVAL] password '" + strings.ToUpper(userSuppliedPassword) + "' is too weak", userSuppliedPassword},
		{"[EINVAL] password '" + strings.ToLower(userSuppliedPassword) + "' is too weak", userSuppliedPassword},
		{`{"password": "Quote\"And\\Slash-42"}`, `Quote"And\Slash-42`},
	} {
		got := scrubSecretText(tc.msg, tc.secret)
		if strings.Contains(strings.ToLower(got), "horse") || strings.Contains(got, "Slash-42") || !strings.Contains(got, redact.Marker) {
			t.Errorf("scrubSecretText(%q) = %q", tc.msg, got)
		}
	}
	if got := scrubSecretText("unchanged", ""); got != "unchanged" {
		t.Errorf("empty secret changed the text: %q", got)
	}
}

func TestCreateUserErrorScrubsPasswordInAnyCase(t *testing.T) {
	r, fake, af := accountRegistry(t)
	args := map[string]interface{}{"username": "carol", "full_name": "Carol", "password_mode": "set", "password": userSuppliedPassword}
	af.fail[userCreateMethod] = map[string]interface{}{
		"code": -32001, "message": "[EINVAL] user_create.password: '" + strings.ToUpper(userSuppliedPassword) + "' is too weak",
	}
	tok := destroyPreviewToken(t, r, "create_user", args)
	_, err := r.CallTool(t.Context(), "create_user", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil {
		t.Fatal("failed create reported success")
	}
	if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(userSuppliedPassword)) {
		t.Errorf("error leaks the password in another case: %v", err)
	}
	if n := len(fake.called(userCreateMethod)); n != 1 {
		t.Errorf("user.create sent %d time(s), want 1", n)
	}
}

func TestSuppliedPasswordWhitespaceRefused(t *testing.T) {
	r, fake, _ := accountRegistry(t)
	for _, pw := range []string{" " + userSuppliedPassword, userSuppliedPassword + " ", "\t" + userSuppliedPassword, userSuppliedPassword + "\n"} {
		for tool, args := range map[string]map[string]interface{}{
			"create_user": {"username": "carol", "full_name": "Carol", "password_mode": "set", "password": pw},
			"update_user": {"username": "bob", "password_mode": "set", "password": pw},
		} {
			_, err := r.CallTool(t.Context(), tool, args)
			if err == nil || !strings.Contains(err.Error(), "must not start or end with whitespace") || strings.Contains(err.Error(), userSuppliedPassword) {
				t.Errorf("%s %q: err = %v", tool, pw, err)
			}
		}
	}
	assertNoAccountWrites(t, fake)
}

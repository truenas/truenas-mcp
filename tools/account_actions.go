package tools

// Local account write tools (create_user, update_user, create_group,
// update_group). They never grant sudo, TrueNAS roles or membership of
// built-in or administrative groups, never change root, the built-in
// administrator accounts, built-in accounts or the account this server
// signs in as, and never change a uid or gid. A random password is
// generated only when the call executes and is returned once, in its
// result (tools/onetime_secret.go); no password is ever part of a preview,
// a plan, the confirmation token or an error.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	userCreateMethod  = "user.create"
	userUpdateMethod  = "user.update"
	groupCreateMethod = "group.create"
	groupUpdateMethod = "group.update"
)

const (
	// adminUID is ADMIN_UID in the middleware: the truenas_admin account.
	adminUID = 950
	// defaultHomePath is DEFAULT_HOME_PATH in the middleware: no home
	// directory.
	defaultHomePath = "/var/empty"
	// minAccountID and maxAccountID bound an explicit uid or gid. Lower
	// ids belong to the system; higher ones to container id maps.
	minAccountID = 1000
	maxAccountID = 2147000000
	// nobodyID is the uid of nobody and the gid of nogroup.
	nobodyID = 65534
	// maxAccountGroups bounds the groups or members one call may name.
	maxAccountGroups = 32
)

// Password modes of create_user and update_user.
const (
	passwordKeep     = "keep"
	passwordDisabled = "disabled"
	passwordRandom   = "random"
	passwordSet      = "set"
)

// protectedUsernames are refused by name, whatever the account's flags.
var protectedUsernames = map[string]bool{"root": true, "admin": true, "truenas_admin": true}

// privilegedGroupNames are administrative groups. Their members are not
// changed by these tools, and their membership is never changed.
var privilegedGroupNames = map[string]bool{
	"root": true, "wheel": true, "sudo": true, "admin": true, "docker": true,
	"builtin_administrators": true, "truenas_admin": true,
	"truenas_readonly_administrators": true, "truenas_sharing_administrators": true,
	"truenas_webshare_administrators": true,
}

var (
	accountNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$`)
	shellPattern       = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,255}$`)
	emailPattern       = regexp.MustCompile(`^[^@\s]{1,64}@[^@\s]{1,255}$`)
)

// userWriteSelect is what the user write tools read and bind to. Password
// hashes, password history, SSH keys and API keys are not requested.
var userWriteSelect = append(append([]interface{}{}, userQuerySelect...), "email", "last_password_change")

// accountRefusedArgs are arguments the account tools refuse by name, with
// the reason. They are not in the schemas, so validateArgs rejects them;
// accountArgRefusal replaces that generic error with the reason.
var accountRefusedArgs = func() map[string]map[string]string {
	const (
		sudo      = "these tools never grant sudo; sudo commands are managed in the TrueNAS web UI"
		privilege = "these tools never change TrueNAS roles or privileges; they are managed in the TrueNAS web UI"
		idmap     = "container id mapping is not supported by these tools"
	)
	common := map[string]string{
		"sudo": sudo, "sudo_commands": sudo, "sudo_commands_nopasswd": sudo,
		"roles": privilege, "role": privilege, "privilege": privilege, "privileges": privilege,
		"userns_idmap": idmap,
	}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range common {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	userCommon := map[string]string{
		"home_mode":         "the home directory is always created new with the TrueNAS default mode",
		"home_create":       "a home directory is always created new below home (home_create is implied)",
		"random_password":   "use password_mode 'random'",
		"password_disabled": "use password_mode 'disabled'",
		"groups":            "use add_groups (and, for update_user, remove_groups)",
	}
	createUser := with(userCommon)
	createUser["group_create"] = "omit group to create a new primary group named after the user"
	updateUser := with(userCommon)
	updateUser["uid"] = "changing the uid of an existing user is not supported (files keep the old uid)"
	updateUser["new_username"] = "renaming a user is not supported by update_user"
	createGroup := with(map[string]string{"users": "use members"})
	updateGroup := with(map[string]string{
		"gid":      "changing the gid of an existing group is not supported (files keep the old gid)",
		"users":    "use add_members and remove_members",
		"new_name": "renaming a group is not supported by update_group",
	})
	return map[string]map[string]string{
		"create_user": createUser, "update_user": updateUser,
		"create_group": createGroup, "update_group": updateGroup,
	}
}()

// accountArgRefusal turns the unknown-argument error of an account tool
// into an explicit refusal when it names an argument these tools refuse
// on purpose. Only names from accountRefusedArgs are echoed.
func accountArgRefusal(name string, args map[string]interface{}, err error) error {
	refused := accountRefusedArgs[name]
	if refused == nil {
		return err
	}
	var keys []string
	for k := range args {
		if _, ok := refused[k]; ok {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return err
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s is refused: %s", k, refused[k]))
	}
	return fmt.Errorf("%s: %s; nothing was executed", name, strings.Join(parts, "; "))
}

func (r *Registry) registerAccountActionTools() {
	str := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	boolean := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc}
	}
	names := func(desc string) map[string]interface{} {
		return map[string]interface{}{
			"type": "array", "description": desc, "maxItems": maxAccountGroups,
			"items": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 32},
		}
	}
	accountID := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc, "minimum": minAccountID, "maximum": maxAccountID}
	}
	username := map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 32}
	password := map[string]interface{}{
		"type": "string", "minLength": 8, "maxLength": 256,
		"description": "Optional: The new password, with password_mode 'set' only. Never shown in previews, results or errors",
	}
	dryRun := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc, "default": false}
	}
	withDesc := func(m map[string]interface{}, desc string) map[string]interface{} {
		out := map[string]interface{}{"description": desc}
		for k, v := range m {
			out[k] = v
		}
		return out
	}

	r.tools["create_user"] = Tool{
		Definition: mcp.Tool{
			Name:        "create_user",
			Description: "Create a local user account. password_mode chooses how it signs in: 'disabled' (the default: no password login), 'random' (TrueNAS-strength random password generated when the call executes and returned once, in the result only) or 'set' (the password argument). The primary group is a new group named after the user unless group names an existing one. Never grants sudo, TrueNAS roles or membership of built-in or administrative groups (such as builtin_administrators, wheel or root); those groups, and groups with roles or sudo, are refused as group or in add_groups, and sudo or role arguments are refused outright. Another user's primary group (such as the group created with that user) is refused as group or in add_groups, since membership grants that user's group file access. Refused for root, admin, truenas_admin, names that already exist (as a user, or as a group when group is omitted) and uids below 1000. With home, a new directory home/<username> is created; it is refused if that path already exists. The preview shows every field that will be set; it never shows a password.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"username":             withDesc(username, "Required: Name of the new user (letters, digits, '_', '.', '-'; not starting with '.' or '-'; at most 32 characters)"),
					"full_name":            map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256, "description": "Required: Full name or description of the account"},
					"password_mode":        map[string]interface{}{"type": "string", "enum": []string{passwordDisabled, passwordRandom, passwordSet}, "default": passwordDisabled, "description": "Optional: 'disabled' (no password login, the default), 'random' (generated when the call executes, returned once in the result) or 'set' (use password)"},
					"password":             password,
					"group":                str("Optional: Existing local group to use as the primary group. Omit to create a new group named after the user"),
					"add_groups":           names("Optional: Existing local groups (names) to add the user to as secondary groups"),
					"uid":                  accountID("Optional: uid for the new user (1000 or more). Omit to let TrueNAS choose the next free one"),
					"home":                 str("Optional: Existing directory under /mnt/<pool> in which to create the new home directory <home>/<username>. Omit for no home directory"),
					"shell":                str("Optional: Login shell, a path such as /usr/bin/bash or /usr/sbin/nologin (default: the TrueNAS default)"),
					"email":                str("Optional: Email address of the user"),
					"smb":                  map[string]interface{}{"type": "boolean", "default": false, "description": "Optional: Allow SMB authentication (default: false). Needs a password"},
					"ssh_password_enabled": map[string]interface{}{"type": "boolean", "default": false, "description": "Optional: Allow SSH password login (default: false). Needs a password and home"},
					"locked":               map[string]interface{}{"type": "boolean", "default": false, "description": "Optional: Create the account locked (default: false)"},
					"dry_run":              dryRun("Optional: Preview without creating the user (default: false)"),
				},
				"required": []string{"username", "full_name"},
			},
		},
		Handler: r.handleCreateUser,
		Plan:    planCreateUser,
		Execute: r.executeCreateUser,
	}

	r.tools["update_user"] = Tool{
		Definition: mcp.Tool{
			Name:        "update_user",
			Description: "Change a local user account: full name, email, shell, SMB and SSH password login, locked, password (password_mode 'disabled', 'random' or 'set'; a random password is generated when the call executes and returned once, in the result only), primary group and secondary groups (add_groups, remove_groups). Refused for root, admin, truenas_admin and other built-in accounts, the account this server signs in as, directory service users, and users that have TrueNAS roles or sudo or belong to an administrative group (such as builtin_administrators or wheel). Never changes sudo, roles, the uid or the username; built-in and administrative groups, and groups with roles or sudo, are refused in group, add_groups and remove_groups. Another user's primary group (such as the group created with that user) is refused in group and add_groups; leaving one with remove_groups is allowed. home can only be set for a user without a home directory, and creates a new directory home/<username>; moving an existing home directory is refused. The preview shows each change with its current value and never shows a password; the confirmation is bound to the user's current settings, so a change to the user after the preview needs a new one.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"username":             withDesc(username, "Required: Name of the local user to change (it is not renamed)"),
					"full_name":            map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256, "description": "Optional: New full name or description"},
					"email":                str("Optional: New email address; an empty string clears it"),
					"shell":                str("Optional: New login shell, a path such as /usr/bin/bash or /usr/sbin/nologin"),
					"smb":                  boolean("Optional: Allow SMB authentication. Turning it on needs a new password (password_mode 'set' or 'random')"),
					"ssh_password_enabled": boolean("Optional: Allow SSH password login. Needs password login and a home directory"),
					"locked":               boolean("Optional: Lock or unlock the account"),
					"password_mode":        map[string]interface{}{"type": "string", "enum": []string{passwordKeep, passwordDisabled, passwordRandom, passwordSet}, "default": passwordKeep, "description": "Optional: 'keep' (the default: no change), 'disabled' (turn password login off), 'random' (new password generated when the call executes, returned once in the result) or 'set' (use password)"},
					"password":             password,
					"group":                str("Optional: Existing local group to make the primary group"),
					"add_groups":           names("Optional: Existing local groups (names) to add the user to"),
					"remove_groups":        names("Optional: Groups (names) to remove the user from"),
					"home":                 str("Optional: For a user without a home directory only: existing directory under /mnt/<pool> in which to create the new home directory <home>/<username>"),
					"dry_run":              dryRun("Optional: Preview without changing the user (default: false)"),
				},
				"required": []string{"username"},
			},
		},
		Handler: r.handleUpdateUser,
		Plan:    planUpdateUser,
		Execute: r.executeUpdateUser,
	}

	r.tools["create_group"] = Tool{
		Definition: mcp.Tool{
			Name:        "create_group",
			Description: "Create a local group, optionally with members. Never grants sudo or TrueNAS roles: sudo and role arguments are refused. Refused for names that already exist and for administrative names (such as builtin_administrators, wheel or root), and for gids below 1000. Members must be existing local users; root, admin, truenas_admin and other built-in accounts, the account this server signs in as, and users with roles or sudo are refused. The preview shows the group and its members.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":    withDesc(username, "Required: Name of the new group (letters, digits, '_', '.', '-'; not starting with '.' or '-'; at most 32 characters)"),
					"gid":     accountID("Optional: gid for the new group (1000 or more). Omit to let TrueNAS choose the next free one"),
					"smb":     map[string]interface{}{"type": "boolean", "default": false, "description": "Optional: Make the group usable in SMB share ACLs (default: false)"},
					"members": names("Optional: Existing local users (usernames) to add as members"),
					"dry_run": dryRun("Optional: Preview without creating the group (default: false)"),
				},
				"required": []string{"name"},
			},
		},
		Handler: r.handleCreateGroup,
		Plan:    planCreateGroup,
		Execute: r.executeCreateGroup,
	}

	r.tools["update_group"] = Tool{
		Definition: mcp.Tool{
			Name:        "update_group",
			Description: "Change a local group: its SMB flag and its members (add_members, remove_members). Refused for built-in groups, administrative groups (such as builtin_administrators, wheel or root), groups with TrueNAS roles or sudo, and directory service groups. Never changes sudo, roles, the gid or the name. Members added or removed must be local users; root, admin, truenas_admin and other built-in accounts, the account this server signs in as, and users with roles or sudo are refused. Adding a member to a group that is another user's primary group (such as the group created with that user) is refused. The preview shows each change with the current members; the confirmation is bound to the group's current settings and members, so a change after the preview needs a new one.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":           withDesc(username, "Required: Name of the local group to change (it is not renamed)"),
					"smb":            boolean("Optional: Make the group usable in SMB share ACLs, or not"),
					"add_members":    names("Optional: Local users (usernames) to add to the group"),
					"remove_members": names("Optional: Users (usernames) to remove from the group"),
					"dry_run":        dryRun("Optional: Preview without changing the group (default: false)"),
				},
				"required": []string{"name"},
			},
		},
		Handler: r.handleUpdateGroup,
		Plan:    planUpdateGroup,
		Execute: r.executeUpdateGroup,
	}
}

// ---- account reads ----

// userInfo is what the write tools read about a user.
type userInfo struct {
	ID, UID          int
	Username         string
	FullName         string
	Email            interface{}
	Home, Shell      string
	PrimaryGroupID   int
	Groups           []int
	Builtin          bool
	Immutable        bool
	Local            bool
	Locked           bool
	PasswordDisabled bool
	SMB              bool
	SSHPassword      bool
	Roles            []string
	Sudo             bool
	LastPassword     interface{}
	Row              map[string]interface{}
}

func parseUser(row map[string]interface{}) userInfo {
	u := userInfo{Row: row, Email: row["email"], LastPassword: row["last_password_change"]}
	u.ID, _ = intID(row["id"])
	u.UID, _ = intID(row["uid"])
	u.Username, _ = row["username"].(string)
	u.FullName, _ = row["full_name"].(string)
	u.Home, _ = row["home"].(string)
	u.Shell, _ = row["shell"].(string)
	if g, ok := row["group"].(map[string]interface{}); ok {
		u.PrimaryGroupID, _ = intID(g["id"])
	}
	u.Groups = intList(row["groups"])
	sort.Ints(u.Groups)
	u.Builtin, _ = row["builtin"].(bool)
	u.Immutable, _ = row["immutable"].(bool)
	u.Local, _ = row["local"].(bool)
	u.Locked, _ = row["locked"].(bool)
	u.PasswordDisabled, _ = row["password_disabled"].(bool)
	u.SMB, _ = row["smb"].(bool)
	u.SSHPassword, _ = row["ssh_password_enabled"].(bool)
	u.Roles = stringList(row["roles"])
	sort.Strings(u.Roles)
	u.Sudo = len(stringList(row["sudo_commands"]))+len(stringList(row["sudo_commands_nopasswd"])) > 0
	return u
}

// bind is the user's state that a confirmation is bound to.
func (u userInfo) bind() map[string]interface{} {
	return map[string]interface{}{
		"id": u.ID, "uid": u.UID, "username": u.Username, "full_name": u.FullName,
		"email": u.Email, "home": u.Home, "shell": u.Shell,
		"primary_group_id": u.PrimaryGroupID, "groups": u.Groups,
		"builtin": u.Builtin, "immutable": u.Immutable, "local": u.Local,
		"locked": u.Locked, "password_disabled": u.PasswordDisabled, "smb": u.SMB,
		"ssh_password_enabled": u.SSHPassword, "roles": u.Roles, "sudo": u.Sudo,
		"last_password_change": u.LastPassword,
	}
}

// groupInfo is what the write tools read about a group.
type groupInfo struct {
	ID, GID   int
	Name      string
	Builtin   bool
	Immutable bool
	Local     bool
	SMB       bool
	Users     []int
	Roles     []string
	Sudo      bool
	Row       map[string]interface{}
}

func parseGroup(row map[string]interface{}) groupInfo {
	g := groupInfo{Row: row}
	g.ID, _ = intID(row["id"])
	g.GID, _ = intID(row["gid"])
	g.Name, _ = row["name"].(string)
	g.Builtin, _ = row["builtin"].(bool)
	g.Immutable, _ = row["immutable"].(bool)
	g.Local, _ = row["local"].(bool)
	g.SMB, _ = row["smb"].(bool)
	g.Users = intList(row["users"])
	sort.Ints(g.Users)
	g.Roles = stringList(row["roles"])
	sort.Strings(g.Roles)
	g.Sudo = len(stringList(row["sudo_commands"]))+len(stringList(row["sudo_commands_nopasswd"])) > 0
	return g
}

func (g groupInfo) bind() map[string]interface{} {
	return map[string]interface{}{
		"id": g.ID, "gid": g.GID, "name": g.Name, "builtin": g.Builtin,
		"immutable": g.Immutable, "local": g.Local, "smb": g.SMB,
		"users": g.Users, "roles": g.Roles, "sudo": g.Sudo,
	}
}

func queryAccountUsers(ctx context.Context, client *truenas.Client, filter []interface{}) ([]userInfo, error) {
	raw, err := client.Call(ctx, "user.query",
		[]interface{}{filter, []interface{}{"local", "=", true}},
		map[string]interface{}{"select": userWriteSelect},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to look up users: %w", err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected user.query result")
	}
	out := make([]userInfo, 0, len(rows))
	for _, row := range rows {
		if local, _ := row["local"].(bool); local {
			out = append(out, parseUser(row))
		}
	}
	return out, nil
}

func queryAccountGroups(ctx context.Context, client *truenas.Client, filter []interface{}) ([]groupInfo, error) {
	raw, err := client.Call(ctx, "group.query",
		[]interface{}{filter, []interface{}{"local", "=", true}},
		map[string]interface{}{"select": groupQuerySelect},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to look up groups: %w", err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected group.query result")
	}
	out := make([]groupInfo, 0, len(rows))
	for _, row := range rows {
		if local, _ := row["local"].(bool); local {
			out = append(out, parseGroup(row))
		}
	}
	return out, nil
}

func stringsAsArgs(list []string) []interface{} {
	out := make([]interface{}, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

func intsAsArgs(list []int) []interface{} {
	out := make([]interface{}, len(list))
	for i, n := range list {
		out[i] = n
	}
	return out
}

// usersByName returns the local users with these names. Rows the
// middleware returns for other names are dropped.
func usersByName(ctx context.Context, client *truenas.Client, names []string) (map[string]userInfo, error) {
	out := map[string]userInfo{}
	if len(names) == 0 {
		return out, nil
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	users, err := queryAccountUsers(ctx, client, []interface{}{"username", "in", stringsAsArgs(names)})
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if want[u.Username] {
			out[u.Username] = u
		}
	}
	return out, nil
}

func usersByID(ctx context.Context, client *truenas.Client, ids []int) (map[int]userInfo, error) {
	out := map[int]userInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	users, err := queryAccountUsers(ctx, client, []interface{}{"id", "in", intsAsArgs(ids)})
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if want[u.ID] {
			out[u.ID] = u
		}
	}
	return out, nil
}

func userUIDTaken(ctx context.Context, client *truenas.Client, uid int) (bool, error) {
	users, err := queryAccountUsers(ctx, client, []interface{}{"uid", "=", uid})
	if err != nil {
		return false, err
	}
	for _, u := range users {
		if u.UID == uid {
			return true, nil
		}
	}
	return false, nil
}

func groupsByName(ctx context.Context, client *truenas.Client, names []string) (map[string]groupInfo, error) {
	out := map[string]groupInfo{}
	if len(names) == 0 {
		return out, nil
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	groups, err := queryAccountGroups(ctx, client, []interface{}{"name", "in", stringsAsArgs(names)})
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if want[g.Name] {
			out[g.Name] = g
		}
	}
	return out, nil
}

func groupsByID(ctx context.Context, client *truenas.Client, ids []int) (map[int]groupInfo, error) {
	out := map[int]groupInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	groups, err := queryAccountGroups(ctx, client, []interface{}{"id", "in", intsAsArgs(ids)})
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if want[g.ID] {
			out[g.ID] = g
		}
	}
	return out, nil
}

func groupGIDTaken(ctx context.Context, client *truenas.Client, gid int) (bool, error) {
	groups, err := queryAccountGroups(ctx, client, []interface{}{"gid", "=", gid})
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if g.GID == gid {
			return true, nil
		}
	}
	return false, nil
}

// primaryGroupOwners returns, for each group id, the usernames of the
// local users whose primary group it is, sorted.
func primaryGroupOwners(ctx context.Context, client *truenas.Client, ids []int) (map[int][]string, error) {
	out := map[int][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	users, err := queryAccountUsers(ctx, client, []interface{}{"group.id", "in", intsAsArgs(ids)})
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if want[u.PrimaryGroupID] {
			out[u.PrimaryGroupID] = append(out[u.PrimaryGroupID], u.Username)
		}
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out, nil
}

// ---- refusal rules ----

// personalGroupRefusal says why username may not be added to g, or "":
// g is the primary group of another user (such as the group created with
// that user), and membership would give username that user's group file
// access.
func personalGroupRefusal(g groupInfo, owners []string, username string) string {
	for _, o := range owners {
		if o != username {
			return fmt.Sprintf("group '%s' is the primary group of user '%s'; these tools never add another account to a user's personal group, since that grants its file access (change it in the TrueNAS web UI)", g.Name, o)
		}
	}
	return ""
}

// privilegedGroupReason says why g is an administrative group, or "".
func privilegedGroupReason(g groupInfo) string {
	switch {
	case g.GID == 0:
		return "has gid 0"
	case privilegedGroupNames[g.Name]:
		return "is an administrative group"
	case len(g.Roles) > 0:
		return fmt.Sprintf("grants TrueNAS roles (%s)", strings.Join(g.Roles, ", "))
	case g.Sudo:
		return "grants sudo"
	}
	return ""
}

// groupChangeRefusal says why the membership or settings of g may not be
// changed by these tools, or "".
func groupChangeRefusal(g groupInfo) string {
	if !g.Local {
		return fmt.Sprintf("group '%s' is provided by a directory service; change it there", g.Name)
	}
	if why := privilegedGroupReason(g); why != "" {
		return fmt.Sprintf("group '%s' %s; its membership is managed in the TrueNAS web UI, not by these tools", g.Name, why)
	}
	if g.Builtin || g.Immutable {
		return fmt.Sprintf("group '%s' is a built-in group; its membership is managed by TrueNAS or in the web UI, not by these tools", g.Name)
	}
	return ""
}

// protectedUserRefusal refuses a username these tools never create or
// change, from the name alone.
func protectedUserRefusal(username, apiUser string) string {
	switch {
	case username == "root":
		return "user 'root' is the root account; these tools never change it"
	case protectedUsernames[username]:
		return fmt.Sprintf("user '%s' is a built-in administrator account; these tools never change it", username)
	case apiUser != "" && username == apiUser:
		return fmt.Sprintf("user '%s' is the account this server signs in as; change it in the TrueNAS web UI", username)
	}
	return ""
}

// userChangeRefusal says why u may not be changed (or have its group
// membership changed) by these tools, or "". groups must hold every group
// u belongs to; a group that is missing fails closed.
func userChangeRefusal(u userInfo, apiUser string, groups map[int]groupInfo) string {
	if why := protectedUserRefusal(u.Username, apiUser); why != "" {
		return why
	}
	switch {
	case !u.Local:
		return fmt.Sprintf("user '%s' is provided by a directory service; change it there", u.Username)
	case u.UID == 0:
		return fmt.Sprintf("user '%s' has uid 0; these tools never change it", u.Username)
	case u.UID == adminUID:
		return fmt.Sprintf("user '%s' has the built-in administrator uid %d; these tools never change it", u.Username, adminUID)
	case u.Builtin || u.Immutable:
		return fmt.Sprintf("user '%s' is a built-in account; these tools never change it", u.Username)
	case len(u.Roles) > 0:
		return fmt.Sprintf("user '%s' has TrueNAS roles (%s); privileged accounts are managed in the TrueNAS web UI", u.Username, strings.Join(u.Roles, ", "))
	case u.Sudo:
		return fmt.Sprintf("user '%s' has sudo commands; privileged accounts are managed in the TrueNAS web UI", u.Username)
	}
	for _, id := range append([]int{u.PrimaryGroupID}, u.Groups...) {
		g, ok := groups[id]
		if !ok {
			return fmt.Sprintf("could not read group id %d of user '%s', so its privileges cannot be checked", id, u.Username)
		}
		if why := privilegedGroupReason(g); why != "" {
			return fmt.Sprintf("user '%s' is a member of group '%s', which %s; privileged accounts are managed in the TrueNAS web UI", u.Username, g.Name, why)
		}
	}
	return ""
}

// userGroupIDs returns every group id the users belong to.
func userGroupIDs(users ...userInfo) []int {
	var ids []int
	for _, u := range users {
		ids = append(ids, u.PrimaryGroupID)
		ids = append(ids, u.Groups...)
	}
	return ids
}

// ---- argument parsing ----

func validateAccountName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%s is required", kind)
	}
	if !accountNamePattern.MatchString(name) {
		return fmt.Errorf("invalid %s: use letters, digits, '_', '.' and '-', not starting with '.' or '-', at most 32 characters", kind)
	}
	return nil
}

func trimmedArg(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key].(string)
	return strings.TrimSpace(v), ok
}

// nameListArg returns a list of account names, trimmed, validated and
// without duplicates, in the order given.
func nameListArg(args map[string]interface{}, key, kind string) ([]string, error) {
	list, _ := args[key].([]interface{})
	seen := map[string]bool{}
	var out []string
	for _, item := range list {
		s, _ := item.(string)
		s = strings.TrimSpace(s)
		if err := validateAccountName(kind+" in "+key, s); err != nil {
			return nil, err
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

func validateFullName(s string) error {
	if s == "" {
		return fmt.Errorf("full_name must not be empty")
	}
	for _, r := range s {
		if r == ':' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("full_name must not contain ':' or control characters")
		}
	}
	return nil
}

func validateShell(s string) error {
	if !shellPattern.MatchString(s) || path.Clean(s) != s {
		return fmt.Errorf("shell must be an absolute path such as /usr/bin/bash or /usr/sbin/nologin")
	}
	return nil
}

func validateEmail(s string) error {
	if !emailPattern.MatchString(s) {
		return fmt.Errorf("email must be an address such as user@example.com")
	}
	return nil
}

// passwordModeArg returns the password mode and checks that password is
// given exactly when the mode is set. The password value is never echoed.
func passwordModeArg(args map[string]interface{}, def string) (string, error) {
	mode, _ := args["password_mode"].(string)
	if mode == "" {
		mode = def
	}
	pw, _ := args["password"].(string)
	switch {
	case mode == passwordSet && pw == "":
		return "", fmt.Errorf("password_mode 'set' needs password")
	case mode != passwordSet && pw != "":
		return "", fmt.Errorf("password is only used with password_mode 'set' (password_mode is '%s')", mode)
	case mode == passwordSet && strings.TrimSpace(pw) != pw:
		return "", fmt.Errorf("password must not start or end with whitespace")
	}
	return mode, nil
}

func loginModeText(mode string) string {
	switch mode {
	case passwordDisabled:
		return "password login disabled"
	case passwordRandom:
		return "random password, generated when the call executes and returned once in its result"
	case passwordSet:
		return "password supplied with this call (not shown)"
	}
	return "unchanged"
}

// ---- home directories ----

// validateHomeParent checks a home argument: an absolute, clean path under
// /mnt/<pool>, outside datasets TrueNAS manages itself.
func validateHomeParent(p string) error {
	if !strings.HasPrefix(p, "/mnt/") || path.Clean(p) != p || strings.ContainsAny(p, ":\n\r\x00") {
		return fmt.Errorf("home must be an absolute path under /mnt/<pool> such as /mnt/tank/home, without '.', '..' or ':'")
	}
	rest := strings.TrimPrefix(p, "/mnt/")
	if rest == "" || strings.HasPrefix(rest, ".") {
		return fmt.Errorf("home must be under a pool's /mnt/<pool> path")
	}
	if part, ok := managedDatasetPart(rest); ok {
		return fmt.Errorf("home '%s' is inside '%s', which TrueNAS manages itself", p, part)
	}
	return nil
}

// statPath returns filesystem.stat of p, and whether it exists.
func statPath(ctx context.Context, client *truenas.Client, p string) (map[string]interface{}, bool, error) {
	raw, err := client.Call(ctx, "filesystem.stat", p)
	if err != nil {
		var apiErr *truenas.APICallError
		if errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "[ENOENT]") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to check %s: %w", p, err)
	}
	var st map[string]interface{}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, false, fmt.Errorf("unexpected filesystem.stat result for %s", p)
	}
	return st, true, nil
}

// newHome checks that parent is an existing directory and that
// parent/username does not exist yet: TrueNAS would take over an existing
// directory and reset its owner, mode and ACL. It returns the new home
// directory, the refusals and the state to bind.
func newHome(ctx context.Context, client *truenas.Client, parent, username string) (string, []string, map[string]interface{}, error) {
	target := parent + "/" + username
	st, ok, err := statPath(ctx, client, parent)
	if err != nil {
		return "", nil, nil, err
	}
	var blocked []string
	bind := map[string]interface{}{"parent": parent, "parent_exists": ok}
	switch typ, _ := st["type"].(string); {
	case !ok:
		blocked = append(blocked, fmt.Sprintf("home '%s' does not exist; create the dataset or directory first", parent))
	case typ != "DIRECTORY":
		blocked = append(blocked, fmt.Sprintf("home '%s' is a %s, not a directory", parent, strings.ToLower(typ)))
	default:
		bind["parent_type"] = typ
	}
	_, exists, err := statPath(ctx, client, target)
	if err != nil {
		return "", nil, nil, err
	}
	bind["target_exists"] = exists
	if exists {
		blocked = append(blocked, fmt.Sprintf("'%s' already exists; these tools only create a new home directory and never take over an existing one (TrueNAS would reset its owner, mode and ACL)", target))
	}
	return target, blocked, bind, nil
}

// ---- password handling at execution ----

// executionPassword returns the password to send for mode: the caller's
// for set, a new random one for random, "" otherwise.
func executionPassword(mode string, args map[string]interface{}) (string, error) {
	switch mode {
	case passwordSet:
		pw, _ := args["password"].(string)
		if pw == "" {
			return "", fmt.Errorf("password_mode 'set' needs password")
		}
		return pw, nil
	case passwordRandom:
		return generatePassword()
	}
	return "", nil
}

// withPassword returns a copy of plan whose payload (the map at index i of
// CallParams) carries pw. The plan itself, which was hashed into the
// confirmation, never holds a password.
func withPassword(plan *writePlan, i int, pw string) *writePlan {
	if pw == "" {
		return plan
	}
	out := *plan
	out.CallParams = append([]interface{}{}, plan.CallParams...)
	payload, _ := plan.CallParams[i].(map[string]interface{})
	cp := make(map[string]interface{}, len(payload)+1)
	for k, v := range payload {
		cp[k] = v
	}
	cp["password"] = pw
	out.CallParams[i] = cp
	return &out
}

// scrubSecretText replaces secret, and its JSON-escaped form, wherever it
// appears in msg, ignoring case.
func scrubSecretText(msg, secret string) string {
	if secret == "" {
		return msg
	}
	variants := []string{secret}
	if b, err := json.Marshal(secret); err == nil && len(b) > 2 {
		if esc := string(b[1 : len(b)-1]); esc != secret {
			variants = append(variants, esc)
		}
	}
	for _, v := range variants {
		msg = regexp.MustCompile("(?i)"+regexp.QuoteMeta(v)).ReplaceAllString(msg, redact.Marker)
	}
	return msg
}

// accountCallError is err from a write call with pw removed from the text
// the server supplied (the request block is already masked by key).
func accountCallError(what string, err error, pw string) error {
	var apiErr *truenas.APICallError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("failed to %s: %s", what, apiErr.MapServerText(func(s string) string { return scrubSecretText(s, pw) }).Error())
	}
	return fmt.Errorf("failed to %s: %s", what, scrubSecretText(err.Error(), pw))
}

// ---- create_user ----

func planCreateUser(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	username, _ := trimmedArg(args, "username")
	if err := validateAccountName("username", username); err != nil {
		return nil, err
	}
	apiUser := client.APIUser()
	if why := protectedUserRefusal(username, apiUser); why != "" {
		return nil, fmt.Errorf("%s; nothing was created", why)
	}
	fullName, _ := trimmedArg(args, "full_name")
	if err := validateFullName(fullName); err != nil {
		return nil, err
	}
	mode, err := passwordModeArg(args, passwordDisabled)
	if err != nil {
		return nil, err
	}
	if mode == passwordKeep {
		return nil, fmt.Errorf("password_mode must be 'disabled', 'random' or 'set'")
	}
	smb, _ := args["smb"].(bool)
	sshPassword, _ := args["ssh_password_enabled"].(bool)
	locked, _ := args["locked"].(bool)
	groupName, hasGroup := trimmedArg(args, "group")
	if hasGroup {
		if err := validateAccountName("group", groupName); err != nil {
			return nil, err
		}
	}
	addGroups, err := nameListArg(args, "add_groups", "group")
	if err != nil {
		return nil, err
	}
	shell, hasShell := trimmedArg(args, "shell")
	if hasShell {
		if err := validateShell(shell); err != nil {
			return nil, err
		}
	}
	email, hasEmail := trimmedArg(args, "email")
	if hasEmail {
		if err := validateEmail(email); err != nil {
			return nil, err
		}
	}
	home, hasHome := trimmedArg(args, "home")
	if hasHome {
		if err := validateHomeParent(home); err != nil {
			return nil, err
		}
	}
	uid, hasUID := intID(args["uid"])
	if hasUID && uid == nobodyID {
		return nil, fmt.Errorf("uid %d belongs to nobody; choose another uid or omit it", uid)
	}
	switch {
	case smb && mode == passwordDisabled:
		return nil, fmt.Errorf("smb needs a password: TrueNAS does not allow SMB users without password login; use password_mode 'random' or 'set'")
	case sshPassword && mode == passwordDisabled:
		return nil, fmt.Errorf("ssh_password_enabled needs a password; use password_mode 'random' or 'set'")
	case sshPassword && !hasHome:
		return nil, fmt.Errorf("ssh_password_enabled needs a home directory; set home")
	}

	var blocked []string
	existing, err := usersByName(ctx, client, []string{username})
	if err != nil {
		return nil, err
	}
	if _, ok := existing[username]; ok {
		blocked = append(blocked, fmt.Sprintf("user '%s' already exists; use update_user to change it", username))
	}
	if hasUID {
		taken, err := userUIDTaken(ctx, client, uid)
		if err != nil {
			return nil, err
		}
		if taken {
			blocked = append(blocked, fmt.Sprintf("uid %d is already used by another local user; choose another or omit uid", uid))
		}
	}

	lookup := append([]string{}, addGroups...)
	if hasGroup {
		lookup = append(lookup, groupName)
	} else {
		lookup = append(lookup, username)
	}
	groups, err := groupsByName(ctx, client, lookup)
	if err != nil {
		return nil, err
	}
	var ownerIDs []int
	for _, g := range groups {
		ownerIDs = append(ownerIDs, g.ID)
	}
	owners, err := primaryGroupOwners(ctx, client, ownerIDs)
	if err != nil {
		return nil, err
	}
	payload := map[string]interface{}{
		"username":               username,
		"full_name":              fullName,
		"password_disabled":      mode == passwordDisabled,
		"smb":                    smb,
		"ssh_password_enabled":   sshPassword,
		"locked":                 locked,
		"sudo_commands":          []interface{}{},
		"sudo_commands_nopasswd": []interface{}{},
	}
	groupBinds := map[string]interface{}{}
	groupNames := map[int]string{}
	primaryText := fmt.Sprintf("new group '%s'", username)
	if hasGroup {
		g, ok := groups[groupName]
		if !ok {
			return nil, fmt.Errorf("group '%s' not found as a local group", groupName)
		}
		if why := groupChangeRefusal(g); why != "" {
			blocked = append(blocked, why)
		}
		if why := personalGroupRefusal(g, owners[g.ID], username); why != "" {
			blocked = append(blocked, why)
		}
		payload["group"] = g.ID
		payload["group_create"] = false
		groupBinds[g.Name] = g.bind()
		groupNames[g.ID] = g.Name
		primaryText = fmt.Sprintf("'%s' (gid %d)", g.Name, g.GID)
	} else {
		if g, ok := groups[username]; ok {
			blocked = append(blocked, fmt.Sprintf("a group named '%s' already exists, and TrueNAS would make it the primary group; pass group to choose a primary group explicitly, or pick another username", g.Name))
		}
		payload["group_create"] = true
	}
	var missing []string
	secondary := []interface{}{}
	var secondaryText []string
	for _, name := range addGroups {
		if hasGroup && name == groupName {
			return nil, fmt.Errorf("group '%s' is the primary group; do not also list it in add_groups", name)
		}
		g, ok := groups[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if why := groupChangeRefusal(g); why != "" {
			blocked = append(blocked, why)
		}
		if why := personalGroupRefusal(g, owners[g.ID], username); why != "" {
			blocked = append(blocked, why)
		}
		secondary = append(secondary, g.ID)
		secondaryText = append(secondaryText, name)
		groupBinds[g.Name] = g.bind()
		groupNames[g.ID] = g.Name
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("group(s) not found as local groups: %s", strings.Join(missing, ", "))
	}
	payload["groups"] = secondary
	if hasUID {
		payload["uid"] = uid
	}
	if hasShell {
		payload["shell"] = shell
	}
	if hasEmail {
		payload["email"] = email
	}
	bind := map[string]interface{}{
		"login_mode": mode, "username_free": len(existing) == 0, "groups": groupBinds,
		"primary_group_owners": owners,
	}
	homeText := "none (" + defaultHomePath + ")"
	if hasHome {
		target, homeBlocked, homeBind, err := newHome(ctx, client, home, username)
		if err != nil {
			return nil, err
		}
		blocked = append(blocked, homeBlocked...)
		payload["home"] = home
		payload["home_create"] = true
		bind["home"] = homeBind
		homeText = fmt.Sprintf("new directory %s", target)
	}

	uidText := "next free uid"
	if hasUID {
		uidText = fmt.Sprint(uid)
	}
	shellText := "TrueNAS default"
	if hasShell {
		shellText = shell
	}
	view := map[string]interface{}{
		"username": username, "full_name": fullName, "uid": uidText,
		"primary_group": primaryText, "secondary_groups": secondaryText,
		"login_mode": loginModeText(mode), "smb": smb, "ssh_password_enabled": sshPassword,
		"locked": locked, "home": homeText, "shell": shellText,
		"sudo": "none", "roles": "none",
	}
	if hasEmail {
		view["email"] = email
	}
	facts := []string{
		confirmFact("username", username),
		confirmFact("full name", fullName),
		confirmFact("uid", uidText),
		confirmFact("primary group", primaryText),
		confirmFact("secondary groups", strings.Join(secondaryText, ", ")),
		confirmFact("login", loginModeText(mode)),
		confirmFact("smb", fmt.Sprint(smb)),
		confirmFact("ssh password login", fmt.Sprint(sshPassword)),
		confirmFact("home", homeText),
		confirmFact("shell", shellText),
		confirmFact("locked", fmt.Sprint(locked)),
	}
	var warnings []string
	if mode == passwordRandom {
		warnings = append(warnings, "The random password is shown only once, in the result of the call that creates the user; store it then")
	}
	if smb {
		warnings = append(warnings, "TrueNAS adds SMB users to the builtin_users group")
	}
	if mode == passwordDisabled {
		warnings = append(warnings, "Password login is disabled: the user cannot sign in with a password until one is set with update_user")
	}

	return &writePlan{
		Tool:         "create_user",
		Method:       userCreateMethod,
		CallParams:   []interface{}{payload},
		TargetName:   username,
		CurrentState: map[string]interface{}{"username": username, "exists": len(existing) > 0},
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Create local user '%s' (%s)", username, loginModeText(mode)),
			Operation:   "create",
			Target:      username,
			Details:     view,
		}},
		Warnings:       warnings,
		Blocked:        blocked,
		Bind:           bind,
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Create user %s", username), facts...),
		Result:         map[string]interface{}{"login_mode": mode, "group_names": groupNames},
	}, nil
}

func (r *Registry) handleCreateUser(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planCreateUser(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeCreateUser(ctx, client, args, plan)
}

func (r *Registry) executeCreateUser(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	mode, _ := plan.Result["login_mode"].(string)
	pw, err := executionPassword(mode, args)
	if err != nil {
		return "", err
	}
	raw, err := callPlan(ctx, client, withPassword(plan, 0, pw))
	if err != nil {
		return "", accountCallError("create user "+plan.TargetName, err, pw)
	}
	names := map[int]string{}
	if known, ok := plan.Result["group_names"].(map[int]string); ok {
		for id, n := range known {
			names[id] = n
		}
	}
	response := map[string]interface{}{"created": true, "username": plan.TargetName}
	var row map[string]interface{}
	if json.Unmarshal(raw, &row) == nil && row != nil {
		u := parseUser(row)
		if names[u.PrimaryGroupID] == "" {
			if g, ok := row["group"].(map[string]interface{}); ok {
				names[u.PrimaryGroupID] = firstStringOf(g, "bsdgrp_group", "name", "group")
			}
		}
		response["user"] = simplifyUser(row, names)
	} else {
		response["note"] = "The user was created, but user.create returned no user entry; check query_users"
	}
	msg := fmt.Sprintf("Created user %s (%s)", plan.TargetName, loginModeText(mode))
	if mode == passwordRandom {
		r.oneTimeSecrets.add(pw)
		response[mcp.GeneratedPasswordKey] = pw
		msg = fmt.Sprintf("Created user %s. Its random password is in generated_password and is shown only this once: store it now", plan.TargetName)
	}
	response["message"] = msg
	return marshalJSON(response)
}

// ---- update_user ----

func planUpdateUser(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	username, _ := trimmedArg(args, "username")
	if err := validateAccountName("username", username); err != nil {
		return nil, err
	}
	apiUser := client.APIUser()
	if why := protectedUserRefusal(username, apiUser); why != "" {
		return nil, fmt.Errorf("%s; nothing was changed", why)
	}
	mode, err := passwordModeArg(args, passwordKeep)
	if err != nil {
		return nil, err
	}
	fullName, hasFullName := trimmedArg(args, "full_name")
	if hasFullName {
		if err := validateFullName(fullName); err != nil {
			return nil, err
		}
	}
	email, hasEmail := trimmedArg(args, "email")
	if hasEmail && email != "" {
		if err := validateEmail(email); err != nil {
			return nil, err
		}
	}
	shell, hasShell := trimmedArg(args, "shell")
	if hasShell {
		if err := validateShell(shell); err != nil {
			return nil, err
		}
	}
	home, hasHome := trimmedArg(args, "home")
	if hasHome {
		if err := validateHomeParent(home); err != nil {
			return nil, err
		}
	}
	groupName, hasGroup := trimmedArg(args, "group")
	if hasGroup {
		if err := validateAccountName("group", groupName); err != nil {
			return nil, err
		}
	}
	addGroups, err := nameListArg(args, "add_groups", "group")
	if err != nil {
		return nil, err
	}
	removeGroups, err := nameListArg(args, "remove_groups", "group")
	if err != nil {
		return nil, err
	}
	for _, a := range addGroups {
		for _, b := range removeGroups {
			if a == b {
				return nil, fmt.Errorf("group '%s' is in both add_groups and remove_groups", a)
			}
		}
	}

	users, err := usersByName(ctx, client, []string{username})
	if err != nil {
		return nil, err
	}
	u, ok := users[username]
	if !ok {
		return nil, fmt.Errorf("local user '%s' not found (directory service users must be changed in the directory)", username)
	}
	lookup := append(append([]string{}, addGroups...), removeGroups...)
	if hasGroup {
		lookup = append(lookup, groupName)
	}
	named, err := groupsByName(ctx, client, lookup)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, n := range lookup {
		if _, ok := named[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("group(s) not found as local groups: %s", strings.Join(missing, ", "))
	}
	current, err := groupsByID(ctx, client, userGroupIDs(u))
	if err != nil {
		return nil, err
	}
	var ownerIDs []int
	for _, g := range named {
		ownerIDs = append(ownerIDs, g.ID)
	}
	owners, err := primaryGroupOwners(ctx, client, ownerIDs)
	if err != nil {
		return nil, err
	}
	groupNames := map[int]string{}
	for id, g := range current {
		groupNames[id] = g.Name
	}
	for _, g := range named {
		groupNames[g.ID] = g.Name
	}

	var blocked []string
	if why := userChangeRefusal(u, apiUser, current); why != "" {
		blocked = append(blocked, why)
	}

	payload := map[string]interface{}{}
	var changes []string
	change := func(field string, from, to interface{}) {
		changes = append(changes, fmt.Sprintf("%s: %s -> %s", field, confirmShown(field, from), confirmShown(field, to)))
	}
	if hasFullName && fullName != u.FullName {
		payload["full_name"] = fullName
		change("full_name", u.FullName, fullName)
	}
	if hasEmail {
		cur, _ := u.Email.(string)
		if email != cur {
			if email == "" {
				payload["email"] = nil
			} else {
				payload["email"] = email
			}
			change("email", u.Email, payload["email"])
		}
	}
	if hasShell && shell != u.Shell {
		payload["shell"] = shell
		change("shell", u.Shell, shell)
	}
	finalSMB := u.SMB
	if v, ok := args["smb"].(bool); ok && v != u.SMB {
		payload["smb"] = v
		finalSMB = v
		change("smb", u.SMB, v)
	}
	if v, ok := args["locked"].(bool); ok && v != u.Locked {
		payload["locked"] = v
		change("locked", u.Locked, v)
	}
	finalSSH := u.SSHPassword
	if v, ok := args["ssh_password_enabled"].(bool); ok && v != u.SSHPassword {
		payload["ssh_password_enabled"] = v
		finalSSH = v
		change("ssh_password_enabled", u.SSHPassword, v)
	}

	finalDisabled := u.PasswordDisabled
	switch mode {
	case passwordDisabled:
		if !u.PasswordDisabled {
			payload["password_disabled"] = true
			finalDisabled = true
			change("password_disabled", false, true)
		}
	case passwordRandom, passwordSet:
		if u.PasswordDisabled {
			payload["password_disabled"] = false
			finalDisabled = false
			change("password_disabled", true, false)
		}
		changes = append(changes, "login: "+loginModeText(mode))
	}

	bind := map[string]interface{}{"user": u.bind(), "login_mode": mode}
	homeText := u.Home
	if hasHome {
		switch {
		case u.Home != "" && u.Home != defaultHomePath:
			blocked = append(blocked, fmt.Sprintf("user '%s' already has home directory '%s'; moving a home directory copies its data, so update_user refuses it (change it in the TrueNAS web UI)", username, u.Home))
		default:
			target, homeBlocked, homeBind, err := newHome(ctx, client, home, username)
			if err != nil {
				return nil, err
			}
			blocked = append(blocked, homeBlocked...)
			payload["home"] = home
			payload["home_create"] = true
			bind["home"] = homeBind
			homeText = target
			change("home", u.Home, "new directory "+target)
		}
	}

	groupBinds := map[string]interface{}{}
	if hasGroup {
		g := named[groupName]
		if why := groupChangeRefusal(g); why != "" {
			blocked = append(blocked, why)
		}
		groupBinds[g.Name] = g.bind()
		if g.ID != u.PrimaryGroupID {
			if why := personalGroupRefusal(g, owners[g.ID], username); why != "" {
				blocked = append(blocked, why)
			}
			payload["group"] = g.ID
			change("primary_group", groupNames[u.PrimaryGroupID], g.Name)
		}
	}
	newGroups := append([]int{}, u.Groups...)
	member := map[int]bool{}
	for _, id := range u.Groups {
		member[id] = true
	}
	var notes []string
	for _, name := range addGroups {
		g := named[name]
		if why := groupChangeRefusal(g); why != "" {
			blocked = append(blocked, why)
		}
		groupBinds[g.Name] = g.bind()
		switch {
		case g.ID == u.PrimaryGroupID || (hasGroup && name == groupName):
			notes = append(notes, fmt.Sprintf("'%s' is the primary group; not added as a secondary group", name))
		case member[g.ID]:
			notes = append(notes, fmt.Sprintf("'%s' is already a member of '%s'", username, name))
		default:
			if why := personalGroupRefusal(g, owners[g.ID], username); why != "" {
				blocked = append(blocked, why)
			}
			newGroups = append(newGroups, g.ID)
			member[g.ID] = true
			changes = append(changes, fmt.Sprintf("add to group: %s", name))
		}
	}
	for _, name := range removeGroups {
		g := named[name]
		if why := groupChangeRefusal(g); why != "" {
			blocked = append(blocked, why)
		}
		groupBinds[g.Name] = g.bind()
		if !member[g.ID] {
			notes = append(notes, fmt.Sprintf("'%s' is not a secondary member of '%s'", username, name))
			continue
		}
		kept := newGroups[:0]
		for _, id := range newGroups {
			if id != g.ID {
				kept = append(kept, id)
			}
		}
		newGroups = kept
		delete(member, g.ID)
		changes = append(changes, fmt.Sprintf("remove from group: %s", name))
	}
	if len(addGroups)+len(removeGroups) > 0 && !sameInts(newGroups, u.Groups) {
		payload["groups"] = intsAsArgs(newGroups)
	}
	bind["groups"] = groupBinds
	bind["primary_group_owners"] = owners
	currentGroupBinds := map[string]interface{}{}
	for _, g := range current {
		currentGroupBinds[fmt.Sprint(g.ID)] = g.bind()
	}
	bind["current_groups"] = currentGroupBinds

	finalHome := u.Home
	if _, ok := payload["home"]; ok {
		finalHome = homeText
	}
	switch {
	case finalSMB && finalDisabled:
		blocked = append(blocked, fmt.Sprintf("user '%s' would be an SMB user with password login disabled, which TrueNAS does not allow", username))
	case finalSMB && !u.SMB && mode != passwordSet && mode != passwordRandom:
		blocked = append(blocked, "turning smb on needs a new password: TrueNAS stores the SMB hash only when the password is set; use password_mode 'random' or 'set'")
	}
	if finalSSH && (finalDisabled || finalHome == "" || finalHome == defaultHomePath) {
		if _, changed := payload["ssh_password_enabled"]; changed || mode == passwordDisabled {
			blocked = append(blocked, "SSH password login needs password login and a home directory")
		}
	}
	if len(changes) == 0 && len(blocked) == 0 {
		msg := fmt.Sprintf("nothing to change: user '%s' already has these settings", username)
		if len(notes) > 0 {
			msg += " (" + strings.Join(notes, "; ") + ")"
		}
		blocked = append(blocked, msg)
	}

	currentView := map[string]interface{}{
		"username": u.Username, "uid": u.UID, "full_name": u.FullName, "email": u.Email,
		"home": u.Home, "shell": u.Shell, "smb": u.SMB, "locked": u.Locked,
		"password_disabled": u.PasswordDisabled, "ssh_password_enabled": u.SSHPassword,
		"primary_group": groupNames[u.PrimaryGroupID], "groups": namesOf(u.Groups, groupNames),
		"builtin": u.Builtin, "roles": u.Roles, "sudo": u.Sudo,
	}
	var warnings []string
	if mode == passwordRandom {
		warnings = append(warnings, "The random password is shown only once, in the result of the call that sets it; store it then")
	}
	if mode == passwordRandom || mode == passwordSet || mode == passwordDisabled {
		warnings = append(warnings, fmt.Sprintf("'%s' can no longer sign in with the old password", username))
	}
	if v, ok := payload["locked"].(bool); ok && v {
		warnings = append(warnings, fmt.Sprintf("While locked, '%s' cannot sign in at all", username))
	}
	warnings = append(warnings, notes...)

	facts := append([]string{confirmFact("user", username)}, changes...)
	return &writePlan{
		Tool:         "update_user",
		Method:       userUpdateMethod,
		CallParams:   []interface{}{u.ID, payload},
		ParamNames:   []string{"id", "user_update"},
		TargetName:   username,
		CurrentState: currentView,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Change local user '%s'", username),
			Operation:   "update",
			Target:      username,
			Details:     map[string]interface{}{"changes": changes},
		}},
		Warnings:       warnings,
		Blocked:        blocked,
		Bind:           bind,
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Change user %s", username), facts...),
		Result:         map[string]interface{}{"login_mode": mode, "group_names": groupNames, "changes": changes},
	}, nil
}

func sameInts(a, b []int) bool {
	x := append([]int{}, a...)
	y := append([]int{}, b...)
	sort.Ints(x)
	sort.Ints(y)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func namesOf(ids []int, names map[int]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := names[id]; ok {
			out = append(out, n)
		} else {
			out = append(out, fmt.Sprintf("id %d", id))
		}
	}
	sort.Strings(out)
	return out
}

func (r *Registry) handleUpdateUser(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planUpdateUser(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeUpdateUser(ctx, client, args, plan)
}

func (r *Registry) executeUpdateUser(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	mode, _ := plan.Result["login_mode"].(string)
	pw, err := executionPassword(mode, args)
	if err != nil {
		return "", err
	}
	raw, err := callPlan(ctx, client, withPassword(plan, 1, pw))
	if err != nil {
		return "", accountCallError("update user "+plan.TargetName, err, pw)
	}
	names, _ := plan.Result["group_names"].(map[int]string)
	response := map[string]interface{}{
		"updated":  true,
		"username": plan.TargetName,
		"changes":  plan.Result["changes"],
	}
	var row map[string]interface{}
	if json.Unmarshal(raw, &row) == nil && row != nil {
		response["user"] = simplifyUser(row, names)
	} else {
		response["note"] = "The change was sent, but user.update returned no user entry; check query_users"
	}
	msg := fmt.Sprintf("Updated user %s", plan.TargetName)
	if mode == passwordRandom {
		r.oneTimeSecrets.add(pw)
		response[mcp.GeneratedPasswordKey] = pw
		msg += ". Its new random password is in generated_password and is shown only this once: store it now"
	}
	response["message"] = msg
	return marshalJSON(response)
}

// ---- create_group ----

func planCreateGroup(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	name, _ := trimmedArg(args, "name")
	if err := validateAccountName("name", name); err != nil {
		return nil, err
	}
	if privilegedGroupNames[name] {
		return nil, fmt.Errorf("group name '%s' is an administrative group name; these tools never create it; nothing was created", name)
	}
	gid, hasGID := intID(args["gid"])
	if hasGID && gid == nobodyID {
		return nil, fmt.Errorf("gid %d belongs to nogroup; choose another gid or omit it", gid)
	}
	smb, _ := args["smb"].(bool)
	members, err := nameListArg(args, "members", "user")
	if err != nil {
		return nil, err
	}
	apiUser := client.APIUser()

	var blocked []string
	existing, err := groupsByName(ctx, client, []string{name})
	if err != nil {
		return nil, err
	}
	if _, ok := existing[name]; ok {
		blocked = append(blocked, fmt.Sprintf("group '%s' already exists; use update_group to change it", name))
	}
	if hasGID {
		taken, err := groupGIDTaken(ctx, client, gid)
		if err != nil {
			return nil, err
		}
		if taken {
			blocked = append(blocked, fmt.Sprintf("gid %d is already used by another local group; choose another or omit gid", gid))
		}
	}
	users, memberGroups, err := accountMembers(ctx, client, members)
	if err != nil {
		return nil, err
	}
	ids := []interface{}{}
	userBinds := map[string]interface{}{}
	usernames := map[int]string{}
	for _, m := range members {
		u := users[m]
		if why := userChangeRefusal(u, apiUser, memberGroups); why != "" {
			blocked = append(blocked, why)
		}
		ids = append(ids, u.ID)
		userBinds[m] = u.bind()
		usernames[u.ID] = m
	}
	payload := map[string]interface{}{
		"name":                   name,
		"smb":                    smb,
		"users":                  ids,
		"sudo_commands":          []interface{}{},
		"sudo_commands_nopasswd": []interface{}{},
	}
	gidText := "next free gid"
	if hasGID {
		payload["gid"] = gid
		gidText = fmt.Sprint(gid)
	}
	return &writePlan{
		Tool:         "create_group",
		Method:       groupCreateMethod,
		CallParams:   []interface{}{payload},
		TargetName:   name,
		CurrentState: map[string]interface{}{"name": name, "exists": len(existing) > 0},
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Create local group '%s'", name),
			Operation:   "create",
			Target:      name,
			Details: map[string]interface{}{
				"name": name, "gid": gidText, "smb": smb, "members": members,
				"sudo": "none", "roles": "none",
			},
		}},
		Blocked: blocked,
		Bind:    map[string]interface{}{"name_free": len(existing) == 0, "members": userBinds},
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Create group %s", name),
			confirmFact("group", name),
			confirmFact("gid", gidText),
			confirmFact("smb", fmt.Sprint(smb)),
			confirmFact("members", strings.Join(members, ", ")),
		),
		Result: map[string]interface{}{"usernames": usernames},
	}, nil
}

// accountMembers looks up the named local users and every group they
// belong to. A name that is not a local user is an error.
func accountMembers(ctx context.Context, client *truenas.Client, names []string) (map[string]userInfo, map[int]groupInfo, error) {
	users, err := usersByName(ctx, client, names)
	if err != nil {
		return nil, nil, err
	}
	var missing []string
	var list []userInfo
	for _, n := range names {
		u, ok := users[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		list = append(list, u)
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("user(s) not found as local users: %s", strings.Join(missing, ", "))
	}
	groups, err := groupsByID(ctx, client, userGroupIDs(list...))
	if err != nil {
		return nil, nil, err
	}
	return users, groups, nil
}

func (r *Registry) handleCreateGroup(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planCreateGroup(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeCreateGroup(ctx, client, args, plan)
}

func (r *Registry) executeCreateGroup(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	raw, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to create group %s: %w", plan.TargetName, err)
	}
	id, ok := 0, false
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		id, ok = int(n), true
	}
	usernames, _ := plan.Result["usernames"].(map[int]string)
	return groupWriteResult(ctx, client, map[string]interface{}{"created": true}, plan.TargetName, id, ok, usernames, fmt.Sprintf("Created group %s", plan.TargetName))
}

// groupWriteResult reads the group back by id and builds the response
// from fields and the group.
func groupWriteResult(ctx context.Context, client *truenas.Client, fields map[string]interface{}, name string, id int, idOK bool, usernames map[int]string, msg string) (string, error) {
	response := map[string]interface{}{"name": name}
	for k, v := range fields {
		response[k] = v
	}
	if usernames == nil {
		usernames = map[int]string{}
	}
	if !idOK {
		response["note"] = fmt.Sprintf("TrueNAS returned no group id; check query_groups for '%s'", name)
		response["message"] = msg
		return marshalJSON(response)
	}
	response["id"] = id
	groups, err := groupsByID(ctx, client, []int{id})
	switch {
	case err != nil:
		response["note"] = "The change was made, but the group could not be read back: " + boundedErr(err)
	case len(groups) == 0:
		response["note"] = fmt.Sprintf("The change was made, but group id %d was not found when read back", id)
	default:
		g := groups[id]
		var unknown []int
		for _, uid := range g.Users {
			if _, ok := usernames[uid]; !ok {
				unknown = append(unknown, uid)
			}
		}
		if len(unknown) > 0 {
			if more, err := usersByID(ctx, client, unknown); err == nil {
				for uid, u := range more {
					usernames[uid] = u.Username
				}
			}
		}
		response["group"] = simplifyGroup(g.Row, usernames)
	}
	response["message"] = msg
	return marshalJSON(response)
}

// ---- update_group ----

func planUpdateGroup(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	name, _ := trimmedArg(args, "name")
	if err := validateAccountName("name", name); err != nil {
		return nil, err
	}
	if privilegedGroupNames[name] {
		return nil, fmt.Errorf("group '%s' is an administrative group; these tools never change it; nothing was changed", name)
	}
	addMembers, err := nameListArg(args, "add_members", "user")
	if err != nil {
		return nil, err
	}
	removeMembers, err := nameListArg(args, "remove_members", "user")
	if err != nil {
		return nil, err
	}
	for _, a := range addMembers {
		for _, b := range removeMembers {
			if a == b {
				return nil, fmt.Errorf("user '%s' is in both add_members and remove_members", a)
			}
		}
	}
	apiUser := client.APIUser()

	groups, err := groupsByName(ctx, client, []string{name})
	if err != nil {
		return nil, err
	}
	g, ok := groups[name]
	if !ok {
		return nil, fmt.Errorf("local group '%s' not found (directory service groups must be changed in the directory)", name)
	}
	var blocked []string
	if why := groupChangeRefusal(g); why != "" {
		blocked = append(blocked, why)
	}
	currentMembers, err := usersByID(ctx, client, g.Users)
	if err != nil {
		return nil, err
	}
	usernames := map[int]string{}
	for id, u := range currentMembers {
		usernames[id] = u.Username
	}
	named := append(append([]string{}, addMembers...), removeMembers...)
	users, memberGroups, err := accountMembers(ctx, client, named)
	if err != nil {
		return nil, err
	}
	owners, err := primaryGroupOwners(ctx, client, []int{g.ID})
	if err != nil {
		return nil, err
	}

	member := map[int]bool{}
	for _, id := range g.Users {
		member[id] = true
	}
	newUsers := append([]int{}, g.Users...)
	var changes, notes []string
	userBinds := map[string]interface{}{}
	for _, n := range addMembers {
		u := users[n]
		usernames[u.ID] = n
		userBinds[n] = u.bind()
		if why := userChangeRefusal(u, apiUser, memberGroups); why != "" {
			blocked = append(blocked, why)
		}
		if member[u.ID] {
			notes = append(notes, fmt.Sprintf("'%s' is already a member", n))
			continue
		}
		if why := personalGroupRefusal(g, owners[g.ID], n); why != "" {
			blocked = append(blocked, why)
		}
		newUsers = append(newUsers, u.ID)
		member[u.ID] = true
		changes = append(changes, "add member: "+n)
	}
	for _, n := range removeMembers {
		u := users[n]
		usernames[u.ID] = n
		userBinds[n] = u.bind()
		if why := userChangeRefusal(u, apiUser, memberGroups); why != "" {
			blocked = append(blocked, why)
		}
		if !member[u.ID] {
			notes = append(notes, fmt.Sprintf("'%s' is not a member", n))
			continue
		}
		kept := newUsers[:0]
		for _, id := range newUsers {
			if id != u.ID {
				kept = append(kept, id)
			}
		}
		newUsers = kept
		delete(member, u.ID)
		changes = append(changes, "remove member: "+n)
	}
	payload := map[string]interface{}{}
	if v, ok := args["smb"].(bool); ok && v != g.SMB {
		payload["smb"] = v
		changes = append(changes, fmt.Sprintf("smb: %v -> %v", g.SMB, v))
	}
	if !sameInts(newUsers, g.Users) {
		payload["users"] = intsAsArgs(newUsers)
	}
	if len(changes) == 0 && len(blocked) == 0 {
		msg := fmt.Sprintf("nothing to change: group '%s' already has these settings", name)
		if len(notes) > 0 {
			msg += " (" + strings.Join(notes, "; ") + ")"
		}
		blocked = append(blocked, msg)
	}

	facts := []string{confirmFact("group", name)}
	facts = append(facts, changes...)
	return &writePlan{
		Tool:       "update_group",
		Method:     groupUpdateMethod,
		CallParams: []interface{}{g.ID, payload},
		ParamNames: []string{"id", "group_update"},
		TargetName: name,
		CurrentState: map[string]interface{}{
			"name": g.Name, "gid": g.GID, "smb": g.SMB, "builtin": g.Builtin,
			"members": namesOf(g.Users, usernames), "member_count": len(g.Users),
			"roles": g.Roles, "sudo": g.Sudo,
		},
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Change local group '%s'", name),
			Operation:   "update",
			Target:      name,
			Details:     map[string]interface{}{"changes": changes, "members_after": namesOf(newUsers, usernames)},
		}},
		Warnings:       notes,
		Blocked:        blocked,
		Bind:           map[string]interface{}{"group": g.bind(), "members": userBinds, "primary_group_owners": owners[g.ID]},
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Change group %s", name), facts...),
		Result:         map[string]interface{}{"usernames": usernames, "changes": changes},
	}, nil
}

func (r *Registry) handleUpdateGroup(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planUpdateGroup(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeUpdateGroup(ctx, client, args, plan)
}

func (r *Registry) executeUpdateGroup(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if _, err := callPlan(ctx, client, plan); err != nil {
		return "", fmt.Errorf("failed to update group %s: %w", plan.TargetName, err)
	}
	id, ok := intID(plan.CallParams[0])
	usernames, _ := plan.Result["usernames"].(map[int]string)
	fields := map[string]interface{}{"updated": true, "changes": plan.Result["changes"]}
	return groupWriteResult(ctx, client, fields, plan.TargetName, id, ok, usernames, fmt.Sprintf("Updated group %s", plan.TargetName))
}

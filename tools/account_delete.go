package tools

// Local account delete tools (delete_user, delete_group). They follow the
// refusal rules of the account write tools (tools/account_actions.go) and
// never cascade: user.delete is sent with delete_group false, so the
// primary group is kept, and group.delete with delete_users false. Home
// directories are never deleted; because user.delete always removes
// <home>/.ssh, a user whose home directory holds one is refused.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	userDeleteMethod  = "user.delete"
	groupDeleteMethod = "group.delete"
)

func (r *Registry) registerAccountDeleteTools() {
	name := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 32, "description": desc}
	}
	dryRun := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc, "default": false}
	}

	r.tools["delete_user"] = Tool{
		Definition: mcp.Tool{
			Name:        "delete_user",
			Description: "Delete one local user account with user.delete. Nothing else is deleted: the user's primary group is kept (delete_group false is sent; delete it afterwards with delete_group if it is no longer needed), the home directory and every file the user owns are kept, and files keep the user's numeric uid as owner. Refused for root, admin, truenas_admin and other built-in accounts, uid 0 and the built-in administrator uid, the account this server signs in as, directory service users, users that have TrueNAS roles or sudo or belong to an administrative group (such as builtin_administrators or wheel), the SMB guest account, and users whose home directory contains a .ssh directory (user.delete always removes <home>/.ssh, so move it away first). The preview shows the user, its groups, what is kept and what is removed (group memberships, the SMB password entry); the confirmation is bound to the user's current settings, so a change to the user after the preview needs a new one. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"username": name("Required: Name of the local user to delete"),
					"dry_run":  dryRun("Optional: Preview without deleting the user (default: false)"),
				},
				"required": []string{"username"},
			},
		},
		Handler: r.handleDeleteUser,
		Plan:    planDeleteUser,
		Execute: r.executeDeleteUser,
	}

	r.tools["delete_group"] = Tool{
		Definition: mcp.Tool{
			Name:        "delete_group",
			Description: "Delete one local group with group.delete. No user is deleted or changed except that the group's members lose their membership (delete_users false is sent). Files keep the group's numeric gid as their group, and filesystem ACL entries that name the gid stay. Refused for groups that are still the primary group of any user (such as the group created with a user; delete the user first, or give it another primary group with update_user), built-in groups, administrative groups (such as builtin_administrators, wheel or root), groups with gid 0, groups with TrueNAS roles or sudo, directory service groups, and groups the account this server signs in as belongs to. The preview shows the group and its members; the confirmation is bound to the group's current settings and members, so a change after the preview needs a new one. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":    name("Required: Name of the local group to delete"),
					"dry_run": dryRun("Optional: Preview without deleting the group (default: false)"),
				},
				"required": []string{"name"},
			},
		},
		Handler: r.handleDeleteGroup,
		Plan:    planDeleteGroup,
		Execute: r.executeDeleteGroup,
	}
}

// smbGuestAccount returns the SMB service's guest account. user.delete
// resets it to nobody when it names the deleted user, which changes the
// SMB service settings, so an unreadable value fails the plan.
func smbGuestAccount(ctx context.Context, client *truenas.Client) (string, error) {
	raw, err := client.Call(ctx, "smb.config")
	if err != nil {
		return "", fmt.Errorf("could not read the SMB guest account (smb.config failed: %w), so the delete cannot be checked; nothing was changed", err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg == nil {
		return "", fmt.Errorf("could not read the SMB guest account (smb.config returned an unexpected result); nothing was changed")
	}
	guest, ok := cfg["guest"].(string)
	if !ok {
		return "", fmt.Errorf("could not read the SMB guest account (smb.config reported no guest); nothing was changed")
	}
	return guest, nil
}

// groupDeleteRefusal says why g may not be deleted by these tools, or "".
// It applies the rules of groupChangeRefusal, plus the built-in
// administrator gid.
func groupDeleteRefusal(g groupInfo) string {
	switch {
	case !g.Local:
		return fmt.Sprintf("group '%s' is provided by a directory service; delete it there", g.Name)
	case g.GID == adminUID:
		return fmt.Sprintf("group '%s' has the built-in administrator gid %d; these tools never delete it", g.Name, adminUID)
	}
	if why := privilegedGroupReason(g); why != "" {
		return fmt.Sprintf("group '%s' %s; these tools never delete it (privileged groups are managed in the TrueNAS web UI)", g.Name, why)
	}
	if g.Builtin || g.Immutable {
		return fmt.Sprintf("group '%s' is a built-in group; these tools never delete it", g.Name)
	}
	return ""
}

// hasHomeDir reports whether home is a home directory rather than the
// "no home" placeholder.
func hasHomeDir(home string) bool {
	return home != "" && home != defaultHomePath
}

// ---- delete_user ----

func planDeleteUser(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	username, _ := trimmedArg(args, "username")
	if err := validateAccountName("username", username); err != nil {
		return nil, err
	}
	apiUser := client.APIUser()
	if why := protectedUserRefusal(username, apiUser); why != "" {
		return nil, fmt.Errorf("%s; nothing was deleted", why)
	}
	users, err := usersByName(ctx, client, []string{username})
	if err != nil {
		return nil, err
	}
	u, ok := users[username]
	if !ok {
		return nil, fmt.Errorf("local user '%s' not found (directory service users must be deleted in the directory)", username)
	}
	groups, err := groupsByID(ctx, client, userGroupIDs(u))
	if err != nil {
		return nil, err
	}
	guest, err := smbGuestAccount(ctx, client)
	if err != nil {
		return nil, err
	}

	var blocked []string
	if why := userChangeRefusal(u, apiUser, groups); why != "" {
		blocked = append(blocked, why)
	}
	if guest == u.Username {
		blocked = append(blocked, fmt.Sprintf("user '%s' is the SMB guest account; TrueNAS would reset the SMB service's guest account to nobody, so choose another guest account in the SMB service settings first", u.Username))
	}
	bind := map[string]interface{}{"user": u.bind(), "smb_guest": guest}
	sshDir := ""
	if hasHomeDir(u.Home) {
		sshDir = u.Home + "/.ssh"
		_, exists, err := statPath(ctx, client, sshDir)
		if err != nil {
			return nil, err
		}
		bind["ssh_dir_exists"] = exists
		if exists {
			blocked = append(blocked, fmt.Sprintf("'%s' exists, and user.delete always removes the .ssh directory of the user's home directory (SSH keys included); these tools never delete files, so move it out of the home directory first", sshDir))
		}
	}
	groupBinds := map[string]interface{}{}
	groupNames := map[int]string{}
	for id, g := range groups {
		groupBinds[fmt.Sprint(id)] = g.bind()
		groupNames[id] = g.Name
	}
	bind["groups"] = groupBinds

	primary := groupNames[u.PrimaryGroupID]
	if primary == "" {
		primary = fmt.Sprintf("id %d", u.PrimaryGroupID)
	}
	secondary := namesOf(u.Groups, groupNames)
	homeText := "none"
	if hasHomeDir(u.Home) {
		homeText = u.Home + " (kept)"
	}
	current := map[string]interface{}{
		"username": u.Username, "uid": u.UID, "full_name": u.FullName,
		"home": u.Home, "shell": u.Shell, "smb": u.SMB, "locked": u.Locked,
		"password_disabled": u.PasswordDisabled, "primary_group": primary,
		"groups": secondary, "builtin": u.Builtin, "roles": u.Roles, "sudo": u.Sudo,
	}
	kept := []string{fmt.Sprintf("primary group '%s' (delete_group is false)", primary)}
	if hasHomeDir(u.Home) {
		kept = append(kept, fmt.Sprintf("home directory %s and its files, except %s, which TrueNAS always removes", u.Home, sshDir))
	}
	kept = append(kept, fmt.Sprintf("every file owned by uid %d, which keeps that numeric owner", u.UID))
	removed := []string{"the user account"}
	if len(secondary) > 0 {
		removed = append(removed, "membership of "+strings.Join(secondary, ", "))
	}
	if u.SMB {
		removed = append(removed, "the user's SMB password entry")
	}

	warnings := []string{
		fmt.Sprintf("Files and directories owned by '%s' keep uid %d as their owner, and filesystem ACL entries for uid %d stay; a user created later with uid %d gains that access", u.Username, u.UID, u.UID, u.UID),
		"Settings that name the user (NFS share user mappings, replication or rsync tasks, cron jobs, app configuration) are not changed and stop resolving to an account",
	}
	if u.SMB {
		warnings = append(warnings, fmt.Sprintf("SMB share ACL entries for the user's SID stop matching anyone, and '%s' can no longer connect over SMB", u.Username))
	}
	if g, ok := groups[u.PrimaryGroupID]; ok && g.Name == u.Username {
		warnings = append(warnings, fmt.Sprintf("Group '%s' is kept; once no user has it as primary group it can be deleted with delete_group", g.Name))
	}
	warnings = append(warnings, "TrueNAS reloads the SSH and user services to apply the change")

	return &writePlan{
		Tool:         "delete_user",
		Method:       userDeleteMethod,
		CallParams:   []interface{}{u.ID, map[string]interface{}{"delete_group": false}},
		ParamNames:   []string{"id", "options"},
		TargetName:   u.Username,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Delete local user '%s' (uid %d); its primary group and home directory are kept", u.Username, u.UID),
			Operation:   "delete",
			Target:      u.Username,
			Details: map[string]interface{}{
				"id": u.ID, "uid": u.UID, "removed": removed, "kept": kept,
				"delete_group": false, "home_deleted": false,
			},
		}},
		Warnings: warnings,
		Blocked:  blocked,
		Bind:     bind,
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Delete user %s; primary group and home directory are kept", u.Username),
			confirmFact("user", u.Username),
			confirmFact("uid", u.UID),
			confirmFact("primary group (kept)", primary),
			confirmFact("home", homeText),
			confirmFact("groups left", strings.Join(secondary, ", ")),
			confirmFact("smb", u.SMB),
		),
		Result: map[string]interface{}{"uid": u.UID, "home": u.Home, "primary_group": primary},
	}, nil
}

func (r *Registry) handleDeleteUser(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planDeleteUser(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeDeleteUser(ctx, client, args, plan)
}

func (r *Registry) executeDeleteUser(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	raw, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to delete user %s: %w; check query_users before retrying", plan.TargetName, err)
	}
	var id float64
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", fmt.Errorf("user.delete returned an unexpected result; check whether user %s still exists with query_users before retrying", plan.TargetName)
	}
	home, _ := plan.Result["home"].(string)
	response := map[string]interface{}{
		"deleted": true, "username": plan.TargetName, "uid": plan.Result["uid"],
		"primary_group_kept": plan.Result["primary_group"], "home_deleted": false,
	}
	msg := fmt.Sprintf("Deleted user %s; its primary group '%v' is kept", plan.TargetName, plan.Result["primary_group"])
	if hasHomeDir(home) {
		response["home_kept"] = home
		msg += fmt.Sprintf(" and its home directory %s is kept", home)
	}
	users, err := usersByName(ctx, client, []string{plan.TargetName})
	switch {
	case err != nil:
		response["note"] = "The delete finished, but the users could not be read back: " + boundedErr(err)
	case len(users) > 0:
		response["note"] = fmt.Sprintf("TrueNAS reported success, but user %s is still listed; check query_users", plan.TargetName)
	}
	response["message"] = msg
	return marshalJSON(response)
}

// ---- delete_group ----

func planDeleteGroup(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	name, _ := trimmedArg(args, "name")
	if err := validateAccountName("name", name); err != nil {
		return nil, err
	}
	if privilegedGroupNames[name] {
		return nil, fmt.Errorf("group '%s' is an administrative group; these tools never delete it; nothing was deleted", name)
	}
	apiUser := client.APIUser()
	groups, err := groupsByName(ctx, client, []string{name})
	if err != nil {
		return nil, err
	}
	g, ok := groups[name]
	if !ok {
		return nil, fmt.Errorf("local group '%s' not found (directory service groups must be deleted in the directory)", name)
	}
	owners, err := primaryGroupOwners(ctx, client, []int{g.ID})
	if err != nil {
		return nil, err
	}
	members, err := usersByID(ctx, client, g.Users)
	if err != nil {
		return nil, err
	}
	usernames := map[int]string{}
	memberBinds := map[string]interface{}{}
	for id, u := range members {
		usernames[id] = u.Username
		memberBinds[u.Username] = u.bind()
	}
	memberNames := namesOf(g.Users, usernames)

	var blocked []string
	if why := groupDeleteRefusal(g); why != "" {
		blocked = append(blocked, why)
	}
	if o := owners[g.ID]; len(o) > 0 {
		blocked = append(blocked, fmt.Sprintf("group '%s' is the primary group of user(s) %s; TrueNAS would move them to nogroup, so delete those users first or give them another primary group with update_user", g.Name, strings.Join(o, ", ")))
	}
	if apiUser != "" {
		for _, u := range members {
			if u.Username == apiUser {
				blocked = append(blocked, fmt.Sprintf("the account this server signs in as ('%s') is a member of group '%s'; change it in the TrueNAS web UI", apiUser, g.Name))
			}
		}
	}

	warnings := []string{
		fmt.Sprintf("Files and directories with group '%s' keep gid %d, and filesystem ACL entries for gid %d stay; a group created later with gid %d gains that access", g.Name, g.GID, g.GID, g.GID),
		"Settings that name the group (NFS share group mappings, app configuration) are not changed and stop resolving to a group",
	}
	if len(memberNames) > 0 {
		warnings = append(warnings, fmt.Sprintf("Members lose the group and the access it grants: %s", strings.Join(memberNames, ", ")))
	}
	if g.SMB {
		warnings = append(warnings, "TrueNAS removes the group's SMB group mapping: SMB share ACL entries for its SID stop matching anyone")
	}
	return &writePlan{
		Tool:       "delete_group",
		Method:     groupDeleteMethod,
		CallParams: []interface{}{g.ID, map[string]interface{}{"delete_users": false}},
		ParamNames: []string{"id", "options"},
		TargetName: g.Name,
		CurrentState: map[string]interface{}{
			"name": g.Name, "gid": g.GID, "smb": g.SMB, "builtin": g.Builtin,
			"members": memberNames, "member_count": len(g.Users),
			"primary_group_of": owners[g.ID], "roles": g.Roles, "sudo": g.Sudo,
		},
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Delete local group '%s' (gid %d); no user is deleted", g.Name, g.GID),
			Operation:   "delete",
			Target:      g.Name,
			Details: map[string]interface{}{
				"id": g.ID, "gid": g.GID, "members_removed": memberNames, "delete_users": false,
			},
		}},
		Warnings: warnings,
		Blocked:  blocked,
		Bind:     map[string]interface{}{"group": g.bind(), "members": memberBinds, "primary_group_owners": owners[g.ID]},
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Delete group %s; no user is deleted", g.Name),
			confirmFact("group", g.Name),
			confirmFact("gid", g.GID),
			confirmFact("members losing the group", strings.Join(memberNames, ", ")),
			confirmFact("smb", g.SMB),
		),
		Result: map[string]interface{}{"gid": g.GID},
	}, nil
}

func (r *Registry) handleDeleteGroup(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planDeleteGroup(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeDeleteGroup(ctx, client, args, plan)
}

func (r *Registry) executeDeleteGroup(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	raw, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to delete group %s: %w; check query_groups before retrying", plan.TargetName, err)
	}
	var id float64
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", fmt.Errorf("group.delete returned an unexpected result; check whether group %s still exists with query_groups before retrying", plan.TargetName)
	}
	response := map[string]interface{}{
		"deleted": true, "name": plan.TargetName, "gid": plan.Result["gid"], "users_deleted": false,
		"message": fmt.Sprintf("Deleted group %s; no user was deleted", plan.TargetName),
	}
	groups, err := groupsByName(ctx, client, []string{plan.TargetName})
	switch {
	case err != nil:
		response["note"] = "The delete finished, but the groups could not be read back: " + boundedErr(err)
	case len(groups) > 0:
		response["note"] = fmt.Sprintf("TrueNAS reported success, but group %s is still listed; check query_groups", plan.TargetName)
	}
	return marshalJSON(response)
}

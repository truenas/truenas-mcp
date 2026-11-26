package tools

// Local account query tools and their handlers (query_users,
// query_groups). Both are read only. Output is built field by field from
// an allow list, so password hashes, password history, 2FA secrets, SSH
// keys and API key ids never reach the client even if the middleware
// returns them.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// maxGroupMembersListed caps the member names listed per group;
// builtin_users can hold every SMB user.
const maxGroupMembersListed = 100

func (r *Registry) registerAccountTools() {
	r.tools["query_users"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_users",
			Description: "Query user accounts. Returns username, uid, full name, builtin/immutable/locked flags, whether password login is disabled, whether SSH password login is enabled, primary and secondary groups, home, shell, SMB, 2FA and sudo flags, and roles. Never returns password hashes, 2FA secrets, SSH keys or API keys. Local accounts only unless 'include_directory_users' is true. Use 'limit' to control result size (default: 50, maximum: 1000).",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"username": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Return only the user with this exact username",
					},
					"builtin": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: true for built-in system accounts only, false for accounts created by an administrator only (default: both)",
					},
					"locked": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Filter by locked state (default: both)",
					},
					"include_directory_users": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Also return Active Directory / LDAP users when a directory service is joined (default: false)",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of users to return (default: 50, maximum: 1000)",
						"minimum":     1,
					},
				},
			},
		},
		Handler: handleQueryUsers,
	}

	r.tools["query_groups"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_groups",
			Description: "Query groups. Returns name, gid, builtin and SMB flags, sudo flag, roles, and members by username (at most 100 listed per group; member_count is the full count). Local groups only unless 'include_directory_groups' is true. Use 'limit' to control result size (default: 50, maximum: 1000).",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Return only the group with this exact name",
					},
					"builtin": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: true for built-in groups only, false for groups created by an administrator only (default: both)",
					},
					"include_directory_groups": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Also return Active Directory / LDAP groups when a directory service is joined (default: false)",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of groups to return (default: 50, maximum: 1000)",
						"minimum":     1,
					},
				},
			},
		},
		Handler: handleQueryGroups,
	}
}

// userQuerySelect is every user.query field simplifyUser reads. unixhash,
// smbhash, password_history, sshpubkey and api_keys are deliberately not
// requested.
var userQuerySelect = []interface{}{
	"id", "uid", "username", "full_name", "builtin", "immutable", "local", "locked",
	"password_disabled", "group", "groups", "home", "shell", "smb",
	"twofactor_auth_configured", "sudo_commands", "sudo_commands_nopasswd", "roles",
	"ssh_password_enabled",
}

var groupQuerySelect = []interface{}{
	"id", "gid", "name", "builtin", "immutable", "local", "smb", "users", "roles",
	"sudo_commands", "sudo_commands_nopasswd",
}

func handleQueryUsers(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	filters := []interface{}{}
	includeDS, _ := args["include_directory_users"].(bool)
	if !includeDS {
		filters = append(filters, []interface{}{"local", "=", true})
	}
	if username, ok := args["username"].(string); ok && username != "" {
		filters = append(filters, []interface{}{"username", "=", username})
	}
	if builtin, ok := args["builtin"].(bool); ok {
		filters = append(filters, []interface{}{"builtin", "=", builtin})
	}
	if locked, ok := args["locked"].(bool); ok {
		filters = append(filters, []interface{}{"locked", "=", locked})
	}
	limit, capped := queryLimit(args)

	raw, err := client.Call(ctx, "user.query", filters, map[string]interface{}{
		"select":   userQuerySelect,
		"order_by": []string{"uid"},
		"limit":    limit,
	})
	if err != nil {
		return "", err
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", fmt.Errorf("failed to parse users: %w", err)
	}

	var groupIDs []int
	for _, row := range rows {
		groupIDs = append(groupIDs, intList(row["groups"])...)
	}
	var groupNames map[int]string
	var groupErr error
	if in := uniqueIDs(groupIDs); len(in) > 0 {
		raw, err := client.Call(ctx, "group.query", lookupFilters(in, includeDS),
			map[string]interface{}{"select": []interface{}{"id", "name"}})
		groupNames, groupErr = namesByID(raw, err, "name")
	}

	users := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		users = append(users, simplifyUser(row, groupNames))
	}

	response := map[string]interface{}{
		"users":      users,
		"user_count": len(users),
	}
	var note string
	total, known := len(rows), true
	if len(rows) >= limit {
		total, known = queryCount(ctx, client, "user.query", filters)
	}
	if known {
		response["total_users"] = total
		if len(users) < total {
			note = fmt.Sprintf("Showing %d of %d users (limited)", len(users), total)
		}
	} else {
		note = "User total is unknown; the count query failed"
	}
	if groupErr != nil {
		note = joinNote(note, "secondary group names could not be looked up; see unresolved_group_ids")
	}
	if capped {
		note = joinNote(note, "limit was capped to 1000")
	}
	if note != "" {
		response["note"] = note
	}
	return marshalJSON(response)
}

func handleQueryGroups(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	filters := []interface{}{}
	includeDS, _ := args["include_directory_groups"].(bool)
	if !includeDS {
		filters = append(filters, []interface{}{"local", "=", true})
	}
	if name, ok := args["name"].(string); ok && name != "" {
		filters = append(filters, []interface{}{"name", "=", name})
	}
	if builtin, ok := args["builtin"].(bool); ok {
		filters = append(filters, []interface{}{"builtin", "=", builtin})
	}
	limit, capped := queryLimit(args)

	raw, err := client.Call(ctx, "group.query", filters, map[string]interface{}{
		"select":   groupQuerySelect,
		"order_by": []string{"gid"},
		"limit":    limit,
	})
	if err != nil {
		return "", err
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", fmt.Errorf("failed to parse groups: %w", err)
	}

	// Look up only the member ids each group will list.
	var userIDs []int
	for _, row := range rows {
		userIDs = append(userIDs, listedIDs(intList(row["users"]), maxGroupMembersListed)...)
	}
	var usernames map[int]string
	var userErr error
	if in := uniqueIDs(userIDs); len(in) > 0 {
		raw, err := client.Call(ctx, "user.query", lookupFilters(in, includeDS),
			map[string]interface{}{"select": []interface{}{"id", "username"}})
		usernames, userErr = namesByID(raw, err, "username")
	}

	groups := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		groups = append(groups, simplifyGroup(row, usernames))
	}

	response := map[string]interface{}{
		"groups":      groups,
		"group_count": len(groups),
	}
	var note string
	total, known := len(rows), true
	if len(rows) >= limit {
		total, known = queryCount(ctx, client, "group.query", filters)
	}
	if known {
		response["total_groups"] = total
		if len(groups) < total {
			note = fmt.Sprintf("Showing %d of %d groups (limited)", len(groups), total)
		}
	} else {
		note = "Group total is unknown; the count query failed"
	}
	if userErr != nil {
		note = joinNote(note, "member usernames could not be looked up; see unresolved_member_ids")
	}
	if capped {
		note = joinNote(note, "limit was capped to 1000")
	}
	if note != "" {
		response["note"] = note
	}
	return marshalJSON(response)
}

// lookupFilters is the filter list for an id-to-name lookup. Unless
// directory accounts were asked for, it stays local so the lookup skips
// the directory cache.
func lookupFilters(in []interface{}, includeDS bool) []interface{} {
	filters := []interface{}{[]interface{}{"id", "in", in}}
	if !includeDS {
		filters = append(filters, []interface{}{"local", "=", true})
	}
	return filters
}

// uniqueIDs returns ids without duplicates, in first-seen order, ready to
// use as an "in" filter value.
func uniqueIDs(ids []int) []interface{} {
	seen := map[int]bool{}
	in := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			in = append(in, id)
		}
	}
	return in
}

// namesByID maps account entry ids (not uids or gids) from a select
// [id, field] query result to the field's value.
func namesByID(raw json.RawMessage, callErr error, field string) (map[int]string, error) {
	names := map[int]string{}
	if callErr != nil {
		return names, callErr
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return names, err
	}
	for _, row := range rows {
		id, okID := row["id"].(float64)
		name, okName := row[field].(string)
		if okID && okName && name != "" {
			names[int(id)] = name
		}
	}
	return names, nil
}

func simplifyUser(row map[string]interface{}, groupNames map[int]string) map[string]interface{} {
	out := map[string]interface{}{}
	copyString(out, row, "username", "username")
	copyString(out, row, "full_name", "full_name")
	copyString(out, row, "home", "home")
	copyString(out, row, "shell", "shell")
	copyInt(out, row, "id", "id")
	copyInt(out, row, "uid", "uid")
	for _, k := range []string{"builtin", "immutable", "local", "locked", "password_disabled", "smb"} {
		copyBool(out, row, k, k)
	}
	copyBool(out, row, "twofactor_auth_configured", "two_factor_enabled")
	copyBool(out, row, "ssh_password_enabled", "ssh_password_enabled")
	addSudoFlags(out, row)
	copyRoles(out, row)

	if g, ok := row["group"].(map[string]interface{}); ok {
		if name := firstStringOf(g, "bsdgrp_group", "name", "group"); name != "" {
			out["primary_group"] = name
		}
		for _, k := range []string{"bsdgrp_gid", "gid"} {
			if gid, ok := g[k].(float64); ok {
				out["primary_gid"] = int(gid)
				break
			}
		}
	}

	names, unresolved := resolveNames(intList(row["groups"]), groupNames, 0)
	out["groups"] = names
	if len(unresolved) > 0 {
		out["unresolved_group_ids"] = unresolved
	}
	return out
}

func simplifyGroup(row map[string]interface{}, usernames map[int]string) map[string]interface{} {
	out := map[string]interface{}{}
	copyString(out, row, "name", "name")
	copyInt(out, row, "id", "id")
	copyInt(out, row, "gid", "gid")
	for _, k := range []string{"builtin", "immutable", "local", "smb"} {
		copyBool(out, row, k, k)
	}
	addSudoFlags(out, row)
	copyRoles(out, row)

	ids := intList(row["users"])
	out["member_count"] = len(ids)
	names, unresolved := resolveNames(ids, usernames, maxGroupMembersListed)
	out["members"] = names
	if len(unresolved) > 0 {
		out["unresolved_member_ids"] = unresolved
	}
	if len(names)+len(unresolved) < len(ids) {
		out["members_truncated"] = true
	}
	return out
}

// addSudoFlags reports whether any sudo commands are configured, and
// whether any run without a password prompt, without listing the commands.
func addSudoFlags(out, row map[string]interface{}) {
	_, hasSudo := row["sudo_commands"]
	_, hasNoPasswd := row["sudo_commands_nopasswd"]
	if !hasSudo && !hasNoPasswd {
		return
	}
	withPrompt := len(stringList(row["sudo_commands"]))
	noPrompt := len(stringList(row["sudo_commands_nopasswd"]))
	out["sudo"] = withPrompt+noPrompt > 0
	out["sudo_nopasswd"] = noPrompt > 0
}

// resolveNames maps ids to sorted names. Ids without a name are returned
// separately. max > 0 caps how many ids are considered.
func resolveNames(ids []int, names map[int]string, max int) ([]string, []int) {
	sorted := listedIDs(ids, max)
	resolved := make([]string, 0, len(sorted))
	var unresolved []int
	for _, id := range sorted {
		if name, ok := names[id]; ok {
			resolved = append(resolved, name)
		} else {
			unresolved = append(unresolved, id)
		}
	}
	sort.Strings(resolved)
	return resolved, unresolved
}

// listedIDs returns ids sorted, capped at max when max > 0: the ids
// resolveNames considers.
func listedIDs(ids []int, max int) []int {
	sorted := append([]int(nil), ids...)
	sort.Ints(sorted)
	if max > 0 && len(sorted) > max {
		sorted = sorted[:max]
	}
	return sorted
}

func intList(v interface{}) []int {
	list, _ := v.([]interface{})
	out := make([]int, 0, len(list))
	for _, item := range list {
		if n, ok := item.(float64); ok {
			out = append(out, int(n))
		}
	}
	return out
}

// copyRoles copies the role names, sorted.
func copyRoles(out, row map[string]interface{}) {
	if _, ok := row["roles"].([]interface{}); !ok {
		return
	}
	roles := stringList(row["roles"])
	sort.Strings(roles)
	out["roles"] = roles
}

func firstStringOf(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func copyString(out, row map[string]interface{}, from, to string) {
	if s, ok := row[from].(string); ok {
		out[to] = s
	}
}

func copyInt(out, row map[string]interface{}, from, to string) {
	if n, ok := row[from].(float64); ok {
		out[to] = int(n)
	}
}

func copyBool(out, row map[string]interface{}, from, to string) {
	if b, ok := row[from].(bool); ok {
		out[to] = b
	}
}

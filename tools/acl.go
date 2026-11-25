package tools

// Read-only ACL tools (get_share_acl, get_filesystem_acl) for answering
// "why can't this user reach this share?". Both project middleware
// results onto fixed field lists.

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerACLTools() {
	r.tools["get_share_acl"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_share_acl",
			Description: "Get the share-level ACL of an SMB share: who each entry applies to (user or group name, SID, Unix id), whether it is ALLOWED or DENIED, and the permission level (FULL, CHANGE, READ). Also returns the share settings that affect access (path, enabled, readonly, hostsallow/hostsdeny). The share ACL only applies to SMB connections; SMB access also needs the filesystem ACL of the share path to allow it, which get_filesystem_acl returns. Give exactly one of 'share_name' or 'share_id'.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"share_name": map[string]interface{}{
						"type":        "string",
						"description": "SMB share name (case-insensitive). HOMES means the home-directory share",
					},
					"share_id": map[string]interface{}{
						"type":        "integer",
						"description": "SMB share id (from query_shares)",
						"minimum":     1,
					},
				},
			},
		},
		Handler: handleGetShareACL,
	}

	r.tools["get_filesystem_acl"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_filesystem_acl",
			Description: "Get the filesystem ACL of a file or directory under /mnt/<pool>: ACL type (NFS4, POSIX1E or DISABLED), owner and group (uid/gid with names), Unix mode bits, and each ACL entry (tag, who, ALLOW/DENY for NFS4, permissions, inheritance flags; access or default scope and effective permissions after the mask for POSIX). NFSv4 entries are returned in evaluation order. Only absolute paths under a pool mountpoint (/mnt/<pool>) are accepted; system paths, relative paths, '..' and symlinks anywhere in the path that lead outside /mnt/<pool> are refused, and symlinks inside a pool are read at their target. Set 'simplified' to show NFSv4 permissions and flags as basic names (FULL_CONTROL, MODIFY, READ, TRAVERSE; INHERIT, NOINHERIT) where they match exactly.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Absolute path under /mnt/<pool>, e.g. /mnt/tank/shares/docs",
					},
					"simplified": map[string]interface{}{
						"type":        "boolean",
						"description": "Show NFSv4 permissions and flags as basic names where they match exactly (default: false, every permission listed)",
						"default":     false,
					},
				},
				"required": []string{"path"},
			},
		},
		Handler: handleGetFilesystemACL,
	}
}

// shareACLQuerySelect is the sharing.smb.query select for get_share_acl.
var shareACLQuerySelect = []interface{}{
	"id", "name", "path", "enabled", "readonly", "browsable",
	"access_based_share_enumeration", "locked", "purpose", "options",
}

// smbSharePermMeaning describes the SMB share permission levels.
var smbSharePermMeaning = map[string]string{
	"FULL":   "read, write, delete, change permissions and take ownership",
	"CHANGE": "read, write and delete",
	"READ":   "read and list only",
	"CUSTOM": "non-standard access mask set outside TrueNAS",
}

func handleGetShareACL(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	name, hasName := args["share_name"].(string)
	name = strings.TrimSpace(name)
	id, hasID := args["share_id"].(float64)
	if hasName && name != "" && hasID {
		return "", fmt.Errorf("give either share_name or share_id, not both")
	}
	if !hasID && name == "" {
		return "", fmt.Errorf("share_name or share_id is required")
	}

	homes := !hasID && strings.EqualFold(name, "HOMES")
	var filters []interface{}
	switch {
	case hasID:
		filters = []interface{}{[]interface{}{"id", "=", int(id)}}
	case homes:
		filters = []interface{}{[]interface{}{"options.home", "=", true}}
	default:
		filters = []interface{}{[]interface{}{"name", "C=", name}}
	}
	raw, err := client.Call(ctx, "sharing.smb.query", filters, map[string]interface{}{"select": shareACLQuerySelect})
	if err != nil {
		return "", fmt.Errorf("failed to query SMB shares: %w", err)
	}
	var shares []map[string]interface{}
	if err := json.Unmarshal(raw, &shares); err != nil {
		return "", fmt.Errorf("failed to parse SMB shares: %w", err)
	}
	if len(shares) == 0 {
		if hasID {
			return "", fmt.Errorf("SMB share id %d not found", int(id))
		}
		return "", fmt.Errorf("SMB share %q not found", name)
	}
	share := shares[0]

	aclName, _ := share["name"].(string)
	if homes {
		aclName = "HOMES"
	}
	raw, err = client.Call(ctx, "sharing.smb.getacl", map[string]interface{}{"share_name": aclName})
	if err != nil {
		return "", fmt.Errorf("failed to get ACL of SMB share %q: %w", aclName, err)
	}
	var acl struct {
		ShareACL []map[string]interface{} `json:"share_acl"`
	}
	if err := json.Unmarshal(raw, &acl); err != nil {
		return "", fmt.Errorf("failed to parse SMB share ACL: %w", err)
	}

	entries := make([]map[string]interface{}, 0, len(acl.ShareACL))
	unresolved, denied := false, false
	for _, ae := range acl.ShareACL {
		entry := map[string]interface{}{
			"who":     ae["ae_who_str"],
			"who_sid": ae["ae_who_sid"],
			"type":    ae["ae_type"],
			"perm":    ae["ae_perm"],
		}
		if perm, _ := ae["ae_perm"].(string); smbSharePermMeaning[perm] != "" {
			entry["perm_meaning"] = smbSharePermMeaning[perm]
		}
		if who, ok := ae["ae_who_id"].(map[string]interface{}); ok {
			entry["id_type"] = who["id_type"]
			entry["unix_id"] = who["id"]
		}
		if s, _ := ae["ae_who_str"].(string); s == "" {
			unresolved = true
		}
		if t, _ := ae["ae_type"].(string); t == "DENIED" {
			denied = true
		}
		entries = append(entries, entry)
	}

	notes := []string{"The share ACL only applies to SMB access. The filesystem ACL of the share path (get_filesystem_acl) must also allow it; the more restrictive of the two wins."}
	if denied {
		notes = append(notes, "A DENIED entry that matches the user or any of their groups overrides ALLOWED entries.")
	}
	if len(entries) == 1 && entries[0]["who_sid"] == "S-1-1-0" && entries[0]["type"] == "ALLOWED" && entries[0]["perm"] == "FULL" {
		notes = append(notes, "This is the default share ACL (everyone, FULL): the share ACL does not restrict anyone, so look at the filesystem ACL and share settings.")
	}
	if unresolved {
		notes = append(notes, "Entries with no 'who' have a SID that could not be mapped to a user or group, for example a deleted account or an unreachable domain.")
	}
	if enabled, ok := share["enabled"].(bool); ok && !enabled {
		notes = append(notes, "The share is disabled, so nobody can connect to it.")
	}
	if locked, _ := share["locked"].(bool); locked {
		notes = append(notes, "The share path is on a locked dataset, so it cannot be accessed until the dataset is unlocked.")
	}

	return marshalJSON(map[string]interface{}{
		"share":     shareACLContext(share),
		"share_acl": entries,
		"count":     len(entries),
		"notes":     notes,
	})
}

// shareACLContext returns the share settings that affect who can connect.
func shareACLContext(share map[string]interface{}) map[string]interface{} {
	out := pickAllowed(share, []string{
		"id", "name", "path", "enabled", "readonly", "browsable",
		"access_based_share_enumeration", "locked", "purpose",
	})
	if opts, ok := share["options"].(map[string]interface{}); ok {
		for _, k := range []string{"hostsallow", "hostsdeny", "guestok", "home"} {
			if v, ok := opts[k]; ok {
				out[k] = v
			}
		}
	}
	return out
}

// validateACLPath accepts only absolute, canonical paths under
// /mnt/<pool> and returns the path without a trailing slash. Hidden
// top-level mounts such as /mnt/.ix-apps are system paths and refused.
func validateACLPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.ContainsFunc(p, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		return "", fmt.Errorf("path contains control characters")
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path must be absolute, under /mnt/<pool> (got %q)", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path must not contain '..' (got %q)", p)
		}
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if !strings.HasPrefix(p, "/mnt/") {
		return "", fmt.Errorf("only paths under /mnt/<pool> are allowed; %q is outside the storage pools", p)
	}
	if path.Clean(p) != p {
		return "", fmt.Errorf("path must not contain empty or '.' segments (got %q)", p)
	}
	pool, _, _ := strings.Cut(strings.TrimPrefix(p, "/mnt/"), "/")
	if strings.HasPrefix(pool, ".") {
		return "", fmt.Errorf("%q is a system path, not a pool", "/mnt/"+pool)
	}
	return p, nil
}

// Limits for resolveACLPath: path components below /mnt/<pool>, and
// symlinks followed in total.
const (
	maxACLPathDepth   = 64
	maxACLSymlinkHops = 16
)

// statACLPath returns filesystem.stat of p. It does not follow a final
// symlink: the result then has type SYMLINK and a fully resolved realpath.
func statACLPath(ctx context.Context, client *truenas.Client, p string) (map[string]interface{}, error) {
	raw, err := client.Call(ctx, "filesystem.stat", p)
	if err != nil {
		return nil, fmt.Errorf("failed to stat %s: %w", p, err)
	}
	var st map[string]interface{}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("failed to parse filesystem.stat result: %w", err)
	}
	return st, nil
}

// resolveACLPath walks a validated path one component at a time from
// /mnt/<pool> down and returns the resolved target and its stat.
//
// A single stat of the full path is not enough: on 25.10, filesystem.stat
// only resolves realpath when the final component is a symlink, yet it
// follows links earlier in the path, so /mnt/<pool>/a/link/file with link
// pointing outside the pool would pass. Here every component is stat'ed on
// its own, so every symlink is the final component of some stat and its
// realpath is resolved. Each symlink must resolve under /mnt/<pool> (it is
// read at its target), and each /mnt/<pool> reached must be a mountpoint,
// so a plain directory under /mnt is not taken for a pool.
func resolveACLPath(ctx context.Context, client *truenas.Client, p string) (string, map[string]interface{}, error) {
	pool, rest, _ := strings.Cut(strings.TrimPrefix(p, "/mnt/"), "/")
	var comps []string
	if rest != "" {
		comps = strings.Split(rest, "/")
	}
	if len(comps) > maxACLPathDepth {
		return "", nil, fmt.Errorf("path has more than %d components below /mnt/%s", maxACLPathDepth, pool)
	}

	pools := map[string]map[string]interface{}{}
	poolRoot := func(name string) (map[string]interface{}, error) {
		if st, ok := pools[name]; ok {
			return st, nil
		}
		root := "/mnt/" + name
		st, err := statACLPath(ctx, client, root)
		if err != nil {
			return nil, err
		}
		if typ, _ := st["type"].(string); typ != "DIRECTORY" {
			return nil, fmt.Errorf("%s is not a pool mountpoint (type %v)", root, st["type"])
		}
		if mnt, _ := st["is_mountpoint"].(bool); !mnt {
			return nil, fmt.Errorf("%s is not a pool mountpoint", root)
		}
		pools[name] = st
		return st, nil
	}

	cur := "/mnt/" + pool
	st, err := poolRoot(pool)
	if err != nil {
		return "", nil, err
	}
	hops := 0
	for _, seg := range comps {
		next := cur + "/" + seg
		if st, err = statACLPath(ctx, client, next); err != nil {
			return "", nil, err
		}
		for {
			if typ, _ := st["type"].(string); typ != "SYMLINK" {
				break
			}
			if hops++; hops > maxACLSymlinkHops {
				return "", nil, fmt.Errorf("too many symlinks in %s", p)
			}
			real, _ := st["realpath"].(string)
			resolved, err := validateACLPath(real)
			if err != nil {
				return "", nil, fmt.Errorf("%s resolves to %s, which is outside /mnt/<pool>; refusing to read its ACL", next, real)
			}
			name, _, _ := strings.Cut(strings.TrimPrefix(resolved, "/mnt/"), "/")
			if _, err := poolRoot(name); err != nil {
				return "", nil, fmt.Errorf("%s resolves to %s: %w; refusing to read its ACL", next, resolved, err)
			}
			next = resolved
			if st, err = statACLPath(ctx, client, next); err != nil {
				return "", nil, err
			}
		}
		cur = next
	}
	return cur, st, nil
}

func handleGetFilesystemACL(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	requested, _ := args["path"].(string)
	p, err := validateACLPath(requested)
	if err != nil {
		return "", err
	}
	simplified, _ := args["simplified"].(bool)

	target, st, err := resolveACLPath(ctx, client, p)
	if err != nil {
		return "", err
	}

	// A symlink swapped in after the walk above and before getacl is not
	// caught. That gap is acceptable for a read-only tool: getacl changes
	// nothing and needs the same FILESYSTEM_ATTRS_READ role as stat.
	raw, err := client.Call(ctx, "filesystem.getacl", target, simplified, true)
	if err != nil {
		return "", fmt.Errorf("failed to get ACL of %s: %w", target, err)
	}
	var acl map[string]interface{}
	if err := json.Unmarshal(raw, &acl); err != nil {
		return "", fmt.Errorf("failed to parse filesystem ACL: %w", err)
	}

	acltype, _ := acl["acltype"].(string)
	aces, _ := acl["acl"].([]interface{})
	out := map[string]interface{}{
		"path":    p,
		"acltype": acltype,
		"trivial": acl["trivial"],
	}
	if target != p {
		out["resolved_path"] = target
	}
	if t, ok := st["type"]; ok {
		out["type"] = t
	}
	if mode, ok := st["mode"].(float64); ok {
		out["mode"] = fmt.Sprintf("%04o", int64(mode)&0o7777)
	}
	out["owner"] = aclPrincipal("uid", acl["uid"], st["uid"], firstNonEmptyString(acl["user"], st["user"], aceWho(aces, "owner@", "USER_OBJ")))
	out["group"] = aclPrincipal("gid", acl["gid"], st["gid"], firstNonEmptyString(acl["group"], st["group"], aceWho(aces, "group@", "GROUP_OBJ")))

	var entries []map[string]interface{}
	var notes []string
	switch acltype {
	case "NFS4":
		entries = nfs4Entries(aces)
		if flags, ok := acl["aclflags"].(map[string]interface{}); ok {
			out["aclflags"] = pickAllowed(flags, []string{"autoinherit", "protected", "defaulted"})
		}
		notes = append(notes, "NFSv4 entries are checked in order: for each requested permission, the first entry that matches the user (owner@, group@, everyone@, the user or one of their groups) and lists that permission decides ALLOW or DENY. INHERIT_ONLY entries apply only to new children.")
		if simplified {
			notes = append(notes, "Permissions and flags that match a basic set are shown as basic_perms and basic_flags; the rest are listed in full.")
		}
	case "POSIX1E":
		entries = posixEntries(aces)
		notes = append(notes, "Access entries apply to this path; default entries are only inherited by new files and directories. MASK caps named USER and GROUP entries and GROUP_OBJ; 'effective' shows the result where it differs.")
	default:
		notes = append(notes, "ACLs are disabled on this dataset; only the owner, group and mode bits apply.")
	}
	if entries == nil {
		entries = []map[string]interface{}{}
	}
	for _, e := range entries {
		if _, hasID := e["id"]; hasID && e["who"] == nil {
			notes = append(notes, "Entries with an id but no 'who' could not be resolved to a user or group name.")
			break
		}
	}
	notes = append(notes, "SMB access also needs the share ACL to allow it (get_share_acl).")
	out["entries"] = entries
	out["count"] = len(entries)
	out["notes"] = notes
	return marshalJSON(out)
}

// aclPrincipal returns {uid|gid, name} for the owner or group, preferring
// the id getacl reported over the one from stat.
func aclPrincipal(idKey string, aclID, statID interface{}, name string) map[string]interface{} {
	id := aclID
	if id == nil {
		id = statID
	}
	out := map[string]interface{}{idKey: id, "name": nil}
	if name != "" {
		out["name"] = name
	}
	return out
}

// firstNonEmptyString returns the first non-empty string among vals.
func firstNonEmptyString(vals ...interface{}) string {
	for _, v := range vals {
		if s, _ := v.(string); s != "" {
			return s
		}
	}
	return ""
}

// aceWho returns the resolved name of the first entry with one of tags.
func aceWho(aces []interface{}, tags ...string) interface{} {
	for _, a := range aces {
		ace, _ := a.(map[string]interface{})
		tag, _ := ace["tag"].(string)
		for _, t := range tags {
			if tag == t {
				return ace["who"]
			}
		}
	}
	return nil
}

var (
	nfs4PermOrder = []string{
		"READ_DATA", "WRITE_DATA", "APPEND_DATA", "READ_NAMED_ATTRS", "WRITE_NAMED_ATTRS",
		"EXECUTE", "DELETE", "DELETE_CHILD", "READ_ATTRIBUTES", "WRITE_ATTRIBUTES",
		"READ_ACL", "WRITE_ACL", "WRITE_OWNER", "SYNCHRONIZE",
	}
	nfs4FlagOrder = []string{"FILE_INHERIT", "DIRECTORY_INHERIT", "NO_PROPAGATE_INHERIT", "INHERIT_ONLY", "INHERITED"}
)

// setNames lists the keys of a permission or flag object that are true,
// in the given order, then any other true keys sorted.
func setNames(m map[string]interface{}, order []string) []string {
	names := []string{}
	known := map[string]bool{}
	for _, k := range order {
		known[k] = true
		if v, _ := m[k].(bool); v {
			names = append(names, k)
		}
	}
	var extra []string
	for k, v := range m {
		if b, _ := v.(bool); b && !known[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(names, extra...)
}

// aceIdentity copies tag, who and (for named entries) id of an entry.
func aceIdentity(ace map[string]interface{}) map[string]interface{} {
	e := map[string]interface{}{"tag": ace["tag"], "who": ace["who"]}
	if id, ok := ace["id"].(float64); ok && id >= 0 {
		e["id"] = id
	}
	return e
}

func nfs4Entries(aces []interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(aces))
	for _, a := range aces {
		ace, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		e := aceIdentity(ace)
		e["type"] = ace["type"]
		if perms, ok := ace["perms"].(map[string]interface{}); ok {
			if basic, ok := perms["BASIC"].(string); ok {
				e["basic_perms"] = basic
			} else {
				e["perms"] = setNames(perms, nfs4PermOrder)
			}
		}
		if flags, ok := ace["flags"].(map[string]interface{}); ok {
			if basic, ok := flags["BASIC"].(string); ok {
				e["basic_flags"] = basic
			} else {
				e["flags"] = setNames(flags, nfs4FlagOrder)
			}
		}
		out = append(out, e)
	}
	return out
}

// posixPermString renders READ/WRITE/EXECUTE as "rwx" with '-' for unset.
func posixPermString(perms map[string]interface{}) string {
	b := []byte("---")
	for i, k := range []string{"READ", "WRITE", "EXECUTE"} {
		if v, _ := perms[k].(bool); v {
			b[i] = "rwx"[i]
		}
	}
	return string(b)
}

func maskPerms(perm, mask string) string {
	b := []byte(perm)
	for i := range b {
		if mask[i] == '-' {
			b[i] = '-'
		}
	}
	return string(b)
}

func posixEntries(aces []interface{}) []map[string]interface{} {
	masks := map[bool]string{}
	for _, a := range aces {
		ace, _ := a.(map[string]interface{})
		if tag, _ := ace["tag"].(string); tag == "MASK" {
			perms, _ := ace["perms"].(map[string]interface{})
			def, _ := ace["default"].(bool)
			masks[def] = posixPermString(perms)
		}
	}
	out := make([]map[string]interface{}, 0, len(aces))
	for _, a := range aces {
		ace, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		e := aceIdentity(ace)
		def, _ := ace["default"].(bool)
		e["scope"] = "access"
		if def {
			e["scope"] = "default"
		}
		perms, _ := ace["perms"].(map[string]interface{})
		perm := posixPermString(perms)
		e["perms"] = perm
		tag, _ := ace["tag"].(string)
		if mask, ok := masks[def]; ok && (tag == "USER" || tag == "GROUP" || tag == "GROUP_OBJ") {
			if eff := maskPerms(perm, mask); eff != perm {
				e["effective"] = eff
			}
		}
		out = append(out, e)
	}
	return out
}

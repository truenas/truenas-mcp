package tools

// Share write tools for existing shares (update_smb_share,
// delete_smb_share, update_nfs_share, delete_nfs_share) and the share-level
// ACL of an SMB share (set_share_acl). Each call acts on one share. The
// global SMB and NFS service settings (smb.update, nfs.update) and
// filesystem ACLs are never changed, and deleting a share removes only its
// configuration, never data.

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	sharingSMBUpdateMethod = "sharing.smb.update"
	sharingSMBDeleteMethod = "sharing.smb.delete"
	sharingSMBSetACLMethod = "sharing.smb.setacl"
	sharingNFSUpdateMethod = "sharing.nfs.update"
	sharingNFSDeleteMethod = "sharing.nfs.delete"
)

const (
	// maxShareListEntries bounds hostsallow, hostsdeny, networks and hosts.
	maxShareListEntries = 64
	// maxShareACLEntries bounds the entries of one share ACL.
	maxShareACLEntries = 64
	// maxShareCommentLen bounds a share comment.
	maxShareCommentLen = 1024
	// everyoneSID is the well-known SID of Everyone.
	everyoneSID = "S-1-1-0"
	// builtinAdminsSID is the SID of builtin_administrators (BUILTIN\Administrators).
	builtinAdminsSID = "S-1-5-32-544"
	// smbLegacyPurpose is the only purpose whose options include the host lists on update.
	smbLegacyPurpose = "LEGACY_SHARE"
)

// reservedSMBShareNames are names Samba or Windows give a special meaning:
// the smb.conf sections global, printers and homes, and the
// administrative IPC$, ADMIN$ and PRINT$ shares. Drive shares (C$) are
// matched by driveShareName.
var reservedSMBShareNames = map[string]bool{
	"global": true, "printers": true, "homes": true,
	"ipc$": true, "admin$": true, "print$": true,
}

var (
	driveShareName = regexp.MustCompile(`^[A-Za-z]\$$`)
	sidPattern     = regexp.MustCompile(`^S-1-[0-9]{1,14}(-[0-9]{1,10}){1,15}$`)
)

// unixSIDPrefixes are the SIDs Samba derives from Unix ids for accounts
// that are not SMB accounts; sharing.smb.setacl refuses them.
var unixSIDPrefixes = []string{"S-1-22-1-", "S-1-22-2-"}

func (r *Registry) registerShareActionTools() {
	str := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	boolean := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc}
	}
	dryRun := map[string]interface{}{"type": "boolean", "description": "Optional: Preview without changing anything (default: false)", "default": false}
	shareID := map[string]interface{}{"type": "integer", "minimum": 1, "description": "SMB share id (from query_shares). Give exactly one of share_id or share_name"}
	shareName := map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 80, "description": "SMB share name (case-insensitive). Give exactly one of share_id or share_name"}
	nfsID := map[string]interface{}{"type": "integer", "minimum": 1, "description": "Required: NFS share id (from query_shares)"}
	list := func(desc string) map[string]interface{} {
		return map[string]interface{}{
			"type": "array", "description": desc, "maxItems": maxShareListEntries,
			"items": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256},
		}
	}
	newPath := map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1024,
		"description": "Optional: New share path: an existing directory below /mnt/<pool>, given in normalised form (no trailing '/', '//', '.' or '..') and reached without symlinks. Pool roots and datasets TrueNAS manages itself are refused. No files are moved"}
	comment := map[string]interface{}{"type": "string", "maxLength": maxShareCommentLen, "description": "Optional: New description (\"\" clears it)"}

	r.tools["update_smb_share"] = Tool{
		Definition: mcp.Tool{
			Name:        "update_smb_share",
			Description: "Change the settings of one existing SMB share: its name, path, comment, enabled, readonly, browsable, access_based_share_enumeration, hostsallow or hostsdeny. Only the settings given that differ from the current ones are sent. The preview shows the share's current settings and each change (old -> new). Renaming breaks client paths and mapped drives that use the old name; a new path serves a different directory and moves no files. A share can be pointed at any ordinary dataset folder below /mnt/<pool>, including other users' or teams' data and home directories; ownership is not checked. hostsallow and hostsdeny can be changed only for shares whose purpose is LEGACY_SHARE. Refused for the homes share and other reserved or administrative names (global, printers, homes, IPC$, ADMIN$, PRINT$, drive shares such as C$), for shares on a locked dataset or whose lock state is unknown, and for shares whose current or new path is not below /mnt/<pool> (pool roots, /mnt itself, EXTERNAL DFS proxies, datasets TrueNAS manages itself such as the boot pool, .system, ix-apps, ix-applications, .ix-virt or any dot-prefixed component, non-normalised paths, and paths that go through a symlink). The share's purpose, options other than the host lists, audit settings, share ACL and the global SMB service settings are not changed. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"share_id":                       shareID,
					"share_name":                     shareName,
					"new_name":                       map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 80, "description": "Optional: New share name (max 80 characters, none of \\ / [ ] : | < > + = ; , * ? \"; not a reserved name; not used by another share)"},
					"path":                           newPath,
					"comment":                        comment,
					"enabled":                        boolean("Optional: Enable or disable the share. Disabling it cuts off every SMB client"),
					"readonly":                       boolean("Optional: Prevent SMB clients from creating or changing files"),
					"browsable":                      boolean("Optional: Show the share in network browse lists"),
					"access_based_share_enumeration": boolean("Optional: List the share only to users the share ACL grants access"),
					"hostsallow":                     list("Optional: Replace the hosts or networks allowed to connect ([] allows every host)"),
					"hostsdeny":                      list("Optional: Replace the hosts or networks refused ([] refuses none)"),
					"dry_run":                        dryRun,
				},
			},
		},
		Handler: handleUpdateSMBShare,
		Plan:    planUpdateSMBShare,
		Execute: executeUpdateSMBShare,
	}

	r.tools["delete_smb_share"] = Tool{
		Definition: mcp.Tool{
			Name:        "delete_smb_share",
			Description: "Delete one SMB share's configuration. SMB clients using the share are disconnected and lose access, and client paths and mapped drives to it stop working. Only the share is removed: the directory and every file in it are kept. The preview shows the share's current settings. Refused for the same shares as update_smb_share: the homes share and other reserved or administrative names, shares on a locked dataset or whose lock state is unknown, and shares whose path is not below /mnt/<pool> or is in a dataset TrueNAS manages itself. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"share_id":   shareID,
					"share_name": shareName,
					"dry_run":    dryRun,
				},
			},
		},
		Handler: handleDeleteSMBShare,
		Plan:    planDeleteSMBShare,
		Execute: executeDeleteSMBShare,
	}

	r.tools["update_nfs_share"] = Tool{
		Definition: mcp.Tool{
			Name:        "update_nfs_share",
			Description: "Change the settings of one existing NFS share: its path, comment, enabled, ro, networks, hosts, maproot_user, maproot_group, mapall_user, mapall_group or security. Only the settings given that differ from the current ones are sent. The preview shows the share's current settings, each change (old -> new) and how root and other clients are mapped afterwards. A new path exports a different directory and moves no files. A share can be pointed at any ordinary dataset folder below /mnt/<pool>, including other users' or teams' data and home directories; ownership is not checked. Mapping every client to root (mapall_user root), keeping root as root (no_root_squash) or removing every network and host limit is allowed with an explicit warning in the preview; no extra flag is needed. Refused for shares on a locked dataset or whose lock state is unknown, and for shares whose current or new path is not below /mnt/<pool> (pool roots, /mnt itself, datasets TrueNAS manages itself such as the boot pool, .system, ix-apps, ix-applications, .ix-virt or any dot-prefixed component, non-normalised paths, and paths that go through a symlink). Aliases, expose_snapshots and the global NFS service settings are not changed. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"share_id":      nfsID,
					"path":          newPath,
					"comment":       comment,
					"enabled":       boolean("Optional: Enable or disable the export. Disabling it cuts off every NFS client"),
					"ro":            boolean("Optional: Export read-only"),
					"networks":      list("Optional: Replace the authorized networks in CIDR notation, e.g. ['192.0.2.0/24'] ([] with no hosts allows every client)"),
					"hosts":         list("Optional: Replace the authorized hosts (IP addresses or hostnames, no quotes or spaces; [] with no networks allows every client)"),
					"maproot_user":  str("Optional: Map root clients to this user (\"\" clears it)"),
					"maproot_group": str("Optional: Map root clients to this group (\"\" clears it)"),
					"mapall_user":   str("Optional: Map every client to this user (\"\" clears it)"),
					"mapall_group":  str("Optional: Map every client group to this group (\"\" clears it)"),
					"security": map[string]interface{}{
						"type": "array", "description": "Optional: Replace the security mechanisms ([] uses the default, SYS)", "maxItems": 4,
						"items": map[string]interface{}{"type": "string", "enum": []string{"SYS", "KRB5", "KRB5I", "KRB5P"}},
					},
					"dry_run": dryRun,
				},
				"required": []string{"share_id"},
			},
		},
		Handler: handleUpdateNFSShare,
		Plan:    planUpdateNFSShare,
		Execute: executeUpdateNFSShare,
	}

	r.tools["delete_nfs_share"] = Tool{
		Definition: mcp.Tool{
			Name:        "delete_nfs_share",
			Description: "Delete one NFS share's configuration. NFS clients that have the export mounted lose access. Only the export is removed: the directory and every file in it are kept. The preview shows the share's current settings. Refused for shares on a locked dataset or whose lock state is unknown, and for shares whose path is not below /mnt/<pool> or is in a dataset TrueNAS manages itself. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"share_id": nfsID,
					"dry_run":  dryRun,
				},
				"required": []string{"share_id"},
			},
		},
		Handler: handleDeleteNFSShare,
		Plan:    planDeleteNFSShare,
		Execute: executeDeleteNFSShare,
	}

	r.tools["set_share_acl"] = Tool{
		Definition: mcp.Tool{
			Name:        "set_share_acl",
			Description: "Replace the share-level ACL of one SMB share (sharing.smb.setacl). The new ACL replaces the current one as a whole: entries not listed are removed. Each entry names its principal by who_type: 'everyone', 'user' or 'group' (a local user or group with SMB enabled; who is its name), or 'sid' (who is the SID of such a user or group, or S-1-1-0). Directory service (Active Directory, LDAP, IPA) users and groups are refused, by name or by SID, as are unknown names, non-SMB accounts, Unix-encoded SIDs (S-1-22-...) and the same principal twice. An ACL that grants FULL to nobody, that has a DENIED entry for everyone, or that has a DENIED entry for an administrative group (builtin_administrators, or any group that grants TrueNAS roles), is refused unless allow_no_full_control=true, because it can lock every user, or the administrators, out of the share. The preview shows the current share ACL and the new one. This changes only the share ACL, which applies to SMB access; the filesystem ACL of the share path is not changed (get_filesystem_acl). Refused for the same shares as update_smb_share. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"share_id":   shareID,
					"share_name": shareName,
					"entries": map[string]interface{}{
						"type":        "array",
						"description": "Required: The complete new share ACL, at least one entry",
						"maxItems":    maxShareACLEntries,
						"items": map[string]interface{}{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]interface{}{
								"who_type": map[string]interface{}{"type": "string", "enum": []string{"everyone", "user", "group", "sid"}, "description": "How who names the principal"},
								"who":      map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256, "description": "User name, group name or SID; omit for everyone"},
								"perm":     map[string]interface{}{"type": "string", "enum": []string{"FULL", "CHANGE", "READ"}, "description": "FULL (read, write, delete, change permissions, take ownership), CHANGE (read, write, delete) or READ"},
								"type":     map[string]interface{}{"type": "string", "enum": []string{"ALLOWED", "DENIED"}, "description": "ALLOWED or DENIED; a matching DENIED entry overrides ALLOWED entries"},
							},
							"required": []string{"who_type", "perm", "type"},
						},
					},
					"allow_no_full_control": map[string]interface{}{"type": "boolean", "default": false,
						"description": "Optional: Allow an ACL in which no entry grants FULL, or with a DENIED entry for everyone or for an administrative group (default: false). Refused when the ACL does not need it"},
					"dry_run": dryRun,
				},
				"required": []string{"entries"},
			},
		},
		Handler: handleSetShareACL,
		Plan:    planSetShareACL,
		Execute: executeSetShareACL,
	}
}

// ---- shared helpers ----

// smbShareRef is how a call names an SMB share.
type smbShareRef struct {
	id    int
	hasID bool
	name  string
}

func smbShareRefArg(args map[string]interface{}) (smbShareRef, error) {
	name, _ := trimmedArg(args, "share_name")
	idv, hasID := args["share_id"]
	switch {
	case hasID && name != "":
		return smbShareRef{}, fmt.Errorf("give either share_name or share_id, not both")
	case hasID:
		id, ok := intID(idv)
		if !ok || id < 1 {
			return smbShareRef{}, fmt.Errorf("share_id must be a positive integer")
		}
		return smbShareRef{id: id, hasID: true}, nil
	case name == "":
		return smbShareRef{}, fmt.Errorf("share_name or share_id is required")
	}
	return smbShareRef{name: name}, nil
}

func (s smbShareRef) String() string {
	if s.hasID {
		return fmt.Sprintf("SMB share id %d", s.id)
	}
	return fmt.Sprintf("SMB share %q", s.name)
}

// queryShareRows runs sharing.smb.query (or, with nfs, sharing.nfs.query)
// and keeps only the rows keep accepts, so a middleware that ignores part
// of the filter cannot add shares.
func queryShareRows(ctx context.Context, client *truenas.Client, nfs bool, filters []interface{}, sel []interface{}, keep func(map[string]interface{}) bool) ([]map[string]interface{}, error) {
	var raw json.RawMessage
	var err error
	kind := "SMB"
	if nfs {
		kind = "NFS"
		raw, err = client.Call(ctx, "sharing.nfs.query", filters, map[string]interface{}{"select": sel})
	} else {
		raw, err = client.Call(ctx, "sharing.smb.query", filters, map[string]interface{}{"select": sel})
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query %s shares: %w", kind, err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("failed to parse %s shares", kind)
	}
	out := rows[:0]
	for _, row := range rows {
		if keep(row) {
			out = append(out, row)
		}
	}
	return out, nil
}

func rowHasID(id int) func(map[string]interface{}) bool {
	return func(row map[string]interface{}) bool {
		got, ok := intID(row["id"])
		return ok && got == id
	}
}

// lookupSMBShare returns the one SMB share ref names.
func lookupSMBShare(ctx context.Context, client *truenas.Client, ref smbShareRef) (map[string]interface{}, error) {
	filters := []interface{}{[]interface{}{"name", "C=", ref.name}}
	keep := func(row map[string]interface{}) bool {
		n, _ := row["name"].(string)
		return strings.EqualFold(n, ref.name)
	}
	if ref.hasID {
		filters = []interface{}{[]interface{}{"id", "=", ref.id}}
		keep = rowHasID(ref.id)
	}
	rows, err := queryShareRows(ctx, client, false, filters, smbShareQuerySelect, keep)
	if err != nil {
		return nil, err
	}
	switch len(rows) {
	case 0:
		return nil, fmt.Errorf("%s not found", ref)
	case 1:
		if _, ok := intID(rows[0]["id"]); !ok {
			return nil, fmt.Errorf("%s has no readable id", ref)
		}
		return rows[0], nil
	}
	return nil, fmt.Errorf("%s matches %d shares", ref, len(rows))
}

func lookupNFSShare(ctx context.Context, client *truenas.Client, id int) (map[string]interface{}, error) {
	rows, err := queryShareRows(ctx, client, true, []interface{}{[]interface{}{"id", "=", id}}, nfsShareQuerySelect, rowHasID(id))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("NFS share id %d not found", id)
	}
	return rows[0], nil
}

func nfsShareIDArg(args map[string]interface{}) (int, error) {
	id, ok := intID(args["share_id"])
	if !ok || id < 1 {
		return 0, fmt.Errorf("share_id is required and must be a positive integer")
	}
	return id, nil
}

// smbShareNameRefusal is why name may not be given to or edited on a share.
func smbShareNameRefusal(name string) string {
	l := strings.ToLower(name)
	if reservedSMBShareNames[l] || driveShareName.MatchString(name) {
		return fmt.Sprintf("'%s' is a reserved or administrative SMB share name (global, printers, homes, IPC$, ADMIN$, PRINT$ and drive shares such as C$); these tools do not create or change such shares", name)
	}
	return ""
}

// sharePathRefusal is why p is not a path these tools let a share use, or
// "". It accepts only a normalised path strictly below /mnt/<pool> that is
// not in a dataset TrueNAS manages itself (managedDatasetPart, in any
// letter case).
func sharePathRefusal(p string) string {
	switch {
	case p == "":
		return "the share has no path"
	case p == "EXTERNAL":
		return "path EXTERNAL (a DFS proxy share) is not below /mnt/<pool>"
	case strings.ContainsFunc(p, func(c rune) bool { return c < 0x20 || c == 0x7f }):
		return "path contains control characters"
	case !strings.HasPrefix(p, "/mnt/"):
		return fmt.Sprintf("path %q is not below /mnt/<pool>", p)
	case path.Clean(p) != p:
		return fmt.Sprintf("path %q is not normalised (a trailing '/', '//', '.' or '..'); give the cleaned path", p)
	}
	rest := strings.TrimPrefix(p, "/mnt/")
	pool, sub, _ := strings.Cut(rest, "/")
	switch {
	case strings.HasPrefix(pool, "."):
		return fmt.Sprintf("path %q is a system path, not in a pool", p)
	case sub == "":
		return fmt.Sprintf("path %q is the root of pool '%s'; share a dataset or directory below it", p, pool)
	}
	// Compared in lower case, so IX-APPS or Boot-Pool on a case-insensitive
	// pool is refused like ix-apps or boot-pool.
	if part, ok := managedDatasetPart(strings.ToLower(rest)); ok {
		return fmt.Sprintf("path %q is inside '%s', which TrueNAS manages itself", p, part)
	}
	return ""
}

// checkNewSharePath walks p one component at a time (resolveACLPath) and
// returns what the plan binds to: the directory's stat identity. It is a
// refusal when p is not a directory or any component is a symlink.
func checkNewSharePath(ctx context.Context, client *truenas.Client, p string) (map[string]interface{}, string, error) {
	target, st, err := resolveACLPath(ctx, client, p)
	if err != nil {
		return nil, "", fmt.Errorf("path %s cannot be used: %w", p, err)
	}
	bind := map[string]interface{}{
		"path": p, "realpath": st["realpath"], "type": st["type"],
		"inode": st["inode"], "dev": st["dev"], "is_mountpoint": st["is_mountpoint"],
	}
	if target != p {
		return bind, fmt.Sprintf("path %s goes through a symlink (it resolves to %s); give the real path", p, target), nil
	}
	if typ, _ := st["type"].(string); typ != "DIRECTORY" {
		return bind, fmt.Sprintf("path %s is not a directory (type %v)", p, st["type"]), nil
	}
	return bind, "", nil
}

// shareLockRefusal refuses a share whose lock state is true or unknown.
func shareLockRefusal(kind string, share map[string]interface{}) string {
	switch v := share["locked"].(type) {
	case bool:
		if !v {
			return ""
		}
		return fmt.Sprintf("the %s share is on a locked dataset; unlock the dataset first", kind)
	}
	return fmt.Sprintf("the lock state of the %s share could not be read, so it is treated as locked", kind)
}

// smbShareRefusals are the reasons an SMB share may not be changed.
func smbShareRefusals(share map[string]interface{}) []string {
	var out []string
	name, _ := share["name"].(string)
	if why := smbShareNameRefusal(name); why != "" {
		out = append(out, why)
	}
	opts, _ := share["options"].(map[string]interface{})
	if home, _ := opts["home"].(bool); home {
		out = append(out, fmt.Sprintf("'%s' is the homes share (user home directories); these tools do not change it", name))
	}
	if why := shareLockRefusal("SMB", share); why != "" {
		out = append(out, why)
	}
	p, _ := share["path"].(string)
	if why := sharePathRefusal(p); why != "" {
		out = append(out, "current "+why)
	}
	return out
}

func nfsShareRefusals(share map[string]interface{}) []string {
	var out []string
	if why := shareLockRefusal("NFS", share); why != "" {
		out = append(out, why)
	}
	p, _ := share["path"].(string)
	if why := sharePathRefusal(p); why != "" {
		out = append(out, "current "+why)
	}
	return out
}

func sameJSON(a, b interface{}) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func shareChange(key string, old, new interface{}) string {
	return fmt.Sprintf("%s: %s -> %s", key, confirmShown(key, old), confirmShown(key, new))
}

// stringListArg returns a list argument as trimmed strings, refusing
// control characters and duplicates.
func stringListArg(args map[string]interface{}, key string) ([]interface{}, bool, error) {
	raw, given := args[key]
	if !given {
		return nil, false, nil
	}
	items, ok := raw.([]interface{})
	if !ok {
		return nil, true, fmt.Errorf("%s must be a list of strings", key)
	}
	seen := map[string]bool{}
	out := []interface{}{}
	for _, it := range items {
		s, ok := it.(string)
		s = strings.TrimSpace(s)
		if !ok || s == "" {
			return nil, true, fmt.Errorf("%s entries must be non-empty strings", key)
		}
		if strings.ContainsFunc(s, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
			return nil, true, fmt.Errorf("%s entries cannot contain control characters", key)
		}
		if seen[s] {
			return nil, true, fmt.Errorf("%s lists %q twice", key, s)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, true, nil
}

// listValue is a share list field as a []interface{}; null is empty.
func listValue(v interface{}) []interface{} {
	l, _ := v.([]interface{})
	if l == nil {
		return []interface{}{}
	}
	return l
}

func smbNetworkPath(host, name string) string {
	return fmt.Sprintf("\\\\%s\\%s", host, name)
}

// ---- update_smb_share ----

func planUpdateSMBShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	ref, err := smbShareRefArg(args)
	if err != nil {
		return nil, err
	}
	share, err := lookupSMBShare(ctx, client, ref)
	if err != nil {
		return nil, err
	}
	id, _ := intID(share["id"])
	name, _ := share["name"].(string)
	curPath, _ := share["path"].(string)
	blocked := smbShareRefusals(share)
	payload := map[string]interface{}{}
	var changes, notes, warnings []string
	bind := map[string]interface{}{"share": share}

	if newName, ok := trimmedArg(args, "new_name"); ok {
		if err := validateShareName(newName); err != nil {
			return nil, err
		}
		if why := smbShareNameRefusal(newName); why != "" {
			return nil, fmt.Errorf("%s", why)
		}
		if newName == name {
			notes = append(notes, fmt.Sprintf("the share is already named '%s'", name))
		} else {
			taken, err := queryShareRows(ctx, client, false,
				[]interface{}{[]interface{}{"name", "C=", newName}}, []interface{}{"id", "name"},
				func(row map[string]interface{}) bool {
					n, _ := row["name"].(string)
					other, _ := intID(row["id"])
					return strings.EqualFold(n, newName) && other != id
				})
			if err != nil {
				return nil, err
			}
			ids := []int{}
			for _, row := range taken {
				other, _ := intID(row["id"])
				ids = append(ids, other)
			}
			sort.Ints(ids)
			bind["new_name_used_by"] = ids
			if len(ids) > 0 {
				blocked = append(blocked, fmt.Sprintf("another SMB share is already named '%s' (share names are case-insensitive)", newName))
			}
			payload["name"] = newName
			changes = append(changes, shareChange("name", name, newName))
			warnings = append(warnings, fmt.Sprintf("Clients that use the old name ('%s': mapped drives, bookmarks, backup jobs) must be changed to '%s'; check the share ACL with get_share_acl after the rename", name, newName))
		}
	}

	if p, ok := args["path"].(string); ok {
		if why := sharePathRefusal(p); why != "" {
			return nil, fmt.Errorf("%s", why)
		}
		if p == curPath {
			notes = append(notes, fmt.Sprintf("the share path is already %s", p))
		} else {
			pathBind, why, err := checkNewSharePath(ctx, client, p)
			if err != nil {
				return nil, err
			}
			bind["new_path"] = pathBind
			if why != "" {
				blocked = append(blocked, why)
			}
			payload["path"] = p
			changes = append(changes, shareChange("path", curPath, p))
			warnings = append(warnings, fmt.Sprintf("The share will serve %s instead of %s: clients see that directory's files and permissions, whoever owns them (this can be another user's or team's data); no files are moved or copied", p, curPath))
		}
	}

	if c, ok := args["comment"].(string); ok {
		cur, _ := share["comment"].(string)
		if c == cur {
			notes = append(notes, "the comment is unchanged")
		} else {
			payload["comment"] = c
			changes = append(changes, shareChange("comment", cur, c))
		}
	}

	for _, key := range []string{"enabled", "readonly", "browsable", "access_based_share_enumeration"} {
		v, ok := args[key].(bool)
		if !ok {
			continue
		}
		cur, curOK := share[key].(bool)
		if curOK && cur == v {
			notes = append(notes, fmt.Sprintf("%s is already %v", key, v))
			continue
		}
		payload[key] = v
		changes = append(changes, shareChange(key, share[key], v))
		switch {
		case key == "enabled" && !v:
			warnings = append(warnings, "SMB clients lose access to the share while it is disabled")
		case key == "readonly" && !v:
			warnings = append(warnings, "SMB clients can create, change and delete files (as far as the share and filesystem ACLs allow)")
		case key == "readonly" && v:
			warnings = append(warnings, "SMB clients can no longer create or change files in the share")
		}
	}

	opts, _ := share["options"].(map[string]interface{})
	newOpts := map[string]interface{}{}
	for k, v := range opts {
		newOpts[k] = v
	}
	optsChanged := false
	for _, key := range []string{"hostsallow", "hostsdeny"} {
		list, given, err := stringListArg(args, key)
		if err != nil {
			return nil, err
		}
		if !given {
			continue
		}
		// Only LegacyOpt accepts the host lists on update; 25.10 also
		// returns them for other purposes, so their presence is not enough.
		if purpose, _ := share["purpose"].(string); purpose != smbLegacyPurpose {
			return nil, fmt.Errorf("SMB share '%s' has purpose %v; %s can be changed only for %s shares", name, share["purpose"], key, smbLegacyPurpose)
		}
		cur := opts[key]
		if sameJSON(listValue(cur), list) {
			notes = append(notes, fmt.Sprintf("%s is unchanged", key))
			continue
		}
		newOpts[key] = list
		optsChanged = true
		changes = append(changes, shareChange(key, listValue(cur), list))
		if key == "hostsallow" && len(list) == 0 {
			warnings = append(warnings, "hostsallow is empty: every host that can reach the server can connect (subject to hostsdeny)")
		}
	}
	if optsChanged {
		// sharing.smb.update requires purpose with options and replaces
		// options as a whole, so the current purpose and every other
		// option are sent back unchanged.
		payload["purpose"] = share["purpose"]
		payload["options"] = newOpts
	}

	if len(changes) == 0 && len(blocked) == 0 {
		msg := fmt.Sprintf("nothing to change: SMB share '%s' already has these settings", name)
		if len(notes) > 0 {
			msg += " (" + strings.Join(notes, "; ") + ")"
		}
		blocked = append(blocked, msg)
	}
	warnings = append(warnings, notes...)
	warnings = append(warnings, "TrueNAS reloads (or, for some changes, restarts) the SMB service to apply the change")
	warnings = append(warnings, shareVersionWarnings(ctx, client)...)

	shown := map[string]interface{}{}
	for k, v := range payload {
		shown[k] = v
	}
	if o, ok := shown["options"]; ok {
		shown["options"] = pickAllowedMap(o, smbOptionFields)
	}
	host, hostNote := lookupNASHostname(ctx, client)
	current := map[string]interface{}{
		"share":        simplifySMBShare(share),
		"network_path": smbNetworkPath(host, name),
	}
	if hostNote != "" {
		current["hostname_note"] = hostNote
	}
	facts := []string{confirmFact("share", name), confirmFact("path", curPath)}
	facts = append(facts, changes...)
	return &writePlan{
		Tool:         "update_smb_share",
		Method:       sharingSMBUpdateMethod,
		CallParams:   []interface{}{id, payload},
		ParamNames:   []string{"id", "data"},
		TargetName:   name,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Change SMB share '%s'", name),
			Operation:   "update",
			Target:      name,
			Details:     map[string]interface{}{"changes": changes, "set": shown},
		}},
		Warnings:       warnings,
		Blocked:        blocked,
		Bind:           bind,
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Change SMB share %s", name), facts...),
		Result:         map[string]interface{}{"id": id, "changes": changes},
	}, nil
}

func handleUpdateSMBShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planUpdateSMBShare(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeUpdateSMBShare(ctx, client, args, plan)
}

func executeUpdateSMBShare(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if _, err := callPlan(ctx, client, plan); err != nil {
		return "", fmt.Errorf("failed to update SMB share %s: %w; check query_shares before retrying", plan.TargetName, err)
	}
	id, _ := plan.Result["id"].(int)
	payload, _ := plan.CallParams[1].(map[string]interface{})
	response := map[string]interface{}{"updated": true, "id": id, "changes": plan.Result["changes"]}
	share, err := lookupSMBShare(ctx, client, smbShareRef{id: id, hasID: true})
	if err != nil {
		response["note"] = "The update finished, but the share could not be read back: " + boundedErr(err)
		return marshalJSON(response)
	}
	response["share"] = simplifySMBShare(share)
	if diff := shareReadBackDiff(share, payload); len(diff) > 0 {
		response["note"] = fmt.Sprintf("TrueNAS reported success, but these settings read back differently: %s; check query_shares", strings.Join(diff, ", "))
	}
	name, _ := share["name"].(string)
	response["message"] = fmt.Sprintf("Updated SMB share %s", name)
	return marshalJSON(response)
}

// shareReadBackDiff lists the payload keys the share does not read back
// with the value sent. For options, only the host lists are compared.
func shareReadBackDiff(share, payload map[string]interface{}) []string {
	var diff []string
	for k, v := range payload {
		switch k {
		case "purpose":
			continue
		case "options":
			sent, _ := v.(map[string]interface{})
			got, _ := share["options"].(map[string]interface{})
			for _, o := range []string{"hostsallow", "hostsdeny"} {
				if _, ok := sent[o]; ok && !sameJSON(listValue(got[o]), listValue(sent[o])) {
					diff = append(diff, o)
				}
			}
			continue
		}
		got := share[k]
		if l, ok := v.([]interface{}); ok {
			if !sameJSON(listValue(got), l) {
				diff = append(diff, k)
			}
			continue
		}
		if v == nil {
			if s, _ := got.(string); s != "" {
				diff = append(diff, k)
			}
			continue
		}
		if !sameJSON(got, v) {
			diff = append(diff, k)
		}
	}
	sort.Strings(diff)
	return diff
}

// ---- delete_smb_share ----

func planDeleteSMBShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	ref, err := smbShareRefArg(args)
	if err != nil {
		return nil, err
	}
	share, err := lookupSMBShare(ctx, client, ref)
	if err != nil {
		return nil, err
	}
	id, _ := intID(share["id"])
	name, _ := share["name"].(string)
	p, _ := share["path"].(string)
	host, hostNote := lookupNASHostname(ctx, client)
	networkPath := smbNetworkPath(host, name)
	current := map[string]interface{}{"share": simplifySMBShare(share), "network_path": networkPath}
	if hostNote != "" {
		current["hostname_note"] = hostNote
	}
	return &writePlan{
		Tool:         "delete_smb_share",
		Method:       sharingSMBDeleteMethod,
		CallParams:   []interface{}{id},
		ParamNames:   []string{"id"},
		TargetName:   name,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Delete the configuration of SMB share '%s'; the files in %s are kept", name, p),
			Operation:   "delete",
			Target:      name,
			Details:     map[string]interface{}{"id": id, "name": name, "path": p, "data_deleted": false},
		}},
		Warnings: append([]string{
			fmt.Sprintf("SMB clients lose access to %s: TrueNAS disconnects clients that are using the share, and mapped drives, bookmarks and backup jobs that use it stop working", networkPath),
			fmt.Sprintf("Only the share configuration is removed; the directory %s and every file in it are kept", p),
			"To share the directory again, create a new share with create_smb_share",
		}, shareVersionWarnings(ctx, client)...),
		Blocked: smbShareRefusals(share),
		Bind:    map[string]interface{}{"share": share},
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Delete SMB share %s: clients lose access, files are kept", name),
			confirmFact("share", name), confirmFact("path", p), confirmFact("enabled", share["enabled"])),
		Result: map[string]interface{}{"id": id, "path": p},
	}, nil
}

func handleDeleteSMBShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planDeleteSMBShare(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeDeleteSMBShare(ctx, client, args, plan)
}

func executeDeleteSMBShare(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	id, _ := plan.Result["id"].(int)
	return shareDeleteResult(ctx, client, plan, "SMB", func() ([]map[string]interface{}, error) {
		return queryShareRows(ctx, client, false, []interface{}{[]interface{}{"id", "=", id}}, []interface{}{"id"}, rowHasID(id))
	})
}

// shareDeleteResult runs a delete plan and reads the share back.
func shareDeleteResult(ctx context.Context, client *truenas.Client, plan *writePlan, kind string, readBack func() ([]map[string]interface{}, error)) (string, error) {
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to delete %s share %s: %w; check query_shares before retrying", kind, plan.TargetName, err)
	}
	var ok bool
	if err := json.Unmarshal(result, &ok); err != nil || !ok {
		return "", fmt.Errorf("%s returned an unexpected result; check whether %s share %s still exists with query_shares before retrying", plan.Method, kind, plan.TargetName)
	}
	p, _ := plan.Result["path"].(string)
	response := map[string]interface{}{
		"deleted": true, "id": plan.Result["id"], "share": plan.TargetName, "path": p, "data_deleted": false,
		"message": fmt.Sprintf("Deleted %s share %s; the files in %s are kept", kind, plan.TargetName, p),
	}
	rows, err := readBack()
	switch {
	case err != nil:
		response["note"] = "The delete finished, but the shares could not be read back: " + boundedErr(err)
	case len(rows) > 0:
		response["note"] = fmt.Sprintf("TrueNAS reported success, but %s share %s is still listed; check query_shares", kind, plan.TargetName)
	}
	return marshalJSON(response)
}

// ---- update_nfs_share ----

// nfsExposureWarnings are the explicit warnings for an export that, after
// the change, maps every client to root, keeps root as root, or has no
// network or host limits. These are warnings only: no flag is needed, so
// they are also shown in the confirmation. Each is given only when the
// change touches the settings it is about.
func nfsExposureWarnings(after map[string]interface{}, limitsChanged, mappingChanged bool) []string {
	var out []string
	open := len(listValue(after["networks"])) == 0 && len(listValue(after["hosts"])) == 0
	if open && (limitsChanged || mappingChanged) {
		out = append(out, "WARNING: the export has no network or host limits afterwards: every client that can reach the server can mount it")
	}
	if !mappingChanged {
		return out
	}
	mapallUser, _ := after["mapall_user"].(string)
	maprootUser, _ := after["maproot_user"].(string)
	maprootGroup, _ := after["maproot_group"].(string)
	switch {
	case nfsRootIDName(mapallUser, false):
		out = append(out, "WARNING: every client user is mapped to root (mapall_user root): anyone who can mount the export has root access to every file in it")
	case mapallUser == "" && nfsRootIDName(maprootUser, false) && nfsRootIDName(maprootGroup, true):
		out = append(out, "WARNING: root on a client stays root on the export (no_root_squash): a client's root user has root access to every file in it")
	}
	return out
}

// shareVersionWarnings warns when the connected TrueNAS is not 25.10.x, or
// its version cannot be read: the share option shapes these tools send
// were only checked against 25.10.
func shareVersionWarnings(ctx context.Context, client *truenas.Client) []string {
	if client == nil {
		return nil
	}
	v, err, ok := client.SessionVersion(ctx)
	switch {
	case !ok || err != nil || v.IsZero():
		return []string{"The TrueNAS version could not be read; the share settings and option shapes these tools send were only checked against TrueNAS 25.10, so check the result with query_shares"}
	case v.Major == 25 && v.Minor == 10:
		return nil
	}
	return []string{fmt.Sprintf("The share settings and option shapes these tools send were only checked against TrueNAS 25.10; the connected system runs %s, so check the result with query_shares", v)}
}

// nfsMapFields are the NFS user and group mapping settings; "" clears one.
var nfsMapFields = []string{"maproot_user", "maproot_group", "mapall_user", "mapall_group"}

func planUpdateNFSShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	id, err := nfsShareIDArg(args)
	if err != nil {
		return nil, err
	}
	share, err := lookupNFSShare(ctx, client, id)
	if err != nil {
		return nil, err
	}
	curPath, _ := share["path"].(string)
	blocked := nfsShareRefusals(share)
	payload := map[string]interface{}{}
	var changes, notes, warnings []string
	bind := map[string]interface{}{"share": share}

	if p, ok := args["path"].(string); ok {
		if why := sharePathRefusal(p); why != "" {
			return nil, fmt.Errorf("%s", why)
		}
		if p == curPath {
			notes = append(notes, fmt.Sprintf("the share path is already %s", p))
		} else {
			pathBind, why, err := checkNewSharePath(ctx, client, p)
			if err != nil {
				return nil, err
			}
			bind["new_path"] = pathBind
			if why != "" {
				blocked = append(blocked, why)
			}
			payload["path"] = p
			changes = append(changes, shareChange("path", curPath, p))
			warnings = append(warnings, fmt.Sprintf("The export will serve %s instead of %s, whoever owns its files (this can be another user's or team's data): clients must remount it at the new path, and existing mounts of %s fail; no files are moved or copied", p, curPath, curPath))
		}
	}
	if c, ok := args["comment"].(string); ok {
		cur, _ := share["comment"].(string)
		if c == cur {
			notes = append(notes, "the comment is unchanged")
		} else {
			payload["comment"] = c
			changes = append(changes, shareChange("comment", cur, c))
		}
	}
	for _, key := range []string{"enabled", "ro"} {
		v, ok := args[key].(bool)
		if !ok {
			continue
		}
		if cur, curOK := share[key].(bool); curOK && cur == v {
			notes = append(notes, fmt.Sprintf("%s is already %v", key, v))
			continue
		}
		payload[key] = v
		changes = append(changes, shareChange(key, share[key], v))
		switch {
		case key == "enabled" && !v:
			warnings = append(warnings, "NFS clients lose access to the export while it is disabled")
		case key == "ro" && !v:
			warnings = append(warnings, "NFS clients can create, change and delete files (as far as the filesystem permissions allow)")
		}
	}
	for _, key := range []string{"networks", "hosts", "security"} {
		list, given, err := stringListArg(args, key)
		if err != nil {
			return nil, err
		}
		if !given {
			continue
		}
		for _, it := range list {
			s := it.(string)
			switch key {
			case "networks":
				if err := validateCIDR(s); err != nil {
					return nil, fmt.Errorf("invalid network CIDR '%s': %w", s, err)
				}
			case "hosts":
				if err := validateNFSHost(s); err != nil {
					return nil, fmt.Errorf("invalid host '%s': %w", s, err)
				}
			}
		}
		if sameJSON(listValue(share[key]), list) {
			notes = append(notes, fmt.Sprintf("%s is unchanged", key))
			continue
		}
		payload[key] = list
		changes = append(changes, shareChange(key, listValue(share[key]), list))
	}
	mappingChanged := false
	for _, key := range nfsMapFields {
		v, ok := trimmedArg(args, key)
		if !ok {
			continue
		}
		cur, _ := share[key].(string)
		if v == cur {
			notes = append(notes, fmt.Sprintf("%s is unchanged", key))
			continue
		}
		mappingChanged = true
		if v == "" {
			payload[key] = nil
		} else {
			payload[key] = v
		}
		changes = append(changes, shareChange(key, cur, v))
	}

	after := map[string]interface{}{}
	for k, v := range share {
		after[k] = v
	}
	for k, v := range payload {
		after[k] = v
	}
	_, netChanged := payload["networks"]
	_, hostsChanged := payload["hosts"]
	exposure := nfsExposureWarnings(after, netChanged || hostsChanged, mappingChanged)
	warnings = append(warnings, exposure...)
	if mappingChanged {
		warnings = append(warnings, "After the change: "+nfsRootMappingWarning(after))
	}

	if len(changes) == 0 && len(blocked) == 0 {
		msg := fmt.Sprintf("nothing to change: NFS share %d (%s) already has these settings", id, curPath)
		if len(notes) > 0 {
			msg += " (" + strings.Join(notes, "; ") + ")"
		}
		blocked = append(blocked, msg)
	}
	warnings = append(warnings, notes...)
	warnings = append(warnings, "TrueNAS reloads the NFS exports to apply the change")

	warnings = append(warnings, shareVersionWarnings(ctx, client)...)

	target := fmt.Sprintf("%d (%s)", id, curPath)
	facts := []string{confirmFact("share id", id), confirmFact("path", curPath)}
	facts = append(facts, changes...)
	facts = append(facts, exposure...)
	return &writePlan{
		Tool:         "update_nfs_share",
		Method:       sharingNFSUpdateMethod,
		CallParams:   []interface{}{id, payload},
		ParamNames:   []string{"id", "data"},
		TargetName:   target,
		CurrentState: map[string]interface{}{"share": simplifyNFSShare(share)},
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Change NFS share %s", target),
			Operation:   "update",
			Target:      curPath,
			Details:     map[string]interface{}{"changes": changes, "set": payload},
		}},
		Warnings:       warnings,
		Blocked:        blocked,
		Bind:           bind,
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Change NFS share %s", curPath), facts...),
		Result:         map[string]interface{}{"id": id, "changes": changes},
	}, nil
}

func handleUpdateNFSShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planUpdateNFSShare(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeUpdateNFSShare(ctx, client, args, plan)
}

func executeUpdateNFSShare(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if _, err := callPlan(ctx, client, plan); err != nil {
		return "", fmt.Errorf("failed to update NFS share %s: %w; check query_shares before retrying", plan.TargetName, err)
	}
	id, _ := plan.Result["id"].(int)
	payload, _ := plan.CallParams[1].(map[string]interface{})
	response := map[string]interface{}{"updated": true, "id": id, "changes": plan.Result["changes"]}
	share, err := lookupNFSShare(ctx, client, id)
	if err != nil {
		response["note"] = "The update finished, but the share could not be read back: " + boundedErr(err)
		return marshalJSON(response)
	}
	response["share"] = simplifyNFSShare(share)
	if diff := shareReadBackDiff(share, payload); len(diff) > 0 {
		response["note"] = fmt.Sprintf("TrueNAS reported success, but these settings read back differently: %s; check query_shares", strings.Join(diff, ", "))
	}
	response["message"] = fmt.Sprintf("Updated NFS share %d", id)
	return marshalJSON(response)
}

// ---- delete_nfs_share ----

func planDeleteNFSShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	id, err := nfsShareIDArg(args)
	if err != nil {
		return nil, err
	}
	share, err := lookupNFSShare(ctx, client, id)
	if err != nil {
		return nil, err
	}
	p, _ := share["path"].(string)
	target := fmt.Sprintf("%d (%s)", id, p)
	return &writePlan{
		Tool:         "delete_nfs_share",
		Method:       sharingNFSDeleteMethod,
		CallParams:   []interface{}{id},
		ParamNames:   []string{"id"},
		TargetName:   target,
		CurrentState: map[string]interface{}{"share": simplifyNFSShare(share)},
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Delete the configuration of NFS share %s; the files in %s are kept", target, p),
			Operation:   "delete",
			Target:      p,
			Details:     map[string]interface{}{"id": id, "path": p, "data_deleted": false},
		}},
		Warnings: append([]string{
			fmt.Sprintf("NFS clients lose access to %s: mounts of the export fail and fstab entries or automounts that use it stop working", p),
			fmt.Sprintf("Only the export configuration is removed; the directory %s and every file in it are kept", p),
			"To export the directory again, create a new share with create_nfs_share",
		}, shareVersionWarnings(ctx, client)...),
		Blocked: nfsShareRefusals(share),
		Bind:    map[string]interface{}{"share": share},
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Delete NFS share %s: clients lose access, files are kept", p),
			confirmFact("share id", id), confirmFact("path", p), confirmFact("enabled", share["enabled"])),
		Result: map[string]interface{}{"id": id, "path": p},
	}, nil
}

func handleDeleteNFSShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planDeleteNFSShare(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeDeleteNFSShare(ctx, client, args, plan)
}

func executeDeleteNFSShare(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	id, _ := plan.Result["id"].(int)
	return shareDeleteResult(ctx, client, plan, "NFS", func() ([]map[string]interface{}, error) {
		return queryShareRows(ctx, client, true, []interface{}{[]interface{}{"id", "=", id}}, []interface{}{"id"}, rowHasID(id))
	})
}

// ---- set_share_acl ----

// shareACLWho is a resolved share ACL principal.
type shareACLWho struct {
	Kind string `json:"who_type"`
	Name string `json:"name"`
	ID   *int   `json:"id,omitempty"`
	SID  string `json:"sid"`
}

// shareACLEntry is one entry of a share ACL as it is compared and bound.
type shareACLEntry struct {
	SID  string
	Who  interface{}
	Perm string
	Type string
	// admin is why the principal is an administrative group, or "".
	admin string
}

func (e shareACLEntry) key() string { return e.SID + "|" + e.Type + "|" + e.Perm }

func (e shareACLEntry) shown() map[string]interface{} {
	return map[string]interface{}{"who": e.Who, "sid": e.SID, "type": e.Type, "perm": e.Perm}
}

func (e shareACLEntry) text() string {
	who, _ := e.Who.(string)
	if who == "" {
		who = e.SID
	}
	return fmt.Sprintf("%s %s %s", who, e.Type, e.Perm)
}

func aclKeys(entries []shareACLEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.key()
	}
	sort.Strings(out)
	return out
}

func aclTexts(entries []shareACLEntry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = e.text()
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

func aclShown(entries []shareACLEntry) []interface{} {
	out := make([]interface{}, len(entries))
	for i, e := range entries {
		out[i] = e.shown()
	}
	return out
}

// currentShareACL reads the share ACL with sharing.smb.getacl.
func currentShareACL(ctx context.Context, client *truenas.Client, name string) ([]shareACLEntry, error) {
	raw, err := client.Call(ctx, "sharing.smb.getacl", map[string]interface{}{"share_name": name})
	if err != nil {
		return nil, fmt.Errorf("failed to get the share ACL of SMB share %s: %w", name, err)
	}
	return parseShareACL(raw)
}

func parseShareACL(raw json.RawMessage) ([]shareACLEntry, error) {
	var acl struct {
		ShareACL []map[string]interface{} `json:"share_acl"`
	}
	if err := json.Unmarshal(raw, &acl); err != nil {
		return nil, fmt.Errorf("unexpected share ACL result")
	}
	out := make([]shareACLEntry, 0, len(acl.ShareACL))
	for _, ae := range acl.ShareACL {
		sid, _ := ae["ae_who_sid"].(string)
		perm, _ := ae["ae_perm"].(string)
		typ, _ := ae["ae_type"].(string)
		out = append(out, shareACLEntry{SID: sid, Who: ae["ae_who_str"], Perm: perm, Type: typ})
	}
	return out, nil
}

// smbAccount is a user or group as these lookups read it.
type smbAccount struct {
	Name  string
	ID    int
	IDOK  bool
	SMB   bool
	SID   string
	Group bool
	// Local is true only when the row says local is true: a directory
	// service account, or a row without the field, is not local.
	Local bool
	// Roles are a group's TrueNAS roles; RolesOK is false when the row
	// has no readable roles list.
	Roles   []string
	RolesOK bool
}

// querySMBAccounts looks up local users or groups. The filters are
// narrowed to local accounts; rows are still checked one by one
// (accountSIDRefusal), so a middleware that ignores part of the filter
// cannot let a directory service account through.
func querySMBAccounts(ctx context.Context, client *truenas.Client, group bool, filters []interface{}) ([]smbAccount, error) {
	var raw json.RawMessage
	var err error
	filters = append(append([]interface{}{}, filters...), []interface{}{"local", "=", true})
	if group {
		raw, err = client.Call(ctx, "group.query", filters, map[string]interface{}{"select": []interface{}{"id", "gid", "name", "smb", "sid", "local", "roles"}})
	} else {
		raw, err = client.Call(ctx, "user.query", filters, map[string]interface{}{"select": []interface{}{"id", "uid", "username", "smb", "sid", "local"}})
	}
	kind := "users"
	if group {
		kind = "groups"
	}
	if err != nil {
		return nil, fmt.Errorf("failed to look up %s: %w", kind, err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("unexpected %s lookup result", kind)
	}
	out := make([]smbAccount, 0, len(rows))
	for _, row := range rows {
		a := smbAccount{Group: group}
		if group {
			a.Name, _ = row["name"].(string)
			a.ID, a.IDOK = intID(row["gid"])
			if roles, ok := roleList(row["roles"]); ok {
				a.Roles, a.RolesOK = roles, true
			}
		} else {
			a.Name, _ = row["username"].(string)
			a.ID, a.IDOK = intID(row["uid"])
		}
		a.SMB, _ = row["smb"].(bool)
		a.SID, _ = row["sid"].(string)
		a.Local, _ = row["local"].(bool)
		out = append(out, a)
	}
	return out, nil
}

func unixSID(sid string) bool {
	for _, p := range unixSIDPrefixes {
		if strings.HasPrefix(sid, p) {
			return true
		}
	}
	return false
}

// accountSIDRefusal is why a for-SMB lookup result cannot be used.
func accountSIDRefusal(a smbAccount) string {
	kind := "user"
	if a.Group {
		kind = "group"
	}
	switch {
	case !a.Local:
		return fmt.Sprintf("%s '%s' is not a local %s (a directory service account, or its local flag could not be read); share ACLs set with this tool use local accounts only", kind, a.Name, kind)
	case !a.SMB:
		return fmt.Sprintf("%s '%s' is not an SMB %s (smb is false), so it cannot be used in a share ACL", kind, a.Name, kind)
	case a.SID == "" || unixSID(a.SID):
		return fmt.Sprintf("%s '%s' has no SMB SID, so it cannot be used in a share ACL", kind, a.Name)
	}
	return ""
}

// adminGroupReason is why a is an administrative group, one a DENIED entry
// for can lock administrators out of the share, or "". A group whose roles
// cannot be read counts as one.
func adminGroupReason(a smbAccount) string {
	switch {
	case !a.Group:
		return ""
	case a.SID == builtinAdminsSID || privilegedGroupNames[a.Name]:
		return "is an administrative group"
	case !a.RolesOK:
		return "has roles that could not be read"
	case len(a.Roles) > 0:
		return fmt.Sprintf("grants TrueNAS roles (%s)", strings.Join(a.Roles, ", "))
	}
	return ""
}

// resolveShareACLEntries checks and resolves the entries argument. The
// error is for a malformed argument; the reasons are refusals that depend
// on the system's accounts.
func resolveShareACLEntries(ctx context.Context, client *truenas.Client, args map[string]interface{}) ([]shareACLEntry, []shareACLWho, []string, error) {
	items, _ := args["entries"].([]interface{})
	if len(items) == 0 {
		return nil, nil, nil, fmt.Errorf("entries must list at least one share ACL entry (the complete new ACL)")
	}
	if len(items) > maxShareACLEntries {
		return nil, nil, nil, fmt.Errorf("entries has more than %d entries", maxShareACLEntries)
	}
	var reasons []string
	var entries []shareACLEntry
	var whos []shareACLWho
	var smbAll map[bool][]smbAccount
	for i, it := range items {
		m, ok := it.(map[string]interface{})
		if !ok {
			return nil, nil, nil, fmt.Errorf("entries[%d] must be an object", i)
		}
		kind, _ := m["who_type"].(string)
		who, _ := m["who"].(string)
		who = strings.TrimSpace(who)
		perm, _ := m["perm"].(string)
		typ, _ := m["type"].(string)
		switch perm {
		case "FULL", "CHANGE", "READ":
		default:
			return nil, nil, nil, fmt.Errorf("entries[%d].perm must be FULL, CHANGE or READ", i)
		}
		if typ != "ALLOWED" && typ != "DENIED" {
			return nil, nil, nil, fmt.Errorf("entries[%d].type must be ALLOWED or DENIED", i)
		}
		w := shareACLWho{Kind: kind}
		admin := ""
		switch kind {
		case "everyone":
			if who != "" {
				return nil, nil, nil, fmt.Errorf("entries[%d]: omit who for who_type 'everyone'", i)
			}
			w.Name, w.SID = "everyone", everyoneSID
		case "user", "group":
			if who == "" {
				return nil, nil, nil, fmt.Errorf("entries[%d]: who is required for who_type '%s'", i, kind)
			}
			field := "username"
			if kind == "group" {
				field = "name"
			}
			accts, err := querySMBAccounts(ctx, client, kind == "group", []interface{}{[]interface{}{field, "=", who}})
			if err != nil {
				return nil, nil, nil, err
			}
			var match []smbAccount
			for _, a := range accts {
				if a.Name == who {
					match = append(match, a)
				}
			}
			w.Name = who
			switch {
			case len(match) == 0:
				reasons = append(reasons, fmt.Sprintf("local %s '%s' not found (directory service accounts are refused)", kind, who))
				continue
			case len(match) > 1:
				reasons = append(reasons, fmt.Sprintf("%s name '%s' matches %d accounts", kind, who, len(match)))
				continue
			}
			if why := accountSIDRefusal(match[0]); why != "" {
				reasons = append(reasons, why)
				continue
			}
			if match[0].IDOK {
				id := match[0].ID
				w.ID = &id
			}
			w.SID = match[0].SID
			admin = adminGroupReason(match[0])
		case "sid":
			if !sidPattern.MatchString(who) {
				return nil, nil, nil, fmt.Errorf("entries[%d]: who %q is not a SID (S-1-...)", i, who)
			}
			if unixSID(who) {
				reasons = append(reasons, fmt.Sprintf("SID %s is a Unix-encoded SID (S-1-22-...), which share ACLs do not accept; enable SMB for the account and use its SMB SID", who))
				continue
			}
			w.SID = who
			if who == everyoneSID {
				w.Name = "everyone"
				break
			}
			if smbAll == nil {
				smbAll = map[bool][]smbAccount{}
				for _, g := range []bool{false, true} {
					accts, err := querySMBAccounts(ctx, client, g, []interface{}{[]interface{}{"smb", "=", true}})
					if err != nil {
						return nil, nil, nil, err
					}
					smbAll[g] = accts
				}
			}
			var match []smbAccount
			for _, g := range []bool{false, true} {
				for _, a := range smbAll[g] {
					if a.SMB && a.SID == who {
						match = append(match, a)
					}
				}
			}
			if len(match) == 0 && !strings.HasPrefix(who, "S-1-5-21-") {
				reasons = append(reasons, fmt.Sprintf("SID %s is not a local SMB user or group (of the well-known SIDs, only S-1-1-0, everyone, can be used)", who))
				continue
			}
			if len(match) != 1 {
				reasons = append(reasons, fmt.Sprintf("SID %s does not belong to exactly one local SMB user or group (directory service accounts are refused)", who))
				continue
			}
			if why := accountSIDRefusal(match[0]); why != "" {
				reasons = append(reasons, why)
				continue
			}
			w.Name = match[0].Name
			admin = adminGroupReason(match[0])
			if match[0].IDOK {
				id := match[0].ID
				w.ID = &id
			}
		default:
			return nil, nil, nil, fmt.Errorf("entries[%d].who_type must be everyone, user, group or sid", i)
		}
		whos = append(whos, w)
		entries = append(entries, shareACLEntry{SID: w.SID, Who: w.Name, Perm: perm, Type: typ, admin: admin})
	}
	seen := map[string]string{}
	for _, e := range entries {
		who, _ := e.Who.(string)
		if prev, ok := seen[e.SID]; ok {
			return nil, nil, nil, fmt.Errorf("entries name the same principal twice (%s and %s, SID %s); give one entry per principal", prev, who, e.SID)
		}
		seen[e.SID] = who
	}
	return entries, whos, reasons, nil
}

// shareACLLockout is why acl can lock every user, or the administrators,
// out of the share, or "".
func shareACLLockout(acl []shareACLEntry) string {
	full := false
	var why []string
	for _, e := range acl {
		if e.Type == "ALLOWED" && e.Perm == "FULL" {
			full = true
		}
		if e.Type == "DENIED" && e.SID == everyoneSID {
			why = append(why, fmt.Sprintf("it denies %s to everyone, which overrides every ALLOWED entry", e.Perm))
		}
		if e.Type == "DENIED" && e.admin != "" {
			why = append(why, fmt.Sprintf("it denies %s to group '%v', which %s, so administrators can be locked out", e.Perm, e.Who, e.admin))
		}
	}
	if !full {
		why = append([]string{"no ALLOWED entry grants FULL, so no user can change permissions or take ownership through the share"}, why...)
	}
	return strings.Join(why, "; ")
}

func planSetShareACL(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	ref, err := smbShareRefArg(args)
	if err != nil {
		return nil, err
	}
	allowNoFull, _ := args["allow_no_full_control"].(bool)
	share, err := lookupSMBShare(ctx, client, ref)
	if err != nil {
		return nil, err
	}
	name, _ := share["name"].(string)
	blocked := smbShareRefusals(share)
	entries, whos, reasons, err := resolveShareACLEntries(ctx, client, args)
	if err != nil {
		return nil, err
	}
	blocked = append(blocked, reasons...)
	cur, err := currentShareACL(ctx, client, name)
	if err != nil {
		return nil, err
	}

	var warnings []string
	lockout := ""
	if len(reasons) == 0 {
		lockout = shareACLLockout(entries)
	}
	switch {
	case lockout != "" && !allowNoFull:
		blocked = append(blocked, fmt.Sprintf("the new share ACL can lock every user, or the administrators, out of the share: %s; add an ALLOWED FULL entry for an administrator or group and leave administrative groups out of DENIED entries, or set allow_no_full_control=true after reviewing it", lockout))
	case lockout == "" && allowNoFull && len(reasons) == 0:
		blocked = append(blocked, "allow_no_full_control is set, but the new share ACL grants FULL and denies nothing to everyone or to an administrative group; omit it")
	case lockout != "":
		warnings = append(warnings, "allow_no_full_control: "+lockout)
	}
	if len(blocked) == 0 && sameJSON(aclKeys(cur), aclKeys(entries)) {
		blocked = append(blocked, fmt.Sprintf("nothing to change: SMB share '%s' already has this share ACL", name))
	}
	warnings = append(warnings,
		"The share ACL is replaced as a whole: current entries that are not listed are removed",
		"This changes only the share ACL, which applies to SMB access; the filesystem ACL of the share path is not changed (get_filesystem_acl). The more restrictive of the two wins",
	)
	for _, e := range entries {
		if e.Type == "DENIED" {
			warnings = append(warnings, "A DENIED entry that matches a user or any of their groups overrides ALLOWED entries")
			break
		}
	}
	warnings = append(warnings, shareVersionWarnings(ctx, client)...)

	aces := make([]interface{}, len(entries))
	for i, e := range entries {
		aces[i] = map[string]interface{}{"ae_who_sid": e.SID, "ae_perm": e.Perm, "ae_type": e.Type}
	}
	call := map[string]interface{}{"share_name": name, "share_acl": aces}
	curBind := make([]interface{}, len(cur))
	for i, e := range cur {
		curBind[i] = e.key()
	}
	return &writePlan{
		Tool:       "set_share_acl",
		Method:     sharingSMBSetACLMethod,
		CallParams: []interface{}{call},
		ParamNames: []string{"smb_setacl"},
		TargetName: name,
		CurrentState: map[string]interface{}{
			"share":     shareACLContext(share),
			"share_acl": aclShown(cur),
		},
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Replace the share ACL of SMB share '%s' (%d entries)", name, len(entries)),
			Operation:   "setacl",
			Target:      name,
			Details:     map[string]interface{}{"new_share_acl": aclShown(entries), "filesystem_acl_changed": false},
		}},
		Warnings: warnings,
		Blocked:  blocked,
		Bind:     map[string]interface{}{"share": share, "current_acl": curBind, "principals": whos},
		ConfirmSummary: newConfirmSummary(fmt.Sprintf("Replace the share ACL of SMB share %s", name),
			confirmFact("share", name),
			"current ACL: "+aclTexts(cur),
			"new ACL: "+aclTexts(entries),
			confirmBoolFact("allow_no_full_control", allowNoFull),
		),
		Result: map[string]interface{}{"keys": aclKeys(entries)},
	}, nil
}

func handleSetShareACL(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planSetShareACL(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeSetShareACL(ctx, client, args, plan)
}

func executeSetShareACL(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	raw, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to set the share ACL of SMB share %s: %w; check get_share_acl before retrying", plan.TargetName, err)
	}
	response := map[string]interface{}{"updated": true, "share": plan.TargetName}
	got, err := parseShareACL(raw)
	if err != nil {
		response["note"] = "The share ACL was set, but the result could not be read; check get_share_acl"
	} else {
		response["share_acl"] = aclShown(got)
		if want, _ := plan.Result["keys"].([]string); !sameJSON(aclKeys(got), want) {
			response["note"] = "TrueNAS reported success, but the share ACL reads back differently; check get_share_acl"
		}
	}
	response["message"] = fmt.Sprintf("Replaced the share ACL of SMB share %s", plan.TargetName)
	return marshalJSON(response)
}

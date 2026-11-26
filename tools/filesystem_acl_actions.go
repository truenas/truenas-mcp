package tools

// Filesystem ACL write tool (set_filesystem_acl). It replaces the ACL of
// one file or directory below /mnt/<pool>/... with filesystem.setacl, a
// job tracked as a task. It never strips an ACL, never changes the owner
// or group, never changes the ACL type and never crosses into child
// datasets; a recursive change needs recursive=true, and its preview shows
// the size of the subtree.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	filesystemSetACLMethod = "filesystem.setacl"
)

const (
	// maxFilesystemACLEntries bounds the entries of one filesystem ACL.
	maxFilesystemACLEntries = 64
	// maxShownChildDatasets bounds the child datasets a preview lists.
	maxShownChildDatasets = 20
	// setFilesystemACLTaskTTL is how long a filesystem.setacl job is tracked.
	setFilesystemACLTaskTTL = 24 * time.Hour
	// aclUndefinedID is ACL_UNDEFINED_ID in the middleware: no id, or
	// "keep the current owner or group" for uid and gid.
	aclUndefinedID = -1
)

var (
	nfs4SpecialTags = map[string]bool{"owner@": true, "group@": true, "everyone@": true}
	posixObjectTags = map[string]bool{"USER_OBJ": true, "GROUP_OBJ": true, "OTHER": true, "MASK": true}
	posixPermsArg   = regexp.MustCompile(`^[r-][w-][x-]$`)
)

// filesystemACLRefusedArgs are the set_filesystem_acl arguments refused by
// name, with the reason (see accountArgRefusal).
var filesystemACLRefusedArgs = func() map[string]string {
	const (
		strip = "removing (stripping) an ACL is not supported by this tool"
		owner = "this tool never changes the owner or group of files; only the ACL entries are replaced"
	)
	return map[string]string{
		"stripacl": strip, "strip": strip, "strip_acl": strip,
		"uid": owner, "gid": owner, "user": owner, "group": owner, "owner": owner,
		"traverse":               "recursive changes never cross into child datasets (traverse is always off)",
		"canonicalize":           "NFSv4 entries are always put in canonical order (the TrueNAS default)",
		"validate_effective_acl": "TrueNAS always checks that the principals can reach the path (the TrueNAS default)",
		"acltype":                "the path's current ACL type is always used; changing the ACL type is not supported",
		"dacl":                   "use entries",
		"options":                "use recursive",
		"nfs41_flags":            "NFSv4 ACL flags are not changed by this tool",
	}
}()

func (r *Registry) registerFilesystemACLActionTools() {
	names := func(enum []string, desc string) map[string]interface{} {
		return map[string]interface{}{
			"type": "array", "description": desc, "maxItems": len(enum),
			"items": map[string]interface{}{"type": "string", "enum": enum},
		}
	}
	r.tools["set_filesystem_acl"] = Tool{
		Definition: mcp.Tool{
			Name:        "set_filesystem_acl",
			Description: "Replace the filesystem ACL of one file or directory below /mnt/<pool> with filesystem.setacl, which runs as a job tracked with tasks_get. The new ACL replaces the current one as a whole, in the path's current ACL type: NFSv4 entries (tag owner@, group@, everyone@, USER or GROUP; type ALLOW or DENY; perms FULL_CONTROL, MODIFY, READ or TRAVERSE, or advanced_perms; flags INHERIT or NOINHERIT, or advanced_flags) for an NFS4 ACL, POSIX entries (tag USER_OBJ, GROUP_OBJ, OTHER, MASK, USER or GROUP; posix_perms such as 'rwx' or 'r-x'; default for inherited entries) for a POSIX1E ACL. USER and GROUP entries name a local user or group by who; directory service (Active Directory, LDAP, IPA) users and groups and unknown names are refused. Only the path itself changes unless recursive=true, which applies the ACL to every file and directory below it in the same dataset and needs inheritable NFSv4 entries or POSIX default entries; child datasets are never changed, and the preview shows the size of the subtree (entries directly in the path, objects in use in the dataset, child datasets). Never strips an ACL, changes the owner or group, or changes the ACL type. Refused for pool roots, paths outside /mnt/<pool>, non-normalised paths and paths through a symlink, datasets TrueNAS manages itself (the boot pool, .system, ix-apps, ix-applications, .ix-virt or any dot-prefixed component), read-only or locked datasets, and paths whose ACL type is DISABLED. The preview shows the current ACL and the new one; the confirmation is bound to the path's identity, its dataset and its current ACL, so a change after the preview needs a new one. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1024,
						"description": "Required: Absolute path of a file or directory below /mnt/<pool>, e.g. /mnt/tank/shares/docs (normalised, no symlinks; not a pool root)"},
					"entries": map[string]interface{}{
						"type":        "array",
						"description": "Required: The complete new ACL, at least one entry, in the path's ACL type (get_filesystem_acl shows it)",
						"maxItems":    maxFilesystemACLEntries,
						"items": map[string]interface{}{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]interface{}{
								"tag":            map[string]interface{}{"type": "string", "enum": []string{"owner@", "group@", "everyone@", "USER", "GROUP", "USER_OBJ", "GROUP_OBJ", "OTHER", "MASK"}, "description": "NFSv4: owner@, group@, everyone@, USER or GROUP. POSIX: USER_OBJ, GROUP_OBJ, OTHER, MASK, USER or GROUP"},
								"who":            map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 32, "description": "Local user (tag USER) or group (tag GROUP) name; omit for the other tags"},
								"type":           map[string]interface{}{"type": "string", "enum": []string{"ALLOW", "DENY"}, "description": "NFSv4 only: ALLOW or DENY"},
								"perms":          map[string]interface{}{"type": "string", "enum": []string{"FULL_CONTROL", "MODIFY", "READ", "TRAVERSE"}, "description": "NFSv4 only: a basic permission set. Give perms or advanced_perms"},
								"advanced_perms": names(nfs4PermOrder, "NFSv4 only: the individual permissions, e.g. ['READ_DATA', 'READ_ATTRIBUTES', 'READ_ACL', 'EXECUTE', 'SYNCHRONIZE']"),
								"flags":          map[string]interface{}{"type": "string", "enum": []string{"INHERIT", "NOINHERIT"}, "description": "NFSv4 only: basic inheritance flags (directories only for INHERIT). Give flags or advanced_flags"},
								"advanced_flags": names(nfs4FlagOrder, "NFSv4 only: the individual inheritance flags ([] for none)"),
								"posix_perms":    map[string]interface{}{"type": "string", "minLength": 3, "maxLength": 3, "description": "POSIX only: read, write and execute as three characters, e.g. 'rwx', 'r-x' or '---'"},
								"default":        map[string]interface{}{"type": "boolean", "description": "POSIX only: a default entry, inherited by new files and directories (directories only; default: false)"},
							},
							"required": []string{"tag"},
						},
					},
					"recursive": map[string]interface{}{"type": "boolean", "default": false,
						"description": "Optional: Also apply the ACL to every file and directory below path in the same dataset (default: false); they get the inherited ACL, so the entries must include inheritable NFSv4 entries or POSIX default entries. Child datasets are never changed. The preview shows the size of the subtree"},
					"dry_run": map[string]interface{}{"type": "boolean", "default": false, "description": "Optional: Preview without changing the ACL (default: false)"},
				},
				"required": []string{"path", "entries"},
			},
		},
		Handler: r.handleSetFilesystemACL,
		Plan:    planSetFilesystemACL,
		Execute: r.executeSetFilesystemACL,
	}
}

// aclWritePathRefusal is why p, already validated by validateACLPath, is
// not a path set_filesystem_acl changes, or "".
func aclWritePathRefusal(p string) string {
	rest := strings.TrimPrefix(p, "/mnt/")
	pool, sub, _ := strings.Cut(rest, "/")
	if sub == "" {
		return fmt.Sprintf("path %s is the root of pool '%s'; set the ACL of a dataset or directory below it", p, pool)
	}
	if part, ok := managedDatasetPart(strings.ToLower(rest)); ok {
		return fmt.Sprintf("path %s is inside '%s', which TrueNAS manages itself", p, part)
	}
	return ""
}

// aclPathFS is what set_filesystem_acl reads about the path's filesystem.
type aclPathFS struct {
	Source, FSType, Dest, FSID string
	Flags                      []string
	Objects                    int64 // objects in use, -1 when unknown
}

func readACLPathFS(ctx context.Context, client *truenas.Client, p string) (aclPathFS, error) {
	fs := aclPathFS{Objects: -1}
	raw, err := client.Call(ctx, "filesystem.statfs", p)
	if err != nil {
		return fs, fmt.Errorf("failed to read the filesystem of %s (filesystem.statfs): %w", p, err)
	}
	var st map[string]interface{}
	if err := json.Unmarshal(raw, &st); err != nil || st == nil {
		return fs, fmt.Errorf("unexpected filesystem.statfs result for %s", p)
	}
	fs.Source, _ = st["source"].(string)
	fs.FSType, _ = st["fstype"].(string)
	fs.Dest, _ = st["dest"].(string)
	fs.FSID, _ = st["fsid"].(string)
	fs.Flags = stringList(st["flags"])
	sort.Strings(fs.Flags)
	files, ok1 := st["files"].(float64)
	free, ok2 := st["free_files"].(float64)
	if ok1 && ok2 && files >= free {
		fs.Objects = int64(files - free)
	}
	return fs, nil
}

func (fs aclPathFS) bind() map[string]interface{} {
	return map[string]interface{}{"source": fs.Source, "fstype": fs.FSType, "dest": fs.Dest, "fsid": fs.FSID, "flags": fs.Flags}
}

// childDataset is a dataset below the path's dataset.
type childDataset struct {
	Name, Mountpoint string
	Locked           interface{}
}

// datasetAndChildren reads the path's dataset and every dataset below it.
// The name prefix filter also matches siblings such as tank/data2 for
// tank/data; they are dropped here.
func datasetAndChildren(ctx context.Context, client *truenas.Client, ds string) (map[string]interface{}, []childDataset, error) {
	raw, err := client.Call(ctx, "pool.dataset.query",
		[]interface{}{[]interface{}{"name", "^", ds}},
		map[string]interface{}{"select": []interface{}{"id", "name", "mountpoint", "locked"}, "order_by": []interface{}{"name"}},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query dataset %s: %w", ds, err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, nil, fmt.Errorf("unexpected pool.dataset.query result")
	}
	var own map[string]interface{}
	var children []childDataset
	for _, row := range rows {
		name, _ := row["name"].(string)
		mp, _ := row["mountpoint"].(string)
		switch {
		case name == ds:
			own = map[string]interface{}{"name": name, "mountpoint": mp, "locked": row["locked"]}
		case strings.HasPrefix(name, ds+"/"):
			children = append(children, childDataset{Name: name, Mountpoint: mp, Locked: row["locked"]})
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	return own, children, nil
}

// below reports whether p is mp or inside it.
func below(p, mp string) bool {
	return mp != "" && (p == mp || strings.HasPrefix(p, mp+"/"))
}

// currentFilesystemACL reads the ACL of p with filesystem.getacl, not
// simplified, with names resolved.
func currentFilesystemACL(ctx context.Context, client *truenas.Client, p string) (map[string]interface{}, error) {
	raw, err := client.Call(ctx, "filesystem.getacl", p, false, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get the ACL of %s: %w", p, err)
	}
	var acl map[string]interface{}
	if err := json.Unmarshal(raw, &acl); err != nil || acl == nil {
		return nil, fmt.Errorf("unexpected filesystem.getacl result for %s", p)
	}
	return acl, nil
}

// fsACLEntry is one parsed entry of the entries argument.
type fsACLEntry struct {
	Tag, Who string
	Default  bool
	// Inherits is true for an NFSv4 entry with FILE_INHERIT or
	// DIRECTORY_INHERIT (basic flags INHERIT).
	Inherits bool
	// Payload is the dacl entry sent to filesystem.setacl, without id for
	// USER and GROUP entries until the principal is resolved.
	Payload map[string]interface{}
	// Text is the entry as the preview shows it.
	Text string
}

func boolSet(list []interface{}) (map[string]interface{}, []string, error) {
	out := map[string]interface{}{}
	var names []string
	for _, it := range list {
		s, _ := it.(string)
		if out[s] == true {
			return nil, nil, fmt.Errorf("lists %s twice", s)
		}
		out[s] = true
		names = append(names, s)
	}
	return out, names, nil
}

func parseNFS4Entry(i int, m map[string]interface{}, isDir bool) (fsACLEntry, error) {
	at := func(format string, a ...interface{}) error {
		return fmt.Errorf("entries[%d]: %s", i, fmt.Sprintf(format, a...))
	}
	tag, _ := m["tag"].(string)
	if !nfs4SpecialTags[tag] && tag != "USER" && tag != "GROUP" {
		return fsACLEntry{}, at("tag %s is not an NFSv4 tag (owner@, group@, everyone@, USER, GROUP); the path has an NFS4 ACL", tag)
	}
	for _, k := range []string{"posix_perms", "default"} {
		if _, ok := m[k]; ok {
			return fsACLEntry{}, at("%s is for POSIX ACLs only; the path has an NFS4 ACL", k)
		}
	}
	who, _ := m["who"].(string)
	who = strings.TrimSpace(who)
	if err := whoForTag(tag, who, nfs4SpecialTags[tag]); err != nil {
		return fsACLEntry{}, at("%v", err)
	}
	typ, _ := m["type"].(string)
	if typ != "ALLOW" && typ != "DENY" {
		return fsACLEntry{}, at("type ALLOW or DENY is required for NFSv4 entries")
	}
	e := fsACLEntry{Tag: tag, Who: who, Payload: map[string]interface{}{"tag": tag, "type": typ}}
	basic, hasBasic := m["perms"].(string)
	adv, hasAdv := m["advanced_perms"].([]interface{})
	var permText string
	switch {
	case hasBasic == hasAdv:
		return fsACLEntry{}, at("give exactly one of perms or advanced_perms")
	case hasBasic:
		e.Payload["perms"] = map[string]interface{}{"BASIC": basic}
		permText = basic
	default:
		if len(adv) == 0 {
			return fsACLEntry{}, at("advanced_perms must list at least one permission")
		}
		set, list, err := boolSet(adv)
		if err != nil {
			return fsACLEntry{}, at("advanced_perms %v", err)
		}
		e.Payload["perms"] = set
		permText = strings.Join(list, "|")
	}
	basicFlags, hasBasicFlags := m["flags"].(string)
	advFlags, hasAdvFlags := m["advanced_flags"].([]interface{})
	flagText := ""
	inherits := false
	switch {
	case hasBasicFlags == hasAdvFlags:
		return fsACLEntry{}, at("give exactly one of flags or advanced_flags")
	case hasBasicFlags:
		e.Payload["flags"] = map[string]interface{}{"BASIC": basicFlags}
		flagText = basicFlags
		inherits = basicFlags == "INHERIT"
	default:
		set, list, err := boolSet(advFlags)
		if err != nil {
			return fsACLEntry{}, at("advanced_flags %v", err)
		}
		if set["INHERIT_ONLY"] == true && set["FILE_INHERIT"] != true && set["DIRECTORY_INHERIT"] != true {
			return fsACLEntry{}, at("INHERIT_ONLY needs FILE_INHERIT or DIRECTORY_INHERIT")
		}
		inherits = set["FILE_INHERIT"] == true || set["DIRECTORY_INHERIT"] == true || set["INHERIT_ONLY"] == true
		e.Payload["flags"] = set
		flagText = strings.Join(list, "|")
		if flagText == "" {
			flagText = "no flags"
		}
	}
	if inherits && !isDir {
		return fsACLEntry{}, at("inheritance flags apply only to directories, and the path is a file")
	}
	e.Inherits = inherits
	if nfs4SpecialTags[tag] {
		e.Payload["id"] = aclUndefinedID
	}
	e.Text = fmt.Sprintf("%s %s %s (%s)", principalText(tag, who), typ, permText, flagText)
	return e, nil
}

func parsePOSIXEntry(i int, m map[string]interface{}, isDir bool) (fsACLEntry, error) {
	at := func(format string, a ...interface{}) error {
		return fmt.Errorf("entries[%d]: %s", i, fmt.Sprintf(format, a...))
	}
	tag, _ := m["tag"].(string)
	if !posixObjectTags[tag] && tag != "USER" && tag != "GROUP" {
		return fsACLEntry{}, at("tag %s is not a POSIX tag (USER_OBJ, GROUP_OBJ, OTHER, MASK, USER, GROUP); the path has a POSIX1E ACL", tag)
	}
	for _, k := range []string{"type", "perms", "advanced_perms", "flags", "advanced_flags"} {
		if _, ok := m[k]; ok {
			return fsACLEntry{}, at("%s is for NFSv4 ACLs only; the path has a POSIX1E ACL (use posix_perms and default)", k)
		}
	}
	who, _ := m["who"].(string)
	who = strings.TrimSpace(who)
	if err := whoForTag(tag, who, posixObjectTags[tag]); err != nil {
		return fsACLEntry{}, at("%v", err)
	}
	perms, _ := m["posix_perms"].(string)
	if !posixPermsArg.MatchString(perms) {
		return fsACLEntry{}, at("posix_perms is required for POSIX entries, as three characters such as 'rwx' or 'r-x'")
	}
	def, _ := m["default"].(bool)
	if def && !isDir {
		return fsACLEntry{}, at("default entries apply only to directories, and the path is a file")
	}
	e := fsACLEntry{Tag: tag, Who: who, Default: def, Payload: map[string]interface{}{
		"tag": tag, "default": def,
		"perms": map[string]interface{}{"READ": perms[0] == 'r', "WRITE": perms[1] == 'w', "EXECUTE": perms[2] == 'x'},
	}}
	if posixObjectTags[tag] {
		e.Payload["id"] = aclUndefinedID
	}
	scope := "access"
	if def {
		scope = "default"
	}
	e.Text = fmt.Sprintf("%s %s %s", scope, principalText(tag, who), perms)
	return e, nil
}

func whoForTag(tag, who string, special bool) error {
	switch {
	case special && who != "":
		return fmt.Errorf("omit who for tag %s", tag)
	case !special && who == "":
		return fmt.Errorf("who (a local %s name) is required for tag %s", strings.ToLower(tag), tag)
	case !special:
		return validateAccountName("who", who)
	}
	return nil
}

func principalText(tag, who string) string {
	if who == "" {
		return tag
	}
	return fmt.Sprintf("%s %s", strings.ToLower(tag), who)
}

// posixStructureError checks that each POSIX ACL (access, and default if
// any default entry is given) has exactly one USER_OBJ, GROUP_OBJ and
// OTHER entry, at most one MASK, and a MASK when it names users or groups.
func posixStructureError(entries []fsACLEntry) error {
	for _, def := range []bool{false, true} {
		count := map[string]int{}
		n := 0
		for _, e := range entries {
			if e.Default == def {
				count[e.Tag]++
				n++
			}
		}
		if def && n == 0 {
			continue
		}
		scope := "access"
		if def {
			scope = "default"
		}
		for _, tag := range []string{"USER_OBJ", "GROUP_OBJ", "OTHER"} {
			if count[tag] != 1 {
				return fmt.Errorf("the %s ACL needs exactly one %s entry (it has %d)", scope, tag, count[tag])
			}
		}
		if count["MASK"] > 1 {
			return fmt.Errorf("the %s ACL has %d MASK entries; give at most one", scope, count["MASK"])
		}
		if count["USER"]+count["GROUP"] > 0 && count["MASK"] == 0 {
			return fmt.Errorf("the %s ACL names users or groups, so it needs a MASK entry", scope)
		}
	}
	return nil
}

// recursiveInheritError refuses a recursive change whose ACL has nothing
// for the files and directories below the path to inherit: filesystem.setacl
// gives each of them the ACL inherited from the new one, which would be
// empty (NFSv4 entries without FILE_INHERIT or DIRECTORY_INHERIT, or a
// POSIX ACL without default entries).
func recursiveInheritError(acltype string, entries []fsACLEntry) error {
	for _, e := range entries {
		if (acltype == "NFS4" && e.Inherits) || (acltype == "POSIX1E" && e.Default) {
			return nil
		}
	}
	if acltype == "POSIX1E" {
		return fmt.Errorf("recursive needs default entries (default: true): the files and directories below the path get the ACL inherited from the new one, and an ACL without default entries leaves them nothing to inherit; add default entries or leave recursive off")
	}
	return fmt.Errorf("recursive needs at least one inheritable entry (flags INHERIT, or advanced_flags with FILE_INHERIT or DIRECTORY_INHERIT): the files and directories below the path get the ACL inherited from the new one, and with no inheritable entries it would be empty; add inheritance flags or leave recursive off")
}

// nfs4GrantsWriteACL reports whether an ALLOW entry lets someone change
// the ACL afterwards through the filesystem. DENY entries never count.
func nfs4GrantsWriteACL(entries []fsACLEntry) bool {
	for _, e := range entries {
		if e.Payload["type"] != "ALLOW" {
			continue
		}
		perms, _ := e.Payload["perms"].(map[string]interface{})
		if perms["BASIC"] == "FULL_CONTROL" || perms["WRITE_ACL"] == true {
			return true
		}
	}
	return false
}

// currentACLTexts renders the current ACL entries for the preview.
func currentACLTexts(acltype string, aces []interface{}) []map[string]interface{} {
	switch acltype {
	case "NFS4":
		return nfs4Entries(aces)
	case "POSIX1E":
		return posixEntries(aces)
	}
	return []map[string]interface{}{}
}

// uncheckedVersion returns the connected TrueNAS version when it is known
// and not 25.10.x, the release the write tools' request shapes were
// checked against.
func uncheckedVersion(ctx context.Context, client *truenas.Client) (string, bool) {
	if client == nil {
		return "", false
	}
	v, err, ok := client.SessionVersion(ctx)
	if !ok || err != nil || v.IsZero() || (v.Major == 25 && v.Minor == 10) {
		return "", false
	}
	return v.String(), true
}

func planSetFilesystemACL(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	requested, _ := args["path"].(string)
	p, err := validateACLPath(requested)
	if err != nil {
		return nil, err
	}
	if why := aclWritePathRefusal(p); why != "" {
		return nil, fmt.Errorf("%s; nothing was changed", why)
	}
	items, _ := args["entries"].([]interface{})
	if len(items) == 0 {
		return nil, fmt.Errorf("entries must list at least one ACL entry (the complete new ACL); removing an ACL is not supported")
	}
	if len(items) > maxFilesystemACLEntries {
		return nil, fmt.Errorf("entries has more than %d entries", maxFilesystemACLEntries)
	}
	recursive, _ := args["recursive"].(bool)

	target, st, err := resolveACLPath(ctx, client, p)
	if err != nil {
		return nil, fmt.Errorf("path %s cannot be used: %w", p, err)
	}
	if target != p {
		return nil, fmt.Errorf("path %s goes through a symlink (it resolves to %s); give the real path", p, target)
	}
	typ, _ := st["type"].(string)
	switch {
	case typ != "DIRECTORY" && typ != "FILE":
		return nil, fmt.Errorf("path %s is a %v, not a file or directory", p, st["type"])
	case recursive && typ != "DIRECTORY":
		return nil, fmt.Errorf("recursive applies only to directories, and %s is a file", p)
	}
	isDir := typ == "DIRECTORY"

	fs, err := readACLPathFS(ctx, client, p)
	if err != nil {
		return nil, err
	}
	var blocked []string
	pool, _, _ := strings.Cut(strings.TrimPrefix(p, "/mnt/"), "/")
	dsPool, _, _ := strings.Cut(fs.Source, "/")
	switch {
	case fs.FSType != "zfs":
		blocked = append(blocked, fmt.Sprintf("path %s is not on a ZFS dataset (filesystem type %q)", p, fs.FSType))
	case fs.Source == "" || dsPool != pool:
		blocked = append(blocked, fmt.Sprintf("path %s is on %q, not on a dataset of pool '%s'", p, fs.Source, pool))
	}
	if part, ok := managedDatasetPart(strings.ToLower(fs.Source)); ok {
		blocked = append(blocked, fmt.Sprintf("path %s is on dataset %s, inside '%s', which TrueNAS manages itself", p, fs.Source, part))
	}
	for _, f := range fs.Flags {
		if f == "RO" {
			blocked = append(blocked, fmt.Sprintf("dataset %s is mounted read-only, so its ACLs cannot be changed", fs.Source))
		}
	}
	dataset, children, err := datasetAndChildren(ctx, client, fs.Source)
	if err != nil {
		return nil, err
	}
	switch {
	case dataset == nil:
		blocked = append(blocked, fmt.Sprintf("dataset %s of path %s was not found with pool.dataset.query", fs.Source, p))
	case dataset["locked"] != false:
		blocked = append(blocked, fmt.Sprintf("dataset %s is locked, or its lock state could not be read", fs.Source))
	}
	var inside []childDataset
	for _, c := range children {
		if c.Locked != false && below(p, c.Mountpoint) {
			blocked = append(blocked, fmt.Sprintf("path %s is inside dataset %s, which is locked or whose lock state could not be read", p, c.Name))
		}
		if below(c.Mountpoint, p) {
			inside = append(inside, c)
		}
	}

	acl, err := currentFilesystemACL(ctx, client, p)
	if err != nil {
		return nil, err
	}
	acltype, _ := acl["acltype"].(string)
	curAces, _ := acl["acl"].([]interface{})
	var entries []fsACLEntry
	switch acltype {
	case "NFS4", "POSIX1E":
		for i, it := range items {
			m, ok := it.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("entries[%d] must be an object", i)
			}
			var e fsACLEntry
			if acltype == "NFS4" {
				e, err = parseNFS4Entry(i, m, isDir)
			} else {
				e, err = parsePOSIXEntry(i, m, isDir)
			}
			if err != nil {
				return nil, err
			}
			entries = append(entries, e)
		}
		if acltype == "POSIX1E" {
			if err := posixStructureError(entries); err != nil {
				return nil, err
			}
		}
		if recursive {
			if err := recursiveInheritError(acltype, entries); err != nil {
				return nil, err
			}
		}
	case "DISABLED":
		blocked = append(blocked, fmt.Sprintf("ACLs are disabled on dataset %s (ACL type DISABLED); only the owner, group and mode bits apply, and this tool does not change the ACL type", fs.Source))
	default:
		return nil, fmt.Errorf("filesystem.getacl reported ACL type %q for %s, which this tool does not handle", acltype, p)
	}

	var userNames, groupNames []string
	for _, e := range entries {
		switch e.Tag {
		case "USER":
			userNames = append(userNames, e.Who)
		case "GROUP":
			groupNames = append(groupNames, e.Who)
		}
	}
	users, err := usersByName(ctx, client, userNames)
	if err != nil {
		return nil, err
	}
	groups, err := groupsByName(ctx, client, groupNames)
	if err != nil {
		return nil, err
	}
	principals := map[string]interface{}{}
	dacl := make([]interface{}, 0, len(entries))
	newTexts := make([]string, 0, len(entries))
	for _, e := range entries {
		payload := map[string]interface{}{}
		for k, v := range e.Payload {
			payload[k] = v
		}
		switch e.Tag {
		case "USER":
			u, ok := users[e.Who]
			if !ok {
				blocked = append(blocked, fmt.Sprintf("local user '%s' not found (directory service users and groups are refused)", e.Who))
				continue
			}
			payload["id"] = u.UID
			principals["user:"+e.Who] = map[string]interface{}{"id": u.ID, "uid": u.UID, "local": u.Local}
		case "GROUP":
			g, ok := groups[e.Who]
			if !ok {
				blocked = append(blocked, fmt.Sprintf("local group '%s' not found (directory service users and groups are refused)", e.Who))
				continue
			}
			payload["id"] = g.GID
			principals["group:"+e.Who] = map[string]interface{}{"id": g.ID, "gid": g.GID, "local": g.Local}
		}
		dacl = append(dacl, payload)
		newTexts = append(newTexts, e.Text)
	}

	var warnings []string
	warnings = append(warnings, "The ACL is replaced as a whole: current entries that are not listed are removed")
	if acltype == "NFS4" && len(entries) > 0 && !nfs4GrantsWriteACL(entries) {
		warnings = append(warnings, "No ALLOW entry grants FULL_CONTROL or WRITE_ACL: afterwards only the file's owner (who keeps the right to change the ACL) and root can change this ACL through the filesystem")
	}
	for _, e := range entries {
		if e.Payload["type"] == "DENY" {
			warnings = append(warnings, "A DENY entry that matches a user or any of their groups overrides ALLOW entries that come after it")
			break
		}
	}
	warnings = append(warnings, "The owner and group of the files are not changed")
	subtree := map[string]interface{}{}
	var childNames []string
	for _, c := range inside {
		childNames = append(childNames, c.Name)
	}
	subtreeText := "no: only " + p + " itself"
	bind := map[string]interface{}{
		"stat": map[string]interface{}{
			"realpath": st["realpath"], "type": st["type"], "inode": st["inode"], "dev": st["dev"],
			"mount_id": st["mount_id"], "uid": st["uid"], "gid": st["gid"], "mode": st["mode"],
		},
		"filesystem": fs.bind(),
		"dataset":    dataset,
		"acl":        map[string]interface{}{"acltype": acltype, "acl": acl["acl"], "trivial": acl["trivial"], "aclflags": acl["aclflags"], "uid": acl["uid"], "gid": acl["gid"]},
		"principals": principals,
	}
	if recursive {
		n, err := countDirEntries(ctx, client, p)
		if err != nil {
			blocked = append(blocked, fmt.Sprintf("could not count the entries of %s (%s), so the size of the recursive change cannot be shown", p, boundedErr(err)))
		}
		shown := childNames
		if len(shown) > maxShownChildDatasets {
			shown = shown[:maxShownChildDatasets]
		}
		subtree = map[string]interface{}{
			"dataset":                    fs.Source,
			"dataset_mountpoint":         fs.Dest,
			"entries_directly_in_path":   n,
			"child_datasets_not_changed": shown,
			"child_dataset_count":        len(childNames),
			"traverse":                   false,
		}
		objects := "unknown"
		if fs.Objects >= 0 {
			subtree["dataset_objects_in_use"] = fs.Objects
			objects = fmt.Sprint(fs.Objects)
		}
		subtreeText = fmt.Sprintf("yes: %d entries directly in %s; dataset %s has %s objects in use (an upper bound for the subtree); %d child dataset(s) not changed", n, p, fs.Source, objects, len(childNames))
		bind["child_datasets"] = childNames
		warnings = append(warnings, fmt.Sprintf("Every file and directory below %s in dataset %s gets this ACL (entries that are not inherited apply to %s only, per the inheritance rules); their current ACLs are replaced and cannot be restored by this tool", p, fs.Source, p))
		if len(childNames) > 0 {
			warnings = append(warnings, fmt.Sprintf("Child datasets below %s are not changed (traverse is off): %s", p, strings.Join(shown, ", ")))
		}
		warnings = append(warnings, "A recursive change on a large tree can take a long time; follow the job with tasks_get")
	} else if isDir {
		warnings = append(warnings, fmt.Sprintf("Only %s itself changes: files and directories already in it keep their ACLs, and the new ACL's inheritable entries apply to ones created later (set recursive to change existing ones)", p))
	}
	if v, ok := uncheckedVersion(ctx, client); ok {
		warnings = append(warnings, fmt.Sprintf("The filesystem.setacl request shape this tool sends was only checked against TrueNAS 25.10; the connected system runs %s, so check the result with get_filesystem_acl", v))
	}

	options := map[string]interface{}{
		"stripacl": false, "recursive": recursive, "traverse": false,
		"canonicalize": true, "validate_effective_acl": true,
	}
	call := map[string]interface{}{
		"path": p, "dacl": dacl, "options": options,
		"uid": aclUndefinedID, "gid": aclUndefinedID, "acltype": acltype,
	}
	current := map[string]interface{}{
		"path": p, "type": typ, "dataset": fs.Source, "acltype": acltype,
		"trivial": acl["trivial"], "entries": currentACLTexts(acltype, curAces),
		"uid": acl["uid"], "user": acl["user"], "gid": acl["gid"], "group": acl["group"],
	}
	details := map[string]interface{}{
		"path": p, "acltype": acltype, "new_acl": newTexts, "recursive": recursive,
		"stripacl": false, "owner_changed": false, "child_datasets_changed": false,
	}
	if recursive {
		details["subtree"] = subtree
	}
	curTexts := make([]string, 0, len(curAces))
	for _, e := range currentACLTexts(acltype, curAces) {
		b, _ := json.Marshal(e)
		curTexts = append(curTexts, string(b))
	}
	headline := fmt.Sprintf("Replace the ACL of %s", p)
	if recursive {
		headline = fmt.Sprintf("Replace the ACL of %s and everything below it in dataset %s", p, fs.Source)
	}
	return &writePlan{
		Tool:         "set_filesystem_acl",
		Method:       filesystemSetACLMethod,
		CallParams:   []interface{}{call},
		ParamNames:   []string{"filesystem_acl"},
		JobMethod:    filesystemSetACLMethod,
		TargetName:   p,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: headline,
			Operation:   "setacl",
			Target:      p,
			Details:     details,
		}},
		Warnings: warnings,
		Blocked:  blocked,
		Bind:     bind,
		ConfirmSummary: newConfirmSummary(headline,
			confirmFact("path", p),
			confirmFact("dataset", fs.Source),
			confirmFact("acl type", acltype),
			confirmFact("recursive", subtreeText),
			"current ACL: "+strings.Join(curTexts, "; "),
			"new ACL: "+strings.Join(newTexts, "; "),
		),
		Result: map[string]interface{}{"recursive": recursive, "dataset": fs.Source},
	}, nil
}

// countDirEntries counts the entries directly in directory p.
func countDirEntries(ctx context.Context, client *truenas.Client, p string) (int, error) {
	raw, err := client.Call(ctx, "filesystem.listdir", p, []interface{}{}, map[string]interface{}{"count": true})
	if err != nil {
		return 0, err
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
		return 0, fmt.Errorf("unexpected filesystem.listdir count")
	}
	return int(n), nil
}

func (r *Registry) handleSetFilesystemACL(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planSetFilesystemACL(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeSetFilesystemACL(ctx, client, args, plan)
}

func (r *Registry) executeSetFilesystemACL(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to set the ACL of %s: %w; check get_filesystem_acl and query_jobs before retrying", plan.TargetName, err)
	}
	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("filesystem.setacl returned no job id for %s; check get_filesystem_acl and query_jobs before retrying", plan.TargetName)
	}
	recursive, _ := plan.Result["recursive"].(bool)
	response := map[string]interface{}{"path": plan.TargetName, "recursive": recursive, "job_id": jobID}
	msg := fmt.Sprintf("Setting the ACL of %s started (job %d)", plan.TargetName, jobID)
	if recursive {
		msg = fmt.Sprintf("Setting the ACL of %s and everything below it in dataset %v started (job %d)", plan.TargetName, plan.Result["dataset"], jobID)
	}
	if r.taskManager != nil {
		opts := []tasks.TaskOption{}
		if plan.JobMethod != "" {
			opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
		}
		task, err := r.taskManager.CreateJobTask(ctx, "set_filesystem_acl", args, jobID, setFilesystemACLTaskTTL, opts...)
		if err != nil {
			response["note"] = "The job started, but it could not be tracked as a task: " + boundedErr(err) + "; follow it with query_jobs"
		} else {
			response["task_id"] = task.TaskID
			response["task_status"] = task.Status
			response["poll_interval"] = task.PollInterval
			msg += fmt.Sprintf(". Track the job with tasks_get task_id %s", task.TaskID)
		}
	}
	response["message"] = msg + ". Check the result with get_filesystem_acl when it finishes"
	return marshalJSON(response)
}

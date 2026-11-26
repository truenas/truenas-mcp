package tools

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/truenas/truenas-mcp/redact"
)

const (
	fakeUserSID  = "S-1-5-21-1111111111-2222222222-3333333333-1001"
	fakeGroupSID = "S-1-5-21-1111111111-2222222222-3333333333-2001"
	fakeLostSID  = "S-1-5-21-1111111111-2222222222-3333333333-9999"
)

func teamDocsShare() map[string]interface{} {
	return map[string]interface{}{
		"id": 3.0, "name": "TeamDocs", "path": "/mnt/tank/shares/docs", "enabled": true,
		"readonly": false, "browsable": true, "access_based_share_enumeration": false,
		"locked": false, "purpose": "DEFAULT_SHARE",
		"options": map[string]interface{}{
			"hostsallow": []interface{}{"192.0.2.0/24"}, "hostsdeny": []interface{}{},
			"guestok": false, "auxsmbconf": "sentinel-aux-param = 1",
			"path_suffix": "sentinel-suffix/%U",
		},
	}
}

// teamDocsACL models a 25.10 sharing.smb.getacl result: entries for a
// mapped user, a mapped group, an unmapped SID and everyone@.
func teamDocsACL() map[string]interface{} {
	return map[string]interface{}{
		"share_name": "TeamDocs",
		"share_acl": []interface{}{
			map[string]interface{}{"ae_who_sid": fakeUserSID, "ae_perm": "READ", "ae_type": "DENIED",
				"ae_who_id": map[string]interface{}{"id_type": "USER", "id": 3001.0}, "ae_who_str": "exampleuser"},
			map[string]interface{}{"ae_who_sid": fakeGroupSID, "ae_perm": "CHANGE", "ae_type": "ALLOWED",
				"ae_who_id": map[string]interface{}{"id_type": "GROUP", "id": 4001.0}, "ae_who_str": "examplegroup"},
			map[string]interface{}{"ae_who_sid": fakeLostSID, "ae_perm": "FULL", "ae_type": "ALLOWED",
				"ae_who_id": nil, "ae_who_str": nil},
			map[string]interface{}{"ae_who_sid": "S-1-1-0", "ae_perm": "READ", "ae_type": "ALLOWED",
				"ae_who_id": nil, "ae_who_str": "everyone@"},
		},
	}
}

func notesContain(resp map[string]interface{}, sub string) bool {
	notes, _ := resp["notes"].([]interface{})
	for _, n := range notes {
		if s, _ := n.(string); strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestGetShareACLByName(t *testing.T) {
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"sharing.smb.query":  []interface{}{teamDocsShare()},
		"sharing.smb.getacl": teamDocsACL(),
	}, nil)
	r := NewRegistry(client, nil)
	out, err := r.CallTool(t.Context(), "get_share_acl", map[string]interface{}{"share_name": "teamdocs"})
	if err != nil {
		t.Fatal(err)
	}
	if final := redact.ToolOutput(out); final != out {
		t.Errorf("redaction backstop changed the output:\nbefore: %s\nafter: %s", out, final)
	}
	if strings.Contains(out, "auxsmbconf") || strings.Contains(out, "sentinel-aux-param") {
		t.Errorf("output contains auxsmbconf:\n%s", out)
	}
	if strings.Contains(out, "path_suffix") || strings.Contains(out, "sentinel-suffix") {
		t.Errorf("output contains path_suffix:\n%s", out)
	}

	q := lastParams(t, fake, "sharing.smb.query")
	if got, _ := json.Marshal(q[0]); string(got) != `[["name","C=","teamdocs"]]` {
		t.Errorf("query filters = %s", got)
	}
	g := lastParams(t, fake, "sharing.smb.getacl")
	if got, _ := json.Marshal(g); string(got) != `[{"share_name":"TeamDocs"}]` {
		t.Errorf("getacl params = %s, want the share's own name", got)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	share := resp["share"].(map[string]interface{})
	if share["path"] != "/mnt/tank/shares/docs" || share["readonly"] != false {
		t.Errorf("share = %v", share)
	}
	if allow, _ := share["hostsallow"].([]interface{}); len(allow) != 1 || allow[0] != "192.0.2.0/24" {
		t.Errorf("hostsallow = %v", share["hostsallow"])
	}
	entries := resp["share_acl"].([]interface{})
	if len(entries) != 4 || resp["count"] != 4.0 {
		t.Fatalf("share_acl = %v", entries)
	}
	first := entries[0].(map[string]interface{})
	for k, want := range map[string]interface{}{"who": "exampleuser", "who_sid": fakeUserSID, "type": "DENIED", "perm": "READ", "id_type": "USER", "unix_id": 3001.0, "perm_meaning": "read and list only"} {
		if first[k] != want {
			t.Errorf("entry 0 %s = %v, want %v", k, first[k], want)
		}
	}
	if lost := entries[2].(map[string]interface{}); lost["who"] != nil || lost["who_sid"] != fakeLostSID {
		t.Errorf("unmapped entry = %v", lost)
	}
	for _, sub := range []string{"get_filesystem_acl", "DENIED entry", "could not be mapped"} {
		if !notesContain(resp, sub) {
			t.Errorf("notes missing %q: %v", sub, resp["notes"])
		}
	}
	if notesContain(resp, "default share ACL") {
		t.Errorf("notes call a restricted ACL the default: %v", resp["notes"])
	}
}

func TestGetShareACLByID(t *testing.T) {
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"sharing.smb.query": []interface{}{teamDocsShare()},
		"sharing.smb.getacl": map[string]interface{}{"share_name": "TeamDocs", "share_acl": []interface{}{
			map[string]interface{}{"ae_who_sid": "S-1-1-0", "ae_perm": "FULL", "ae_type": "ALLOWED", "ae_who_id": nil, "ae_who_str": "everyone@"},
		}},
	}, nil)
	r := NewRegistry(client, nil)
	resp := callTool(t, r, "get_share_acl", map[string]interface{}{"share_id": 3.0})
	q := lastParams(t, fake, "sharing.smb.query")
	if got, _ := json.Marshal(q[0]); string(got) != `[["id","=",3]]` {
		t.Errorf("query filters = %s", got)
	}
	if got, _ := json.Marshal(lastParams(t, fake, "sharing.smb.getacl")); string(got) != `[{"share_name":"TeamDocs"}]` {
		t.Errorf("getacl params = %s", got)
	}
	if !notesContain(resp, "default share ACL") {
		t.Errorf("notes do not flag the default ACL: %v", resp["notes"])
	}
}

func TestGetShareACLHomes(t *testing.T) {
	home := teamDocsShare()
	home["name"] = "users"
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"sharing.smb.query":  []interface{}{home},
		"sharing.smb.getacl": teamDocsACL(),
	}, nil)
	r := NewRegistry(client, nil)
	callTool(t, r, "get_share_acl", map[string]interface{}{"share_name": "homes"})
	if got, _ := json.Marshal(lastParams(t, fake, "sharing.smb.query")[0]); string(got) != `[["options.home","=",true]]` {
		t.Errorf("query filters = %s", got)
	}
	if got, _ := json.Marshal(lastParams(t, fake, "sharing.smb.getacl")); string(got) != `[{"share_name":"HOMES"}]` {
		t.Errorf("getacl params = %s", got)
	}
}

func TestGetShareACLArgumentErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"neither", map[string]interface{}{}, "share_name or share_id is required"},
		{"blank name", map[string]interface{}{"share_name": "  "}, "share_name or share_id is required"},
		{"both", map[string]interface{}{"share_name": "TeamDocs", "share_id": 3.0}, "not both"},
		{"unknown name", map[string]interface{}{"share_name": "NoSuchShare"}, `SMB share "NoSuchShare" not found`},
		{"unknown id", map[string]interface{}{"share_id": 99.0}, "SMB share id 99 not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFakeClientWith(t, map[string]interface{}{"sharing.smb.query": []interface{}{}}, nil)
			r := NewRegistry(client, nil)
			_, err := r.CallTool(t.Context(), "get_share_acl", tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if calls := fake.called("sharing.smb.getacl"); len(calls) != 0 {
				t.Errorf("getacl was called: %v", calls)
			}
		})
	}
}

func dirStat(realpath string) map[string]interface{} {
	return map[string]interface{}{
		"realpath": realpath, "type": "DIRECTORY", "mode": float64(0o40770),
		"uid": 3001.0, "gid": 4001.0, "user": "exampleuser", "group": "examplegroup", "acl": true,
		"is_mountpoint": false,
	}
}

func mountStat(p string) map[string]interface{} {
	st := dirStat(p)
	st["is_mountpoint"] = true
	return st
}

// linkStat models 25.10 filesystem.stat of a symlink: the link is not
// followed and realpath is fully resolved.
func linkStat(at, target string) map[string]interface{} {
	return map[string]interface{}{"realpath": target, "type": "SYMLINK", "mode": float64(0o120777),
		"uid": 0.0, "gid": 0.0, "user": "root", "group": "root", "acl": false, "is_mountpoint": false}
}

// newACLFake serves filesystem.stat per path, like 25.10 does for a path
// with no symlinks in it: /mnt/tank and /mnt/other are pool mountpoints,
// any other path under them is a directory, and fs overrides or adds
// entries. Paths elsewhere fail with ENOENT, so a test sees if the tool
// stats a path outside the pools. After maxACLFakeStats calls every stat
// fails, so a walk that never ends fails its test instead of hanging.
func newACLFake(t *testing.T, results map[string]interface{}, fs map[string]map[string]interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{results: results}
	var stats atomic.Int64
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "filesystem.stat" {
			return nil, nil, false
		}
		if stats.Add(1) > maxACLFakeStats {
			return nil, map[string]interface{}{"code": -32001, "message": "fake stat call budget exhausted"}, true
		}
		var args []string
		if err := json.Unmarshal(params, &args); err != nil || len(args) != 1 {
			return nil, map[string]interface{}{"code": -32602, "message": "bad params"}, true
		}
		p := args[0]
		if st, ok := fs[p]; ok {
			return st, nil, true
		}
		switch {
		case p == "/mnt/tank" || p == "/mnt/other":
			return mountStat(p), nil, true
		case strings.HasPrefix(p, "/mnt/tank/") || strings.HasPrefix(p, "/mnt/other/"):
			return dirStat(p), nil, true
		}
		return nil, map[string]interface{}{"code": -32001, "message": "[ENOENT] Path " + p + " not found"}, true
	}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

// maxACLFakeStats bounds filesystem.stat calls per newACLFake, well above
// what any bounded walk needs (maxACLPathDepth components plus
// maxACLSymlinkHops links, each with a pool root).
const maxACLFakeStats = 4 * (maxACLPathDepth + maxACLSymlinkHops)

// statPaths returns the paths filesystem.stat was called with, in order.
func statPaths(t *testing.T, fake *fakeMiddleware) []string {
	t.Helper()
	var out []string
	for _, c := range fake.called("filesystem.stat") {
		var args []string
		if err := json.Unmarshal(c.Params, &args); err != nil || len(args) != 1 {
			t.Fatalf("stat params = %s", c.Params)
		}
		out = append(out, args[0])
	}
	return out
}

// nfs4ACL models a 25.10 filesystem.getacl NFS4 result with resolve_ids.
func nfs4ACL(simplified bool) map[string]interface{} {
	full := map[string]interface{}{"READ_DATA": true, "WRITE_DATA": true, "APPEND_DATA": true, "READ_NAMED_ATTRS": true,
		"WRITE_NAMED_ATTRS": true, "EXECUTE": true, "DELETE": true, "DELETE_CHILD": true, "READ_ATTRIBUTES": true,
		"WRITE_ATTRIBUTES": true, "READ_ACL": true, "WRITE_ACL": true, "WRITE_OWNER": true, "SYNCHRONIZE": true}
	inherit := map[string]interface{}{"FILE_INHERIT": true, "DIRECTORY_INHERIT": true, "NO_PROPAGATE_INHERIT": false, "INHERIT_ONLY": false, "INHERITED": false}
	var ownerPerms, ownerFlags interface{} = full, inherit
	if simplified {
		ownerPerms = map[string]interface{}{"BASIC": "FULL_CONTROL"}
		ownerFlags = map[string]interface{}{"BASIC": "INHERIT"}
	}
	return map[string]interface{}{
		"path": "/mnt/tank/shares/docs", "uid": 3001.0, "gid": 4001.0, "user": nil, "group": "examplegroup",
		"acltype": "NFS4", "trivial": false,
		"aclflags": map[string]interface{}{"autoinherit": false, "protected": false, "defaulted": false},
		"acl": []interface{}{
			map[string]interface{}{"tag": "USER", "id": 3002.0, "who": "otheruser", "type": "DENY",
				"perms": map[string]interface{}{"READ_DATA": false, "WRITE_DATA": true, "APPEND_DATA": true},
				"flags": map[string]interface{}{"FILE_INHERIT": false, "INHERIT_ONLY": false}},
			map[string]interface{}{"tag": "owner@", "id": -1.0, "who": "exampleuser", "type": "ALLOW",
				"perms": ownerPerms, "flags": ownerFlags},
			map[string]interface{}{"tag": "GROUP", "id": 4999.0, "who": nil, "type": "ALLOW",
				"perms": map[string]interface{}{"READ_DATA": true, "EXECUTE": true, "READ_ATTRIBUTES": true},
				"flags": map[string]interface{}{"INHERITED": true}},
			map[string]interface{}{"tag": "everyone@", "id": -1.0, "who": nil, "type": "ALLOW",
				"perms": map[string]interface{}{"READ_ATTRIBUTES": true, "READ_ACL": true, "SYNCHRONIZE": true},
				"flags": map[string]interface{}{}},
		},
	}
}

func TestGetFilesystemACLNFS4(t *testing.T) {
	r, fake := newACLFake(t, map[string]interface{}{"filesystem.getacl": nfs4ACL(false)}, nil)
	out, err := r.CallTool(t.Context(), "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/shares/docs/"})
	if err != nil {
		t.Fatal(err)
	}
	if final := redact.ToolOutput(out); final != out {
		t.Errorf("redaction backstop changed the output:\nbefore: %s\nafter: %s", out, final)
	}
	if got := strings.Join(statPaths(t, fake), " "); got != "/mnt/tank /mnt/tank/shares /mnt/tank/shares/docs" {
		t.Errorf("stat paths = %s, want each component from the pool down", got)
	}
	if got, _ := json.Marshal(lastParams(t, fake, "filesystem.getacl")); string(got) != `["/mnt/tank/shares/docs",false,true]` {
		t.Errorf("getacl params = %s, want [path, simplified=false, resolve_ids=true]", got)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["acltype"] != "NFS4" || resp["mode"] != "0770" || resp["type"] != "DIRECTORY" || resp["path"] != "/mnt/tank/shares/docs" {
		t.Errorf("header = %v", resp)
	}
	if _, ok := resp["resolved_path"]; ok {
		t.Errorf("resolved_path set for a non-symlink: %v", resp["resolved_path"])
	}
	// getacl's user lookup missed (a directory service user); the stat name fills it.
	if owner := resp["owner"].(map[string]interface{}); owner["uid"] != 3001.0 || owner["name"] != "exampleuser" {
		t.Errorf("owner = %v", owner)
	}
	if group := resp["group"].(map[string]interface{}); group["gid"] != 4001.0 || group["name"] != "examplegroup" {
		t.Errorf("group = %v", group)
	}
	entries := resp["entries"].([]interface{})
	if len(entries) != 4 {
		t.Fatalf("entries = %v", entries)
	}
	deny := entries[0].(map[string]interface{})
	if deny["type"] != "DENY" || deny["who"] != "otheruser" || deny["id"] != 3002.0 {
		t.Errorf("entry 0 = %v, want the DENY entry first (evaluation order)", deny)
	}
	if got, _ := json.Marshal(deny["perms"]); string(got) != `["WRITE_DATA","APPEND_DATA"]` {
		t.Errorf("deny perms = %s", got)
	}
	if got, _ := json.Marshal(deny["flags"]); string(got) != `[]` {
		t.Errorf("deny flags = %s", got)
	}
	owner := entries[1].(map[string]interface{})
	if _, ok := owner["id"]; ok {
		t.Errorf("owner@ entry has an id: %v", owner)
	}
	if perms := owner["perms"].([]interface{}); len(perms) != 14 || perms[0] != "READ_DATA" || perms[13] != "SYNCHRONIZE" {
		t.Errorf("owner@ perms = %v", perms)
	}
	if got, _ := json.Marshal(owner["flags"]); string(got) != `["FILE_INHERIT","DIRECTORY_INHERIT"]` {
		t.Errorf("owner@ flags = %s", got)
	}
	if got, _ := json.Marshal(entries[2].(map[string]interface{})["flags"]); string(got) != `["INHERITED"]` {
		t.Errorf("group flags = %s", got)
	}
	for _, sub := range []string{"checked in order", "could not be resolved", "get_share_acl"} {
		if !notesContain(resp, sub) {
			t.Errorf("notes missing %q: %v", sub, resp["notes"])
		}
	}
}

func TestGetFilesystemACLSimplified(t *testing.T) {
	r, fake := newACLFake(t, map[string]interface{}{"filesystem.getacl": nfs4ACL(true)}, nil)
	resp := callTool(t, r, "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/shares/docs", "simplified": true})
	if got, _ := json.Marshal(lastParams(t, fake, "filesystem.getacl")); string(got) != `["/mnt/tank/shares/docs",true,true]` {
		t.Errorf("getacl params = %s", got)
	}
	owner := resp["entries"].([]interface{})[1].(map[string]interface{})
	if owner["basic_perms"] != "FULL_CONTROL" || owner["basic_flags"] != "INHERIT" {
		t.Errorf("owner@ = %v", owner)
	}
	if _, ok := owner["perms"]; ok {
		t.Errorf("owner@ has both basic_perms and perms: %v", owner)
	}
	if deny := resp["entries"].([]interface{})[0].(map[string]interface{}); deny["type"] != "DENY" || deny["perms"] == nil {
		t.Errorf("non-basic DENY entry not listed in full: %v", deny)
	}
	if !notesContain(resp, "basic_perms") {
		t.Errorf("notes = %v", resp["notes"])
	}
}

func TestGetFilesystemACLPOSIX(t *testing.T) {
	rwx := map[string]interface{}{"READ": true, "WRITE": true, "EXECUTE": true}
	rx := map[string]interface{}{"READ": true, "WRITE": false, "EXECUTE": true}
	none := map[string]interface{}{"READ": false, "WRITE": false, "EXECUTE": false}
	acl := map[string]interface{}{
		"path": "/mnt/tank/posix", "uid": 3001.0, "gid": 4001.0, "user": "exampleuser", "group": "examplegroup",
		"acltype": "POSIX1E", "trivial": false,
		"acl": []interface{}{
			map[string]interface{}{"tag": "USER_OBJ", "id": -1.0, "who": "exampleuser", "default": false, "perms": rwx},
			map[string]interface{}{"tag": "USER", "id": 3002.0, "who": "otheruser", "default": false, "perms": rwx},
			map[string]interface{}{"tag": "GROUP_OBJ", "id": -1.0, "who": "examplegroup", "default": false, "perms": rx},
			map[string]interface{}{"tag": "MASK", "id": -1.0, "who": nil, "default": false, "perms": rx},
			map[string]interface{}{"tag": "OTHER", "id": -1.0, "who": nil, "default": false, "perms": none},
			map[string]interface{}{"tag": "USER_OBJ", "id": -1.0, "who": "exampleuser", "default": true, "perms": rwx},
			map[string]interface{}{"tag": "GROUP", "id": 4002.0, "who": "othergroup", "default": true, "perms": rwx},
			map[string]interface{}{"tag": "MASK", "id": -1.0, "who": nil, "default": true, "perms": rwx},
		},
	}
	r, _ := newACLFake(t, map[string]interface{}{"filesystem.getacl": acl}, nil)
	resp := callTool(t, r, "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/posix"})
	entries := resp["entries"].([]interface{})
	if len(entries) != 8 {
		t.Fatalf("entries = %v", entries)
	}
	user := entries[1].(map[string]interface{})
	if user["scope"] != "access" || user["perms"] != "rwx" || user["effective"] != "r-x" || user["who"] != "otheruser" {
		t.Errorf("named user = %v, want rwx masked to r-x", user)
	}
	if obj := entries[0].(map[string]interface{}); obj["effective"] != nil || obj["perms"] != "rwx" {
		t.Errorf("USER_OBJ = %v, the mask does not apply to it", obj)
	}
	if other := entries[4].(map[string]interface{}); other["perms"] != "---" {
		t.Errorf("OTHER = %v", other)
	}
	def := entries[6].(map[string]interface{})
	if def["scope"] != "default" || def["perms"] != "rwx" || def["effective"] != nil {
		t.Errorf("default group = %v, its own default mask is rwx", def)
	}
	if !notesContain(resp, "default entries") {
		t.Errorf("notes = %v", resp["notes"])
	}
}

func TestGetFilesystemACLDisabled(t *testing.T) {
	r, _ := newACLFake(t, map[string]interface{}{
		"filesystem.getacl": map[string]interface{}{"path": "/mnt/tank/plain", "uid": 0.0, "gid": 0.0, "user": "root", "group": "wheel",
			"acltype": "DISABLED", "acl": nil, "trivial": true},
	}, nil)
	resp := callTool(t, r, "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/plain"})
	if resp["acltype"] != "DISABLED" || resp["count"] != 0.0 {
		t.Errorf("resp = %v", resp)
	}
	if entries, ok := resp["entries"].([]interface{}); !ok || len(entries) != 0 {
		t.Errorf("entries = %v, want []", resp["entries"])
	}
	if owner := resp["owner"].(map[string]interface{}); owner["uid"] != 0.0 || owner["name"] != "root" {
		t.Errorf("owner = %v", owner)
	}
	if !notesContain(resp, "ACLs are disabled") {
		t.Errorf("notes = %v", resp["notes"])
	}
}

func TestGetFilesystemACLRefusesPathsOutsidePools(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"", "path is required"},
		{"tank/shares", "must be absolute"},
		{"./shares", "must be absolute"},
		{"/etc", "outside the storage pools"},
		{"/etc/shadow", "outside the storage pools"},
		{"/var/db/system", "outside the storage pools"},
		{"/root/.ssh", "outside the storage pools"},
		{"/", "outside the storage pools"},
		{"/mnt", "outside the storage pools"},
		{"/mnt/", "outside the storage pools"},
		{"/mntx/tank", "outside the storage pools"},
		{"/mnt/tank/../../etc", "'..'"},
		{"/mnt/tank/shares/..", "'..'"},
		{"/mnt/../etc", "'..'"},
		{"/mnt//tank", "empty or '.'"},
		{"/mnt/tank//shares", "empty or '.'"},
		{"/mnt/tank/./shares", "empty or '.'"},
		{"/mnt/.ix-apps", "system path"},
		{"/mnt/.ix-apps/app_configs", "system path"},
		{"/mnt/tank/shares\x00/etc", "control characters"},
		{"/mnt/tank/shares\n", "control characters"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			client, fake := newFakeClient(t)
			r := NewRegistry(client, nil)
			_, err := r.CallTool(t.Context(), "get_filesystem_acl", map[string]interface{}{"path": tc.path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("path %q: err = %v, want %q", tc.path, err, tc.want)
			}
			if n := len(fake.called("filesystem.stat")) + len(fake.called("filesystem.getacl")); n != 0 {
				t.Errorf("path %q reached the middleware (%d calls)", tc.path, n)
			}
		})
	}
}

func TestValidateACLPathAccepts(t *testing.T) {
	for in, want := range map[string]string{
		"/mnt/tank":                    "/mnt/tank",
		"/mnt/tank/":                   "/mnt/tank",
		"/mnt/tank/shares/docs":        "/mnt/tank/shares/docs",
		"/mnt/tank/shares/docs/":       "/mnt/tank/shares/docs",
		"/mnt/tank/shares/.hidden":     "/mnt/tank/shares/.hidden",
		"/mnt/tank/docs/.zfs/snapshot": "/mnt/tank/docs/.zfs/snapshot",
		"/mnt/tank/with space/x..y":    "/mnt/tank/with space/x..y",
	} {
		got, err := validateACLPath(in)
		if err != nil || got != want {
			t.Errorf("validateACLPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestGetFilesystemACLSymlinks(t *testing.T) {
	refused := func(t *testing.T, fake *fakeMiddleware, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if calls := fake.called("filesystem.getacl"); len(calls) != 0 {
			t.Errorf("getacl was called: %v", calls)
		}
		for _, p := range statPaths(t, fake) {
			if !strings.HasPrefix(p, "/mnt/") {
				t.Errorf("stat reached %s outside the pools", p)
			}
		}
	}
	t.Run("last component outside the pool is refused", func(t *testing.T) {
		r, fake := newACLFake(t, nil, map[string]map[string]interface{}{
			"/mnt/tank/escape": linkStat("/mnt/tank/escape", "/etc"),
		})
		_, err := r.CallTool(t.Context(), "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/escape"})
		refused(t, fake, err, "/mnt/tank/escape resolves to /etc")
	})
	t.Run("middle component outside the pool is refused", func(t *testing.T) {
		// 25.10 stat of /mnt/tank/share/link/passwd follows link but
		// reports realpath unchanged, so only the walk catches it.
		r, fake := newACLFake(t, nil, map[string]map[string]interface{}{
			"/mnt/tank/share/link":        linkStat("/mnt/tank/share/link", "/etc"),
			"/mnt/tank/share/link/passwd": {"realpath": "/mnt/tank/share/link/passwd", "type": "FILE", "is_mountpoint": false},
		})
		_, err := r.CallTool(t.Context(), "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/share/link/passwd"})
		refused(t, fake, err, "/mnt/tank/share/link resolves to /etc")
		for _, p := range statPaths(t, fake) {
			if p == "/mnt/tank/share/link/passwd" {
				t.Errorf("stat continued past the refused link")
			}
		}
	})
	t.Run("link to a hidden system mount is refused", func(t *testing.T) {
		// On a real system /mnt/.ix-apps is a mountpoint, so only the
		// path check on the link target refuses it.
		r, fake := newACLFake(t, nil, map[string]map[string]interface{}{
			"/mnt/tank/apps":            linkStat("/mnt/tank/apps", "/mnt/.ix-apps"),
			"/mnt/.ix-apps":             mountStat("/mnt/.ix-apps"),
			"/mnt/.ix-apps/app_configs": dirStat("/mnt/.ix-apps/app_configs"),
		})
		_, err := r.CallTool(t.Context(), "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/apps/app_configs"})
		refused(t, fake, err, "/mnt/tank/apps resolves to /mnt/.ix-apps, which is outside /mnt/<pool>")
	})
	t.Run("link with a dot-dot target is refused", func(t *testing.T) {
		// The pool part of these targets is a real pool, so only the
		// path check on the link target refuses them.
		for _, target := range []string{"/mnt/tank/../../etc", "../../etc"} {
			r, fake := newACLFake(t, nil, map[string]map[string]interface{}{
				"/mnt/tank/up": linkStat("/mnt/tank/up", target),
			})
			_, err := r.CallTool(t.Context(), "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/up/passwd"})
			refused(t, fake, err, "/mnt/tank/up resolves to "+target+", which is outside /mnt/<pool>")
		}
	})
	t.Run("link to a non-pool directory under /mnt is refused", func(t *testing.T) {
		r, fake := newACLFake(t, nil, map[string]map[string]interface{}{
			"/mnt/tank/scratch-link": linkStat("/mnt/tank/scratch-link", "/mnt/scratch/x"),
			"/mnt/scratch":           dirStat("/mnt/scratch"),
		})
		_, err := r.CallTool(t.Context(), "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/scratch-link"})
		refused(t, fake, err, "not a pool mountpoint")
	})
	t.Run("symlink loop is refused", func(t *testing.T) {
		// A real realpath never stays a link; this guards the hop cap.
		r, fake := newACLFake(t, nil, map[string]map[string]interface{}{
			"/mnt/tank/a": linkStat("/mnt/tank/a", "/mnt/tank/b"),
			"/mnt/tank/b": linkStat("/mnt/tank/b", "/mnt/tank/a"),
		})
		_, err := r.CallTool(t.Context(), "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/a"})
		refused(t, fake, err, "too many symlinks")
	})
	t.Run("last component inside a pool reads the target", func(t *testing.T) {
		r, fake := newACLFake(t, map[string]interface{}{"filesystem.getacl": nfs4ACL(false)}, map[string]map[string]interface{}{
			"/mnt/tank/docs-link": linkStat("/mnt/tank/docs-link", "/mnt/tank/shares/docs"),
		})
		resp := callTool(t, r, "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/docs-link"})
		if got, _ := json.Marshal(lastParams(t, fake, "filesystem.getacl")); string(got) != `["/mnt/tank/shares/docs",false,true]` {
			t.Errorf("getacl params = %s", got)
		}
		if resp["path"] != "/mnt/tank/docs-link" || resp["resolved_path"] != "/mnt/tank/shares/docs" {
			t.Errorf("path/resolved_path = %v/%v", resp["path"], resp["resolved_path"])
		}
		// Owner and mode come from the target, not from the link itself.
		if resp["type"] != "DIRECTORY" || resp["mode"] != "0770" {
			t.Errorf("type/mode = %v/%v, want the target's", resp["type"], resp["mode"])
		}
	})
	t.Run("middle component into another pool reads the target", func(t *testing.T) {
		r, fake := newACLFake(t, map[string]interface{}{"filesystem.getacl": nfs4ACL(false)}, map[string]map[string]interface{}{
			"/mnt/tank/projects": linkStat("/mnt/tank/projects", "/mnt/other/projects"),
		})
		resp := callTool(t, r, "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/projects/alpha"})
		if got, _ := json.Marshal(lastParams(t, fake, "filesystem.getacl")); string(got) != `["/mnt/other/projects/alpha",false,true]` {
			t.Errorf("getacl params = %s", got)
		}
		if resp["resolved_path"] != "/mnt/other/projects/alpha" {
			t.Errorf("resolved_path = %v", resp["resolved_path"])
		}
		want := "/mnt/tank /mnt/tank/projects /mnt/other /mnt/other/projects /mnt/other/projects/alpha"
		if got := strings.Join(statPaths(t, fake), " "); got != want {
			t.Errorf("stat paths = %s, want %s", got, want)
		}
	})
	t.Run("nested path without links passes", func(t *testing.T) {
		r, fake := newACLFake(t, map[string]interface{}{"filesystem.getacl": nfs4ACL(false)}, nil)
		resp := callTool(t, r, "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank/shares/docs/2026/q1"})
		if got, _ := json.Marshal(lastParams(t, fake, "filesystem.getacl")); string(got) != `["/mnt/tank/shares/docs/2026/q1",false,true]` {
			t.Errorf("getacl params = %s", got)
		}
		if _, ok := resp["resolved_path"]; ok {
			t.Errorf("resolved_path set without a symlink: %v", resp["resolved_path"])
		}
		if n := len(statPaths(t, fake)); n != 5 {
			t.Errorf("stat calls = %d, want one per component", n)
		}
	})
}

func TestGetFilesystemACLRequiresPoolMountpoint(t *testing.T) {
	for name, st := range map[string]map[string]interface{}{
		"plain directory": dirStat("/mnt/scratch"),
		"symlink":         linkStat("/mnt/scratch", "/mnt/tank"),
		"no mount field":  {"realpath": "/mnt/scratch", "type": "DIRECTORY"},
		// is_mountpoint alone is not enough: the pool root must be a
		// directory.
		"mountpoint that is a file":    {"realpath": "/mnt/scratch", "type": "FILE", "is_mountpoint": true},
		"mountpoint that is a symlink": {"realpath": "/mnt/scratch", "type": "SYMLINK", "is_mountpoint": true},
	} {
		t.Run(name, func(t *testing.T) {
			r, fake := newACLFake(t, nil, map[string]map[string]interface{}{"/mnt/scratch": st})
			_, err := r.CallTool(t.Context(), "get_filesystem_acl", map[string]interface{}{"path": "/mnt/scratch/data"})
			if err == nil || !strings.Contains(err.Error(), "/mnt/scratch is not a pool mountpoint") {
				t.Fatalf("err = %v", err)
			}
			if got := strings.Join(statPaths(t, fake), " "); got != "/mnt/scratch" {
				t.Errorf("stat paths = %s, want only the pool root", got)
			}
			if calls := fake.called("filesystem.getacl"); len(calls) != 0 {
				t.Errorf("getacl was called: %v", calls)
			}
		})
	}
}

func TestGetFilesystemACLDepthCap(t *testing.T) {
	r, fake := newACLFake(t, nil, nil)
	_, err := r.CallTool(t.Context(), "get_filesystem_acl", map[string]interface{}{"path": "/mnt/tank" + strings.Repeat("/d", maxACLPathDepth+1)})
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v", err)
	}
	if n := len(fake.called("filesystem.stat")); n != 0 {
		t.Errorf("stat calls = %d, want none", n)
	}
}

func TestACLToolsAreReadOnly(t *testing.T) {
	r := NewRegistry(nil, nil)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	for _, name := range []string{"get_share_acl", "get_filesystem_acl"} {
		if safetyOf(name).Access != Read || !names[name] {
			t.Errorf("%s: access %v, listed under --read-only %v", name, safetyOf(name).Access, names[name])
		}
	}
}

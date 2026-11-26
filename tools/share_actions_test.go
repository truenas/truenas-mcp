package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

const (
	aliceSID  = "S-1-5-21-111-222-333-1001"
	staffSID  = "S-1-5-21-111-222-333-2001"
	adminsSID = "S-1-5-32-544"
)

// shareSMBRow is an SMB share as sharing.smb.query returns it on 25.10.
func shareSMBRow(id int, name, p string) map[string]interface{} {
	return map[string]interface{}{
		"id": float64(id), "name": name, "path": p, "enabled": true, "comment": "", "purpose": "DEFAULT_SHARE",
		"readonly": false, "browsable": true, "access_based_share_enumeration": false, "locked": false,
		"audit":   map[string]interface{}{"enable": false, "watch_list": []interface{}{}, "ignore_list": []interface{}{}},
		"options": map[string]interface{}{"aapl_name_mangling": false},
	}
}

func shareNFSRow(id int, p string) map[string]interface{} {
	return map[string]interface{}{
		"id": float64(id), "path": p, "aliases": []interface{}{}, "comment": "", "networks": []interface{}{"192.0.2.0/24"},
		"hosts": []interface{}{}, "ro": false, "maproot_user": "nobody", "maproot_group": "nogroup",
		"mapall_user": nil, "mapall_group": nil, "security": []interface{}{}, "enabled": true, "locked": false,
		"expose_snapshots": false,
	}
}

func shareACE(sid string, who interface{}, typ, perm string) map[string]interface{} {
	return map[string]interface{}{"ae_who_sid": sid, "ae_who_str": who, "ae_who_id": nil, "ae_type": typ, "ae_perm": perm}
}

// Fixtures for fakeResults, used by the tests that plan every tool against
// the shared fake: one SMB share, one NFS share and an everyone READ ACL.
func shareFixtureSMBRows() []interface{} {
	return []interface{}{shareSMBRow(1, "docs", "/mnt/tank/shares/docs")}
}

func shareFixtureNFSRows() []interface{} {
	return []interface{}{shareNFSRow(1, "/mnt/tank/shares/data")}
}

func shareFixtureACL() map[string]interface{} {
	return map[string]interface{}{"share_name": "docs", "share_acl": []interface{}{shareACE(everyoneSID, "everyone@", "ALLOWED", "READ")}}
}

// shareFake answers the methods the share write tools use from mutable
// fixtures, the way 25.10 does: sharing.*.update merges the fields sent
// into the share, sharing.*.delete removes it, sharing.smb.setacl replaces
// the share ACL, and filesystem.stat does not follow a final symlink.
type shareFake struct {
	mu     sync.Mutex
	smb    map[int]map[string]interface{}
	nfs    map[int]map[string]interface{}
	acl    map[string][]interface{} // lower-case share name -> entries
	stat   map[string]map[string]interface{}
	users  []map[string]interface{}
	groups []map[string]interface{}
	fail   map[string]string
	// rawAccounts makes user.query and group.query ignore their filters,
	// so the client-side checks on each row are what refuses.
	rawAccounts bool
	// deleteResult, when set, is what sharing.*.delete returns.
	deleteResult interface{}
}

func newShareFake() *shareFake {
	f := &shareFake{
		smb:  map[int]map[string]interface{}{},
		nfs:  map[int]map[string]interface{}{},
		acl:  map[string][]interface{}{},
		stat: map[string]map[string]interface{}{},
		fail: map[string]string{},
	}
	f.smb[1] = shareSMBRow(1, "docs", "/mnt/tank/shares/docs")
	f.smb[1]["comment"] = "Team documents"
	legacy := shareSMBRow(2, "legacy", "/mnt/tank/shares/legacy")
	legacy["purpose"] = "LEGACY_SHARE"
	legacy["options"] = map[string]interface{}{
		"recyclebin": false, "path_suffix": nil, "hostsallow": []interface{}{"192.0.2.0/24"}, "hostsdeny": []interface{}{},
		"guestok": false, "streams": true, "durablehandle": true, "shadowcopy": true, "fsrvp": false, "home": false,
		"acl": true, "afp": false, "timemachine": false, "timemachine_quota": 0, "vuid": "", "auxsmbconf": "",
	}
	f.smb[2] = legacy
	homes := shareSMBRow(3, "homes", "/mnt/tank/home")
	homes["options"] = map[string]interface{}{"home": true}
	f.smb[3] = homes
	f.smb[4] = shareSMBRow(4, "ADMIN$", "/mnt/tank/shares/admin")
	locked := shareSMBRow(5, "vault", "/mnt/tank/secure/vault")
	locked["locked"] = true
	f.smb[5] = locked
	f.smb[6] = shareSMBRow(6, "poolroot", "/mnt/tank")
	f.smb[7] = shareSMBRow(7, "apps", "/mnt/tank/ix-apps/app_mounts")
	f.smb[8] = shareSMBRow(8, "boot", "/mnt/boot-pool/data")
	unknown := shareSMBRow(9, "unknownlock", "/mnt/tank/shares/u")
	unknown["locked"] = nil
	f.smb[9] = unknown
	external := shareSMBRow(10, "dfs", "EXTERNAL")
	external["purpose"] = "EXTERNAL_SHARE"
	f.smb[10] = external
	f.smb[11] = shareSMBRow(11, "C$", "/mnt/tank/shares/c")
	f.smb[12] = shareSMBRow(12, "media", "/mnt/tank/shares/media")
	f.smb[13] = shareSMBRow(13, "hidden", "/mnt/tank/data/.hidden")

	f.nfs[1] = shareNFSRow(1, "/mnt/tank/shares/data")
	lockedNFS := shareNFSRow(2, "/mnt/tank/secure/export")
	lockedNFS["locked"] = true
	f.nfs[2] = lockedNFS
	f.nfs[3] = shareNFSRow(3, "/mnt/tank")
	f.nfs[4] = shareNFSRow(4, "/mnt/tank/.system/nfs")

	f.acl["docs"] = []interface{}{shareACE(everyoneSID, "everyone@", "ALLOWED", "FULL")}
	f.acl["media"] = []interface{}{shareACE(everyoneSID, "everyone@", "ALLOWED", "FULL")}

	dir := func(p string, mount bool, inode int) {
		f.stat[p] = map[string]interface{}{"realpath": p, "type": "DIRECTORY", "is_mountpoint": mount, "inode": float64(inode), "dev": 1.0}
	}
	dir("/mnt/tank", true, 1)
	dir("/mnt/tank/shares", true, 2)
	dir("/mnt/tank/shares/docs", true, 3)
	dir("/mnt/tank/shares/new", true, 4)
	dir("/mnt/tank/shares/data", true, 5)
	f.stat["/mnt/tank/shares/link"] = map[string]interface{}{"realpath": "/mnt/tank/shares/new", "type": "SYMLINK", "is_mountpoint": false, "inode": 6.0, "dev": 1.0}
	f.stat["/mnt/tank/shares/file.txt"] = map[string]interface{}{"realpath": "/mnt/tank/shares/file.txt", "type": "FILE", "is_mountpoint": false, "inode": 7.0, "dev": 1.0}

	f.users = []map[string]interface{}{
		{"id": 10.0, "uid": 3000.0, "username": "alice", "smb": true, "sid": aliceSID, "local": true},
		{"id": 11.0, "uid": 3002.0, "username": "bob", "smb": false, "sid": nil, "local": true},
		{"id": 12.0, "uid": 3003.0, "username": "carol", "smb": true, "sid": "S-1-22-1-3003", "local": true},
	}
	f.groups = []map[string]interface{}{
		{"id": 40.0, "gid": 544.0, "name": "builtin_administrators", "group": "builtin_administrators", "smb": true, "sid": adminsSID, "local": true, "roles": []interface{}{"FULL_ADMIN"}},
		{"id": 50.0, "gid": 3100.0, "name": "staff", "group": "staff", "smb": true, "sid": staffSID, "local": true, "roles": []interface{}{}},
	}
	return f
}

func shareFilterMatch(row map[string]interface{}, filters []interface{}) bool {
	for _, fl := range filters {
		arr, _ := fl.([]interface{})
		if len(arr) != 3 {
			continue
		}
		field, _ := arr[0].(string)
		op, _ := arr[1].(string)
		got := row[field]
		switch op {
		case "=":
			if !sameJSON(got, arr[2]) {
				return false
			}
		case "C=":
			a, _ := got.(string)
			b, _ := arr[2].(string)
			if !strings.EqualFold(a, b) {
				return false
			}
		}
	}
	return true
}

func copyRow(row map[string]interface{}) map[string]interface{} {
	b, _ := json.Marshal(row)
	var out map[string]interface{}
	_ = json.Unmarshal(b, &out)
	return out
}

func (f *shareFake) handle(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if msg, ok := f.fail[method]; ok {
		return nil, rpcErr(msg), true
	}
	var p []interface{}
	_ = json.Unmarshal(params, &p)
	filters := func() []interface{} {
		if len(p) > 0 {
			l, _ := p[0].([]interface{})
			return l
		}
		return nil
	}
	query := func(rows map[int]map[string]interface{}) []interface{} {
		out := []interface{}{}
		for id := 1; id <= 100; id++ {
			if row, ok := rows[id]; ok && shareFilterMatch(row, filters()) {
				out = append(out, copyRow(row))
			}
		}
		return out
	}
	idArg := func() int {
		id, _ := intID(p[0])
		return id
	}
	switch method {
	case "sharing.smb.query":
		return query(f.smb), nil, true
	case "sharing.nfs.query":
		return query(f.nfs), nil, true
	case "sharing.smb.getacl":
		m, _ := p[0].(map[string]interface{})
		name, _ := m["share_name"].(string)
		return map[string]interface{}{"share_name": name, "share_acl": f.acl[strings.ToLower(name)]}, nil, true
	case "sharing.smb.setacl":
		m, _ := p[0].(map[string]interface{})
		name, _ := m["share_name"].(string)
		entries, _ := m["share_acl"].([]interface{})
		var out []interface{}
		for _, e := range entries {
			ae, _ := e.(map[string]interface{})
			sid, _ := ae["ae_who_sid"].(string)
			out = append(out, shareACE(sid, f.sidName(sid), fmt.Sprint(ae["ae_type"]), fmt.Sprint(ae["ae_perm"])))
		}
		f.acl[strings.ToLower(name)] = out
		return map[string]interface{}{"share_name": name, "share_acl": out}, nil, true
	case "sharing.smb.update", "sharing.nfs.update":
		rows := f.smb
		if method == "sharing.nfs.update" {
			rows = f.nfs
		}
		id := idArg()
		data, _ := p[1].(map[string]interface{})
		for k, v := range data {
			rows[id][k] = v
		}
		return copyRow(rows[id]), nil, true
	case "sharing.smb.delete", "sharing.nfs.delete":
		rows := f.smb
		if method == "sharing.nfs.delete" {
			rows = f.nfs
		}
		delete(rows, idArg())
		if f.deleteResult != nil {
			return f.deleteResult, nil, true
		}
		return true, nil, true
	case "filesystem.stat":
		path, _ := p[0].(string)
		st, ok := f.stat[path]
		if !ok {
			return nil, rpcErr(fmt.Sprintf("[ENOENT] %s: path not found", path)), true
		}
		return st, nil, true
	case "user.query", "group.query":
		rows := f.users
		if method == "group.query" {
			rows = f.groups
		}
		out := []interface{}{}
		for _, row := range rows {
			if f.rawAccounts || shareFilterMatch(row, filters()) {
				out = append(out, copyRow(row))
			}
		}
		return out, nil, true
	}
	return nil, nil, false
}

func (f *shareFake) sidName(sid string) interface{} {
	if sid == everyoneSID {
		return "everyone@"
	}
	for _, rows := range [][]map[string]interface{}{f.users, f.groups} {
		for _, row := range rows {
			if row["sid"] == sid {
				if n, ok := row["username"]; ok {
					return n
				}
				return row["name"]
			}
		}
	}
	return nil
}

func (f *shareFake) registry(t *testing.T) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{handle: f.handle}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

func (f *shareFake) mutate(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

var shareWriteMethods = []string{
	"sharing.smb.update", "sharing.smb.delete", "sharing.smb.setacl", "sharing.nfs.update", "sharing.nfs.delete",
}

func assertNoShareWrites(t *testing.T, fake *fakeMiddleware) {
	t.Helper()
	for _, m := range shareWriteMethods {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("%s sent %d time(s)", m, n)
		}
	}
}

func dryRunOf(t *testing.T, r *Registry, tool string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	return callTool(t, r, tool, mergeArgs(args, map[string]interface{}{"dry_run": true}))
}

func plannedDetails(t *testing.T, dry map[string]interface{}) map[string]interface{} {
	t.Helper()
	actions, _ := dry["planned_actions"].([]interface{})
	if len(actions) != 1 {
		t.Fatalf("planned_actions = %v", dry["planned_actions"])
	}
	d, _ := actions[0].(map[string]interface{})["details"].(map[string]interface{})
	return d
}

func assertContainsAll(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s missing %q:\n%s", what, w, body)
		}
	}
}

// ---- classification, read-only ----

var shareActionTools = []string{"update_smb_share", "delete_smb_share", "update_nfs_share", "delete_nfs_share", "set_share_acl"}

func TestShareActionToolsSafety(t *testing.T) {
	r := NewRegistry(nil, nil)
	for _, name := range shareActionTools {
		if got := safetyOf(name); got != (Safety{Access: Destructive}) {
			t.Errorf("%s safety = %+v, want destructive, not idempotent", name, got)
		}
		tool := r.tools[name]
		if tool.Plan == nil || tool.Execute == nil {
			t.Errorf("%s has no Plan or Execute", name)
		}
		if !hasParam(tool, "confirmation_token") || !hasParam(tool, "dry_run") {
			t.Errorf("%s lacks confirmation_token or dry_run", name)
		}
		if a := tool.Definition.Annotations; a == nil || !a.DestructiveHint || a.IdempotentHint || a.ReadOnlyHint {
			t.Errorf("%s annotations = %+v", name, a)
		}
		if readOnlyDryRunTools[name] {
			t.Errorf("%s is listed for read-only dry runs", name)
		}
		for _, p := range []string{"recursive", "purpose", "options", "audit", "auxsmbconf", "stripacl", "dacl", "uid", "gid"} {
			if hasParam(tool, p) {
				t.Errorf("%s declares %s", name, p)
			}
		}
	}
}

func TestShareActionToolsRefusedInReadOnlyMode(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	args := map[string]map[string]interface{}{
		"update_smb_share": {"share_id": 1.0, "comment": "x"},
		"delete_smb_share": {"share_id": 1.0},
		"update_nfs_share": {"share_id": 1.0, "comment": "x"},
		"delete_nfs_share": {"share_id": 1.0},
		"set_share_acl":    {"share_id": 1.0, "entries": []interface{}{map[string]interface{}{"who_type": "everyone", "perm": "READ", "type": "ALLOWED"}}},
	}
	for _, name := range shareActionTools {
		if names[name] {
			t.Errorf("read-only lists %s", name)
		}
		for _, dry := range []bool{true, false} {
			if _, err := r.CallTool(t.Context(), name, mergeArgs(args[name], map[string]interface{}{"dry_run": dry})); err == nil || !strings.Contains(err.Error(), "read-only mode") {
				t.Errorf("read-only %s dry_run=%v: err = %v", name, dry, err)
			}
		}
	}
	for _, m := range append([]string{"sharing.smb.query", "sharing.nfs.query", "sharing.smb.getacl"}, shareWriteMethods...) {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("read-only mode sent %s %d time(s)", m, n)
		}
	}
	if err := r.SetPolicy(Policy{ReadOnly: true, Allow: []string{"update_smb_share"}}); err == nil {
		t.Error("--allow-tools with a share write tool accepted in read-only mode")
	}
}

// ---- update_smb_share ----

func TestUpdateSMBShareConfirmFlow(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	args := map[string]interface{}{"share_name": "DOCS", "new_name": "TeamDocs", "comment": "Shared docs", "readonly": true, "browsable": true}
	preview, out := confirmAndRun(t, r, fake, "update_smb_share", "sharing.smb.update", args)

	body := previewBody(t, preview)
	assertContainsAll(t, "preview", body,
		`"path":"/mnt/tank/shares/docs"`, `"comment":"Team documents"`, `\\\\nas\\docs`,
		`name: docs -> TeamDocs`, `comment: Team documents -> Shared docs`, `readonly: false -> true`,
		"must be changed to 'TeamDocs'", "can no longer create or change files", "browsable is already true")
	p := snapActionParams(t, fake, "sharing.smb.update")
	if len(p) != 2 || p[0] != 1.0 || previewText(t, p[1]) != `{"comment":"Shared docs","name":"TeamDocs","readonly":true}` {
		t.Errorf("sharing.smb.update params = %s", previewText(t, p))
	}
	resp := decodeObj(t, out)
	if resp["updated"] != true || resp["note"] != nil {
		t.Errorf("response = %v", resp)
	}
	if f.smb[1]["name"] != "TeamDocs" || f.smb[1]["readonly"] != true {
		t.Errorf("share after update = %v", f.smb[1])
	}
}

func TestUpdateSMBShareDryRunShowsCurrentSettings(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	dry := dryRunOf(t, r, "update_smb_share", map[string]interface{}{"share_id": 1.0, "enabled": false})
	if dry["dry_run"] != true || dry["executed"] != false {
		t.Errorf("dry run flags = %v", dry)
	}
	share := dry["current_state"].(map[string]interface{})["share"].(map[string]interface{})
	for k, want := range map[string]interface{}{"name": "docs", "path": "/mnt/tank/shares/docs", "enabled": true, "readonly": false, "browsable": true, "comment": "Team documents"} {
		if share[k] != want {
			t.Errorf("current_state.share.%s = %v, want %v", k, share[k], want)
		}
	}
	d := plannedDetails(t, dry)
	if previewText(t, d["changes"]) != `["enabled: true -\u003e false"]` {
		t.Errorf("changes = %s", previewText(t, d["changes"]))
	}
	if previewText(t, d["set"]) != `{"enabled":false}` {
		t.Errorf("set = %s", previewText(t, d["set"]))
	}
	assertContainsAll(t, "warnings", previewText(t, dry["warnings"]), "lose access to the share while it is disabled")
	assertNoShareWrites(t, fake)
}

func TestUpdateSMBShareMovesPath(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	preview, _ := confirmAndRun(t, r, fake, "update_smb_share", "sharing.smb.update", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/shares/new"})
	assertContainsAll(t, "preview", previewBody(t, preview), "path: /mnt/tank/shares/docs -> /mnt/tank/shares/new", "no files are moved or copied")
	p := snapActionParams(t, fake, "sharing.smb.update")
	if previewText(t, p[1]) != `{"path":"/mnt/tank/shares/new"}` {
		t.Errorf("params = %s", previewText(t, p))
	}
}

func TestUpdateSMBShareHostLists(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	confirmAndRun(t, r, fake, "update_smb_share", "sharing.smb.update", map[string]interface{}{"share_id": 2.0, "hostsallow": []interface{}{"192.0.2.10", "192.0.2.0/25"}})
	p := snapActionParams(t, fake, "sharing.smb.update")
	data := p[1].(map[string]interface{})
	if data["purpose"] != "LEGACY_SHARE" {
		t.Errorf("purpose not sent with options: %v", data)
	}
	opts := data["options"].(map[string]interface{})
	if previewText(t, opts["hostsallow"]) != `["192.0.2.10","192.0.2.0/25"]` || opts["shadowcopy"] != true || opts["acl"] != true || opts["auxsmbconf"] != "" {
		t.Errorf("options = %v; every other option must be sent back unchanged", opts)
	}
	if len(data) != 2 {
		t.Errorf("payload = %v", data)
	}

	// The preview never shows auxsmbconf, even though it is sent back.
	f.mutate(func() { f.smb[2]["options"].(map[string]interface{})["auxsmbconf"] = "veto files = /x/" })
	dry := dryRunOf(t, r, "update_smb_share", map[string]interface{}{"share_id": 2.0, "hostsdeny": []interface{}{"ALL"}})
	if s := previewText(t, dry); strings.Contains(s, "auxsmbconf") || strings.Contains(s, "veto files") {
		t.Errorf("preview shows auxsmbconf:\n%s", s)
	}
	r, fake = newShareFake().registry(t)
	assertRefused(t, r, fake, "update_smb_share", "sharing.smb.update", map[string]interface{}{"share_id": 1.0, "hostsallow": []interface{}{"192.0.2.10"}},
		"can be changed only for LEGACY_SHARE shares")
}

func TestUpdateSMBShareRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"homes share", map[string]interface{}{"share_id": 3.0, "comment": "x"}, "reserved or administrative SMB share name"},
		{"homes flag", map[string]interface{}{"share_id": 3.0, "comment": "x"}, "is the homes share"},
		{"admin share", map[string]interface{}{"share_name": "admin$", "comment": "x"}, "reserved or administrative"},
		{"drive share", map[string]interface{}{"share_id": 11.0, "comment": "x"}, "reserved or administrative"},
		{"locked", map[string]interface{}{"share_id": 5.0, "comment": "x"}, "is on a locked dataset"},
		{"lock unknown", map[string]interface{}{"share_id": 9.0, "comment": "x"}, "lock state of the SMB share could not be read"},
		{"pool root share", map[string]interface{}{"share_id": 6.0, "comment": "x"}, "is the root of pool 'tank'"},
		{"ix-apps share", map[string]interface{}{"share_id": 7.0, "comment": "x"}, "inside 'ix-apps'"},
		{"boot pool share", map[string]interface{}{"share_id": 8.0, "comment": "x"}, "inside 'boot-pool'"},
		{"hidden dataset share", map[string]interface{}{"share_id": 13.0, "comment": "x"}, "inside '.hidden'"},
		{"external share", map[string]interface{}{"share_id": 10.0, "comment": "x"}, "DFS proxy"},
		{"rename to global", map[string]interface{}{"share_id": 1.0, "new_name": "global"}, "reserved"},
		{"rename to homes", map[string]interface{}{"share_id": 1.0, "new_name": "Homes"}, "reserved"},
		{"rename to IPC$", map[string]interface{}{"share_id": 1.0, "new_name": "IPC$"}, "reserved or administrative"},
		{"rename to D$", map[string]interface{}{"share_id": 1.0, "new_name": "D$"}, "reserved or administrative"},
		{"rename to invalid", map[string]interface{}{"share_id": 1.0, "new_name": "a/b"}, "share name cannot contain"},
		{"rename to taken", map[string]interface{}{"share_id": 1.0, "new_name": "MEDIA"}, "another SMB share is already named 'MEDIA'"},
		{"path /mnt", map[string]interface{}{"share_id": 1.0, "path": "/mnt"}, "not below /mnt/<pool>"},
		{"path /mnt/", map[string]interface{}{"share_id": 1.0, "path": "/mnt/"}, "not normalised"},
		{"pool root", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank"}, "is the root of pool 'tank'"},
		{"pool root slash", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/"}, "not normalised"},
		{"double slash", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank//shares/new"}, "not normalised"},
		{"dot dot", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/shares/../shares/new"}, "not normalised"},
		{"dot dot escape", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/../../etc"}, "not normalised"},
		{"dot", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/./shares"}, "not normalised"},
		{"system mount", map[string]interface{}{"share_id": 1.0, "path": "/mnt/.ix-apps/app_mounts"}, "is a system path"},
		{"system dataset", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/.system/samba4"}, "inside '.system'"},
		{"ix-applications", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/ix-applications/x"}, "inside 'ix-applications'"},
		{"ix-apps", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/ix-apps"}, "inside 'ix-apps'"},
		{"ix-virt", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/.ix-virt/x"}, "inside '.ix-virt'"},
		{"boot-pool", map[string]interface{}{"share_id": 1.0, "path": "/mnt/boot-pool/x"}, "inside 'boot-pool'"},
		{"freenas-boot", map[string]interface{}{"share_id": 1.0, "path": "/mnt/freenas-boot/x"}, "inside 'freenas-boot'"},
		{"dot component", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/data/.zfs/snapshot"}, "inside '.zfs'"},
		{"outside /mnt", map[string]interface{}{"share_id": 1.0, "path": "/etc"}, "not below /mnt/<pool>"},
		{"relative", map[string]interface{}{"share_id": 1.0, "path": "tank/shares"}, "not below /mnt/<pool>"},
		{"symlink", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/shares/link"}, "goes through a symlink"},
		{"file", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/shares/file.txt"}, "is not a directory"},
		{"missing path", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/shares/none"}, "cannot be used"},
		{"nothing to change", map[string]interface{}{"share_id": 1.0, "comment": "Team documents", "enabled": true}, "nothing to change"},
		{"no change at all", map[string]interface{}{"share_id": 1.0}, "nothing to change"},
		{"id and name", map[string]interface{}{"share_id": 1.0, "share_name": "docs", "comment": "x"}, "not both"},
		{"no share", map[string]interface{}{"comment": "x"}, "share_name or share_id is required"},
		{"not found", map[string]interface{}{"share_id": 99.0, "comment": "x"}, "not found"},
		{"duplicate host", map[string]interface{}{"share_id": 2.0, "hostsallow": []interface{}{"192.0.2.1", "192.0.2.1"}}, "twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFake()
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "update_smb_share", "sharing.smb.update", tc.args, tc.want)
		})
	}
}

// A share that changes between the preview and the confirmed call is
// refused at confirm, and nothing is written.
func TestUpdateSMBShareTokenRejectedAfterStateChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    map[string]interface{}
		change  func(f *shareFake)
		blocked string
	}{
		{"comment changed elsewhere", map[string]interface{}{"share_id": 1.0, "readonly": true},
			func(f *shareFake) { f.smb[1]["comment"] = "changed in the web UI" }, ""},
		{"share disabled elsewhere", map[string]interface{}{"share_id": 1.0, "readonly": true},
			func(f *shareFake) { f.smb[1]["enabled"] = false }, ""},
		{"dataset locked", map[string]interface{}{"share_id": 1.0, "readonly": true},
			func(f *shareFake) { f.smb[1]["locked"] = true }, "locked dataset"},
		{"share moved", map[string]interface{}{"share_id": 1.0, "readonly": true},
			func(f *shareFake) { f.smb[1]["path"] = "/mnt/tank" }, "root of pool"},
		{"share became the homes share", map[string]interface{}{"share_id": 1.0, "readonly": true},
			func(f *shareFake) { f.smb[1]["options"] = map[string]interface{}{"home": true} }, "homes share"},
		{"new name taken", map[string]interface{}{"share_id": 1.0, "new_name": "Archive"},
			func(f *shareFake) { f.smb[20] = shareSMBRow(20, "archive", "/mnt/tank/shares/archive") }, "already named"},
		{"new path replaced", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/shares/new"},
			func(f *shareFake) { f.stat["/mnt/tank/shares/new"]["inode"] = 99.0 }, ""},
		{"new path now a symlink", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/shares/new"},
			func(f *shareFake) {
				f.stat["/mnt/tank/shares/new"] = map[string]interface{}{"realpath": "/mnt/tank/shares/docs", "type": "SYMLINK", "inode": 4.0, "dev": 1.0}
			}, "symlink"},
		{"host list changed", map[string]interface{}{"share_id": 2.0, "hostsdeny": []interface{}{"192.0.2.99"}},
			func(f *shareFake) { f.smb[2]["options"].(map[string]interface{})["guestok"] = true }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFake()
			r, fake := f.registry(t)
			tok := destroyPreviewToken(t, r, "update_smb_share", tc.args)
			f.mutate(func() { tc.change(f) })
			err := assertTokenRejected(t, r, fake, "update_smb_share", "sharing.smb.update", tc.args, tok)
			if tc.blocked != "" && (err == nil || !strings.Contains(err.Error(), tc.blocked)) {
				t.Errorf("not refused for the change: err = %v", err)
			}
			assertNoShareWrites(t, fake)
		})
	}
}

// ---- delete_smb_share ----

func TestDeleteSMBShareConfirmFlow(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	preview, out := confirmAndRun(t, r, fake, "delete_smb_share", "sharing.smb.delete", map[string]interface{}{"share_name": "docs"})
	body := previewBody(t, preview)
	assertContainsAll(t, "preview", body,
		`"comment":"Team documents"`, `"path":"/mnt/tank/shares/docs"`, "SMB clients lose access", "disconnects clients",
		"every file in it are kept", `"data_deleted":false`)
	summary := preview["preview"].(map[string]interface{})["confirm_summary"].(map[string]interface{})
	if h, _ := summary["headline"].(string); !strings.Contains(h, "clients lose access, files are kept") {
		t.Errorf("headline = %q", h)
	}
	if p := snapActionParams(t, fake, "sharing.smb.delete"); previewText(t, p) != `[1]` {
		t.Errorf("sharing.smb.delete params = %s", previewText(t, p))
	}
	resp := decodeObj(t, out)
	if resp["deleted"] != true || resp["data_deleted"] != false || resp["note"] != nil {
		t.Errorf("response = %v", resp)
	}
	if _, ok := f.smb[1]; ok {
		t.Error("share still exists")
	}
	for _, m := range []string{"filesystem.stat", "pool.dataset.delete", "filesystem.setacl"} {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("delete sent %s", m)
		}
	}
}

func TestDeleteSMBShareRefusals(t *testing.T) {
	for _, tc := range []struct {
		id   float64
		want string
	}{
		{3, "is the homes share"}, {4, "reserved or administrative"}, {11, "reserved or administrative"},
		{5, "locked dataset"}, {9, "lock state"}, {6, "root of pool"}, {7, "inside 'ix-apps'"},
		{8, "inside 'boot-pool'"}, {10, "DFS proxy"}, {13, "inside '.hidden'"},
	} {
		t.Run(fmt.Sprint(tc.id), func(t *testing.T) {
			f := newShareFake()
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "delete_smb_share", "sharing.smb.delete", map[string]interface{}{"share_id": tc.id}, tc.want)
		})
	}
}

func TestDeleteSMBShareTokenRejectedAfterStateChange(t *testing.T) {
	for name, change := range map[string]func(f *shareFake){
		"settings changed": func(f *shareFake) { f.smb[1]["readonly"] = true },
		"share replaced":   func(f *shareFake) { f.smb[1]["path"] = "/mnt/tank/shares/new" },
		"dataset locked":   func(f *shareFake) { f.smb[1]["locked"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newShareFake()
			r, fake := f.registry(t)
			args := map[string]interface{}{"share_id": 1.0}
			tok := destroyPreviewToken(t, r, "delete_smb_share", args)
			f.mutate(func() { change(f) })
			assertTokenRejected(t, r, fake, "delete_smb_share", "sharing.smb.delete", args, tok)
			if _, ok := f.smb[1]; !ok {
				t.Error("share deleted")
			}
		})
	}
}

// ---- update_nfs_share ----

func TestUpdateNFSShareConfirmFlow(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	args := map[string]interface{}{"share_id": 1.0, "ro": true, "hosts": []interface{}{"192.0.2.10", "client.example.com"}, "maproot_user": "", "maproot_group": "", "comment": "Data"}
	preview, out := confirmAndRun(t, r, fake, "update_nfs_share", "sharing.nfs.update", args)
	assertContainsAll(t, "preview", previewBody(t, preview),
		`"networks":["192.0.2.0/24"]`, `"maproot_user":"nobody"`,
		"ro: false -> true", "maproot_user: nobody -> ", "hosts: [] -> [192.0.2.10,client.example.com]",
		"After the change: Root clients are squashed to nobody (root_squash)")
	p := snapActionParams(t, fake, "sharing.nfs.update")
	if len(p) != 2 || p[0] != 1.0 || previewText(t, p[1]) != `{"comment":"Data","hosts":["192.0.2.10","client.example.com"],"maproot_group":null,"maproot_user":null,"ro":true}` {
		t.Errorf("sharing.nfs.update params = %s", previewText(t, p))
	}
	if resp := decodeObj(t, out); resp["updated"] != true || resp["note"] != nil {
		t.Errorf("response = %v", resp)
	}
}

func TestUpdateNFSShareWarnings(t *testing.T) {
	f := newShareFake()
	r, _ := f.registry(t)
	dry := dryRunOf(t, r, "update_nfs_share", map[string]interface{}{"share_id": 1.0, "networks": []interface{}{}, "maproot_user": "root", "maproot_group": "wheel"})
	assertContainsAll(t, "warnings", previewText(t, dry["warnings"]), "no network or host restrictions", "no_root_squash")
	dry = dryRunOf(t, r, "update_nfs_share", map[string]interface{}{"share_id": 1.0, "enabled": false, "path": "/mnt/tank/shares/new"})
	assertContainsAll(t, "warnings", previewText(t, dry["warnings"]), "lose access to the export", "clients must remount it")
}

func TestUpdateNFSShareRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"locked", map[string]interface{}{"share_id": 2.0, "comment": "x"}, "locked dataset"},
		{"pool root share", map[string]interface{}{"share_id": 3.0, "comment": "x"}, "root of pool"},
		{"system share", map[string]interface{}{"share_id": 4.0, "comment": "x"}, "inside '.system'"},
		{"pool root", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank"}, "root of pool"},
		{"mnt", map[string]interface{}{"share_id": 1.0, "path": "/mnt"}, "not below /mnt/<pool>"},
		{"double slash", map[string]interface{}{"share_id": 1.0, "path": "/mnt//tank/x"}, "not normalised"},
		{"dot dot", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/x/.."}, "not normalised"},
		{"ix-apps", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/ix-apps/x"}, "inside 'ix-apps'"},
		{"boot pool", map[string]interface{}{"share_id": 1.0, "path": "/mnt/boot-pool/ROOT"}, "inside 'boot-pool'"},
		{"symlink", map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/shares/link"}, "goes through a symlink"},
		{"bad cidr", map[string]interface{}{"share_id": 1.0, "networks": []interface{}{"192.0.2.0"}}, "invalid network CIDR"},
		{"bad host", map[string]interface{}{"share_id": 1.0, "hosts": []interface{}{"a b"}}, "invalid host"},
		{"nothing to change", map[string]interface{}{"share_id": 1.0, "ro": false, "networks": []interface{}{"192.0.2.0/24"}, "maproot_user": "nobody"}, "nothing to change"},
		{"not found", map[string]interface{}{"share_id": 9.0, "comment": "x"}, "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFake()
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "update_nfs_share", "sharing.nfs.update", tc.args, tc.want)
		})
	}
}

func TestUpdateNFSShareTokenRejectedAfterStateChange(t *testing.T) {
	for name, change := range map[string]func(f *shareFake){
		"networks changed": func(f *shareFake) { f.nfs[1]["networks"] = []interface{}{} },
		"mapping changed":  func(f *shareFake) { f.nfs[1]["maproot_user"] = "root" },
		"dataset locked":   func(f *shareFake) { f.nfs[1]["locked"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newShareFake()
			r, fake := f.registry(t)
			args := map[string]interface{}{"share_id": 1.0, "ro": true}
			tok := destroyPreviewToken(t, r, "update_nfs_share", args)
			f.mutate(func() { change(f) })
			assertTokenRejected(t, r, fake, "update_nfs_share", "sharing.nfs.update", args, tok)
			if f.nfs[1]["ro"] != false {
				t.Error("share written")
			}
		})
	}
}

// ---- delete_nfs_share ----

func TestDeleteNFSShareConfirmFlow(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	preview, out := confirmAndRun(t, r, fake, "delete_nfs_share", "sharing.nfs.delete", map[string]interface{}{"share_id": 1.0})
	assertContainsAll(t, "preview", previewBody(t, preview),
		`"networks":["192.0.2.0/24"]`, "NFS clients lose access", "every file in it are kept", "files are kept")
	if p := snapActionParams(t, fake, "sharing.nfs.delete"); previewText(t, p) != `[1]` {
		t.Errorf("params = %s", previewText(t, p))
	}
	if resp := decodeObj(t, out); resp["deleted"] != true || resp["data_deleted"] != false || resp["note"] != nil {
		t.Errorf("response = %v", resp)
	}
}

func TestDeleteNFSShareRefusalsAndBinding(t *testing.T) {
	for _, tc := range []struct {
		id   float64
		want string
	}{{2, "locked dataset"}, {3, "root of pool"}, {4, "inside '.system'"}, {9, "not found"}} {
		f := newShareFake()
		r, fake := f.registry(t)
		assertRefused(t, r, fake, "delete_nfs_share", "sharing.nfs.delete", map[string]interface{}{"share_id": tc.id}, tc.want)
	}
	f := newShareFake()
	r, fake := f.registry(t)
	args := map[string]interface{}{"share_id": 1.0}
	tok := destroyPreviewToken(t, r, "delete_nfs_share", args)
	f.mutate(func() { f.nfs[1]["hosts"] = []interface{}{"192.0.2.50"} })
	assertTokenRejected(t, r, fake, "delete_nfs_share", "sharing.nfs.delete", args, tok)
	if _, ok := f.nfs[1]; !ok {
		t.Error("share deleted")
	}
}

// ---- set_share_acl ----

func aclEntry(whoType, who, perm, typ string) map[string]interface{} {
	e := map[string]interface{}{"who_type": whoType, "perm": perm, "type": typ}
	if who != "" {
		e["who"] = who
	}
	return e
}

func aclArgs(share string, entries ...map[string]interface{}) map[string]interface{} {
	list := make([]interface{}, len(entries))
	for i, e := range entries {
		list[i] = e
	}
	return map[string]interface{}{"share_name": share, "entries": list}
}

func TestSetShareACLConfirmFlow(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	args := aclArgs("docs",
		aclEntry("group", "builtin_administrators", "FULL", "ALLOWED"),
		aclEntry("user", "alice", "CHANGE", "ALLOWED"),
		aclEntry("sid", staffSID, "READ", "ALLOWED"),
		aclEntry("everyone", "", "READ", "DENIED"),
	)
	args["allow_no_full_control"] = true
	preview, out := confirmAndRun(t, r, fake, "set_share_acl", "sharing.smb.setacl", args)
	body := previewBody(t, preview)
	assertContainsAll(t, "preview", body,
		`"share_acl":[{"perm":"FULL","sid":"S-1-1-0","type":"ALLOWED","who":"everyone@"}]`,
		"current ACL: everyone@ ALLOWED FULL",
		"new ACL: builtin_administrators ALLOWED FULL; alice ALLOWED CHANGE; staff ALLOWED READ; everyone DENIED READ",
		"replaced as a whole", "filesystem ACL of the share path is not changed", "DENIED entry",
		"allow_no_full_control: it denies READ to everyone")
	p := snapActionParams(t, fake, "sharing.smb.setacl")
	want := `[{"share_acl":[{"ae_perm":"FULL","ae_type":"ALLOWED","ae_who_sid":"S-1-5-32-544"},{"ae_perm":"CHANGE","ae_type":"ALLOWED","ae_who_sid":"S-1-5-21-111-222-333-1001"},{"ae_perm":"READ","ae_type":"ALLOWED","ae_who_sid":"S-1-5-21-111-222-333-2001"},{"ae_perm":"READ","ae_type":"DENIED","ae_who_sid":"S-1-1-0"}],"share_name":"docs"}]`
	if got := previewText(t, p); got != want {
		t.Errorf("sharing.smb.setacl params =\n%s\nwant\n%s", got, want)
	}
	resp := decodeObj(t, out)
	if resp["updated"] != true || resp["note"] != nil {
		t.Errorf("response = %v", resp)
	}
	for _, m := range []string{"filesystem.setacl", "filesystem.setperm", "sharing.smb.update"} {
		if len(fake.called(m)) != 0 {
			t.Errorf("set_share_acl sent %s", m)
		}
	}
}

func TestSetShareACLDryRunShowsBothACLs(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	dry := dryRunOf(t, r, "set_share_acl", aclArgs("media", aclEntry("group", "staff", "FULL", "ALLOWED")))
	cur := dry["current_state"].(map[string]interface{})
	if previewText(t, cur["share_acl"]) != `[{"perm":"FULL","sid":"S-1-1-0","type":"ALLOWED","who":"everyone@"}]` {
		t.Errorf("current share_acl = %s", previewText(t, cur["share_acl"]))
	}
	if share := cur["share"].(map[string]interface{}); share["name"] != "media" || share["path"] != "/mnt/tank/shares/media" {
		t.Errorf("current share = %v", share)
	}
	d := plannedDetails(t, dry)
	if previewText(t, d["new_share_acl"]) != `[{"perm":"FULL","sid":"S-1-5-21-111-222-333-2001","type":"ALLOWED","who":"staff"}]` || d["filesystem_acl_changed"] != false {
		t.Errorf("details = %v", d)
	}
	assertNoShareWrites(t, fake)
}

func TestSetShareACLRefusals(t *testing.T) {
	full := aclEntry("group", "builtin_administrators", "FULL", "ALLOWED")
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"no FULL entry", aclArgs("docs", aclEntry("user", "alice", "CHANGE", "ALLOWED")), "no ALLOWED entry grants FULL"},
		{"FULL only denied", aclArgs("docs", aclEntry("user", "alice", "FULL", "DENIED"), aclEntry("everyone", "", "READ", "ALLOWED")), "allow_no_full_control=true"},
		{"everyone denied", aclArgs("docs", full, aclEntry("everyone", "", "CHANGE", "DENIED")), "denies CHANGE to everyone"},
		{"everyone denied by SID", aclArgs("docs", full, aclEntry("sid", everyoneSID, "READ", "DENIED")), "denies READ to everyone"},
		{"flag not needed", mergeArgs(aclArgs("docs", full), map[string]interface{}{"allow_no_full_control": true}), "omit it"},
		{"unknown user", aclArgs("docs", full, aclEntry("user", "mallory", "READ", "ALLOWED")), "user 'mallory' not found"},
		{"unknown group", aclArgs("docs", full, aclEntry("group", "nobodyhere", "READ", "ALLOWED")), "group 'nobodyhere' not found"},
		{"non-SMB user", aclArgs("docs", full, aclEntry("user", "bob", "READ", "ALLOWED")), "is not an SMB user"},
		{"Unix SID user", aclArgs("docs", full, aclEntry("user", "carol", "READ", "ALLOWED")), "has no SMB SID"},
		{"Unix SID", aclArgs("docs", full, aclEntry("sid", "S-1-22-1-3003", "READ", "ALLOWED")), "Unix-encoded SID"},
		{"unknown SID", aclArgs("docs", full, aclEntry("sid", "S-1-5-21-9-9-9-500", "READ", "ALLOWED")), "does not belong to exactly one local SMB user or group"},
		{"malformed SID", aclArgs("docs", full, aclEntry("sid", "S-1-x", "READ", "ALLOWED")), "is not a SID"},
		{"SID with spaces", aclArgs("docs", full, aclEntry("sid", "S-1-5-32 -544", "READ", "ALLOWED")), "is not a SID"},
		{"who for everyone", aclArgs("docs", full, aclEntry("everyone", "x", "READ", "ALLOWED")), "omit who"},
		{"missing who", aclArgs("docs", full, aclEntry("user", "", "READ", "ALLOWED")), "who is required"},
		{"same principal twice", aclArgs("docs", full, aclEntry("user", "alice", "READ", "ALLOWED"), aclEntry("sid", aliceSID, "CHANGE", "ALLOWED")), "same principal twice"},
		{"everyone twice", aclArgs("docs", aclEntry("everyone", "", "FULL", "ALLOWED"), aclEntry("sid", everyoneSID, "READ", "ALLOWED")), "same principal twice"},
		{"empty", aclArgs("docs"), "at least one share ACL entry"},
		{"unchanged", aclArgs("docs", aclEntry("everyone", "", "FULL", "ALLOWED")), "nothing to change"},
		{"homes share", aclArgs("homes", full), "is the homes share"},
		{"HOMES alias", aclArgs("HOMES", full), "reserved or administrative"},
		{"admin share", aclArgs("ADMIN$", full), "reserved or administrative"},
		{"locked share", aclArgs("vault", full), "locked dataset"},
		{"pool root share", aclArgs("poolroot", full), "root of pool"},
		{"ix-apps share", aclArgs("apps", full), "inside 'ix-apps'"},
		{"unknown share", aclArgs("nosuch", full), "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFake()
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "set_share_acl", "sharing.smb.setacl", tc.args, tc.want)
		})
	}
}

// The override lets a reviewed ACL without FULL through.
func TestSetShareACLAllowNoFullControl(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	args := mergeArgs(aclArgs("docs", aclEntry("everyone", "", "READ", "ALLOWED")), map[string]interface{}{"allow_no_full_control": true})
	preview, _ := confirmAndRun(t, r, fake, "set_share_acl", "sharing.smb.setacl", args)
	assertContainsAll(t, "preview", previewBody(t, preview), "allow_no_full_control: no ALLOWED entry grants FULL", "allow_no_full_control: true")
	if got := previewText(t, f.acl["docs"]); !strings.Contains(got, `"ae_perm":"READ"`) {
		t.Errorf("acl after = %s", got)
	}
}

func TestSetShareACLTokenRejectedAfterStateChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(f *shareFake)
		blocked string
	}{
		{"share ACL changed elsewhere", func(f *shareFake) {
			f.acl["docs"] = []interface{}{shareACE(everyoneSID, "everyone@", "ALLOWED", "CHANGE")}
		}, ""},
		{"principal's SID changed", func(f *shareFake) { f.users[0]["sid"] = "S-1-5-21-111-222-333-1999" }, ""},
		{"principal lost SMB", func(f *shareFake) { f.users[0]["smb"] = false }, "is not an SMB user"},
		{"share settings changed", func(f *shareFake) { f.smb[1]["browsable"] = false }, ""},
		{"share locked", func(f *shareFake) { f.smb[1]["locked"] = true }, "locked dataset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFake()
			r, fake := f.registry(t)
			args := aclArgs("docs", aclEntry("group", "staff", "FULL", "ALLOWED"), aclEntry("user", "alice", "READ", "ALLOWED"))
			tok := destroyPreviewToken(t, r, "set_share_acl", args)
			before := previewText(t, f.acl["docs"])
			f.mutate(func() { tc.change(f) })
			if tc.name != "share ACL changed elsewhere" && previewText(t, f.acl["docs"]) != before {
				t.Fatal("fixture changed the ACL")
			}
			err := assertTokenRejected(t, r, fake, "set_share_acl", "sharing.smb.setacl", args, tok)
			if tc.blocked != "" && (err == nil || !strings.Contains(err.Error(), tc.blocked)) {
				t.Errorf("not refused for the change: err = %v", err)
			}
		})
	}
}

// ---- all ----

// Execute refuses a blocked plan on its own, whatever the caller checked.
func TestShareActionExecuteRefusesBlockedPlan(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	ctx := t.Context()
	for _, tc := range []struct {
		plan    func() (*writePlan, error)
		execute func(*writePlan) (string, error)
	}{
		{func() (*writePlan, error) {
			return planUpdateSMBShare(ctx, r.client, map[string]interface{}{"share_id": 5.0, "comment": "x"})
		},
			func(p *writePlan) (string, error) { return executeUpdateSMBShare(ctx, r.client, nil, p) }},
		{func() (*writePlan, error) {
			return planDeleteSMBShare(ctx, r.client, map[string]interface{}{"share_id": 5.0})
		},
			func(p *writePlan) (string, error) { return executeDeleteSMBShare(ctx, r.client, nil, p) }},
		{func() (*writePlan, error) {
			return planUpdateNFSShare(ctx, r.client, map[string]interface{}{"share_id": 2.0, "ro": true})
		},
			func(p *writePlan) (string, error) { return executeUpdateNFSShare(ctx, r.client, nil, p) }},
		{func() (*writePlan, error) {
			return planDeleteNFSShare(ctx, r.client, map[string]interface{}{"share_id": 2.0})
		},
			func(p *writePlan) (string, error) { return executeDeleteNFSShare(ctx, r.client, nil, p) }},
		{func() (*writePlan, error) {
			return planSetShareACL(ctx, r.client, aclArgs("vault", aclEntry("everyone", "", "FULL", "ALLOWED")))
		}, func(p *writePlan) (string, error) { return executeSetShareACL(ctx, r.client, nil, p) }},
	} {
		plan, err := tc.plan()
		if err != nil || len(plan.Blocked) == 0 {
			t.Fatalf("plan not blocked: %v %v", plan, err)
		}
		if _, err := tc.execute(plan); err == nil || !strings.Contains(err.Error(), "locked dataset") {
			t.Errorf("%s execute: err = %v", plan.Tool, err)
		}
	}
	assertNoShareWrites(t, fake)
}

func TestShareActionMiddlewareError(t *testing.T) {
	f := newShareFake()
	r, fake := f.registry(t)
	args := map[string]interface{}{"share_id": 1.0, "comment": "new"}
	tok := destroyPreviewToken(t, r, "update_smb_share", args)
	f.mutate(func() { f.fail["sharing.smb.update"] = "[EINVAL] sharingsmb_update.comment: invalid" })
	_, err := r.CallTool(t.Context(), "update_smb_share", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "failed to update SMB share docs") || !strings.Contains(err.Error(), "check query_shares") {
		t.Errorf("err = %v", err)
	}
	if len(fake.called("sharing.smb.update")) != 1 {
		t.Error("update not sent once")
	}
}

func TestSharePathRefusal(t *testing.T) {
	for p, want := range map[string]string{
		"/mnt/tank/shares/docs":     "",
		"/mnt/tank/a b/c":           "",
		"/mnt/tank":                 "root of pool",
		"/mnt":                      "not below",
		"/mnt/":                     "not normalised",
		"/":                         "not below",
		"":                          "no path",
		"EXTERNAL":                  "DFS proxy",
		"/mnt/tank/x/":              "not normalised",
		"/mnt/tank//x":              "not normalised",
		"/mnt/tank/x/../y":          "not normalised",
		"/mnt/tank/x/./y":           "not normalised",
		"/mnt/tank/x\n":             "control characters",
		"/mnt/.system/x":            "system path",
		"/mnt/tank/.system":         "inside '.system'",
		"/mnt/tank/ix-apps/a":       "inside 'ix-apps'",
		"/mnt/tank/ix-applications": "inside 'ix-applications'",
		"/mnt/tank/.ix-virt":        "inside '.ix-virt'",
		"/mnt/boot-pool/ROOT":       "inside 'boot-pool'",
		"/mnt/freenas-boot/x":       "inside 'freenas-boot'",
		"/mnt/tank/a/.snap":         "inside '.snap'",
		"/mnt/tank/IX-APPS/a":       "inside 'ix-apps'",
		"/mnt/tank/Ix-Applications": "inside 'ix-applications'",
		"/mnt/Boot-Pool/ROOT":       "inside 'boot-pool'",
		"/mnt/FREENAS-BOOT/x":       "inside 'freenas-boot'",
	} {
		got := sharePathRefusal(p)
		if (want == "") != (got == "") || (want != "" && !strings.Contains(got, want)) {
			t.Errorf("sharePathRefusal(%q) = %q, want %q", p, got, want)
		}
	}
}

// ---- review follow-ups ----

const (
	daveSID        = "S-1-5-21-777-888-999-1104"
	domainAdminSID = "S-1-5-21-777-888-999-512"
)

// withDirectoryAccounts adds a directory service user and group, as
// user.query and group.query return them when a directory is joined.
func withDirectoryAccounts(f *shareFake) {
	f.users = append(f.users, map[string]interface{}{"id": 90.0, "uid": 1000001104.0, "username": `AD\dave`, "smb": true, "sid": daveSID, "local": false})
	f.groups = append(f.groups, map[string]interface{}{"id": 91.0, "gid": 1000000512.0, "name": `AD\domain admins`, "group": `AD\domain admins`, "smb": true, "sid": domainAdminSID, "local": false, "roles": []interface{}{}})
}

func TestSetShareACLRefusesDirectoryAccounts(t *testing.T) {
	full := aclEntry("group", "builtin_administrators", "FULL", "ALLOWED")
	for _, raw := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			entry    map[string]interface{}
			filtered string
			unfilter string
		}{
			{"user by name", aclEntry("user", `AD\dave`, "READ", "ALLOWED"), `local user 'AD\dave' not found`, `user 'AD\dave' is not a local user`},
			{"group by name", aclEntry("group", `AD\domain admins`, "READ", "ALLOWED"), `local group 'AD\domain admins' not found`, `group 'AD\domain admins' is not a local group`},
			{"user by SID", aclEntry("sid", daveSID, "READ", "ALLOWED"), "does not belong to exactly one local SMB user or group", `user 'AD\dave' is not a local user`},
			{"group by SID", aclEntry("sid", domainAdminSID, "READ", "ALLOWED"), "does not belong to exactly one local SMB user or group", `group 'AD\domain admins' is not a local group`},
		} {
			t.Run(fmt.Sprintf("%s raw=%v", tc.name, raw), func(t *testing.T) {
				f := newShareFake()
				withDirectoryAccounts(f)
				f.rawAccounts = raw
				r, fake := f.registry(t)
				want := tc.filtered
				if raw {
					want = tc.unfilter
				}
				assertRefused(t, r, fake, "set_share_acl", "sharing.smb.setacl", aclArgs("docs", full, tc.entry), want)
				for _, m := range []string{"user.query", "group.query"} {
					for _, c := range fake.called(m) {
						if !strings.Contains(string(c.Params), `["local","=",true]`) {
							t.Errorf("%s sent without the local filter: %s", m, c.Params)
						}
					}
				}
			})
		}
	}
}

// A row without a readable local flag is not taken for a local account.
func TestSetShareACLLocalFlagMissingFailsClosed(t *testing.T) {
	full := aclEntry("group", "builtin_administrators", "FULL", "ALLOWED")
	for _, tc := range []struct {
		name  string
		entry map[string]interface{}
		want  string
	}{
		{"user by name", aclEntry("user", "alice", "READ", "ALLOWED"), "user 'alice' is not a local user"},
		{"user by SID", aclEntry("sid", aliceSID, "READ", "ALLOWED"), "user 'alice' is not a local user"},
		{"group by name", aclEntry("group", "staff", "READ", "ALLOWED"), "group 'staff' is not a local group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFake()
			f.rawAccounts = true
			delete(f.users[0], "local")
			delete(f.groups[1], "local")
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "set_share_acl", "sharing.smb.setacl", aclArgs("docs", full, tc.entry), tc.want)
		})
	}
}

// With filters ignored, the per-row name and smb checks still pick the
// right account and refuse the rest.
func TestSetShareACLClientSideAccountChecks(t *testing.T) {
	newFake := func() *shareFake {
		f := newShareFake()
		f.rawAccounts = true
		// erin has an SMB-style SID but SMB is off for her.
		f.users = append(f.users, map[string]interface{}{"id": 13.0, "uid": 3004.0, "username": "erin", "smb": false, "sid": "S-1-5-21-111-222-333-1005", "local": true})
		return f
	}
	f := newFake()
	r, _ := f.registry(t)
	dry := dryRunOf(t, r, "set_share_acl", aclArgs("docs", aclEntry("group", "staff", "FULL", "ALLOWED"), aclEntry("user", "alice", "READ", "ALLOWED")))
	if got := previewText(t, plannedDetails(t, dry)["new_share_acl"]); got != `[{"perm":"FULL","sid":"S-1-5-21-111-222-333-2001","type":"ALLOWED","who":"staff"},{"perm":"READ","sid":"S-1-5-21-111-222-333-1001","type":"ALLOWED","who":"alice"}]` {
		t.Errorf("new_share_acl = %s", got)
	}
	full := aclEntry("group", "staff", "FULL", "ALLOWED")
	for _, tc := range []struct {
		name  string
		entry map[string]interface{}
		want  string
	}{
		{"name not returned by filter", aclEntry("user", "mallory", "READ", "ALLOWED"), "local user 'mallory' not found"},
		{"name differs in case", aclEntry("user", "Alice", "READ", "ALLOWED"), "local user 'Alice' not found"},
		{"non-SMB SID", aclEntry("sid", "S-1-5-21-111-222-333-1005", "READ", "ALLOWED"), "does not belong to exactly one local SMB user or group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "set_share_acl", "sharing.smb.setacl", aclArgs("docs", full, tc.entry), tc.want)
		})
	}
}

func TestSetShareACLAmbiguousPrincipals(t *testing.T) {
	full := aclEntry("group", "builtin_administrators", "FULL", "ALLOWED")
	for _, tc := range []struct {
		name   string
		change func(f *shareFake)
		entry  map[string]interface{}
		want   string
	}{
		{"two users with one name", func(f *shareFake) {
			f.users = append(f.users, map[string]interface{}{"id": 14.0, "uid": 3010.0, "username": "alice", "smb": true, "sid": "S-1-5-21-111-222-333-1010", "local": true})
		}, aclEntry("user", "alice", "READ", "ALLOWED"), "user name 'alice' matches 2 accounts"},
		{"two groups with one name", func(f *shareFake) {
			f.groups = append(f.groups, map[string]interface{}{"id": 51.0, "gid": 3101.0, "name": "staff", "smb": true, "sid": "S-1-5-21-111-222-333-2002", "local": true, "roles": []interface{}{}})
		}, aclEntry("group", "staff", "READ", "ALLOWED"), "group name 'staff' matches 2 accounts"},
		{"SID of a user and a group", func(f *shareFake) {
			f.groups = append(f.groups, map[string]interface{}{"id": 52.0, "gid": 3102.0, "name": "alicegrp", "smb": true, "sid": aliceSID, "local": true, "roles": []interface{}{}})
		}, aclEntry("sid", aliceSID, "READ", "ALLOWED"), "does not belong to exactly one local SMB user or group"},
		{"SID of two users", func(f *shareFake) {
			f.users = append(f.users, map[string]interface{}{"id": 15.0, "uid": 3011.0, "username": "alice2", "smb": true, "sid": aliceSID, "local": true})
		}, aclEntry("sid", aliceSID, "READ", "ALLOWED"), "does not belong to exactly one local SMB user or group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFake()
			tc.change(f)
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "set_share_acl", "sharing.smb.setacl", aclArgs("docs", full, tc.entry), tc.want)
		})
	}
}

// A DENIED entry for an administrative group needs allow_no_full_control.
func TestSetShareACLDeniedAdminGroup(t *testing.T) {
	addGroups := func(f *shareFake) {
		f.groups = append(f.groups,
			map[string]interface{}{"id": 60.0, "gid": 3200.0, "name": "ops", "smb": true, "sid": "S-1-5-21-111-222-333-3200", "local": true, "roles": []interface{}{"SHARING_ADMIN"}},
			map[string]interface{}{"id": 61.0, "gid": 3201.0, "name": "noroles", "smb": true, "sid": "S-1-5-21-111-222-333-3201", "local": true})
	}
	everyoneFull := aclEntry("everyone", "", "FULL", "ALLOWED")
	for _, tc := range []struct {
		name  string
		entry map[string]interface{}
		want  string
	}{
		{"builtin_administrators by name", aclEntry("group", "builtin_administrators", "READ", "DENIED"), "denies READ to group 'builtin_administrators', which is an administrative group"},
		{"builtin_administrators by SID", aclEntry("sid", adminsSID, "CHANGE", "DENIED"), "denies CHANGE to group 'builtin_administrators', which is an administrative group"},
		{"group with roles", aclEntry("group", "ops", "FULL", "DENIED"), "denies FULL to group 'ops', which grants TrueNAS roles (SHARING_ADMIN)"},
		{"group with roles by SID", aclEntry("sid", "S-1-5-21-111-222-333-3200", "READ", "DENIED"), "which grants TrueNAS roles"},
		{"group whose roles cannot be read", aclEntry("group", "noroles", "READ", "DENIED"), "has roles that could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFake()
			addGroups(f)
			r, fake := f.registry(t)
			args := aclArgs("docs", everyoneFull, tc.entry)
			assertRefused(t, r, fake, "set_share_acl", "sharing.smb.setacl", args, tc.want)
			assertRefused(t, r, fake, "set_share_acl", "sharing.smb.setacl", args, "allow_no_full_control=true")
			// The override lets the reviewed ACL through, with the reason shown.
			f2 := newShareFake()
			addGroups(f2)
			r2, fake2 := f2.registry(t)
			preview, _ := confirmAndRun(t, r2, fake2, "set_share_acl", "sharing.smb.setacl", mergeArgs(args, map[string]interface{}{"allow_no_full_control": true}))
			assertContainsAll(t, "preview", previewBody(t, preview), "allow_no_full_control: it denies", tc.want, "administrators can be locked out")
		})
	}
	// DENIED for an ordinary group, and ALLOWED for an administrative
	// one, need no override.
	f := newShareFake()
	addGroups(f)
	r, fake := f.registry(t)
	confirmAndRun(t, r, fake, "set_share_acl", "sharing.smb.setacl", aclArgs("docs", aclEntry("group", "ops", "FULL", "ALLOWED"), aclEntry("group", "staff", "CHANGE", "DENIED")))
	r, fake = newShareFake().registry(t)
	assertRefused(t, r, fake, "set_share_acl", "sharing.smb.setacl",
		mergeArgs(aclArgs("docs", everyoneFull, aclEntry("group", "staff", "READ", "DENIED")), map[string]interface{}{"allow_no_full_control": true}), "omit it")
}

// The host lists are changed only on LEGACY_SHARE shares, even when the
// options of another purpose list them.
func TestUpdateSMBShareHostListsNeedLegacyPurpose(t *testing.T) {
	for _, purpose := range []string{"DEFAULT_SHARE", "MULTIPROTOCOL_SHARE", "TIMEMACHINE_SHARE", "TIME_LOCKED_SHARE", "PRIVATE_DATASETS_SHARE"} {
		t.Run(purpose, func(t *testing.T) {
			f := newShareFake()
			f.smb[1]["purpose"] = purpose
			f.smb[1]["options"] = map[string]interface{}{"hostsallow": []interface{}{}, "hostsdeny": []interface{}{}}
			r, fake := f.registry(t)
			for _, key := range []string{"hostsallow", "hostsdeny"} {
				assertRefused(t, r, fake, "update_smb_share", "sharing.smb.update",
					map[string]interface{}{"share_id": 1.0, key: []interface{}{"192.0.2.10"}},
					"has purpose "+purpose+"; "+key+" can be changed only for LEGACY_SHARE shares")
			}
		})
	}
}

func TestShareHostListsRefuseControlCharacters(t *testing.T) {
	for _, tc := range []struct {
		tool, method string
		args         map[string]interface{}
	}{
		{"update_smb_share", "sharing.smb.update", map[string]interface{}{"share_id": 2.0, "hostsallow": []interface{}{"192.0.2.1\x07"}}},
		{"update_smb_share", "sharing.smb.update", map[string]interface{}{"share_id": 2.0, "hostsdeny": []interface{}{"bad\nhost"}}},
		{"update_nfs_share", "sharing.nfs.update", map[string]interface{}{"share_id": 1.0, "hosts": []interface{}{"client\x00.example.com"}}},
		{"update_nfs_share", "sharing.nfs.update", map[string]interface{}{"share_id": 1.0, "networks": []interface{}{"192.0.2.0/24\x7f"}}},
	} {
		f := newShareFake()
		r, fake := f.registry(t)
		assertRefused(t, r, fake, tc.tool, tc.method, tc.args, "cannot contain control characters")
	}
}

func TestUpdateSharePathRefusesManagedDatasetsInAnyCase(t *testing.T) {
	for _, tc := range []struct {
		p, want string
	}{
		{"/mnt/tank/IX-APPS/x", "inside 'ix-apps'"},
		{"/mnt/tank/Ix-Applications", "inside 'ix-applications'"},
		{"/mnt/Boot-Pool/data", "inside 'boot-pool'"},
	} {
		f := newShareFake()
		r, fake := f.registry(t)
		assertRefused(t, r, fake, "update_smb_share", "sharing.smb.update", map[string]interface{}{"share_id": 1.0, "path": tc.p}, tc.want)
		assertRefused(t, r, fake, "update_nfs_share", "sharing.nfs.update", map[string]interface{}{"share_id": 1.0, "path": tc.p}, tc.want)
	}
	f := newShareFake()
	f.smb[1]["path"] = "/mnt/tank/IX-APPS/app_mounts"
	r, fake := f.registry(t)
	assertRefused(t, r, fake, "delete_smb_share", "sharing.smb.delete", map[string]interface{}{"share_id": 1.0}, "current path \"/mnt/tank/IX-APPS/app_mounts\" is inside 'ix-apps'")
}

// The new NFS path is bound into the token like the SMB one.
func TestUpdateNFSShareNewPathBound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(f *shareFake)
		blocked string
	}{
		{"new path replaced", func(f *shareFake) { f.stat["/mnt/tank/shares/new"]["inode"] = 99.0 }, ""},
		{"new path on another device", func(f *shareFake) { f.stat["/mnt/tank/shares/new"]["dev"] = 2.0 }, ""},
		{"new path now a symlink", func(f *shareFake) {
			f.stat["/mnt/tank/shares/new"] = map[string]interface{}{"realpath": "/mnt/tank/shares/docs", "type": "SYMLINK", "inode": 4.0, "dev": 1.0}
		}, "symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFake()
			r, fake := f.registry(t)
			args := map[string]interface{}{"share_id": 1.0, "path": "/mnt/tank/shares/new"}
			tok := destroyPreviewToken(t, r, "update_nfs_share", args)
			f.mutate(func() { tc.change(f) })
			err := assertTokenRejected(t, r, fake, "update_nfs_share", "sharing.nfs.update", args, tok)
			if tc.blocked != "" && (err == nil || !strings.Contains(err.Error(), tc.blocked)) {
				t.Errorf("not refused for the change: err = %v", err)
			}
			if f.nfs[1]["path"] != "/mnt/tank/shares/data" {
				t.Error("share written")
			}
		})
	}
}

// A delete that does not return true is reported as an unknown outcome.
func TestShareDeleteUnexpectedResult(t *testing.T) {
	for _, res := range []interface{}{false, map[string]interface{}{"id": 1.0}, "ok"} {
		for _, tc := range []struct {
			tool, method string
		}{{"delete_smb_share", "sharing.smb.delete"}, {"delete_nfs_share", "sharing.nfs.delete"}} {
			t.Run(fmt.Sprintf("%s %v", tc.tool, res), func(t *testing.T) {
				f := newShareFake()
				f.deleteResult = res
				r, fake := f.registry(t)
				args := map[string]interface{}{"share_id": 1.0}
				tok := destroyPreviewToken(t, r, tc.tool, args)
				_, err := r.CallTool(t.Context(), tc.tool, mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
				if err == nil || !strings.Contains(err.Error(), tc.method+" returned an unexpected result") || !strings.Contains(err.Error(), "query_shares before retrying") {
					t.Errorf("err = %v", err)
				}
				if n := len(fake.called(tc.method)); n != 1 {
					t.Errorf("%s sent %d time(s)", tc.method, n)
				}
			})
		}
	}
}

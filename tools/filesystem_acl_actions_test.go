package tools

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// fsACLFake serves what set_filesystem_acl reads: filesystem.stat (as
// newACLFake does), filesystem.statfs from the dataset mountpoints,
// pool.dataset.query, filesystem.getacl, filesystem.listdir, the local
// users and groups of account_actions_test.go, and filesystem.setacl as a
// job.
type fsACLFake struct {
	mu    sync.Mutex
	stats map[string]map[string]interface{} // filesystem.stat overrides
	// datasets: name, mountpoint, locked, ro, fstype; and for tests:
	// mounted (statfs reports it even when locked is not false), fsid
	// (statfs fsid), query_mountpoint (pool.dataset.query mountpoint) and
	// query_hidden (pool.dataset.query leaves the row out).
	datasets []map[string]interface{}
	acls     map[string]map[string]interface{} // filesystem.getacl by path
	entries  int                               // filesystem.listdir count
	files    float64                           // statfs files
	free     float64                           // statfs free_files
	users    []map[string]interface{}
	groups   []map[string]interface{}
	fail     map[string]map[string]interface{}
	jobs     []map[string]interface{}
	version  string
}

func fsDataset(name, mp string) map[string]interface{} {
	return map[string]interface{}{"id": name, "name": name, "mountpoint": mp, "locked": false}
}

func newFSACLFake() *fsACLFake {
	ro := fsDataset("tank/ro", "/mnt/tank/ro")
	ro["ro"] = true
	locked := fsDataset("tank/locked", "/mnt/tank/locked")
	locked["locked"] = true
	ext := fsDataset("tank/legacy", "/mnt/tank/legacy")
	ext["fstype"] = "ext4"
	posix := map[string]interface{}{
		"path": "/mnt/tank/shares/posix", "uid": 3001.0, "gid": 4001.0, "user": "exampleuser", "group": "examplegroup",
		"acltype": "POSIX1E", "trivial": true,
		"acl": []interface{}{
			map[string]interface{}{"tag": "USER_OBJ", "id": -1.0, "default": false, "perms": map[string]interface{}{"READ": true, "WRITE": true, "EXECUTE": true}},
			map[string]interface{}{"tag": "GROUP_OBJ", "id": -1.0, "default": false, "perms": map[string]interface{}{"READ": true, "WRITE": false, "EXECUTE": true}},
			map[string]interface{}{"tag": "OTHER", "id": -1.0, "default": false, "perms": map[string]interface{}{"READ": false, "WRITE": false, "EXECUTE": false}},
		},
	}
	return &fsACLFake{
		stats: map[string]map[string]interface{}{
			"/mnt/tank/shares/docs/report.txt": {"realpath": "/mnt/tank/shares/docs/report.txt", "type": "FILE", "mode": float64(0o100660),
				"uid": 3001.0, "gid": 4001.0, "is_mountpoint": false},
			"/mnt/tank/shares/link": linkStat("/mnt/tank/shares/link", "/mnt/tank/shares/docs"),
			"/mnt/tank/shares/posix/notes.txt": {"realpath": "/mnt/tank/shares/posix/notes.txt", "type": "FILE", "mode": float64(0o100660),
				"uid": 3001.0, "gid": 4001.0, "is_mountpoint": false},
		},
		datasets: []map[string]interface{}{
			fsDataset("tank", "/mnt/tank"),
			fsDataset("tank/shares", "/mnt/tank/shares"),
			fsDataset("tank/shares/docs/archive", "/mnt/tank/shares/docs/archive"),
			fsDataset("tank/shares/docs/projects", "/mnt/tank/shares/docs/projects"),
			fsDataset("tank/shares2", "/mnt/tank/shares2"),
			fsDataset("tank/ix-applications/releases", "/mnt/tank/appdata"),
			ro, locked, ext,
		},
		acls: map[string]map[string]interface{}{
			"/mnt/tank/shares/posix":           posix,
			"/mnt/tank/shares/posix/notes.txt": posix,
			"/mnt/tank/shares/plain":           {"path": "/mnt/tank/shares/plain", "uid": 0.0, "gid": 0.0, "acltype": "DISABLED", "trivial": true, "acl": []interface{}{}},
		},
		entries: 12,
		files:   1000,
		free:    400,
		users:   accountUserRows(),
		groups:  accountGroupRows(),
		fail:    map[string]map[string]interface{}{},
	}
}

func (f *fsACLFake) mutate(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// mountFor is the unlocked dataset whose mountpoint holds p most closely,
// as the kernel's mount table would show it.
func (f *fsACLFake) mountFor(p string) map[string]interface{} {
	var best map[string]interface{}
	for _, d := range f.datasets {
		mp, _ := d["mountpoint"].(string)
		if (d["locked"] != false && d["mounted"] != true) || !below(p, mp) {
			continue
		}
		if best == nil || len(mp) > len(best["mountpoint"].(string)) {
			best = d
		}
	}
	return best
}

func (f *fsACLFake) handle(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.fail[method]; ok {
		return nil, e, true
	}
	var p []interface{}
	_ = json.Unmarshal(params, &p)
	arg := func() string {
		if len(p) == 0 {
			return ""
		}
		s, _ := p[0].(string)
		return s
	}
	switch method {
	case "system.info":
		if f.version != "" {
			return map[string]interface{}{"hostname": "nas", "version": f.version}, nil, true
		}
	case "filesystem.stat":
		path := arg()
		if st, ok := f.stats[path]; ok {
			return st, nil, true
		}
		switch {
		case path == "/mnt/tank":
			return mountStat(path), nil, true
		case strings.HasPrefix(path, "/mnt/tank/"):
			st := dirStat(path)
			st["inode"], st["dev"], st["mount_id"] = 100.0, 7.0, 50.0
			return st, nil, true
		}
		return nil, rpcErr("[ENOENT] Path " + path + " not found"), true
	case "filesystem.statfs":
		d := f.mountFor(arg())
		if d == nil {
			return nil, rpcErr("[ENOENT] not mounted"), true
		}
		fstype := "zfs"
		if s, ok := d["fstype"].(string); ok {
			fstype = s
		}
		flags := []interface{}{"RW", "NOATIME"}
		if d["ro"] == true {
			flags = []interface{}{"RO", "NOATIME"}
		}
		fsid := "fsid-" + d["name"].(string)
		if s, ok := d["fsid"].(string); ok {
			fsid = s
		}
		return map[string]interface{}{"source": d["name"], "dest": d["mountpoint"], "fstype": fstype, "flags": flags,
			"fsid": fsid, "files": f.files, "free_files": f.free}, nil, true
	case "pool.dataset.query":
		filters, _ := p[0].([]interface{})
		prefix := ""
		if len(filters) == 1 {
			if c, ok := filters[0].([]interface{}); ok && len(c) == 3 && c[0] == "name" && c[1] == "^" {
				prefix, _ = c[2].(string)
			}
		}
		if prefix == "" {
			return nil, rpcErr("unexpected pool.dataset.query filter"), true
		}
		out := []interface{}{}
		for _, d := range f.datasets {
			if strings.HasPrefix(d["name"].(string), prefix) && d["query_hidden"] != true {
				mp := d["mountpoint"]
				if s, ok := d["query_mountpoint"].(string); ok {
					mp = s
				}
				out = append(out, map[string]interface{}{"id": d["id"], "name": d["name"], "mountpoint": mp, "locked": d["locked"]})
			}
		}
		return out, nil, true
	case "filesystem.getacl":
		if acl, ok := f.acls[arg()]; ok {
			return acl, nil, true
		}
		acl := nfs4ACL(false)
		acl["path"] = arg()
		return acl, nil, true
	case "filesystem.listdir":
		return f.entries, nil, true
	case "user.query":
		return f.users, nil, true
	case "group.query":
		return f.groups, nil, true
	case "core.get_jobs":
		filters, _ := p[0].([]interface{})
		out := []interface{}{}
		for _, j := range f.jobs {
			if jobMatchesFilters(j, filters) {
				out = append(out, j)
			}
		}
		return out, nil, true
	case "filesystem.setacl":
		f.jobs = append(f.jobs, map[string]interface{}{
			"id": 77.0, "method": "filesystem.setacl", "state": "RUNNING", "arguments": p,
			"time_started": map[string]interface{}{"$date": 1767225600000.0},
			"progress":     map[string]interface{}{"percent": 0.0, "description": "", "extra": nil},
		})
		return 77, nil, true
	}
	return nil, nil, false
}

func (f *fsACLFake) registry(t *testing.T) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{handle: f.handle}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

func (f *fsACLFake) registryWithTasks(t *testing.T) (*Registry, *fakeMiddleware, *tasks.Manager) {
	t.Helper()
	fake := &fakeMiddleware{handle: f.handle}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: 5 * time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake, mgr
}

// withFilesystemACLResults adds canned answers to results so
// set_filesystem_acl can plan /mnt/tank/shares/docs in tests that use a
// results table (pool.dataset.query is left to the table).
func withFilesystemACLResults(results map[string]interface{}) map[string]interface{} {
	st := dirStat("/mnt/tank/shares/docs")
	st["is_mountpoint"] = true
	results["filesystem.stat"] = st
	results["filesystem.statfs"] = map[string]interface{}{"source": "tank/shares", "dest": "/mnt/tank/shares", "fstype": "zfs",
		"flags": []interface{}{"RW"}, "fsid": "1", "files": 10.0, "free_files": 5.0}
	results["filesystem.getacl"] = nfs4ACL(false)
	results["filesystem.listdir"] = 3
	return results
}

func nfs4Entry(tag, who, typ, perms, flags string) map[string]interface{} {
	e := map[string]interface{}{"tag": tag, "type": typ, "perms": perms, "flags": flags}
	if who != "" {
		e["who"] = who
	}
	return e
}

func fsACLArgs(p string, entries ...map[string]interface{}) map[string]interface{} {
	list := make([]interface{}, len(entries))
	for i, e := range entries {
		list[i] = e
	}
	return map[string]interface{}{"path": p, "entries": list}
}

func docsACLArgs() map[string]interface{} {
	return fsACLArgs("/mnt/tank/shares/docs",
		nfs4Entry("owner@", "", "ALLOW", "FULL_CONTROL", "INHERIT"),
		nfs4Entry("GROUP", "staff", "ALLOW", "MODIFY", "INHERIT"),
		nfs4Entry("USER", "alice", "ALLOW", "READ", "NOINHERIT"),
	)
}

func setACLPayload(t *testing.T, fake *fakeMiddleware) map[string]interface{} {
	t.Helper()
	return writePayload(t, fake, filesystemSetACLMethod, 0)
}

func assertNoSetACL(t *testing.T, fake *fakeMiddleware) {
	t.Helper()
	if n := len(fake.called(filesystemSetACLMethod)); n != 0 {
		t.Errorf("filesystem.setacl sent %d time(s)", n)
	}
}

func TestSetFilesystemACLClassifiedAndReadOnly(t *testing.T) {
	f := newFSACLFake()
	r, fake := f.registry(t)
	if safetyOf("set_filesystem_acl").Access != Destructive || !hasParam(r.tools["set_filesystem_acl"], "confirmation_token") {
		t.Fatal("set_filesystem_acl is not destructive")
	}
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if listed(r)["set_filesystem_acl"] {
		t.Error("set_filesystem_acl listed in read-only mode")
	}
	for _, extra := range []map[string]interface{}{{}, {"dry_run": true}, {"confirmation_token": "x"}, {"recursive": true}} {
		_, err := r.CallTool(t.Context(), "set_filesystem_acl", mergeArgs(docsACLArgs(), extra))
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%v in read-only mode: err = %v", extra, err)
		}
	}
	for _, m := range []string{"filesystem.stat", "filesystem.getacl", "filesystem.statfs"} {
		if n := len(fake.called(m)); n != 0 {
			t.Errorf("read-only mode sent %s", m)
		}
	}
	assertNoSetACL(t, fake)
}

func TestSetFilesystemACLConfirmFlow(t *testing.T) {
	f := newFSACLFake()
	r, fake, mgr := f.registryWithTasks(t)
	preview, out := confirmAndRun(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, docsACLArgs())
	payload := setACLPayload(t, fake)
	want := `{"acltype":"NFS4","dacl":[` +
		`{"flags":{"BASIC":"INHERIT"},"id":-1,"perms":{"BASIC":"FULL_CONTROL"},"tag":"owner@","type":"ALLOW"},` +
		`{"flags":{"BASIC":"INHERIT"},"id":3100,"perms":{"BASIC":"MODIFY"},"tag":"GROUP","type":"ALLOW"},` +
		`{"flags":{"BASIC":"NOINHERIT"},"id":3000,"perms":{"BASIC":"READ"},"tag":"USER","type":"ALLOW"}],` +
		`"gid":-1,"options":{"canonicalize":true,"recursive":false,"stripacl":false,"traverse":false,"validate_effective_acl":true},` +
		`"path":"/mnt/tank/shares/docs","uid":-1}`
	if got := previewText(t, payload); got != want {
		t.Errorf("filesystem.setacl payload =\n%s\nwant\n%s", got, want)
	}
	if calls := fake.called(filesystemSetACLMethod); len(calls) == 1 {
		var p []interface{}
		_ = json.Unmarshal(calls[0].Params, &p)
		if len(p) != 1 {
			t.Errorf("filesystem.setacl params = %s, want one data argument", calls[0].Params)
		}
	}
	assertContainsAll(t, "preview", previewBody(t, preview),
		"Replace the ACL of /mnt/tank/shares/docs", "recursive: no: only /mnt/tank/shares/docs itself",
		"group staff ALLOW MODIFY (INHERIT)", "user alice ALLOW READ (NOINHERIT)", "owner@ ALLOW FULL_CONTROL (INHERIT)",
		`"stripacl":false`, `"owner_changed":false`, "replaced as a whole", "owner and group of the files are not changed",
		"Only /mnt/tank/shares/docs itself changes", "otheruser")
	if strings.Contains(previewBody(t, preview), "subtree") {
		t.Error("a non-recursive preview shows a subtree")
	}
	if n := len(fake.called("filesystem.listdir")); n != 0 {
		t.Errorf("a non-recursive plan counted entries (%d listdir calls)", n)
	}
	resp := decodeObj(t, out)
	if resp["job_id"] != 77.0 || resp["recursive"] != false || resp["task_id"] == nil {
		t.Fatalf("response = %v", resp)
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "tasks_get") || !strings.Contains(msg, "get_filesystem_acl") {
		t.Errorf("message = %q", msg)
	}
	taskID := resp["task_id"].(string)
	if task := getTask(t, r, mgr, taskID); task["status"] != "working" {
		t.Errorf("task while the job runs = %v", task)
	}
	f.mutate(func() {
		f.jobs[0]["state"] = "SUCCESS"
		f.jobs[0]["result"] = nil
	})
	if task := getTask(t, r, mgr, taskID); task["status"] != "completed" {
		t.Errorf("task after the job finished = %v", task)
	}
}

func TestSetFilesystemACLJobFailureTracked(t *testing.T) {
	f := newFSACLFake()
	r, fake, mgr := f.registryWithTasks(t)
	_, out := confirmAndRun(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, docsACLArgs())
	f.mutate(func() {
		f.jobs[0]["state"] = "FAILED"
		f.jobs[0]["error"] = "[EINVAL] filesystem.acl.dacl: fake failure"
	})
	if task := getTask(t, r, mgr, decodeObj(t, out)["task_id"].(string)); task["status"] != "failed" {
		t.Errorf("task = %v", task)
	}
}

func TestSetFilesystemACLWithoutTaskManager(t *testing.T) {
	f := newFSACLFake()
	r, fake := f.registry(t)
	_, out := confirmAndRun(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, docsACLArgs())
	if resp := decodeObj(t, out); resp["job_id"] != 77.0 || resp["task_id"] != nil {
		t.Errorf("response = %v", resp)
	}
}

func TestSetFilesystemACLRecursiveShowsSubtree(t *testing.T) {
	f := newFSACLFake()
	r, fake := f.registry(t)
	args := mergeArgs(docsACLArgs(), map[string]interface{}{"recursive": true})
	preview, out := confirmAndRun(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, args)
	opts, _ := setACLPayload(t, fake)["options"].(map[string]interface{})
	if opts["recursive"] != true || opts["traverse"] != false || opts["stripacl"] != false {
		t.Errorf("options = %v", opts)
	}
	body := previewBody(t, preview)
	assertContainsAll(t, "preview", body,
		`"entries_directly_in_path":12`, `"dataset_objects_in_use":600`, `"child_dataset_count":2`, `"traverse":false`,
		"tank/shares/docs/archive", "tank/shares/docs/projects",
		"recursive: yes: 12 entries directly in /mnt/tank/shares/docs; dataset tank/shares has 600 objects in use",
		"2 child dataset(s) not changed", "Every file and directory below /mnt/tank/shares/docs", "Child datasets below",
		"and everything below it in dataset tank/shares")
	if strings.Contains(body, "tank/shares2") {
		t.Error("a sibling dataset is listed as a child")
	}
	if calls := fake.called("filesystem.listdir"); len(calls) == 0 || !strings.Contains(string(calls[0].Params), `"count":true`) {
		t.Errorf("listdir calls = %v, want a count", calls)
	}
	if resp := decodeObj(t, out); resp["recursive"] != true {
		t.Errorf("response = %v", resp)
	}
}

// recursive is off unless it is the JSON boolean true, and the token of a
// non-recursive preview does not run a recursive change.
func TestSetFilesystemACLRecursiveNeedsExplicitFlag(t *testing.T) {
	f := newFSACLFake()
	r, fake := f.registry(t)
	tok := destroyPreviewToken(t, r, "set_filesystem_acl", docsACLArgs())
	_, err := r.CallTool(t.Context(), "set_filesystem_acl", mergeArgs(docsACLArgs(), map[string]interface{}{"recursive": true, "confirmation_token": tok}))
	if err == nil {
		t.Error("a non-recursive token ran a recursive change")
	}
	for _, bad := range []interface{}{"true", 1, 1.0} {
		if _, err := r.CallTool(t.Context(), "set_filesystem_acl", mergeArgs(docsACLArgs(), map[string]interface{}{"recursive": bad})); err == nil {
			t.Errorf("recursive %v accepted", bad)
		}
	}
	assertNoSetACL(t, fake)

	_, _ = confirmAndRun(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, mergeArgs(docsACLArgs(), map[string]interface{}{"recursive": false}))
	if opts, _ := setACLPayload(t, fake)["options"].(map[string]interface{}); opts["recursive"] != false {
		t.Errorf("options = %v", opts)
	}
}

func TestSetFilesystemACLPOSIX(t *testing.T) {
	f := newFSACLFake()
	r, fake := f.registry(t)
	posix := func(tag, who, perms string, def bool) map[string]interface{} {
		e := map[string]interface{}{"tag": tag, "posix_perms": perms}
		if who != "" {
			e["who"] = who
		}
		if def {
			e["default"] = true
		}
		return e
	}
	args := fsACLArgs("/mnt/tank/shares/posix",
		posix("USER_OBJ", "", "rwx", false), posix("GROUP_OBJ", "", "r-x", false), posix("OTHER", "", "---", false),
		posix("GROUP", "staff", "rwx", false), posix("MASK", "", "rwx", false),
		posix("USER_OBJ", "", "rwx", true), posix("GROUP_OBJ", "", "r-x", true), posix("OTHER", "", "---", true),
	)
	preview, _ := confirmAndRun(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, args)
	payload := setACLPayload(t, fake)
	dacl, _ := payload["dacl"].([]interface{})
	if payload["acltype"] != "POSIX1E" || len(dacl) != 8 {
		t.Fatalf("payload = %v", payload)
	}
	if got := previewText(t, dacl[3]); got != `{"default":false,"id":3100,"perms":{"EXECUTE":true,"READ":true,"WRITE":true},"tag":"GROUP"}` {
		t.Errorf("named group entry = %s", got)
	}
	if got := previewText(t, dacl[2]); got != `{"default":false,"id":-1,"perms":{"EXECUTE":false,"READ":false,"WRITE":false},"tag":"OTHER"}` {
		t.Errorf("other entry = %s", got)
	}
	if dacl[5].(map[string]interface{})["default"] != true {
		t.Errorf("default entry = %v", dacl[5])
	}
	assertContainsAll(t, "preview", previewBody(t, preview), "access group staff rwx", "default USER_OBJ rwx", "acl type: POSIX1E")
}

func TestSetFilesystemACLEntryErrors(t *testing.T) {
	posix := "/mnt/tank/shares/posix"
	docs := "/mnt/tank/shares/docs"
	e := func(kv ...interface{}) map[string]interface{} {
		m := map[string]interface{}{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	ok := nfs4Entry("owner@", "", "ALLOW", "FULL_CONTROL", "INHERIT")
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"no entries", map[string]interface{}{"path": docs, "entries": []interface{}{}}, "at least one ACL entry"},
		{"posix tag on nfs4", fsACLArgs(docs, e("tag", "USER_OBJ", "posix_perms", "rwx")), "not an NFSv4 tag"},
		{"nfs4 tag on posix", fsACLArgs(posix, ok), "not a POSIX tag"},
		{"posix field on nfs4", fsACLArgs(docs, mergeArgs(ok, e("default", true))), "default is for POSIX ACLs only"},
		{"nfs4 field on posix", fsACLArgs(posix, e("tag", "USER_OBJ", "posix_perms", "rwx", "type", "ALLOW")), "type is for NFSv4 ACLs only"},
		{"no type", fsACLArgs(docs, e("tag", "owner@", "perms", "READ", "flags", "INHERIT")), "type ALLOW or DENY is required"},
		{"no perms", fsACLArgs(docs, e("tag", "owner@", "type", "ALLOW", "flags", "INHERIT")), "exactly one of perms or advanced_perms"},
		{"both perms", fsACLArgs(docs, mergeArgs(ok, e("advanced_perms", []interface{}{"READ_DATA"}))), "exactly one of perms or advanced_perms"},
		{"empty advanced perms", fsACLArgs(docs, e("tag", "owner@", "type", "ALLOW", "advanced_perms", []interface{}{}, "flags", "INHERIT")), "at least one permission"},
		{"duplicate perms", fsACLArgs(docs, e("tag", "owner@", "type", "ALLOW", "advanced_perms", []interface{}{"READ_DATA", "READ_DATA"}, "flags", "INHERIT")), "lists READ_DATA twice"},
		{"no flags", fsACLArgs(docs, e("tag", "owner@", "type", "ALLOW", "perms", "READ")), "exactly one of flags or advanced_flags"},
		{"inherit only alone", fsACLArgs(docs, e("tag", "owner@", "type", "ALLOW", "perms", "READ", "advanced_flags", []interface{}{"INHERIT_ONLY"})), "INHERIT_ONLY needs"},
		{"who on special", fsACLArgs(docs, mergeArgs(ok, e("who", "alice"))), "omit who for tag owner@"},
		{"no who", fsACLArgs(docs, nfs4Entry("USER", "", "ALLOW", "READ", "NOINHERIT")), "who (a local user name) is required"},
		{"bad who", fsACLArgs(docs, nfs4Entry("USER", "a b", "ALLOW", "READ", "NOINHERIT")), "who"},
		{"inherit on file", fsACLArgs(docs+"/report.txt", ok), "inheritance flags apply only to directories"},
		{"posix perms", fsACLArgs(posix, e("tag", "USER_OBJ", "posix_perms", "rxw")), "posix_perms is required"},
		{"posix no other", fsACLArgs(posix, e("tag", "USER_OBJ", "posix_perms", "rwx"), e("tag", "GROUP_OBJ", "posix_perms", "r-x")), "exactly one OTHER entry"},
		{"posix no mask", fsACLArgs(posix, e("tag", "USER_OBJ", "posix_perms", "rwx"), e("tag", "GROUP_OBJ", "posix_perms", "r-x"),
			e("tag", "OTHER", "posix_perms", "---"), e("tag", "USER", "who", "alice", "posix_perms", "rwx")), "needs a MASK entry"},
		{"posix default incomplete", fsACLArgs(posix, e("tag", "USER_OBJ", "posix_perms", "rwx"), e("tag", "GROUP_OBJ", "posix_perms", "r-x"),
			e("tag", "OTHER", "posix_perms", "---"), e("tag", "USER_OBJ", "posix_perms", "rwx", "default", true)), "the default ACL needs exactly one GROUP_OBJ"},
		{"posix two masks", fsACLArgs(posix, e("tag", "USER_OBJ", "posix_perms", "rwx"), e("tag", "GROUP_OBJ", "posix_perms", "r-x"),
			e("tag", "OTHER", "posix_perms", "---"), e("tag", "MASK", "posix_perms", "rwx"), e("tag", "MASK", "posix_perms", "r-x")), "has 2 MASK entries"},
		{"posix default on a file", fsACLArgs(posix+"/notes.txt", e("tag", "USER_OBJ", "posix_perms", "rw-", "default", true)), "default entries apply only to directories"},
		{"recursive on a file", mergeArgs(fsACLArgs(docs+"/report.txt", nfs4Entry("owner@", "", "ALLOW", "READ", "NOINHERIT")), e("recursive", true)), "recursive applies only to directories"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFSACLFake()
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, tc.args, tc.want)
		})
	}
}

func TestSetFilesystemACLPathRefusals(t *testing.T) {
	entry := nfs4Entry("owner@", "", "ALLOW", "FULL_CONTROL", "NOINHERIT")
	for _, tc := range []struct {
		name, path string
		setup      func(f *fsACLFake)
		want       string
	}{
		{"outside the pools", "/etc/ssh", nil, "only paths under /mnt/<pool>"},
		{"relative", "mnt/tank/x", nil, "must be absolute"},
		{"dot dot", "/mnt/tank/shares/../x", nil, "'..'"},
		{"pool root", "/mnt/tank", nil, "root of pool 'tank'"},
		{"system mount", "/mnt/.ix-apps/x", nil, "system path"},
		{"dot-prefixed", "/mnt/tank/.hidden/x", nil, "inside '.hidden'"},
		{"ix-applications", "/mnt/tank/ix-applications/releases", nil, "inside 'ix-applications'"},
		{"ix-apps upper case", "/mnt/tank/IX-APPS/x", nil, "which TrueNAS manages itself"},
		{"system dataset", "/mnt/tank/.system/samba4", nil, "inside '.system'"},
		{"boot pool", "/mnt/boot-pool/ROOT", nil, "which TrueNAS manages itself"},
		{"managed dataset at another mountpoint", "/mnt/tank/appdata/x", nil, "dataset tank/ix-applications/releases, inside 'ix-applications'"},
		{"symlink", "/mnt/tank/shares/link", nil, "goes through a symlink"},
		{"missing", "/mnt/other/x", nil, "cannot be used"},
		{"read-only dataset", "/mnt/tank/ro/x", nil, "mounted read-only"},
		{"locked dataset", "/mnt/tank/locked/x", nil, "inside dataset tank/locked, which is locked"},
		{"not zfs", "/mnt/tank/legacy/x", nil, `not on a ZFS dataset (filesystem type "ext4")`},
		{"acl disabled", "/mnt/tank/shares/plain", nil, "ACL type DISABLED"},
		{"lock state unknown", "/mnt/tank/shares/docs", func(f *fsACLFake) { f.datasets[1]["locked"] = nil }, "tank/shares, which is locked or whose lock state could not be read"},
		{"other pool", "/mnt/tank/shares/docs", func(f *fsACLFake) { f.datasets[1]["name"] = "scratch/shares" }, `not on a dataset of pool 'tank'`},
		{"statfs fails", "/mnt/tank/shares/docs", func(f *fsACLFake) { f.fail["filesystem.statfs"] = rpcErr("boom") }, "failed to read the filesystem"},
		{"getacl fails", "/mnt/tank/shares/docs", func(f *fsACLFake) { f.fail["filesystem.getacl"] = rpcErr("boom") }, "failed to get the ACL"},
		{"unknown acl type", "/mnt/tank/shares/docs", func(f *fsACLFake) {
			f.acls["/mnt/tank/shares/docs"] = map[string]interface{}{"acltype": "NEWTYPE", "acl": []interface{}{}}
		}, `ACL type "NEWTYPE"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFSACLFake()
			if tc.setup != nil {
				tc.setup(f)
			}
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, fsACLArgs(tc.path, entry), tc.want)
		})
	}
}

// The recursive size check fails closed.
func TestSetFilesystemACLRecursiveCountFailureBlocks(t *testing.T) {
	f := newFSACLFake()
	f.fail["filesystem.listdir"] = rpcErr("boom")
	r, fake := f.registry(t)
	assertRefused(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod,
		mergeArgs(docsACLArgs(), map[string]interface{}{"recursive": true}), "size of the recursive change cannot be shown")
	// Without recursive, entries are not counted and the plan goes ahead.
	if _, err := r.CallTool(t.Context(), "set_filesystem_acl", mergeArgs(docsACLArgs(), map[string]interface{}{"dry_run": true})); err != nil {
		t.Errorf("non-recursive dry run: %v", err)
	}
}

func TestSetFilesystemACLRefusesDirectoryServicePrincipals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry map[string]interface{}
		want  string
	}{
		{"directory user", nfs4Entry("USER", "remote", "ALLOW", "READ", "NOINHERIT"), "local user 'remote' not found (directory service"},
		{"unknown user", nfs4Entry("USER", "ghost", "ALLOW", "READ", "NOINHERIT"), "local user 'ghost' not found"},
		{"directory group", nfs4Entry("GROUP", "domain_users", "ALLOW", "READ", "NOINHERIT"), "local group 'domain_users' not found (directory service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFSACLFake()
			f.groups = append(f.groups, func() map[string]interface{} {
				g := accountGroup(90, 1500000, "domain_users", []interface{}{})
				g["local"] = false
				return g
			}())
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod,
				fsACLArgs("/mnt/tank/shares/docs", nfs4Entry("owner@", "", "ALLOW", "FULL_CONTROL", "INHERIT"), tc.entry), tc.want)
		})
	}
}

func TestSetFilesystemACLRefusedArguments(t *testing.T) {
	f := newFSACLFake()
	r, fake := f.registry(t)
	for key, want := range map[string]string{
		"stripacl": "stripping", "uid": "never changes the owner", "group": "never changes the owner",
		"traverse": "never cross into child datasets", "acltype": "changing the ACL type", "dacl": "use entries",
		"options": "use recursive",
	} {
		_, err := r.CallTool(t.Context(), "set_filesystem_acl", mergeArgs(docsACLArgs(), map[string]interface{}{key: true}))
		if err == nil || !strings.Contains(err.Error(), key+" is refused: ") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", key, err)
		}
	}
	assertNoSetACL(t, fake)
}

func TestSetFilesystemACLWarnings(t *testing.T) {
	f := newFSACLFake()
	r, fake := f.registry(t)
	dry := dryRunOf(t, r, "set_filesystem_acl", fsACLArgs("/mnt/tank/shares/docs",
		nfs4Entry("USER", "alice", "DENY", "MODIFY", "NOINHERIT"),
		nfs4Entry("everyone@", "", "ALLOW", "READ", "INHERIT")))
	assertContainsAll(t, "warnings", previewText(t, dry["warnings"]), "No ALLOW entry grants FULL_CONTROL or WRITE_ACL", "A DENY entry")
	dry = dryRunOf(t, r, "set_filesystem_acl", fsACLArgs("/mnt/tank/shares/docs",
		map[string]interface{}{"tag": "owner@", "type": "ALLOW", "advanced_perms": []interface{}{"READ_DATA", "WRITE_ACL"}, "advanced_flags": []interface{}{}}))
	if w := previewText(t, dry["warnings"]); strings.Contains(w, "No ALLOW entry grants") {
		t.Errorf("WRITE_ACL grant still warned: %s", w)
	}
	assertNoSetACL(t, fake)
}

func TestSetFilesystemACLTokenBoundToState(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(f *fsACLFake)
	}{
		{"acl changed", func(f *fsACLFake) {
			acl := nfs4ACL(false)
			acl["acl"] = acl["acl"].([]interface{})[1:]
			f.acls["/mnt/tank/shares/docs"] = acl
		}},
		{"owner changed", func(f *fsACLFake) {
			st := dirStat("/mnt/tank/shares/docs")
			st["uid"] = 0.0
			f.stats["/mnt/tank/shares/docs"] = st
		}},
		{"directory replaced", func(f *fsACLFake) {
			st := dirStat("/mnt/tank/shares/docs")
			st["inode"] = 999.0
			f.stats["/mnt/tank/shares/docs"] = st
		}},
		{"principal uid changed", func(f *fsACLFake) {
			for _, u := range f.users {
				if u["username"] == "alice" {
					u["uid"] = 3999
				}
			}
		}},
		{"group gid changed", func(f *fsACLFake) {
			for _, g := range f.groups {
				if g["name"] == "staff" {
					g["gid"] = 3999
				}
			}
		}},
		{"remounted", func(f *fsACLFake) { f.datasets[1]["name"] = "tank/shares-new" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newFSACLFake()
			r, fake := f.registry(t)
			args := docsACLArgs()
			tok := destroyPreviewToken(t, r, "set_filesystem_acl", args)
			f.mutate(func() { change.mutate(f) })
			assertTokenRejected(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, args, tok)
		})
	}
	// A recursive plan is bound to the child datasets it leaves alone.
	f := newFSACLFake()
	r, fake := f.registry(t)
	args := mergeArgs(docsACLArgs(), map[string]interface{}{"recursive": true})
	tok := destroyPreviewToken(t, r, "set_filesystem_acl", args)
	f.mutate(func() {
		f.datasets = append(f.datasets, fsDataset("tank/shares/docs/new", "/mnt/tank/shares/docs/new"))
	})
	assertTokenRejected(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, args, tok)

	// A state that is now refused is refused on confirm too.
	for _, change := range []struct {
		name   string
		mutate func(f *fsACLFake)
	}{
		{"made read-only", func(f *fsACLFake) { f.datasets[1]["ro"] = true }},
		{"principal removed", func(f *fsACLFake) { f.users = f.users[:3] }},
		{"replaced by a symlink", func(f *fsACLFake) {
			f.stats["/mnt/tank/shares/docs"] = linkStat("/mnt/tank/shares/docs", "/mnt/tank/other")
		}},
		{"acl disabled", func(f *fsACLFake) {
			f.acls["/mnt/tank/shares/docs"] = map[string]interface{}{"acltype": "DISABLED", "acl": []interface{}{}}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newFSACLFake()
			r, fake := f.registry(t)
			args := docsACLArgs()
			tok := destroyPreviewToken(t, r, "set_filesystem_acl", args)
			f.mutate(func() { change.mutate(f) })
			if _, err := r.CallTool(t.Context(), "set_filesystem_acl", mergeArgs(args, map[string]interface{}{"confirmation_token": tok})); err == nil {
				t.Error("ran after the state changed")
			}
			assertNoSetACL(t, fake)
		})
	}
}

func TestSetFilesystemACLMiddlewareErrors(t *testing.T) {
	f := newFSACLFake()
	r, _ := f.registry(t)
	tok := destroyPreviewToken(t, r, "set_filesystem_acl", docsACLArgs())
	f.mutate(func() { f.fail[filesystemSetACLMethod] = rpcErr("[EPERM] fake") })
	_, err := r.CallTool(t.Context(), "set_filesystem_acl", mergeArgs(docsACLArgs(), map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "failed to set the ACL of /mnt/tank/shares/docs") || !strings.Contains(err.Error(), "query_jobs") {
		t.Errorf("err = %v", err)
	}
}

func TestSetFilesystemACLWarnsOnUncheckedVersion(t *testing.T) {
	const note = "only checked against TrueNAS 25.10"
	for version, want := range map[string]bool{"25.10.0": false, "25.10.2.1": false, "26.04.0": true, "27.0.0-MASTER": true} {
		t.Run(version, func(t *testing.T) {
			f := newFSACLFake()
			f.version = version
			r, fake := f.registry(t)
			if err := r.client.RequireSupportedVersion(t.Context()); err != nil {
				t.Fatal(err)
			}
			dry := dryRunOf(t, r, "set_filesystem_acl", docsACLArgs())
			w := previewText(t, dry["warnings"])
			if got := strings.Contains(w, note); got != want {
				t.Errorf("version warning shown = %v, want %v:\n%s", got, want, w)
			}
			if want {
				assertContainsAll(t, "warnings", w, "runs "+version, "get_filesystem_acl")
			}
			assertNoSetACL(t, fake)
		})
	}
}

// The path's own dataset must be found and reported unlocked. The fake's
// statfs still reports the dataset ("mounted"), so only the dataset-row
// check can refuse.
func TestSetFilesystemACLOwnDatasetLockState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(d map[string]interface{})
		want  string
	}{
		{"locked", func(d map[string]interface{}) { d["locked"] = true }, "dataset tank/shares is locked, or its lock state could not be read"},
		{"lock state missing", func(d map[string]interface{}) { delete(d, "locked") }, "dataset tank/shares is locked, or its lock state could not be read"},
		{"lock state not a boolean", func(d map[string]interface{}) { d["locked"] = "no" }, "dataset tank/shares is locked, or its lock state could not be read"},
		{"dataset row missing", func(d map[string]interface{}) { d["query_hidden"] = true }, "dataset tank/shares of path /mnt/tank/shares/docs was not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFSACLFake()
			f.datasets[1]["mounted"] = true
			tc.setup(f.datasets[1])
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, docsACLArgs(), tc.want)
		})
	}
}

// Each part of the binding is changed on its own, with the request and the
// summary unchanged, so only that part of the binding can reject the token.
func TestSetFilesystemACLTokenBoundToIdentity(t *testing.T) {
	docsStat := func(fn func(st map[string]interface{})) func(f *fsACLFake) {
		return func(f *fsACLFake) {
			st := dirStat("/mnt/tank/shares/docs")
			st["inode"], st["dev"], st["mount_id"] = 100.0, 7.0, 50.0
			fn(st)
			f.stats["/mnt/tank/shares/docs"] = st
		}
	}
	setID := func(rows []map[string]interface{}, key, name string) {
		for _, row := range rows {
			if row[key] == name {
				row["id"] = 777
			}
		}
	}
	for _, change := range []struct {
		name   string
		mutate func(f *fsACLFake)
	}{
		{"directory replaced at the same path", docsStat(func(st map[string]interface{}) { st["inode"] = 999.0 })},
		{"mounted again at the same path", docsStat(func(st map[string]interface{}) { st["mount_id"] = 51.0 })},
		{"device changed", docsStat(func(st map[string]interface{}) { st["dev"] = 8.0 })},
		{"realpath changed", docsStat(func(st map[string]interface{}) { st["realpath"] = "/mnt/tank/shares/docs-old" })},
		{"filesystem changed", func(f *fsACLFake) { f.datasets[1]["fsid"] = "fsid-recreated" }},
		{"dataset row changed", func(f *fsACLFake) { f.datasets[1]["query_mountpoint"] = "/mnt/tank/shares-moved" }},
		{"user principal recreated", func(f *fsACLFake) { setID(f.users, "username", "alice") }},
		{"group principal recreated", func(f *fsACLFake) { setID(f.groups, "name", "staff") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newFSACLFake()
			r, fake := f.registry(t)
			args := docsACLArgs()
			tok := destroyPreviewToken(t, r, "set_filesystem_acl", args)
			f.mutate(func() { change.mutate(f) })
			assertTokenRejected(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, args, tok)
		})
	}
	// Unchanged, the same token runs.
	f := newFSACLFake()
	r, fake := f.registry(t)
	confirmAndRun(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, docsACLArgs())
}

// A DENY entry never counts as granting the right to change the ACL.
func TestSetFilesystemACLLockoutWarningIgnoresDeny(t *testing.T) {
	const lockout = "No ALLOW entry grants FULL_CONTROL or WRITE_ACL"
	f := newFSACLFake()
	r, fake := f.registry(t)
	for name, entries := range map[string][]map[string]interface{}{
		"deny full control": {
			nfs4Entry("everyone@", "", "DENY", "FULL_CONTROL", "NOINHERIT"),
			nfs4Entry("owner@", "", "ALLOW", "READ", "NOINHERIT"),
		},
		"deny write acl": {
			{"tag": "everyone@", "type": "DENY", "advanced_perms": []interface{}{"WRITE_ACL"}, "advanced_flags": []interface{}{}},
			nfs4Entry("owner@", "", "ALLOW", "MODIFY", "NOINHERIT"),
		},
	} {
		dry := dryRunOf(t, r, "set_filesystem_acl", fsACLArgs("/mnt/tank/shares/docs", entries...))
		assertContainsAll(t, name, previewText(t, dry["warnings"]), lockout, "only the file's owner", "and root can change this ACL")
	}
	dry := dryRunOf(t, r, "set_filesystem_acl", fsACLArgs("/mnt/tank/shares/docs",
		nfs4Entry("everyone@", "", "ALLOW", "FULL_CONTROL", "NOINHERIT")))
	if w := previewText(t, dry["warnings"]); strings.Contains(w, lockout) {
		t.Errorf("ALLOW FULL_CONTROL still warned: %s", w)
	}
	assertNoSetACL(t, fake)
}

// A recursive change gives everything below the path the inherited ACL,
// so it needs something to inherit.
func TestSetFilesystemACLRecursiveNeedsInheritableEntries(t *testing.T) {
	posix := func(tag, perms string, def bool) map[string]interface{} {
		return map[string]interface{}{"tag": tag, "posix_perms": perms, "default": def}
	}
	access := []map[string]interface{}{posix("USER_OBJ", "rwx", false), posix("GROUP_OBJ", "r-x", false), posix("OTHER", "---", false)}
	defaults := []map[string]interface{}{posix("USER_OBJ", "rwx", true), posix("GROUP_OBJ", "r-x", true), posix("OTHER", "---", true)}
	recursive := map[string]interface{}{"recursive": true}
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string // "" when the plan goes ahead
	}{
		{"nfs4 noinherit only", fsACLArgs("/mnt/tank/shares/docs",
			nfs4Entry("owner@", "", "ALLOW", "FULL_CONTROL", "NOINHERIT"),
			nfs4Entry("everyone@", "", "ALLOW", "READ", "NOINHERIT")), "recursive needs at least one inheritable entry"},
		{"nfs4 no advanced flags", fsACLArgs("/mnt/tank/shares/docs",
			map[string]interface{}{"tag": "owner@", "type": "ALLOW", "perms": "FULL_CONTROL", "advanced_flags": []interface{}{"NO_PROPAGATE_INHERIT"}}),
			"recursive needs at least one inheritable entry"},
		{"posix without defaults", fsACLArgs("/mnt/tank/shares/posix", access...), "recursive needs default entries"},
		{"nfs4 inherit", docsACLArgs(), ""},
		{"nfs4 directory inherit only", fsACLArgs("/mnt/tank/shares/docs",
			map[string]interface{}{"tag": "owner@", "type": "ALLOW", "perms": "FULL_CONTROL", "advanced_flags": []interface{}{"DIRECTORY_INHERIT"}}), ""},
		{"nfs4 file inherit only", fsACLArgs("/mnt/tank/shares/docs",
			map[string]interface{}{"tag": "owner@", "type": "ALLOW", "perms": "FULL_CONTROL", "advanced_flags": []interface{}{"FILE_INHERIT"}}), ""},
		{"posix with defaults", fsACLArgs("/mnt/tank/shares/posix", append(append([]map[string]interface{}{}, access...), defaults...)...), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFSACLFake()
			r, fake := f.registry(t)
			if tc.want != "" {
				assertRefused(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, mergeArgs(tc.args, recursive), tc.want)
				// The same ACL without recursive goes ahead.
				dryRunOf(t, r, "set_filesystem_acl", tc.args)
				return
			}
			confirmAndRun(t, r, fake, "set_filesystem_acl", filesystemSetACLMethod, mergeArgs(tc.args, recursive))
		})
	}
}

func TestBelowBoundary(t *testing.T) {
	for _, tc := range []struct {
		p, mp string
		want  bool
	}{
		{"/mnt/tank/data", "/mnt/tank/data", true},
		{"/mnt/tank/data/x", "/mnt/tank/data", true},
		{"/mnt/tank/data2", "/mnt/tank/data", false},
		{"/mnt/tank/data2/x", "/mnt/tank/data", false},
		{"/mnt/tank", "/mnt/tank/data", false},
		{"/mnt/tank/data", "", false},
	} {
		if got := below(tc.p, tc.mp); got != tc.want {
			t.Errorf("below(%q, %q) = %v, want %v", tc.p, tc.mp, got, tc.want)
		}
	}
}

// An unknown TrueNAS version is not taken to be 25.10: the preview says
// the version could not be read instead of proceeding silently.
func TestSetFilesystemACLWarnsOnUnknownVersion(t *testing.T) {
	f := newFSACLFake()
	r, fake := f.registry(t) // the version is never loaded
	dry := dryRunOf(t, r, "set_filesystem_acl", docsACLArgs())
	assertContainsAll(t, "warnings", previewText(t, dry["warnings"]),
		"only checked against TrueNAS 25.10", "runs a version that could not be read", "get_filesystem_acl")
	assertNoSetACL(t, fake)
}

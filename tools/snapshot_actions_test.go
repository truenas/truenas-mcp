package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// snapActionFake answers the snapshot and dataset methods clone_snapshot,
// hold_snapshot and release_snapshot use, from mutable fixtures, the way
// 25.10 does: hold of an already-held snapshot fails (ZFS EEXIST), release
// of a snapshot without holds is a silent no-op, and release drops every
// hold. Snapshot rows keep native properties under "properties"; a query
// returns only the ones it asks for in extra.properties.
type snapActionFake struct {
	mu        sync.Mutex
	snapshots map[string]map[string]interface{} // id -> row (holds as a map)
	datasets  map[string]map[string]interface{} // name -> row
	retention map[string]interface{}            // id -> middleware retention annotation
	failRet   bool                              // extra.retention queries fail
	fail      map[string]map[string]interface{} // method -> RPC error
	cloneOut  interface{}                       // pool.snapshot.clone result; default true
}

func fsRow(name, mountpoint string) map[string]interface{} {
	row := map[string]interface{}{"id": name, "name": name, "pool": datasetPool(name), "type": "FILESYSTEM",
		"mountpoint": mountpoint, "locked": false, "encrypted": false, "encryption_root": nil}
	if mountpoint == "" {
		row["mountpoint"] = nil
	}
	return row
}

func newSnapActionFake() *snapActionFake {
	f := &snapActionFake{
		snapshots: map[string]map[string]interface{}{},
		datasets:  map[string]map[string]interface{}{},
		retention: map[string]interface{}{},
		fail:      map[string]map[string]interface{}{},
	}
	for name, mp := range map[string]string{
		"tank":           "/mnt/tank",
		"tank/data":      "/mnt/tank/data",
		"tank/restore":   "/mnt/tank/restore",
		"tank/existing":  "/mnt/tank/existing",
		"tank/odd":       "/srv/odd",
		"tank/trick":     "/mnt/tank/../../etc",
		"tank/unmounted": "",
		"tank/secure":    "/mnt/tank/secure",
		"tank/locked":    "/mnt/tank/locked",
		"spare":          "/mnt/spare",
		"spare/data":     "/mnt/spare/data",
	} {
		f.datasets[name] = fsRow(name, mp)
	}
	f.datasets["tank/vol"] = map[string]interface{}{"id": "tank/vol", "name": "tank/vol", "pool": "tank", "type": "VOLUME", "mountpoint": nil, "locked": false, "encrypted": false}
	f.datasets["tank/secure"]["locked"] = true
	f.datasets["tank/secure"]["encrypted"] = true
	f.datasets["tank/locked"]["locked"] = true
	f.datasets["tank/locked"]["encrypted"] = true

	f.addSnap("tank/data", "manual-1")
	f.addSnap("tank/data", "held-1", "truenas")
	f.addSnap("tank/data", "other-hold", "backup")
	f.addSnap("tank/data", "two-holds", "backup", "truenas")
	f.addSnap("tank/secure", "s1")
	f.addSnap("tank", "root-1")
	f.addSnap("tank/.system", "sys-1")
	f.addSnap("tank/data/.ix-apps", "a1")
	f.datasets["tank/.system"] = fsRow("tank/.system", "/mnt/tank/.system")
	f.datasets["tank/data/.ix-apps"] = fsRow("tank/data/.ix-apps", "/mnt/tank/data/.ix-apps")
	f.datasets["tank/ix-applications"] = fsRow("tank/ix-applications", "/mnt/tank/ix-applications")
	return f
}

func (f *snapActionFake) addSnap(dataset, name string, tags ...string) {
	row := snap(dataset, name)
	row["createtxg"] = "100"
	holds := map[string]interface{}{}
	for _, t := range tags {
		holds[t] = 1.0
	}
	row["holds"] = holds
	row["properties"] = map[string]interface{}{
		"createtxg": map[string]interface{}{"value": "100", "rawvalue": "100", "parsed": "100"},
	}
	f.snapshots[dataset+"@"+name] = row
	f.setUserrefs(dataset+"@"+name, len(tags))
}

// setUserrefs sets the native hold count, as 25.10 reports it. Callers in
// a test hold f.mu when the registry may be running.
func (f *snapActionFake) setUserrefs(id string, n int) {
	props := f.snapshots[id]["properties"].(map[string]interface{})
	props["userrefs"] = map[string]interface{}{"value": fmt.Sprint(n), "rawvalue": fmt.Sprint(n), "parsed": float64(n), "source": "NONE"}
}

func (f *snapActionFake) holdTags(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return snapshotHoldTags(f.snapshots[id])
}

func (f *snapActionFake) registry(t *testing.T) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{}
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var p []interface{}
		_ = json.Unmarshal(params, &p)
		if e, ok := f.fail[method]; ok {
			return nil, e, true
		}
		var filters []interface{}
		var opts map[string]interface{}
		if len(p) > 0 {
			filters, _ = p[0].([]interface{})
		}
		if len(p) > 1 {
			opts, _ = p[1].(map[string]interface{})
		}
		switch method {
		case "pool.snapshot.query":
			extra, _ := opts["extra"].(map[string]interface{})
			if extra["retention"] == true && f.failRet {
				return nil, map[string]interface{}{"code": -32001, "message": "retention lookup failed"}, true
			}
			out := []interface{}{}
			for id, s := range f.snapshots {
				if !jobMatchesFilters(s, filters) {
					continue
				}
				row := map[string]interface{}{}
				for k, v := range s {
					if k != "holds" && k != "properties" || k == "holds" && extra["holds"] == true {
						row[k] = v
					}
				}
				props := map[string]interface{}{}
				all, _ := s["properties"].(map[string]interface{})
				wanted, _ := extra["properties"].([]interface{})
				for _, w := range wanted {
					if name, _ := w.(string); all[name] != nil {
						props[name] = all[name]
					}
				}
				row["properties"] = props
				if extra["retention"] == true {
					row["retention"] = f.retention[id]
				}
				out = append(out, row)
			}
			return out, nil, true
		case "pool.dataset.query":
			out := []interface{}{}
			for _, d := range f.datasets {
				if jobMatchesFilters(d, filters) {
					out = append(out, d)
				}
			}
			return out, nil, true
		case "pool.snapshot.clone":
			data, _ := p[0].(map[string]interface{})
			dst, _ := data["dataset_dst"].(string)
			parent := f.datasets[dst[:strings.LastIndex(dst, "/")]]
			mp, _ := parent["mountpoint"].(string)
			f.datasets[dst] = fsRow(dst, mp+dst[strings.LastIndex(dst, "/"):])
			if f.cloneOut != nil {
				return f.cloneOut, nil, true
			}
			return true, nil, true
		case "pool.snapshot.hold":
			id, _ := p[0].(string)
			if f.snapshots[id] == nil {
				return nil, map[string]interface{}{"code": -32001, "message": "Failed to hold snapshot: dataset does not exist"}, true
			}
			holds, _ := f.snapshots[id]["holds"].(map[string]interface{})
			if _, ok := holds["truenas"]; ok {
				return nil, map[string]interface{}{"code": -32001, "message": "Failed to hold snapshot: tag already exists on this dataset"}, true
			}
			holds["truenas"] = 1.0
			f.setUserrefs(id, len(holds))
			return nil, nil, true
		case "pool.snapshot.release":
			id, _ := p[0].(string)
			if f.snapshots[id] == nil {
				return nil, map[string]interface{}{"code": -32001, "message": "Failed to release snapshot: dataset does not exist"}, true
			}
			f.snapshots[id]["holds"] = map[string]interface{}{}
			f.setUserrefs(id, 0)
			return nil, nil, true
		}
		return nil, nil, false
	}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

func snapActionParams(t *testing.T, fake *fakeMiddleware, method string) []interface{} {
	t.Helper()
	calls := fake.called(method)
	if len(calls) != 1 {
		t.Fatalf("%s calls = %d, want 1", method, len(calls))
	}
	var p []interface{}
	if err := json.Unmarshal(calls[0].Params, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func previewText(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- clone_snapshot ----

func TestCloneSnapshotSuccess(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	resp := callTool(t, r, "clone_snapshot", map[string]interface{}{"snapshot": "tank/data@manual-1", "target_dataset": "tank/restore/data"})
	p := snapActionParams(t, fake, "pool.snapshot.clone")
	data := p[0].(map[string]interface{})
	if len(p) != 1 || data["snapshot"] != "tank/data@manual-1" || data["dataset_dst"] != "tank/restore/data" || len(data) != 2 {
		t.Errorf("clone params = %v, want only snapshot and dataset_dst", p)
	}
	if resp["cloned"] != true || resp["source"] != "tank/data@manual-1" || resp["dataset"] != "tank/restore/data" || resp["mountpoint"] != "/mnt/tank/restore/data" {
		t.Errorf("response = %v", resp)
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "cannot be deleted while tank/restore/data exists") {
		t.Errorf("message = %q", msg)
	}
}

func TestCloneSnapshotDryRun(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	resp := callTool(t, r, "clone_snapshot", map[string]interface{}{"snapshot": "tank/data@manual-1", "target_dataset": "tank/restore/data", "dry_run": true})
	if n := len(fake.called("pool.snapshot.clone")); n != 0 {
		t.Fatalf("dry run sent pool.snapshot.clone %d time(s)", n)
	}
	if resp["dry_run"] != true || resp["executed"] != false {
		t.Errorf("dry run flags = %v", resp)
	}
	cur := resp["current_state"].(map[string]interface{})
	if cur["source"].(map[string]interface{})["snapshot"] != "tank/data@manual-1" || cur["target_dataset"] != "tank/restore/data" || cur["target_mountpoint"] != "/mnt/tank/restore/data" {
		t.Errorf("current_state = %v", cur)
	}
	actions := resp["planned_actions"].([]interface{})
	if len(actions) != 1 || !strings.Contains(actions[0].(map[string]interface{})["description"].(string), "Clone snapshot 'tank/data@manual-1' to new dataset 'tank/restore/data'") {
		t.Errorf("planned_actions = %v", actions)
	}
	if w := previewText(t, resp["warnings"]); !strings.Contains(w, "cannot be deleted") {
		t.Errorf("warnings = %s", w)
	}
}

func TestCloneSnapshotRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, snapshot, target, want string
	}{
		{"missing snapshot", "tank/data@nope", "tank/restore/x", "snapshot 'tank/data@nope' not found"},
		{"target exists", "tank/data@manual-1", "tank/existing", "dataset 'tank/existing' already exists"},
		{"other pool", "tank/data@manual-1", "spare/data/x", "is in pool 'spare', but the snapshot is in pool 'tank'"},
		{"pool root", "tank/data@manual-1", "tank", "is a pool root"},
		{"mnt path", "tank/data@manual-1", "/mnt/tank/restore/x", "not a path"},
		{"path outside pool", "tank/data@manual-1", "/etc/x", "not a path"},
		{"hidden dataset", "tank/data@manual-1", "tank/.system/x", "inside a hidden dataset"},
		{"legacy apps target", "tank/data@manual-1", "tank/ix-applications/x", "legacy apps dataset ('ix-applications')"},
		{"hidden source", "tank/.system@sys-1", "tank/restore/x", "snapshot dataset 'tank/.system' is a hidden dataset"},
		{"hidden source below pool", "tank/data/.ix-apps@a1", "tank/restore/x", "is a hidden dataset ('.ix-apps')"},
		{"invalid target", "tank/data@manual-1", "tank/re store", "invalid target_dataset"},
		{"dot-dot target", "tank/data@manual-1", "tank/../x", "invalid target_dataset"},
		{"parent missing", "tank/data@manual-1", "tank/nope/x", "parent dataset 'tank/nope' does not exist"},
		{"parent mounted outside", "tank/data@manual-1", "tank/odd/x", "is mounted at '/srv/odd', outside /mnt/tank"},
		{"parent mountpoint escapes", "tank/data@manual-1", "tank/trick/x", "outside /mnt/tank"},
		{"parent unmounted", "tank/data@manual-1", "tank/unmounted/x", "has no mountpoint, outside /mnt/tank"},
		{"parent volume", "tank/data@manual-1", "tank/vol/x", "is a volume, not a filesystem dataset"},
		{"parent locked", "tank/data@manual-1", "tank/locked/x", "parent dataset 'tank/locked' is locked"},
		{"source locked", "tank/secure@s1", "tank/restore/x", "dataset 'tank/secure' is locked; unlock it before cloning"},
		{"not a snapshot", "tank/data", "tank/restore/x", "full snapshot name"},
		{"bad snapshot name", "tank/data@bad name", "tank/restore/x", "invalid snapshot name"},
		{"dot-dot snapshot", "tank/../data@x", "tank/restore/x", "'.' and '..'"},
		{"path snapshot", "/mnt/tank/data@x", "tank/restore/x", "not a path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapActionFake()
			r, fake := f.registry(t)
			for _, dry := range []bool{true, false} {
				_, err := r.CallTool(t.Context(), "clone_snapshot", map[string]interface{}{"snapshot": tc.snapshot, "target_dataset": tc.target, "dry_run": dry})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("dry_run=%v: err = %v, want %q", dry, err, tc.want)
				}
			}
			if n := len(fake.called("pool.snapshot.clone")); n != 0 {
				t.Errorf("refused clone sent pool.snapshot.clone %d time(s)", n)
			}
		})
	}
}

func TestCloneSnapshotOfPoolRootAllowed(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	resp := callTool(t, r, "clone_snapshot", map[string]interface{}{"snapshot": "tank@root-1", "target_dataset": "tank/restore/root"})
	if resp["cloned"] != true || len(fake.called("pool.snapshot.clone")) != 1 {
		t.Errorf("response = %v", resp)
	}
}

func TestCloneSnapshotMiddlewareErrors(t *testing.T) {
	f := newSnapActionFake()
	f.fail["pool.snapshot.clone"] = map[string]interface{}{"code": -32001, "message": "Failed to clone snapshot: out of space"}
	r, _ := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@manual-1", "target_dataset": "tank/restore/data"}
	if _, err := r.CallTool(t.Context(), "clone_snapshot", args); err == nil || !strings.Contains(err.Error(), "failed to clone snapshot tank/data@manual-1 to tank/restore/data") {
		t.Errorf("clone error = %v", err)
	}

	f = newSnapActionFake()
	f.cloneOut = false
	r, _ = f.registry(t)
	if _, err := r.CallTool(t.Context(), "clone_snapshot", args); err == nil || !strings.Contains(err.Error(), "unexpected result") {
		t.Errorf("false result: err = %v", err)
	}

	f = newSnapActionFake()
	f.fail["pool.snapshot.query"] = map[string]interface{}{"code": -32001, "message": "boom"}
	r, fake := f.registry(t)
	if _, err := r.CallTool(t.Context(), "clone_snapshot", args); err == nil || !strings.Contains(err.Error(), "failed to look up snapshot") {
		t.Errorf("query error = %v", err)
	}
	if n := len(fake.called("pool.snapshot.clone")); n != 0 {
		t.Errorf("clone sent after a failed lookup")
	}
}

// ---- hold_snapshot ----

func TestHoldSnapshotSuccess(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	resp := callTool(t, r, "hold_snapshot", map[string]interface{}{"snapshot": "tank/data@other-hold"})
	p := snapActionParams(t, fake, "pool.snapshot.hold")
	opts, _ := p[1].(map[string]interface{})
	if len(p) != 2 || p[0] != "tank/data@other-hold" || opts["recursive"] != false || len(opts) != 1 {
		t.Errorf("hold params = %v, want [id, {recursive: false}]", p)
	}
	if resp["held"] != true || resp["changed"] != true || resp["already_held"] != false {
		t.Errorf("response = %v", resp)
	}
	if got := previewText(t, resp["holds"]); got != `["backup","truenas"]` {
		t.Errorf("holds read back = %s", got)
	}
}

func TestHoldSnapshotDryRunShowsCurrentHolds(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	resp := callTool(t, r, "hold_snapshot", map[string]interface{}{"snapshot": "tank/data@other-hold", "dry_run": true})
	if n := len(fake.called("pool.snapshot.hold")); n != 0 {
		t.Fatalf("dry run sent pool.snapshot.hold %d time(s)", n)
	}
	cur := resp["current_state"].(map[string]interface{})
	if cur["snapshot"] != "tank/data@other-hold" || previewText(t, cur["holds"]) != `["backup"]` || cur["holds_count"] != 1.0 {
		t.Errorf("current_state = %v", cur)
	}
	if w := previewText(t, resp["warnings"]); !strings.Contains(w, "cannot be deleted") {
		t.Errorf("warnings = %s", w)
	}
}

func TestHoldSnapshotIdempotent(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	for _, id := range []string{"tank/data@held-1", "tank/data@two-holds"} {
		resp := callTool(t, r, "hold_snapshot", map[string]interface{}{"snapshot": id})
		if resp["held"] != true || resp["already_held"] != true || resp["changed"] != false {
			t.Errorf("%s: response = %v", id, resp)
		}
		dry := callTool(t, r, "hold_snapshot", map[string]interface{}{"snapshot": id, "dry_run": true})
		if w := previewText(t, dry["warnings"]); !strings.Contains(w, "already held") {
			t.Errorf("%s: dry-run warnings = %s", id, w)
		}
	}
	if n := len(fake.called("pool.snapshot.hold")); n != 0 {
		t.Errorf("already-held snapshot sent pool.snapshot.hold %d time(s)", n)
	}
}

func TestHoldSnapshotRefusalsAndErrors(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	for _, tc := range []struct {
		args map[string]interface{}
		want string
	}{
		{map[string]interface{}{"snapshot": "tank/data@nope"}, "snapshot 'tank/data@nope' not found"},
		{map[string]interface{}{"snapshot": "tank/data"}, "full snapshot name"},
		{map[string]interface{}{"snapshot": "tank/data@a@b"}, "full snapshot name"},
		{map[string]interface{}{"snapshot": "  "}, "snapshot is required"},
		{map[string]interface{}{"snapshot": "tank/data@" + strings.Repeat("n", 250)}, "too long"},
		{map[string]interface{}{"snapshot": "tank/data@manual-1", "recursive": true}, "recursive"},
	} {
		if _, err := r.CallTool(t.Context(), "hold_snapshot", tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	if n := len(fake.called("pool.snapshot.hold")); n != 0 {
		t.Errorf("refused holds sent pool.snapshot.hold %d time(s)", n)
	}

	f = newSnapActionFake()
	f.fail["pool.snapshot.hold"] = map[string]interface{}{"code": -32001, "message": "Failed to hold snapshot: permission denied"}
	r, _ = f.registry(t)
	if _, err := r.CallTool(t.Context(), "hold_snapshot", map[string]interface{}{"snapshot": "tank/data@manual-1"}); err == nil || !strings.Contains(err.Error(), "failed to hold snapshot tank/data@manual-1") {
		t.Errorf("hold error = %v", err)
	}
}

// ---- release_snapshot ----

func TestReleaseSnapshotConfirmFlow(t *testing.T) {
	f := newSnapActionFake()
	f.retention["tank/data@held-1"] = map[string]interface{}{
		"datetime": map[string]interface{}{"$date": 1767225600000.0}, "source": "periodic_snapshot_task", "periodic_snapshot_task_id": 3.0,
	}
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@held-1"}
	preview, out := confirmAndRun(t, r, fake, "release_snapshot", "pool.snapshot.release", args)

	body := previewText(t, preview["preview"])
	for _, want := range []string{"deletable again", "'truenas' hold on snapshot 'tank/data@held-1', its only hold", "periodic snapshot task 3", "2026-01-01T00:00:00Z"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q:\n%s", want, body)
		}
	}
	summary := preview["preview"].(map[string]interface{})["confirm_summary"].(map[string]interface{})
	if h, _ := summary["headline"].(string); !strings.Contains(h, "becomes deletable again") {
		t.Errorf("confirm headline = %q", h)
	}

	p := snapActionParams(t, fake, "pool.snapshot.release")
	opts, _ := p[1].(map[string]interface{})
	if len(p) != 2 || p[0] != "tank/data@held-1" || opts["recursive"] != false || len(opts) != 1 {
		t.Errorf("release params = %v, want [id, {recursive: false}]", p)
	}
	resp := decodeObj(t, out)
	if resp["released"] != true || resp["deletable"] != true || previewText(t, resp["holds_released"]) != `["truenas"]` {
		t.Errorf("response = %v", resp)
	}
	if tags := f.holdTags("tank/data@held-1"); len(tags) != 0 {
		t.Errorf("holds after release = %v", tags)
	}
}

func TestReleaseSnapshotDryRun(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	resp := callTool(t, r, "release_snapshot", map[string]interface{}{"snapshot": "tank/data@held-1", "dry_run": true})
	if n := len(fake.called("pool.snapshot.release")); n != 0 {
		t.Fatalf("dry run sent pool.snapshot.release %d time(s)", n)
	}
	if _, ok := resp["confirmation_token"]; ok {
		t.Error("dry run issued a token")
	}
	if w := previewText(t, resp["warnings"]); !strings.Contains(w, "is deletable again") {
		t.Errorf("warnings = %s", w)
	}
	cur := resp["current_state"].(map[string]interface{})
	if previewText(t, cur["holds"]) != `["truenas"]` || cur["retention"] != nil {
		t.Errorf("current_state = %v", cur)
	}
}

func TestReleaseSnapshotRefusesWithoutHolds(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	for _, extra := range []map[string]interface{}{{}, {"dry_run": true}, {"confirmation_token": "x"}} {
		out, err := r.CallTool(t.Context(), "release_snapshot", mergeArgs(map[string]interface{}{"snapshot": "tank/data@manual-1"}, extra))
		if err == nil || !strings.Contains(err.Error(), "has no holds") {
			t.Errorf("%v: err = %v", extra, err)
		}
		if strings.Contains(out, `"confirmation_token": "`) {
			t.Errorf("%v: token issued for a snapshot without holds", extra)
		}
	}
	if _, err := r.CallTool(t.Context(), "release_snapshot", map[string]interface{}{"snapshot": "tank/data@nope"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing snapshot: err = %v", err)
	}
	if _, err := r.CallTool(t.Context(), "release_snapshot", map[string]interface{}{"snapshot": "tank/data@held-1", "recursive": true}); err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Errorf("recursive: err = %v", err)
	}
	if n := len(fake.called("pool.snapshot.release")); n != 0 {
		t.Errorf("refused release sent pool.snapshot.release %d time(s)", n)
	}
}

// A hold released elsewhere between the preview and the confirmed call
// must not run a release on a snapshot that no longer has one.
func TestReleaseSnapshotHoldGoneBeforeConfirm(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@held-1"}
	first, err := r.CallTool(t.Context(), "release_snapshot", args)
	if err != nil {
		t.Fatal(err)
	}
	tok := replyToken(t, first)
	f.mu.Lock()
	f.snapshots["tank/data@held-1"]["holds"] = map[string]interface{}{}
	f.setUserrefs("tank/data@held-1", 0)
	f.mu.Unlock()
	if _, err := r.CallTool(t.Context(), "release_snapshot", mergeArgs(args, map[string]interface{}{"confirmation_token": tok})); err == nil || !strings.Contains(err.Error(), "has no holds") {
		t.Errorf("err = %v", err)
	}
	if n := len(fake.called("pool.snapshot.release")); n != 0 {
		t.Errorf("pool.snapshot.release sent %d time(s)", n)
	}
}

func TestReleaseSnapshotErrors(t *testing.T) {
	f := newSnapActionFake()
	f.failRet = true
	r, fake := f.registry(t)
	resp := callTool(t, r, "release_snapshot", map[string]interface{}{"snapshot": "tank/data@held-1", "dry_run": true})
	if w := previewText(t, resp["warnings"]); !strings.Contains(w, "Could not check whether retention") {
		t.Errorf("warnings = %s", w)
	}

	f.fail["pool.snapshot.release"] = map[string]interface{}{"code": -32001, "message": "Failed to release snapshot: busy"}
	args := map[string]interface{}{"snapshot": "tank/data@held-1"}
	first, err := r.CallTool(t.Context(), "release_snapshot", args)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.CallTool(t.Context(), "release_snapshot", mergeArgs(args, map[string]interface{}{"confirmation_token": replyToken(t, first)}))
	if err == nil || !strings.Contains(err.Error(), "failed to release snapshot tank/data@held-1") {
		t.Errorf("release error = %v", err)
	}
	if n := len(fake.called("pool.snapshot.release")); n != 1 {
		t.Errorf("pool.snapshot.release calls = %d", n)
	}
}

func TestSnapshotActionToolsSafety(t *testing.T) {
	for name, want := range map[string]Safety{
		"clone_snapshot":   {Access: Write},
		"hold_snapshot":    {Access: Write, Idempotent: true},
		"release_snapshot": {Access: Destructive},
	} {
		if got := safetyOf(name); got != want {
			t.Errorf("%s safety = %+v, want %+v", name, got, want)
		}
	}
	r := NewRegistry(nil, nil)
	for _, name := range []string{"clone_snapshot", "hold_snapshot", "release_snapshot"} {
		if hasParam(r.tools[name], "recursive") {
			t.Errorf("%s declares recursive; recursive holds, releases and clones are not supported", name)
		}
	}
}

// release_snapshot releases every hold at once, so it must refuse unless
// the 'truenas' hold is provably the only one. Each case is refused for
// preview, dry run and confirmation alike, and nothing is released.
func TestReleaseSnapshotRefusesForeignHolds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *snapActionFake)
		id    string
		want  string
	}{
		{"foreign tag", nil, "tank/data@two-holds", "has 1 hold(s) not placed by TrueNAS (tag(s) backup)"},
		{"foreign tag only", nil, "tank/data@other-hold", "has 1 hold(s) not placed by TrueNAS (tag(s) backup)"},
		{"unlisted hold", func(f *snapActionFake) { f.setUserrefs("tank/data@held-1", 2) }, "tank/data@held-1", "has 1 hold(s) not placed by TrueNAS and not listed (userrefs 2, listed 1)"},
		{"unlisted hold without tags", func(f *snapActionFake) { f.setUserrefs("tank/data@manual-1", 1) }, "tank/data@manual-1", "not listed (userrefs 1, listed 0)"},
		{"userrefs missing", func(f *snapActionFake) {
			delete(f.snapshots["tank/data@held-1"]["properties"].(map[string]interface{}), "userrefs")
		}, "tank/data@held-1", "could not read the hold count (userrefs)"},
		{"userrefs garbled", func(f *snapActionFake) {
			f.snapshots["tank/data@held-1"]["properties"].(map[string]interface{})["userrefs"] = map[string]interface{}{"value": "-", "rawvalue": "-", "parsed": nil}
		}, "tank/data@held-1", "could not read the hold count (userrefs)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapActionFake()
			if tc.setup != nil {
				tc.setup(f)
			}
			r, fake := f.registry(t)
			for _, extra := range []map[string]interface{}{{}, {"dry_run": true}, {"confirmation_token": "x"}} {
				out, err := r.CallTool(t.Context(), "release_snapshot", mergeArgs(map[string]interface{}{"snapshot": tc.id}, extra))
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%v: err = %v, want %q", extra, err, tc.want)
				}
				if err != nil && !strings.Contains(err.Error(), "managed outside this tool") {
					t.Errorf("%v: refusal does not say other holds are managed elsewhere: %v", extra, err)
				}
				if strings.Contains(out, `"confirmation_token": "`) {
					t.Errorf("%v: token issued", extra)
				}
			}
			if n := len(fake.called("pool.snapshot.release")); n != 0 {
				t.Errorf("refused release sent pool.snapshot.release %d time(s)", n)
			}
		})
	}
}

// A token is bound to the previewed hold state: a hold added after the
// preview, listed or not, rejects it even before the foreign-hold refusal.
func TestReleaseSnapshotTokenRejectedAfterHoldAdded(t *testing.T) {
	for _, tc := range []struct {
		name string
		add  func(f *snapActionFake)
	}{
		{"listed tag", func(f *snapActionFake) {
			f.snapshots["tank/data@held-1"]["holds"].(map[string]interface{})["backup"] = 1.0
			f.setUserrefs("tank/data@held-1", 2)
		}},
		{"unlisted hold", func(f *snapActionFake) { f.setUserrefs("tank/data@held-1", 2) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapActionFake()
			r, fake := f.registry(t)
			args := map[string]interface{}{"snapshot": "tank/data@held-1"}
			first, err := r.CallTool(t.Context(), "release_snapshot", args)
			if err != nil {
				t.Fatal(err)
			}
			tok := replyToken(t, first)
			f.mu.Lock()
			tc.add(f)
			f.mu.Unlock()
			_, err = r.CallTool(t.Context(), "release_snapshot", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
			if err == nil || !strings.Contains(err.Error(), "issued for a different planned operation") {
				t.Errorf("token not rejected by its plan binding: err = %v", err)
			}
			if err == nil || !strings.Contains(err.Error(), "not placed by TrueNAS") {
				t.Errorf("not refused for the added hold: err = %v", err)
			}
			if n := len(fake.called("pool.snapshot.release")); n != 0 {
				t.Errorf("pool.snapshot.release sent %d time(s)", n)
			}
		})
	}
}

// A snapshot deleted and recreated under the same name with a hold has a
// new createtxg; a token from the old one must not release it.
func TestReleaseSnapshotTokenRejectedAfterRecreate(t *testing.T) {
	f := newSnapActionFake()
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@held-1"}
	first, err := r.CallTool(t.Context(), "release_snapshot", args)
	if err != nil {
		t.Fatal(err)
	}
	tok := replyToken(t, first)
	f.mu.Lock()
	f.snapshots["tank/data@held-1"]["createtxg"] = "200"
	f.snapshots["tank/data@held-1"]["properties"].(map[string]interface{})["createtxg"] = map[string]interface{}{"value": "200", "rawvalue": "200", "parsed": "200"}
	f.mu.Unlock()
	_, err = r.CallTool(t.Context(), "release_snapshot", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "issued for a different planned operation") {
		t.Errorf("token for the old snapshot was not rejected: err = %v", err)
	}
	if n := len(fake.called("pool.snapshot.release")); n != 0 {
		t.Errorf("pool.snapshot.release sent %d time(s)", n)
	}
}

// The plan binding covers each part of the hold state separately.
func TestReleaseSnapshotPlanBindsHoldState(t *testing.T) {
	hash := func(mutate func(f *snapActionFake)) [32]byte {
		t.Helper()
		f := newSnapActionFake()
		if mutate != nil {
			mutate(f)
		}
		r, _ := f.registry(t)
		plan, err := planReleaseSnapshot(t.Context(), r.client, map[string]interface{}{"snapshot": "tank/data@held-1"})
		if err != nil {
			t.Fatal(err)
		}
		h, err := plan.planHash()
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hash(nil)
	if base != hash(nil) {
		t.Fatal("plan hash is not stable")
	}
	for name, m := range map[string]func(f *snapActionFake){
		"tags": func(f *snapActionFake) {
			f.snapshots["tank/data@held-1"]["holds"].(map[string]interface{})["backup"] = 1.0
		},
		"userrefs":  func(f *snapActionFake) { f.setUserrefs("tank/data@held-1", 2) },
		"createtxg": func(f *snapActionFake) { f.snapshots["tank/data@held-1"]["createtxg"] = "200" },
	} {
		if hash(m) == base {
			t.Errorf("plan hash ignores %s", name)
		}
	}
}

// Without a createtxg the token cannot be bound to this snapshot, so the
// release is refused rather than confirmed by name alone.
func TestReleaseSnapshotRefusesUnreadableCreateTxg(t *testing.T) {
	f := newSnapActionFake()
	delete(f.snapshots["tank/data@held-1"], "createtxg")
	delete(f.snapshots["tank/data@held-1"]["properties"].(map[string]interface{}), "createtxg")
	r, fake := f.registry(t)
	_, err := r.CallTool(t.Context(), "release_snapshot", map[string]interface{}{"snapshot": "tank/data@held-1"})
	if err == nil || !strings.Contains(err.Error(), "could not read the createtxg") {
		t.Errorf("err = %v", err)
	}
	if n := len(fake.called("pool.snapshot.release")); n != 0 {
		t.Errorf("pool.snapshot.release sent %d time(s)", n)
	}
}

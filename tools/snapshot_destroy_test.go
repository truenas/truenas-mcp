package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// snapDestroyFake answers the methods rollback_snapshot and delete_snapshot
// use, from mutable fixtures, the way 25.10 does. pool.snapshot.rollback
// runs 'zfs rollback': newer snapshots need recursive, their clones need
// recursive_clones, and a held newer snapshot fails. pool.snapshot.delete
// fails on a held snapshot, and on one with clones unless defer is set, in
// which case the snapshot stays (marked defer_destroy). extra.retention
// without an id filter fails, as on 25.10.
type snapDestroyFake struct {
	mu        sync.Mutex
	snapshots map[string]map[string]interface{} // id -> row
	datasets  map[string]map[string]interface{} // name -> row
	retention map[string]interface{}            // id -> retention annotation
	failRet   bool
	fail      map[string]map[string]interface{} // method -> RPC error
	deleteOut interface{}                       // pool.snapshot.delete result; default true
	noop      bool                              // rollback and delete change nothing
	// ignoreFilters answers queries with every row, like a middleware
	// that ignores part of a filter.
	ignoreFilters bool
}

func newSnapDestroyFake() *snapDestroyFake {
	f := &snapDestroyFake{
		snapshots: map[string]map[string]interface{}{},
		datasets:  map[string]map[string]interface{}{},
		retention: map[string]interface{}{},
		fail:      map[string]map[string]interface{}{},
	}
	for _, ds := range []string{"tank", "tank/data", "tank/data/child", "tank/data/child/grand", "tank/other"} {
		f.addDataset(ds, "")
	}
	f.addSnap("tank/data", "s1", 100)
	f.addSnap("tank/data", "s2", 200)
	f.addSnap("tank/data", "s3", 300)
	f.addSnap("tank/data/child", "s2", 200)
	f.addSnap("tank/data/child/grand", "s2", 200)
	f.addSnap("tank/other", "s2", 200)
	f.addSnap("tank", "root-1", 50)
	f.addSnap("tank/.system", "sys-1", 60)
	f.addSnap("tank/ix-applications", "a1", 70)
	return f
}

func (f *snapDestroyFake) addDataset(name, origin string) {
	f.datasets[name] = map[string]interface{}{
		"id": name, "name": name, "pool": datasetPool(name), "type": "FILESYSTEM",
		"origin": map[string]interface{}{"value": origin, "rawvalue": origin, "parsed": origin, "source": "NONE"},
	}
}

func (f *snapDestroyFake) addSnap(dataset, name string, txg int, tags ...string) {
	id := dataset + "@" + name
	row := snap(dataset, name)
	row["type"] = "SNAPSHOT"
	row["createtxg"] = fmt.Sprint(txg)
	holds := map[string]interface{}{}
	for _, t := range tags {
		holds[t] = 1.0
	}
	row["holds"] = holds
	row["properties"] = map[string]interface{}{
		"createtxg": map[string]interface{}{"value": fmt.Sprint(txg), "rawvalue": fmt.Sprint(txg), "parsed": fmt.Sprint(txg), "source": "NONE"},
		"clones":    map[string]interface{}{"value": "", "rawvalue": "", "parsed": "", "source": "NONE"},
	}
	f.snapshots[id] = row
	f.setUserrefs(id, len(tags))
}

func (f *snapDestroyFake) props(id string) map[string]interface{} {
	return f.snapshots[id]["properties"].(map[string]interface{})
}

func (f *snapDestroyFake) setUserrefs(id string, n int) {
	f.props(id)["userrefs"] = map[string]interface{}{"value": fmt.Sprint(n), "rawvalue": fmt.Sprint(n), "parsed": float64(n), "source": "NONE"}
}

func (f *snapDestroyFake) hold(id, tag string) {
	holds := f.snapshots[id]["holds"].(map[string]interface{})
	holds[tag] = 1.0
	f.setUserrefs(id, len(holds))
}

func (f *snapDestroyFake) setTxg(id string, txg int) {
	f.snapshots[id]["createtxg"] = fmt.Sprint(txg)
	f.props(id)["createtxg"] = map[string]interface{}{"value": fmt.Sprint(txg), "rawvalue": fmt.Sprint(txg), "parsed": fmt.Sprint(txg), "source": "NONE"}
}

// clonesOf returns the clones of id, from both the origin property of
// datasets and the snapshot's clones property.
func (f *snapDestroyFake) clonesOf(id string) []string {
	seen := map[string]bool{}
	for name, d := range f.datasets {
		if d["origin"].(map[string]interface{})["value"] == id {
			seen[name] = true
		}
	}
	for _, c := range strings.Split(f.props(id)["clones"].(map[string]interface{})["value"].(string), ",") {
		if c != "" {
			seen[c] = true
		}
	}
	out := []string{}
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// addClone records clone as a clone of id: as a dataset whose origin is id
// (viaOrigin) and in the snapshot's clones property (viaProp).
func (f *snapDestroyFake) addClone(id, clone string, viaOrigin, viaProp bool) {
	if viaOrigin {
		f.addDataset(clone, id)
	}
	if viaProp {
		p := f.props(id)["clones"].(map[string]interface{})
		v, _ := p["value"].(string)
		if v != "" {
			v += ","
		}
		v += clone
		f.props(id)["clones"] = map[string]interface{}{"value": v, "rawvalue": v, "parsed": v, "source": "NONE"}
	}
}

func (f *snapDestroyFake) removeSnap(id string) {
	delete(f.snapshots, id)
	for name, d := range f.datasets {
		if d["origin"].(map[string]interface{})["value"] == id {
			delete(f.datasets, name)
		}
	}
}

func (f *snapDestroyFake) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshots[id] != nil
}

func destroyFieldValue(row map[string]interface{}, path string) interface{} {
	var cur interface{} = row
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// destroyMatches supports the filter forms the tools send: =, in, ^ on
// dotted paths, and OR.
func destroyMatches(row map[string]interface{}, filters []interface{}) bool {
	for _, f := range filters {
		arr, _ := f.([]interface{})
		if len(arr) == 2 && arr[0] == "OR" {
			inner, _ := arr[1].([]interface{})
			ok := false
			for _, n := range inner {
				ok = ok || destroyMatches(row, []interface{}{n})
			}
			if !ok {
				return false
			}
			continue
		}
		if len(arr) != 3 {
			return false
		}
		path, _ := arr[0].(string)
		got := fmt.Sprint(destroyFieldValue(row, path))
		switch arr[1] {
		case "=":
			if got != fmt.Sprint(arr[2]) {
				return false
			}
		case "^":
			if !strings.HasPrefix(got, fmt.Sprint(arr[2])) {
				return false
			}
		case "in":
			list, _ := arr[2].([]interface{})
			found := false
			for _, v := range list {
				found = found || fmt.Sprint(v) == got
			}
			if !found {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func filterHasID(filters []interface{}) bool {
	for _, f := range filters {
		if arr, _ := f.([]interface{}); len(arr) == 3 && arr[0] == "id" {
			return true
		}
	}
	return false
}

func rpcErr(msg string) map[string]interface{} {
	return map[string]interface{}{"code": -32001, "message": msg}
}

func (f *snapDestroyFake) handle(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
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
		if extra["retention"] == true {
			if !filterHasID(filters) {
				return nil, rpcErr("`id` or `limit` is required if `retention` is requested"), true
			}
			if f.failRet {
				return nil, rpcErr("retention lookup failed"), true
			}
		}
		out := []interface{}{}
		for id, s := range f.snapshots {
			if !f.ignoreFilters && !destroyMatches(s, filters) {
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
		for name, d := range f.datasets {
			if strings.Contains(name, "/.") {
				continue // internal datasets are not listed
			}
			if f.ignoreFilters || destroyMatches(d, filters) {
				out = append(out, d)
			}
		}
		return out, nil, true
	case "pool.snapshot.rollback":
		id, _ := p[0].(string)
		o, _ := p[1].(map[string]interface{})
		target := f.snapshots[id]
		if target == nil {
			return nil, rpcErr("Failed to rollback snapshot: dataset does not exist"), true
		}
		txg, _ := snapshotTxg(target)
		var newer []string
		for sid, s := range f.snapshots {
			if t, _ := snapshotTxg(s); s["dataset"] == target["dataset"] && t > txg {
				newer = append(newer, sid)
			}
		}
		if len(newer) > 0 && o["recursive"] != true && o["recursive_clones"] != true {
			return nil, rpcErr("Failed to rollback snapshot: more recent snapshots or bookmarks exist; use '-r' to force deletion"), true
		}
		for _, sid := range newer {
			if len(f.clonesOf(sid)) > 0 && o["recursive_clones"] != true {
				return nil, rpcErr("Failed to rollback snapshot: clones of previous snapshots exist; use '-R' to force deletion"), true
			}
			if n, _ := snapshotUserrefs(f.snapshots[sid]); n > 0 {
				return nil, rpcErr("Failed to rollback snapshot: dataset is busy"), true
			}
		}
		if !f.noop {
			for _, sid := range newer {
				f.removeSnap(sid)
			}
		}
		return nil, nil, true
	case "pool.snapshot.delete":
		id, _ := p[0].(string)
		o, _ := p[1].(map[string]interface{})
		if f.snapshots[id] == nil {
			return nil, rpcErr("[ENOENT] snapshot does not exist"), true
		}
		ids := []string{id}
		if o["recursive"] == true {
			ds, name, _ := strings.Cut(id, "@")
			for sid, s := range f.snapshots {
				if strings.HasPrefix(s["dataset"].(string), ds+"/") && s["snapshot_name"] == name {
					ids = append(ids, sid)
				}
			}
		}
		for _, sid := range ids {
			if n, _ := snapshotUserrefs(f.snapshots[sid]); n > 0 {
				return nil, rpcErr("[EFAULT] dataset is busy"), true
			}
			if len(f.clonesOf(sid)) > 0 && o["defer"] != true {
				return nil, rpcErr("[EINVAL] options.defer: Please set this attribute as '" + sid + "' snapshot has dependent clones"), true
			}
		}
		if !f.noop {
			for _, sid := range ids {
				if len(f.clonesOf(sid)) == 0 {
					delete(f.snapshots, sid)
				}
			}
		}
		if f.deleteOut != nil {
			return f.deleteOut, nil, true
		}
		return true, nil, true
	}
	return nil, nil, false
}

func (f *snapDestroyFake) registry(t *testing.T) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{handle: f.handle}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

// mutate changes the fixtures while the registry may be running.
func (f *snapDestroyFake) mutate(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// assertRefused calls tool with args alone, as a dry run and with a bogus
// token; each must fail with want and issue no token, and method must
// never be sent.
func assertRefused(t *testing.T, r *Registry, fake *fakeMiddleware, tool, method string, args map[string]interface{}, want string) {
	t.Helper()
	for _, extra := range []map[string]interface{}{{}, {"dry_run": true}, {"confirmation_token": "x"}} {
		out, err := r.CallTool(t.Context(), tool, mergeArgs(args, extra))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want %q", extra, err, want)
		}
		if strings.Contains(out, `"confirmation_token": "`) {
			t.Errorf("%v: token issued", extra)
		}
	}
	if n := len(fake.called(method)); n != 0 {
		t.Errorf("refused call sent %s %d time(s)", method, n)
	}
}

// previewToken previews args and returns the issued token.
func destroyPreviewToken(t *testing.T, r *Registry, tool string, args map[string]interface{}) string {
	t.Helper()
	first, err := r.CallTool(t.Context(), tool, args)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	tok := replyToken(t, first)
	if tok == "" {
		t.Fatalf("preview issued no token:\n%s", first)
	}
	return tok
}

// assertTokenRejected redeems tok after the state changed and expects the
// plan binding to reject it without sending method.
func assertTokenRejected(t *testing.T, r *Registry, fake *fakeMiddleware, tool, method string, args map[string]interface{}, tok string) error {
	t.Helper()
	_, err := r.CallTool(t.Context(), tool, mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "issued for a different planned operation") {
		t.Errorf("token not rejected by its plan binding: err = %v", err)
	}
	if n := len(fake.called(method)); n != 0 {
		t.Errorf("%s sent %d time(s) after the state changed", method, n)
	}
	return err
}

func previewBody(t *testing.T, preview map[string]interface{}) string {
	t.Helper()
	return previewText(t, preview["preview"]) + "\n" + summaryFacts(t, preview["preview"].(map[string]interface{}))
}

// summaryFacts returns the confirm_summary facts of a dry-run preview,
// one per line, without the quotes confirmFact puts around values.
func summaryFacts(t *testing.T, dry map[string]interface{}) string {
	t.Helper()
	sum, _ := dry["confirm_summary"].(map[string]interface{})
	facts, _ := sum["facts"].([]interface{})
	lines := make([]string, 0, len(facts))
	for _, f := range facts {
		s, _ := f.(string)
		lines = append(lines, strings.ReplaceAll(s, `"`, ""))
	}
	return strings.Join(lines, "\n")
}

// ---- rollback_snapshot ----

func TestRollbackSnapshotNewestConfirmFlow(t *testing.T) {
	f := newSnapDestroyFake()
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@s3"}
	preview, out := confirmAndRun(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", args)

	body := previewBody(t, preview)
	for _, want := range []string{"since 'tank/data@s3' was taken is lost and cannot be recovered", "its child datasets are not", "its newest"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q:\n%s", want, body)
		}
	}
	summary := preview["preview"].(map[string]interface{})["confirm_summary"].(map[string]interface{})
	if h, _ := summary["headline"].(string); !strings.Contains(h, "everything written since the snapshot is lost") {
		t.Errorf("confirm headline = %q", h)
	}
	p := snapActionParams(t, fake, "pool.snapshot.rollback")
	opts, _ := p[1].(map[string]interface{})
	want := map[string]interface{}{"recursive": false, "recursive_clones": false, "force": false, "recursive_rollback": false}
	if len(p) != 2 || p[0] != "tank/data@s3" || previewText(t, opts) != previewText(t, want) {
		t.Errorf("rollback params = %v, want [id, %v]", p, want)
	}
	resp := decodeObj(t, out)
	if resp["rolled_back"] != true || resp["dataset"] != "tank/data" || resp["newest_snapshot"] != "tank/data@s3" {
		t.Errorf("response = %v", resp)
	}
	if previewText(t, resp["snapshots_destroyed"]) != `[]` {
		t.Errorf("snapshots_destroyed = %v", resp["snapshots_destroyed"])
	}
}

func TestRollbackSnapshotDryRun(t *testing.T) {
	f := newSnapDestroyFake()
	r, fake := f.registry(t)
	resp := callTool(t, r, "rollback_snapshot", map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true, "dry_run": true})
	if n := len(fake.called("pool.snapshot.rollback")); n != 0 {
		t.Fatalf("dry run sent pool.snapshot.rollback %d time(s)", n)
	}
	if _, ok := resp["confirmation_token"]; ok || resp["dry_run"] != true || resp["executed"] != false {
		t.Errorf("dry run flags = %v", resp)
	}
	cur := resp["current_state"].(map[string]interface{})
	if cur["dataset"] != "tank/data" || cur["newest"] != "tank/data@s3" || cur["snapshot_count"] != 3.0 {
		t.Errorf("current_state = %v", cur)
	}
	if got := previewText(t, cur["newer_snapshots"]); !strings.Contains(got, "tank/data@s2") || !strings.Contains(got, "tank/data@s3") {
		t.Errorf("newer_snapshots = %s", got)
	}
	actions := resp["planned_actions"].([]interface{})
	details := actions[0].(map[string]interface{})["details"].(map[string]interface{})
	if previewText(t, details["snapshots_destroyed"]) != `["tank/data@s2","tank/data@s3"]` || details["force"] != false {
		t.Errorf("details = %v", details)
	}
	if !f.has("tank/data@s2") || !f.has("tank/data@s3") {
		t.Error("dry run destroyed snapshots")
	}
}

func TestRollbackSnapshotOlderNeedsFlag(t *testing.T) {
	f := newSnapDestroyFake()
	r, fake := f.registry(t)
	assertRefused(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", map[string]interface{}{"snapshot": "tank/data@s1"},
		"destroys 2 newer snapshot(s) (tank/data@s2, tank/data@s3); set allow_destroy_newer=true")
}

func TestRollbackSnapshotOlderWithFlag(t *testing.T) {
	f := newSnapDestroyFake()
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true}
	preview, out := confirmAndRun(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", args)
	body := previewBody(t, preview)
	for _, want := range []string{"2 newer snapshot(s) are destroyed: tank/data@s2, tank/data@s3", "Bookmarks newer than the snapshot are destroyed too", "incremental replication"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q:\n%s", want, body)
		}
	}
	summary := summaryFacts(t, preview["preview"].(map[string]interface{}))
	if !strings.Contains(summary, "snapshots destroyed: tank/data@s2, tank/data@s3") || !strings.Contains(summary, "clones destroyed: none") {
		t.Errorf("confirm_summary = %s", summary)
	}
	p := snapActionParams(t, fake, "pool.snapshot.rollback")
	opts, _ := p[1].(map[string]interface{})
	if opts["recursive"] != true || opts["recursive_clones"] != false || opts["force"] != false || opts["recursive_rollback"] != false {
		t.Errorf("rollback options = %v", opts)
	}
	resp := decodeObj(t, out)
	if previewText(t, resp["snapshots_destroyed"]) != `["tank/data@s2","tank/data@s3"]` || resp["newest_snapshot"] != "tank/data@s1" {
		t.Errorf("response = %v", resp)
	}
	if f.has("tank/data@s2") || !f.has("tank/data/child@s2") {
		t.Error("rollback destroyed the wrong snapshots")
	}
}

func TestRollbackSnapshotFlagRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"allow_destroy_newer on the newest", map[string]interface{}{"snapshot": "tank/data@s3", "allow_destroy_newer": true}, "already the newest snapshot of 'tank/data'; omit allow_destroy_newer"},
		{"destroy_clones without allow_destroy_newer", map[string]interface{}{"snapshot": "tank/data@s3", "destroy_clones": true}, "destroy_clones needs allow_destroy_newer=true"},
		{"destroy_clones without clones", map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true, "destroy_clones": true}, "no newer snapshot has clones; omit it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapDestroyFake()
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", tc.args, tc.want)
		})
	}
}

// Every newer snapshot is checked for holds; an unreadable hold count
// counts as a hold.
func TestRollbackSnapshotRefusesHeldNewer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *snapDestroyFake)
		want  string
	}{
		{"truenas hold", func(f *snapDestroyFake) { f.hold("tank/data@s3", "truenas") }, "tank/data@s3 (holds: truenas)"},
		{"other tag", func(f *snapDestroyFake) { f.hold("tank/data@s2", "backup") }, "tank/data@s2 (holds: backup)"},
		{"unlisted hold", func(f *snapDestroyFake) { f.setUserrefs("tank/data@s2", 1) }, "tank/data@s2 (1 unlisted hold(s))"},
		{"userrefs unreadable", func(f *snapDestroyFake) { delete(f.props("tank/data@s3"), "userrefs") }, "tank/data@s3 (hold count unreadable)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapDestroyFake()
			tc.setup(f)
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback",
				map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true}, tc.want)
		})
	}
}

// A hold on the target itself or on an older snapshot does not block: the
// rollback destroys neither.
func TestRollbackSnapshotIgnoresHoldsOnKeptSnapshots(t *testing.T) {
	f := newSnapDestroyFake()
	f.hold("tank/data@s1", "truenas")
	f.hold("tank/data@s2", "truenas")
	r, fake := f.registry(t)
	confirmAndRun(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", map[string]interface{}{"snapshot": "tank/data@s2", "allow_destroy_newer": true})
	if f.has("tank/data@s3") || !f.has("tank/data@s2") {
		t.Error("rollback did not destroy only the newer snapshot")
	}
}

// Clones are found by the origin of datasets and by the snapshot's clones
// property; either alone blocks without destroy_clones.
func TestRollbackSnapshotRefusesClonesWithoutFlag(t *testing.T) {
	for _, tc := range []struct {
		name               string
		viaOrigin, viaProp bool
	}{{"origin", true, false}, {"clones property", false, true}, {"both", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapDestroyFake()
			f.addClone("tank/data@s2", "tank/restore", tc.viaOrigin, tc.viaProp)
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback",
				map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true},
				"tank/data@s2 (clones: tank/restore); the rollback would have to destroy them too, which needs destroy_clones=true")
		})
	}
}

func TestRollbackSnapshotDestroyClones(t *testing.T) {
	f := newSnapDestroyFake()
	f.addClone("tank/data@s2", "tank/restore", true, true)
	f.addClone("tank/data@s3", "tank/restore2", true, false)
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true, "destroy_clones": true}
	preview, out := confirmAndRun(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", args)
	body := previewBody(t, preview)
	if !strings.Contains(body, "These clones and all their data are destroyed: tank/restore, tank/restore2") || !strings.Contains(body, "clones destroyed: tank/restore, tank/restore2") {
		t.Errorf("preview does not list the clones:\n%s", body)
	}
	p := snapActionParams(t, fake, "pool.snapshot.rollback")
	opts, _ := p[1].(map[string]interface{})
	if opts["recursive"] != true || opts["recursive_clones"] != true || opts["force"] != false {
		t.Errorf("rollback options = %v", opts)
	}
	if resp := decodeObj(t, out); previewText(t, resp["clones_destroyed"]) != `["tank/restore","tank/restore2"]` {
		t.Errorf("response = %v", resp)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.datasets["tank/restore"] != nil || f.datasets["tank/restore2"] != nil {
		t.Error("clones were not destroyed")
	}
}

// destroy_clones destroys only clones the preview can list completely.
func TestRollbackSnapshotRefusesClonesWithDependents(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *snapDestroyFake)
		want  string
	}{
		{"clone with a snapshot", func(f *snapDestroyFake) {
			f.addClone("tank/data@s2", "tank/restore", true, false)
			f.addSnap("tank/restore", "r1", 250)
		}, "clone 'tank/restore' has 1 snapshot(s) of its own (tank/restore@r1)"},
		{"clone with a child dataset", func(f *snapDestroyFake) {
			f.addClone("tank/data@s2", "tank/restore", true, false)
			f.addDataset("tank/restore/sub", "")
		}, "clone 'tank/restore' has 1 child dataset(s)"},
		{"hidden clone", func(f *snapDestroyFake) {
			f.addClone("tank/data@s2", "tank/.hidden/c", true, true)
		}, "clone 'tank/.hidden/c' is not listed by query_datasets"},
		{"clone known only from the clones property", func(f *snapDestroyFake) {
			f.addClone("tank/data@s3", "tank/elsewhere", false, true)
		}, "clone 'tank/elsewhere' is not listed by query_datasets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapDestroyFake()
			tc.setup(f)
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback",
				map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true, "destroy_clones": true}, tc.want)
		})
	}
}

// Clones of the target or of older snapshots are not destroyed and do not
// block.
func TestRollbackSnapshotIgnoresClonesOfKeptSnapshots(t *testing.T) {
	f := newSnapDestroyFake()
	f.addClone("tank/data@s1", "tank/restore", true, true)
	r, fake := f.registry(t)
	confirmAndRun(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true})
	p := snapActionParams(t, fake, "pool.snapshot.rollback")
	if opts, _ := p[1].(map[string]interface{}); opts["recursive_clones"] != false {
		t.Errorf("rollback options = %v", opts)
	}
}

func TestRollbackSnapshotArgumentRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, snapshot, want string
	}{
		{"pool root", "tank@root-1", "is the root dataset of pool 'tank'; rolling back a pool's root dataset is not supported"},
		{"hidden dataset", "tank/.system@sys-1", "inside '.system', which TrueNAS manages itself"},
		{"legacy apps dataset", "tank/ix-applications@a1", "inside 'ix-applications', which TrueNAS manages itself"},
		{"missing snapshot", "tank/data@nope", "snapshot 'tank/data@nope' not found"},
		{"not a snapshot", "tank/data", "full snapshot name"},
		{"path", "/mnt/tank/data@s1", "not a path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapDestroyFake()
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", map[string]interface{}{"snapshot": tc.snapshot}, tc.want)
		})
	}
}

func TestRollbackSnapshotRefusesUnreadableCreateTxg(t *testing.T) {
	f := newSnapDestroyFake()
	delete(f.snapshots["tank/data@s2"], "createtxg")
	delete(f.props("tank/data@s2"), "createtxg")
	r, fake := f.registry(t)
	assertRefused(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", map[string]interface{}{"snapshot": "tank/data@s3"},
		"could not read the createtxg of tank/data@s2")
}

// The token is bound to the dataset's snapshot list and the state of the
// snapshots the rollback destroys: any change after the preview rejects it.
func TestRollbackSnapshotTokenRejectedAfterStateChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    map[string]interface{}
		change  func(f *snapDestroyFake)
		blocked string // the fresh preview's refusal, if any
	}{
		{"new snapshot after the newest", map[string]interface{}{"snapshot": "tank/data@s3"},
			func(f *snapDestroyFake) { f.addSnap("tank/data", "s4", 400) }, "is not the newest snapshot"},
		{"new snapshot after an older target", map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true},
			func(f *snapDestroyFake) { f.addSnap("tank/data", "s4", 400) }, ""},
		{"newest recreated", map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true},
			func(f *snapDestroyFake) { f.setTxg("tank/data@s3", 400) }, ""},
		{"target recreated", map[string]interface{}{"snapshot": "tank/data@s3"},
			func(f *snapDestroyFake) { f.setTxg("tank/data@s3", 400) }, ""},
		{"older snapshot deleted", map[string]interface{}{"snapshot": "tank/data@s3"},
			func(f *snapDestroyFake) { delete(f.snapshots, "tank/data@s1") }, ""},
		{"hold on a newer snapshot", map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true},
			func(f *snapDestroyFake) { f.hold("tank/data@s2", "truenas") }, "are held or may be held"},
		{"clone of a newer snapshot", map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true},
			func(f *snapDestroyFake) { f.addClone("tank/data@s2", "tank/restore", true, false) }, "have dependent clones"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapDestroyFake()
			r, fake := f.registry(t)
			tok := destroyPreviewToken(t, r, "rollback_snapshot", tc.args)
			f.mutate(func() { tc.change(f) })
			err := assertTokenRejected(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", tc.args, tok)
			if tc.blocked != "" && (err == nil || !strings.Contains(err.Error(), tc.blocked)) {
				t.Errorf("not refused for the change: err = %v", err)
			}
		})
	}
}

// The plan binding covers each part of the state separately.
func TestRollbackSnapshotPlanBindsState(t *testing.T) {
	args := map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true, "destroy_clones": true}
	hash := func(mutate func(f *snapDestroyFake)) [32]byte {
		t.Helper()
		f := newSnapDestroyFake()
		f.addClone("tank/data@s2", "tank/restore", true, true)
		if mutate != nil {
			mutate(f)
		}
		r, _ := f.registry(t)
		plan, err := planRollbackSnapshot(t.Context(), r.client, args)
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
	for name, m := range map[string]func(f *snapDestroyFake){
		"snapshot added":      func(f *snapDestroyFake) { f.addSnap("tank/data", "s0", 90) },
		"older createtxg":     func(f *snapDestroyFake) { f.setTxg("tank/data@s1", 99) },
		"newer createtxg":     func(f *snapDestroyFake) { f.setTxg("tank/data@s3", 301) },
		"newer userrefs":      func(f *snapDestroyFake) { f.setUserrefs("tank/data@s3", 1) },
		"newer hold tag":      func(f *snapDestroyFake) { f.hold("tank/data@s2", "truenas") },
		"newer clone added":   func(f *snapDestroyFake) { f.addClone("tank/data@s3", "tank/restore2", true, false) },
		"clone property only": func(f *snapDestroyFake) { f.addClone("tank/data@s2", "tank/restore3", false, true) },
	} {
		if hash(m) == base {
			t.Errorf("plan hash ignores %s", name)
		}
	}
	held := func(f *snapDestroyFake) { f.hold("tank/data@s1", "truenas") }
	if hash(held) == base {
		t.Error("plan hash ignores holds on the snapshot list")
	}
	retagged := func(f *snapDestroyFake) { f.hold("tank/data@s1", "replication") }
	if hash(held) == hash(retagged) {
		t.Error("plan hash ignores hold tag names")
	}
}

func TestRollbackSnapshotMiddlewareErrors(t *testing.T) {
	f := newSnapDestroyFake()
	f.fail["pool.snapshot.rollback"] = rpcErr("Failed to rollback snapshot: dataset is busy")
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@s3"}
	tok := destroyPreviewToken(t, r, "rollback_snapshot", args)
	_, err := r.CallTool(t.Context(), "rollback_snapshot", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "failed to roll back tank/data to tank/data@s3") || !strings.Contains(err.Error(), "dataset is busy") {
		t.Errorf("rollback error = %v", err)
	}
	if n := len(fake.called("pool.snapshot.rollback")); n != 1 {
		t.Errorf("pool.snapshot.rollback calls = %d", n)
	}

	for _, method := range []string{"pool.snapshot.query", "pool.dataset.query"} {
		f = newSnapDestroyFake()
		f.fail[method] = rpcErr("boom")
		r, fake = f.registry(t)
		if _, err := r.CallTool(t.Context(), "rollback_snapshot", map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true}); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%s failure: err = %v", method, err)
		}
		if n := len(fake.called("pool.snapshot.rollback")); n != 0 {
			t.Errorf("%s failure: rollback sent", method)
		}
	}
}

// A rollback the middleware accepts but that leaves newer snapshots is
// reported, not hidden.
func TestRollbackSnapshotReadBackMismatch(t *testing.T) {
	f := newSnapDestroyFake()
	f.noop = true
	r, fake := f.registry(t)
	_, out := confirmAndRun(t, r, fake, "rollback_snapshot", "pool.snapshot.rollback", map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true})
	resp := decodeObj(t, out)
	if note, _ := resp["note"].(string); !strings.Contains(note, "is not the newest snapshot when read back (newest: tank/data@s3)") {
		t.Errorf("response = %v", resp)
	}
}

// ---- delete_snapshot ----

func TestDeleteSnapshotConfirmFlow(t *testing.T) {
	f := newSnapDestroyFake()
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@s2"}
	preview, out := confirmAndRun(t, r, fake, "delete_snapshot", "pool.snapshot.delete", args)
	body := previewBody(t, preview)
	for _, want := range []string{"cannot be recovered", "child datasets' snapshots are not deleted", "retention: none found"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q:\n%s", want, body)
		}
	}
	p := snapActionParams(t, fake, "pool.snapshot.delete")
	opts, _ := p[1].(map[string]interface{})
	if len(p) != 2 || p[0] != "tank/data@s2" || opts["defer"] != false || opts["recursive"] != false || len(opts) != 2 {
		t.Errorf("delete params = %v, want [id, {defer: false, recursive: false}]", p)
	}
	resp := decodeObj(t, out)
	if resp["deleted"] != true || resp["deferred"] != false || previewText(t, resp["snapshots"]) != `["tank/data@s2"]` {
		t.Errorf("response = %v", resp)
	}
	if f.has("tank/data@s2") || !f.has("tank/data/child@s2") {
		t.Error("delete removed the wrong snapshots")
	}
}

func TestDeleteSnapshotDryRun(t *testing.T) {
	f := newSnapDestroyFake()
	r, fake := f.registry(t)
	resp := callTool(t, r, "delete_snapshot", map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true, "dry_run": true})
	if n := len(fake.called("pool.snapshot.delete")); n != 0 {
		t.Fatalf("dry run sent pool.snapshot.delete %d time(s)", n)
	}
	if _, ok := resp["confirmation_token"]; ok || resp["dry_run"] != true {
		t.Errorf("dry run flags = %v", resp)
	}
	details := resp["planned_actions"].([]interface{})[0].(map[string]interface{})["details"].(map[string]interface{})
	if previewText(t, details["snapshots"]) != `["tank/data@s2","tank/data/child/grand@s2","tank/data/child@s2"]` || details["recursive"] != true {
		t.Errorf("details = %v", details)
	}
	if !f.has("tank/data@s2") {
		t.Error("dry run deleted the snapshot")
	}
}

// Recursive covers the same name on every descendant, not on siblings or
// on the parent, and lists each one.
func TestDeleteSnapshotRecursive(t *testing.T) {
	f := newSnapDestroyFake()
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true}
	preview, out := confirmAndRun(t, r, fake, "delete_snapshot", "pool.snapshot.delete", args)
	body := previewBody(t, preview)
	if !strings.Contains(body, "recursive: 3 snapshot(s) are deleted: tank/data@s2, tank/data/child/grand@s2, tank/data/child@s2") {
		t.Errorf("preview does not list every snapshot:\n%s", body)
	}
	p := snapActionParams(t, fake, "pool.snapshot.delete")
	if opts, _ := p[1].(map[string]interface{}); opts["recursive"] != true || opts["defer"] != false {
		t.Errorf("delete options = %v", opts)
	}
	if resp := decodeObj(t, out); previewText(t, resp["snapshots"]) != `["tank/data@s2","tank/data/child/grand@s2","tank/data/child@s2"]` {
		t.Errorf("response = %v", resp)
	}
	if f.has("tank/data/child@s2") || f.has("tank/data/child/grand@s2") || !f.has("tank/other@s2") {
		t.Error("recursive delete removed the wrong snapshots")
	}
}

// With no child snapshot of that name, recursive is not sent.
func TestDeleteSnapshotRecursiveWithoutChildren(t *testing.T) {
	f := newSnapDestroyFake()
	r, fake := f.registry(t)
	preview, _ := confirmAndRun(t, r, fake, "delete_snapshot", "pool.snapshot.delete", map[string]interface{}{"snapshot": "tank/data@s3", "recursive": true})
	if body := previewBody(t, preview); !strings.Contains(body, "No child dataset of 'tank/data' has a snapshot named 's3'") {
		t.Errorf("preview:\n%s", body)
	}
	p := snapActionParams(t, fake, "pool.snapshot.delete")
	if opts, _ := p[1].(map[string]interface{}); opts["recursive"] != false {
		t.Errorf("delete options = %v", opts)
	}
}

func TestDeleteSnapshotRefusesHeld(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *snapDestroyFake)
		args  map[string]interface{}
		want  string
	}{
		{"truenas hold", func(f *snapDestroyFake) { f.hold("tank/data@s2", "truenas") },
			map[string]interface{}{"snapshot": "tank/data@s2"}, "tank/data@s2 (holds: truenas)"},
		{"other tag", func(f *snapDestroyFake) { f.hold("tank/data@s2", "backup") },
			map[string]interface{}{"snapshot": "tank/data@s2"}, "tank/data@s2 (holds: backup)"},
		{"unlisted hold", func(f *snapDestroyFake) { f.setUserrefs("tank/data@s2", 1) },
			map[string]interface{}{"snapshot": "tank/data@s2"}, "tank/data@s2 (1 unlisted hold(s))"},
		{"userrefs unreadable", func(f *snapDestroyFake) { delete(f.props("tank/data@s2"), "userrefs") },
			map[string]interface{}{"snapshot": "tank/data@s2"}, "tank/data@s2 (hold count unreadable)"},
		{"held child in a recursive delete", func(f *snapDestroyFake) { f.hold("tank/data/child/grand@s2", "truenas") },
			map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true}, "tank/data/child/grand@s2 (holds: truenas)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapDestroyFake()
			tc.setup(f)
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "delete_snapshot", "pool.snapshot.delete", tc.args, tc.want)
		})
	}
}

func TestDeleteSnapshotRefusesClonesWithoutFlag(t *testing.T) {
	for _, tc := range []struct {
		name               string
		id                 string
		args               map[string]interface{}
		viaOrigin, viaProp bool
		want               string
	}{
		{"origin", "tank/data@s2", map[string]interface{}{"snapshot": "tank/data@s2"}, true, false, "tank/data@s2 (clones: tank/restore)"},
		{"clones property", "tank/data@s2", map[string]interface{}{"snapshot": "tank/data@s2"}, false, true, "tank/data@s2 (clones: tank/restore)"},
		{"cloned child in a recursive delete", "tank/data/child@s2", map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true}, true, false, "tank/data/child@s2 (clones: tank/restore)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapDestroyFake()
			f.addClone(tc.id, "tank/restore", tc.viaOrigin, tc.viaProp)
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "delete_snapshot", "pool.snapshot.delete", tc.args, tc.want)
			if _, err := r.CallTool(t.Context(), "delete_snapshot", tc.args); err == nil || !strings.Contains(err.Error(), "set defer_if_cloned=true") {
				t.Errorf("refusal does not name defer_if_cloned: %v", err)
			}
		})
	}
}

func TestDeleteSnapshotDeferIfCloned(t *testing.T) {
	f := newSnapDestroyFake()
	f.addClone("tank/data@s2", "tank/restore", true, true)
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@s2", "defer_if_cloned": true}
	preview, out := confirmAndRun(t, r, fake, "delete_snapshot", "pool.snapshot.delete", args)
	body := previewBody(t, preview)
	for _, want := range []string{"marked for deferred deletion and ZFS deletes each one when its last clone is destroyed", "clones: tank/restore", "deferred until clones are destroyed: tank/restore"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q:\n%s", want, body)
		}
	}
	p := snapActionParams(t, fake, "pool.snapshot.delete")
	if opts, _ := p[1].(map[string]interface{}); opts["defer"] != true || opts["recursive"] != false {
		t.Errorf("delete options = %v", opts)
	}
	resp := decodeObj(t, out)
	if resp["deferred"] != true || previewText(t, resp["pending_deletion"]) != `["tank/data@s2"]` {
		t.Errorf("response = %v", resp)
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "removed when their last clone is destroyed") {
		t.Errorf("message = %q", msg)
	}
}

func TestDeleteSnapshotFlagAndArgumentRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"defer_if_cloned without clones", map[string]interface{}{"snapshot": "tank/data@s2", "defer_if_cloned": true}, "no affected snapshot has clones; omit it"},
		{"hidden dataset", map[string]interface{}{"snapshot": "tank/.system@sys-1"}, "inside '.system', which TrueNAS manages itself"},
		{"legacy apps dataset", map[string]interface{}{"snapshot": "tank/ix-applications@a1"}, "inside 'ix-applications'"},
		{"missing snapshot", map[string]interface{}{"snapshot": "tank/data@nope"}, "snapshot 'tank/data@nope' not found"},
		{"not a snapshot", map[string]interface{}{"snapshot": "tank/data"}, "full snapshot name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapDestroyFake()
			r, fake := f.registry(t)
			assertRefused(t, r, fake, "delete_snapshot", "pool.snapshot.delete", tc.args, tc.want)
		})
	}
}

// A recursive delete from the pool root would reach hidden datasets'
// snapshots; it is refused rather than deleting them.
func TestDeleteSnapshotRecursiveRefusesManagedChildren(t *testing.T) {
	f := newSnapDestroyFake()
	f.addSnap("tank", "auto-1", 500)
	f.addSnap("tank/data", "auto-1", 500)
	f.addSnap("tank/.system", "auto-1", 500)
	r, fake := f.registry(t)
	assertRefused(t, r, fake, "delete_snapshot", "pool.snapshot.delete", map[string]interface{}{"snapshot": "tank@auto-1", "recursive": true},
		"tank/.system@auto-1 (in '.system')")
}

func TestDeleteSnapshotOfPoolRootAllowed(t *testing.T) {
	f := newSnapDestroyFake()
	r, fake := f.registry(t)
	confirmAndRun(t, r, fake, "delete_snapshot", "pool.snapshot.delete", map[string]interface{}{"snapshot": "tank@root-1"})
	if f.has("tank@root-1") {
		t.Error("snapshot not deleted")
	}
}

func TestDeleteSnapshotRefusesUnreadableCreateTxg(t *testing.T) {
	f := newSnapDestroyFake()
	delete(f.snapshots["tank/data/child@s2"], "createtxg")
	delete(f.props("tank/data/child@s2"), "createtxg")
	r, fake := f.registry(t)
	assertRefused(t, r, fake, "delete_snapshot", "pool.snapshot.delete", map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true},
		"could not read the createtxg of tank/data/child@s2")
}

func TestDeleteSnapshotRetentionWarnings(t *testing.T) {
	f := newSnapDestroyFake()
	f.retention["tank/data/child@s2"] = map[string]interface{}{
		"datetime": map[string]interface{}{"$date": 1767225600000.0}, "source": "periodic_snapshot_task", "periodic_snapshot_task_id": 3.0,
	}
	r, _ := f.registry(t)
	resp := callTool(t, r, "delete_snapshot", map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true, "dry_run": true})
	w := previewText(t, resp["warnings"])
	if !strings.Contains(w, "tank/data/child@s2 is managed by retention (periodic snapshot task 3, removal at 2026-01-01T00:00:00Z)") {
		t.Errorf("warnings = %s", w)
	}
	if got := summaryFacts(t, resp); !strings.Contains(got, "retention: tank/data/child@s2: periodic snapshot task 3") {
		t.Errorf("confirm_summary = %s", got)
	}

	f.failRet = true
	resp = callTool(t, r, "delete_snapshot", map[string]interface{}{"snapshot": "tank/data@s2", "dry_run": true})
	if w := previewText(t, resp["warnings"]); !strings.Contains(w, "Could not check whether a periodic snapshot task's retention manages these snapshots") {
		t.Errorf("warnings = %s", w)
	}
	if got := summaryFacts(t, resp); !strings.Contains(got, "retention: unknown") {
		t.Errorf("confirm_summary = %s", got)
	}
}

func TestDeleteSnapshotTokenRejectedAfterStateChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    map[string]interface{}
		change  func(f *snapDestroyFake)
		blocked string
	}{
		{"snapshot recreated", map[string]interface{}{"snapshot": "tank/data@s2"},
			func(f *snapDestroyFake) { f.setTxg("tank/data@s2", 900) }, ""},
		{"hold added", map[string]interface{}{"snapshot": "tank/data@s2"},
			func(f *snapDestroyFake) { f.hold("tank/data@s2", "truenas") }, "are held or may be held"},
		{"clone added", map[string]interface{}{"snapshot": "tank/data@s2"},
			func(f *snapDestroyFake) { f.addClone("tank/data@s2", "tank/restore", true, false) }, "have dependent clones"},
		{"child snapshot added", map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true},
			func(f *snapDestroyFake) { f.addDataset("tank/data/new", ""); f.addSnap("tank/data/new", "s2", 200) }, ""},
		{"child snapshot recreated", map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true},
			func(f *snapDestroyFake) { f.setTxg("tank/data/child/grand@s2", 901) }, ""},
		{"child hold added", map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true},
			func(f *snapDestroyFake) { f.setUserrefs("tank/data/child@s2", 1) }, "1 unlisted hold(s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapDestroyFake()
			r, fake := f.registry(t)
			tok := destroyPreviewToken(t, r, "delete_snapshot", tc.args)
			f.mutate(func() { tc.change(f) })
			err := assertTokenRejected(t, r, fake, "delete_snapshot", "pool.snapshot.delete", tc.args, tok)
			if tc.blocked != "" && (err == nil || !strings.Contains(err.Error(), tc.blocked)) {
				t.Errorf("not refused for the change: err = %v", err)
			}
		})
	}
}

func TestDeleteSnapshotPlanBindsState(t *testing.T) {
	args := map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true, "defer_if_cloned": true}
	hash := func(mutate func(f *snapDestroyFake)) [32]byte {
		t.Helper()
		f := newSnapDestroyFake()
		f.addClone("tank/data/child@s2", "tank/restore", true, false)
		if mutate != nil {
			mutate(f)
		}
		r, _ := f.registry(t)
		plan, err := planDeleteSnapshot(t.Context(), r.client, args)
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
	for name, m := range map[string]func(f *snapDestroyFake){
		"target createtxg": func(f *snapDestroyFake) { f.setTxg("tank/data@s2", 201) },
		"child createtxg":  func(f *snapDestroyFake) { f.setTxg("tank/data/child/grand@s2", 201) },
		"child added":      func(f *snapDestroyFake) { f.addSnap("tank/data/child/x", "s2", 200) },
		"clone added":      func(f *snapDestroyFake) { f.addClone("tank/data@s2", "tank/restore2", true, false) },
		"hold tag":         func(f *snapDestroyFake) { f.hold("tank/data@s2", "truenas") },
		"userrefs":         func(f *snapDestroyFake) { f.setUserrefs("tank/data@s2", 2) },
	} {
		if hash(m) == base {
			t.Errorf("plan hash ignores %s", name)
		}
	}
}

func TestDeleteSnapshotMiddlewareErrors(t *testing.T) {
	f := newSnapDestroyFake()
	f.fail["pool.snapshot.delete"] = rpcErr("[EFAULT] dataset is busy")
	r, fake := f.registry(t)
	args := map[string]interface{}{"snapshot": "tank/data@s2"}
	tok := destroyPreviewToken(t, r, "delete_snapshot", args)
	_, err := r.CallTool(t.Context(), "delete_snapshot", mergeArgs(args, map[string]interface{}{"confirmation_token": tok}))
	if err == nil || !strings.Contains(err.Error(), "failed to delete snapshot tank/data@s2") || !strings.Contains(err.Error(), "dataset is busy") {
		t.Errorf("delete error = %v", err)
	}
	if n := len(fake.called("pool.snapshot.delete")); n != 1 {
		t.Errorf("pool.snapshot.delete calls = %d", n)
	}

	f = newSnapDestroyFake()
	f.deleteOut = false
	r, fake = f.registry(t)
	tok = destroyPreviewToken(t, r, "delete_snapshot", args)
	if _, err := r.CallTool(t.Context(), "delete_snapshot", mergeArgs(args, map[string]interface{}{"confirmation_token": tok})); err == nil || !strings.Contains(err.Error(), "unexpected result") {
		t.Errorf("false result: err = %v", err)
	}
	if n := len(fake.called("pool.snapshot.delete")); n != 1 {
		t.Errorf("false result: pool.snapshot.delete calls = %d", n)
	}

	for _, method := range []string{"pool.snapshot.query", "pool.dataset.query"} {
		f = newSnapDestroyFake()
		f.fail[method] = rpcErr("boom")
		r, fake = f.registry(t)
		if _, err := r.CallTool(t.Context(), "delete_snapshot", args); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%s failure: err = %v", method, err)
		}
		if n := len(fake.called("pool.snapshot.delete")); n != 0 {
			t.Errorf("%s failure: delete sent", method)
		}
	}
}

func TestDeleteSnapshotReadBackMismatch(t *testing.T) {
	f := newSnapDestroyFake()
	f.noop = true
	r, fake := f.registry(t)
	_, out := confirmAndRun(t, r, fake, "delete_snapshot", "pool.snapshot.delete", map[string]interface{}{"snapshot": "tank/data@s2"})
	resp := decodeObj(t, out)
	if note, _ := resp["note"].(string); !strings.Contains(note, "still exist when read back") {
		t.Errorf("response = %v", resp)
	}
}

// ---- both ----

func TestSnapshotDestroyToolsSafety(t *testing.T) {
	r := NewRegistry(nil, nil)
	for _, name := range []string{"rollback_snapshot", "delete_snapshot"} {
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
	}
	for _, p := range []string{"force", "recursive_rollback", "recursive_clones", "recursive"} {
		if hasParam(r.tools["rollback_snapshot"], p) {
			t.Errorf("rollback_snapshot declares %s", p)
		}
	}
	if hasParam(r.tools["delete_snapshot"], "defer") {
		t.Error("delete_snapshot declares defer")
	}
}

func TestSnapshotDestroyToolsRefusedInReadOnlyMode(t *testing.T) {
	f := newSnapDestroyFake()
	r, fake := f.registry(t)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	names := listed(r)
	for _, name := range []string{"rollback_snapshot", "delete_snapshot"} {
		if names[name] {
			t.Errorf("read-only lists %s", name)
		}
		for _, dry := range []bool{true, false} {
			if _, err := r.CallTool(t.Context(), name, map[string]interface{}{"snapshot": "tank/data@s3", "dry_run": dry}); err == nil || !strings.Contains(err.Error(), "read-only mode") {
				t.Errorf("read-only %s dry_run=%v: err = %v", name, dry, err)
			}
		}
	}
	if len(fake.called("pool.snapshot.query")) != 0 || len(fake.called("pool.snapshot.rollback")) != 0 || len(fake.called("pool.snapshot.delete")) != 0 {
		t.Error("read-only mode reached the middleware")
	}
}

// Rows the middleware returns despite the filter are dropped: only the
// dataset's own snapshots count for a rollback, only descendants with the
// same name for a recursive delete, and only datasets whose origin is an
// affected snapshot count as clones.
func TestSnapshotDestroyToolsRefilterQueryResults(t *testing.T) {
	f := newSnapDestroyFake()
	f.ignoreFilters = true
	f.addDataset("tank/datax", "")
	f.addSnap("tank/datax", "s2", 999)
	f.addClone("tank/other@s2", "tank/restore", true, false)
	r, fake := f.registry(t)

	resp := callTool(t, r, "rollback_snapshot", map[string]interface{}{"snapshot": "tank/data@s3", "dry_run": true})
	cur := resp["current_state"].(map[string]interface{})
	if cur["snapshot_count"] != 3.0 || cur["newest"] != "tank/data@s3" {
		t.Errorf("rollback counted other datasets' snapshots: %v", cur)
	}

	resp = callTool(t, r, "delete_snapshot", map[string]interface{}{"snapshot": "tank/data@s2", "recursive": true, "dry_run": true})
	details := resp["planned_actions"].([]interface{})[0].(map[string]interface{})["details"].(map[string]interface{})
	if previewText(t, details["snapshots"]) != `["tank/data@s2","tank/data/child/grand@s2","tank/data/child@s2"]` || previewText(t, details["clones"]) != `[]` {
		t.Errorf("recursive delete details = %v", details)
	}
	resp = callTool(t, r, "delete_snapshot", map[string]interface{}{"snapshot": "tank/data@s1", "dry_run": true})
	details = resp["planned_actions"].([]interface{})[0].(map[string]interface{})["details"].(map[string]interface{})
	if previewText(t, details["snapshots"]) != `["tank/data@s1"]` {
		t.Errorf("single delete details = %v", details)
	}
	if len(fake.called("pool.snapshot.delete"))+len(fake.called("pool.snapshot.rollback")) != 0 {
		t.Error("dry runs sent a write")
	}
}

// Execute refuses a blocked plan on its own, whatever the caller checked.
func TestSnapshotDestroyExecuteRefusesBlockedPlan(t *testing.T) {
	f := newSnapDestroyFake()
	f.hold("tank/data@s2", "truenas")
	r, fake := f.registry(t)
	rb, err := planRollbackSnapshot(t.Context(), r.client, map[string]interface{}{"snapshot": "tank/data@s1", "allow_destroy_newer": true})
	if err != nil || len(rb.Blocked) == 0 {
		t.Fatalf("rollback plan not blocked: %v %v", rb, err)
	}
	if _, err := executeRollbackSnapshot(t.Context(), r.client, nil, rb); err == nil || !strings.Contains(err.Error(), "held") {
		t.Errorf("rollback execute: err = %v", err)
	}
	del, err := planDeleteSnapshot(t.Context(), r.client, map[string]interface{}{"snapshot": "tank/data@s2"})
	if err != nil || len(del.Blocked) == 0 {
		t.Fatalf("delete plan not blocked: %v %v", del, err)
	}
	if _, err := executeDeleteSnapshot(t.Context(), r.client, nil, del); err == nil || !strings.Contains(err.Error(), "held") {
		t.Errorf("delete execute: err = %v", err)
	}
	if len(fake.called("pool.snapshot.delete"))+len(fake.called("pool.snapshot.rollback")) != 0 {
		t.Error("blocked plan sent a write")
	}
}

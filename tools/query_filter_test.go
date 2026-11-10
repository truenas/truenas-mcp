package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/redact"
)

func parseQueryParams(t *testing.T, params json.RawMessage) (filters []interface{}, options map[string]interface{}) {
	t.Helper()
	var raw []interface{}
	if err := json.Unmarshal(params, &raw); err != nil {
		t.Fatalf("params: %v (%s)", err, params)
	}
	if len(raw) > 0 {
		filters, _ = raw[0].([]interface{})
	}
	if len(raw) > 1 {
		options, _ = raw[1].(map[string]interface{})
	}
	if options == nil {
		options = map[string]interface{}{}
	}
	return filters, options
}

func hasFilter(filters []interface{}, field, op, value string) bool {
	for _, item := range filters {
		f, ok := item.([]interface{})
		if !ok || len(f) < 3 {
			continue
		}
		if f[0] == field && f[1] == op && f[2] == value {
			return true
		}
	}
	return false
}

// datasetQueryLikeMiddleware models 25.10 and 27.0 pool.dataset.query:
// children are walked only when retrieve_children is true or filters are
// non-empty, and encrypted/locked/key_loaded are present only when a
// crypto property is requested (or when no property list is given).
// "deduplication" is not a ZFS property and is ignored; "dedup" is renamed
// to "deduplication" in the result, as middleware does.
func datasetQueryLikeMiddleware(t *testing.T, params json.RawMessage, rows []interface{}) []interface{} {
	t.Helper()
	filters, options := parseQueryParams(t, params)
	extra, _ := options["extra"].(map[string]interface{})
	retrieveChildren := true
	if v, ok := extra["retrieve_children"].(bool); ok {
		retrieveChildren = v
	}
	props, hasProps := extra["properties"].([]interface{})
	cryptoNames := map[string]bool{
		"encryption": true, "encryptionroot": true, "keyformat": true,
		"keylocation": true, "keystatus": true,
	}
	getCrypto := !hasProps
	wantProp := map[string]bool{}
	if hasProps {
		for _, raw := range props {
			name, _ := raw.(string)
			name = strings.ToLower(name)
			if name == "deduplication" {
				continue // unknown ZFS name, ignored
			}
			if cryptoNames[name] {
				getCrypto = true
			}
			if name == "dedup" {
				wantProp["deduplication"] = true
				continue
			}
			wantProp[name] = true
		}
	}
	recurse := retrieveChildren || len(filters) > 0
	var out []interface{}
	for _, raw := range rows {
		src, _ := raw.(map[string]interface{})
		name, _ := src["name"].(string)
		if !recurse && strings.Contains(name, "/") {
			continue
		}
		keep := true
		for _, item := range filters {
			f, ok := item.([]interface{})
			if !ok || len(f) < 3 {
				continue
			}
			field, _ := f[0].(string)
			op, _ := f[1].(string)
			switch op {
			case "=":
				if src[field] != f[2] {
					keep = false
				}
			case "^":
				val, _ := src[field].(string)
				pref, _ := f[2].(string)
				if !strings.HasPrefix(val, pref) {
					keep = false
				}
			case "in":
				okIn := false
				list, _ := f[2].([]interface{})
				for _, v := range list {
					if src[field] == v {
						okIn = true
						break
					}
				}
				if !okIn {
					keep = false
				}
			}
		}
		if !keep {
			continue
		}
		ds := map[string]interface{}{}
		for k, v := range src {
			ds[k] = v
		}
		if !getCrypto {
			delete(ds, "encrypted")
			delete(ds, "locked")
			delete(ds, "key_loaded")
		}
		if hasProps {
			for _, key := range []string{"used", "available", "usedbysnapshots", "usedbydataset", "usedbychildren", "compression", "compressratio", "deduplication", "dedup", "quota", "refquota"} {
				if !wantProp[key] {
					delete(ds, key)
				}
			}
		}
		out = append(out, ds)
	}
	return out
}

func queryFilterRegistry(t *testing.T, fake *fakeMiddleware) *Registry {
	t.Helper()
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil)
}

func sampleDatasets() []interface{} {
	prop := func(n float64) map[string]interface{} {
		return map[string]interface{}{"parsed": n, "value": "x"}
	}
	ds := func(name, pool string, enc bool, used float64, dedup string) map[string]interface{} {
		row := map[string]interface{}{
			"name": name, "type": "FILESYSTEM", "pool": pool, "encrypted": enc,
			"locked": false, "key_loaded": enc,
			"mountpoint": "/mnt/" + name, "used": prop(used), "available": prop(1),
		}
		if dedup != "" {
			row["dedup"] = map[string]interface{}{"parsed": dedup, "value": strings.ToUpper(dedup)}
			row["deduplication"] = row["dedup"]
		}
		return row
	}
	return []interface{}{
		ds("tank", "tank", false, 300, "off"),
		ds("tank/data", "tank", true, 200, "on"),
		ds("tank/data/sub", "tank", false, 50, "off"),
		ds("tank2", "tank2", false, 100, "off"),
		ds("tank2/media", "tank2", true, 80, "off"),
	}
}

// TestQueryDatasetsPoolFilterAndBounds is the item-5 regression: pool "tank"
// must not match tank2, the dataset query must be bounded, and totals must
// respect encrypted_only.
func TestQueryDatasetsPoolFilterAndBounds(t *testing.T) {
	var gotParams json.RawMessage
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.dataset.query" {
			return nil, nil, false
		}
		gotParams = params
		return datasetQueryLikeMiddleware(t, params, sampleDatasets()), nil, true
	}}
	r := queryFilterRegistry(t, fake)

	out, err := r.CallTool(t.Context(), "query_datasets", map[string]interface{}{"pool": "tank"})
	if err != nil {
		t.Fatal(err)
	}
	filters, options := parseQueryParams(t, gotParams)
	if !hasFilter(filters, "pool", "=", "tank") {
		t.Fatalf("pool filter = %v, want [pool, =, tank]; params=%s", filters, gotParams)
	}
	if hasFilter(filters, "name", "^", "tank") {
		t.Fatalf("still using name prefix filter: %s", gotParams)
	}
	extra, _ := options["extra"].(map[string]interface{})
	if extra["retrieve_children"] != false {
		t.Errorf("retrieve_children = %v, want false; options=%v", extra["retrieve_children"], options)
	}
	props, _ := extra["properties"].([]interface{})
	if len(props) == 0 {
		t.Errorf("missing extra.properties: %s", gotParams)
	}
	resp := decode(t, out)
	counts := map[string]float64{}
	for _, raw := range resp["datasets"].([]interface{}) {
		ds := raw.(map[string]interface{})
		name, _ := ds["name"].(string)
		if strings.HasPrefix(name, "tank2") {
			t.Errorf("pool=tank included %q", name)
		}
		if n, ok := ds["children_count"].(float64); ok {
			counts[name] = n
		}
	}
	if counts["tank"] != 1 || counts["tank/data"] != 1 {
		t.Errorf("children_count = %v, want tank=1 tank/data=1", counts)
	}
	if list, _ := resp["datasets"].([]interface{}); len(list) != 3 {
		t.Errorf("pool=tank datasets = %s, want 3", out)
	}

	out, err = r.CallTool(t.Context(), "query_datasets", map[string]interface{}{"order_by": "name"})
	if err != nil {
		t.Fatal(err)
	}
	filters, options = parseQueryParams(t, gotParams)
	extra, _ = options["extra"].(map[string]interface{})
	if extra["retrieve_children"] == false && len(filters) == 0 {
		t.Errorf("unfiltered query_datasets sent retrieve_children=false with no filters; middleware will return only pool roots; params=%s", gotParams)
	}
	resp = decode(t, out)
	names := map[string]bool{}
	for _, raw := range resp["datasets"].([]interface{}) {
		ds := raw.(map[string]interface{})
		names[ds["name"].(string)] = true
	}
	for _, want := range []string{"tank", "tank/data", "tank/data/sub", "tank2", "tank2/media"} {
		if !names[want] {
			t.Errorf("unfiltered query_datasets missing %q; names=%v body=%s", want, names, out)
		}
	}

	props, _ = extra["properties"].([]interface{})
	hasDedup, hasDeduplication, hasCrypto := false, false, false
	for _, raw := range props {
		switch strings.ToLower(fmt.Sprint(raw)) {
		case "dedup":
			hasDedup = true
		case "deduplication":
			hasDeduplication = true
		case "encryption", "encryptionroot", "keyformat", "keylocation", "keystatus":
			hasCrypto = true
		}
	}
	if hasDeduplication || !hasDedup {
		t.Errorf("properties = %v, want dedup (ZFS name) and not deduplication", props)
	}
	if !hasCrypto {
		t.Errorf("properties = %v, want a crypto property so encrypted/locked/key_loaded are filled", props)
	}
	if data, ok := findDataset(resp, "tank/data"); !ok || data["deduplication"] == nil {
		t.Errorf("tank/data missing deduplication after requesting dedup: %s", out)
	}

	out, err = r.CallTool(t.Context(), "query_datasets", map[string]interface{}{"pool": "tank", "encrypted_only": true})
	if err != nil {
		t.Fatal(err)
	}
	resp = decode(t, out)
	if resp["total_datasets"] != 1.0 {
		t.Errorf("encrypted_only total_datasets = %v, want 1; body=%s", resp["total_datasets"], out)
	}
	if resp["dataset_count"] != 1.0 {
		t.Errorf("encrypted_only dataset_count = %v, want 1", resp["dataset_count"])
	}
	list := resp["datasets"].([]interface{})
	if len(list) != 1 || list[0].(map[string]interface{})["name"] != "tank/data" {
		t.Errorf("encrypted_only datasets = %s", out)
	}

	out, err = r.CallTool(t.Context(), "query_datasets", map[string]interface{}{"encrypted_only": true, "order_by": "name"})
	if err != nil {
		t.Fatal(err)
	}
	resp = decode(t, out)
	list = resp["datasets"].([]interface{})
	got := map[string]bool{}
	for _, raw := range list {
		got[raw.(map[string]interface{})["name"].(string)] = true
	}
	if resp["total_datasets"] != 2.0 || !got["tank/data"] || !got["tank2/media"] || len(list) != 2 {
		t.Errorf("encrypted_only without pool = %s, want tank/data and tank2/media", out)
	}
}

func TestQuerySnapshotsUnfilteredCarriesLimit(t *testing.T) {
	var gotParams json.RawMessage
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.snapshot.query" {
			return nil, nil, false
		}
		_, options := parseQueryParams(t, params)
		if options["count"] == true {
			return 2.0, nil, true
		}
		gotParams = params
		return []interface{}{
			map[string]interface{}{"id": "tank@a", "snapshot_name": "b", "dataset": "tank", "pool": "tank", "properties": map[string]interface{}{}},
			map[string]interface{}{"id": "tank@b", "snapshot_name": "a", "dataset": "tank", "pool": "tank", "properties": map[string]interface{}{}},
		}, nil, true
	}}
	r := queryFilterRegistry(t, fake)
	out, err := r.CallTool(t.Context(), "query_snapshots", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	_, options := parseQueryParams(t, gotParams)
	limit, ok := options["limit"].(float64)
	if !ok || limit <= 0 {
		t.Errorf("unfiltered query_snapshots options = %s, want a positive limit", gotParams)
	}
	_ = out
}

func TestDatasetChildrenCountFromNames(t *testing.T) {
	names := []string{"tank", "tank/data", "tank/data/sub", "tank/home", "tank2", "tank2/x"}
	if got := datasetChildrenCount("tank", names); got != 2 {
		t.Errorf("tank = %d, want 2", got)
	}
	if got := datasetChildrenCount("tank/data", names); got != 1 {
		t.Errorf("tank/data = %d, want 1", got)
	}
	if got := datasetChildrenCount("tank/home", names); got != 0 {
		t.Errorf("tank/home = %d, want 0", got)
	}
}

// TestQueryPoolsAndSharesReturnCompleteRecords checks that query_pools and
// query_shares do not narrow middleware records with a select list, so the
// fields returned on main stay present. A "secret" sentinel on an SMB share
// must be masked in the tool result. query_vms still requests a select list.
func TestQueryPoolsAndSharesReturnCompleteRecords(t *testing.T) {
	var methods = map[string]json.RawMessage{}
	fullPool := map[string]interface{}{
		"id": 1.0, "name": "tank", "status": "ONLINE", "size": 1.0e12,
		"allocated": 4.0e11, "free": 6.0e11, "healthy": true, "warning": false,
		"status_code": "OK", "status_detail": "healthy", "path": "/mnt/tank",
		"topology": map[string]interface{}{"data": []interface{}{"ignored"}},
		"scan": map[string]interface{}{
			"function": "SCRUB", "state": "FINISHED", "errors": 0.0,
			"percentage": 100.0, "end_time": map[string]interface{}{"$date": 1.0},
		},
	}
	fullSMB := map[string]interface{}{
		"id": 1.0, "name": "media", "path": "/mnt/tank/media", "enabled": true,
		"comment": "media", "purpose": "DEFAULT_SHARE", "readonly": false, "browsable": true,
		"options":                        map[string]interface{}{"hostsallow": []interface{}{"192.168.1.0/24"}, "hostsdeny": []interface{}{}},
		"access_based_share_enumeration": true,
		"audit":                          map[string]interface{}{"enable": true},
		"locked":                         false,
		"secret":                         "ignore",
	}
	fullNFS := map[string]interface{}{
		"id": 2.0, "path": "/mnt/tank/nfs", "enabled": true, "ro": true,
		"comment": "nfs", "networks": []interface{}{}, "hosts": []interface{}{},
		"maproot_user": "nobody", "maproot_group": "nogroup",
		"mapall_user": nil, "mapall_group": nil,
		"security": []interface{}{"SYS"}, "expose_snapshots": true, "locked": false,
	}
	applySelect := func(row map[string]interface{}, options map[string]interface{}) map[string]interface{} {
		sel, _ := options["select"].([]interface{})
		if len(sel) == 0 {
			out := map[string]interface{}{}
			for k, v := range row {
				out[k] = v
			}
			return out
		}
		return pickFields(row, sel)
	}
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		methods[method] = params
		_, opts := parseQueryParams(t, params)
		switch method {
		case "pool.query":
			return []interface{}{applySelect(fullPool, opts)}, nil, true
		case "sharing.smb.query":
			return []interface{}{applySelect(fullSMB, opts)}, nil, true
		case "sharing.nfs.query":
			return []interface{}{applySelect(fullNFS, opts)}, nil, true
		case "vm.query":
			return []interface{}{map[string]interface{}{"id": 1.0, "name": "web", "vcpus": 1.0, "cores": 1.0, "threads": 1.0, "memory": 1024.0, "autostart": true, "status": map[string]interface{}{"state": "STOPPED"}}}, nil, true
		default:
			return nil, nil, false
		}
	}}
	r := queryFilterRegistry(t, fake)

	out, err := r.CallTool(t.Context(), "query_pools", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	_, opts := parseQueryParams(t, methods["pool.query"])
	if _, ok := opts["select"]; ok {
		t.Errorf("pool.query options = %s, want no select so fields match main", methods["pool.query"])
	}
	pools := decodeList(t, out)
	if len(pools) != 1 {
		t.Fatalf("query_pools = %s", out)
	}
	for key := range fullPool {
		if _, ok := pools[0][key]; !ok {
			t.Errorf("query_pools missing %s (present on main): %s", key, out)
		}
	}
	if _, isNum := pools[0]["size"].(float64); !isNum {
		t.Errorf("query_pools size = %v (%T), want an integer as on main", pools[0]["size"], pools[0]["size"])
	}
	scan, _ := pools[0]["scan"].(map[string]interface{})
	for _, key := range []string{"errors", "percentage", "end_time"} {
		if _, ok := scan[key]; !ok {
			t.Errorf("query_pools scan missing %s (present on main): %s", key, out)
		}
	}

	out, err = r.CallTool(t.Context(), "query_shares", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	shares := decode(t, out)
	smbList, _ := shares["smb_shares"].([]interface{})
	nfsList, _ := shares["nfs_shares"].([]interface{})
	if len(smbList) != 1 || len(nfsList) != 1 {
		t.Fatalf("query_shares = %s", out)
	}
	smb := smbList[0].(map[string]interface{})
	nfs := nfsList[0].(map[string]interface{})
	for _, key := range []string{"options", "access_based_share_enumeration", "audit", "locked"} {
		if _, ok := smb[key]; !ok {
			t.Errorf("query_shares SMB missing %s (present on main): %s", key, out)
		}
	}
	smbOptions, _ := smb["options"].(map[string]interface{})
	if _, ok := smbOptions["hostsallow"]; !ok {
		t.Errorf("query_shares SMB options missing hostsallow: %s", out)
	}
	for _, key := range []string{"security", "expose_snapshots", "locked"} {
		if _, ok := nfs[key]; !ok {
			t.Errorf("query_shares NFS missing %s (present on main): %s", key, out)
		}
	}
	for key := range fullSMB {
		if _, ok := smb[key]; !ok {
			t.Errorf("query_shares SMB missing %s (present on main): %s", key, out)
		}
	}
	// CallTool returns the handler text; the server redacts it with
	// redact.ToolOutput before the model sees it. The secret sentinel must
	// not survive that step.
	masked := redact.ToolOutput(out)
	var maskedShares map[string]interface{}
	if err := json.Unmarshal([]byte(masked), &maskedShares); err != nil {
		t.Fatalf("masked query_shares: %v (%s)", err, masked)
	}
	maskedSMB, _ := maskedShares["smb_shares"].([]interface{})[0].(map[string]interface{})
	if maskedSMB["secret"] != redact.Marker {
		t.Errorf("query_shares SMB secret = %v, want %s", maskedSMB["secret"], redact.Marker)
	}
	if strings.Contains(masked, "ignore") {
		t.Errorf("query_shares leaked the secret sentinel: %s", masked)
	}
	for key := range fullNFS {
		if _, ok := nfs[key]; !ok {
			t.Errorf("query_shares NFS missing %s (present on main): %s", key, out)
		}
	}
	_, smbOpts := parseQueryParams(t, methods["sharing.smb.query"])
	_, nfsOpts := parseQueryParams(t, methods["sharing.nfs.query"])
	if _, ok := smbOpts["select"]; ok {
		t.Errorf("sharing.smb.query options = %s, want no select", methods["sharing.smb.query"])
	}
	if _, ok := nfsOpts["select"]; ok {
		t.Errorf("sharing.nfs.query options = %s, want no select", methods["sharing.nfs.query"])
	}

	if _, err := r.CallTool(t.Context(), "query_vms", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	_, vmOpts := parseQueryParams(t, methods["vm.query"])
	if !selectContains(vmOpts, "name") || !selectContains(vmOpts, "devices") {
		t.Errorf("vm.query options = %s", methods["vm.query"])
	}
}

func selectContains(options map[string]interface{}, field string) bool {
	sel, _ := options["select"].([]interface{})
	for _, v := range sel {
		if v == field {
			return true
		}
	}
	return false
}

func pickFields(m map[string]interface{}, keys []interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, key := range keys {
		k, _ := key.(string)
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

func findDataset(resp map[string]interface{}, name string) (map[string]interface{}, bool) {
	list, _ := resp["datasets"].([]interface{})
	for _, raw := range list {
		ds, _ := raw.(map[string]interface{})
		if ds["name"] == name {
			return ds, true
		}
	}
	return nil, false
}

func TestQueryLimitIsClamped(t *testing.T) {
	var snapOpts map[string]interface{}
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		filters, options := parseQueryParams(t, params)
		_ = filters
		switch method {
		case "pool.dataset.query":
			return datasetQueryLikeMiddleware(t, params, sampleDatasets()), nil, true
		case "pool.snapshot.query":
			if options["count"] == true {
				return 2.0, nil, true
			}
			snapOpts = options
			return []interface{}{
				map[string]interface{}{"id": "tank@a", "snapshot_name": "a", "dataset": "tank", "pool": "tank", "properties": map[string]interface{}{}},
			}, nil, true
		default:
			return nil, nil, false
		}
	}}
	r := queryFilterRegistry(t, fake)
	dsOut, err := r.CallTool(t.Context(), "query_datasets", map[string]interface{}{"limit": 1e300})
	if err != nil {
		t.Fatalf("query_datasets huge limit: %v", err)
	}
	dsNote, _ := decode(t, dsOut)["note"].(string)
	if !strings.Contains(dsNote, "capped to 1000") {
		t.Errorf("query_datasets capped note = %q, want cap mentioned; body=%s", dsNote, dsOut)
	}
	snapOut, err := r.CallTool(t.Context(), "query_snapshots", map[string]interface{}{"limit": 1e300})
	if err != nil {
		t.Fatalf("query_snapshots huge limit: %v", err)
	}
	snapNote, _ := decode(t, snapOut)["note"].(string)
	if !strings.Contains(snapNote, "capped to 1000") {
		t.Errorf("query_snapshots capped note = %q, want cap mentioned; body=%s", snapNote, snapOut)
	}
	limit, _ := snapOpts["limit"].(float64)
	if limit <= 0 || limit > 1000 {
		t.Errorf("snapshot limit sent to middleware = %v, want 1..1000; options=%v", snapOpts["limit"], snapOpts)
	}
}

func TestQueryLimitGuardsNaNAndInf(t *testing.T) {
	if got, capped := queryLimit(map[string]interface{}{"limit": math.NaN()}); got != defaultQueryLimit || capped {
		t.Errorf("NaN = %d capped=%v, want default %d", got, capped, defaultQueryLimit)
	}
	if got, capped := queryLimit(map[string]interface{}{"limit": math.Inf(1)}); got != maxQueryLimit || !capped {
		t.Errorf("+Inf = %d capped=%v, want %d capped", got, capped, maxQueryLimit)
	}
	if got, capped := queryLimit(map[string]interface{}{"limit": math.Inf(-1)}); got != defaultQueryLimit || capped {
		t.Errorf("-Inf = %d capped=%v, want default %d", got, capped, defaultQueryLimit)
	}
	if got, capped := queryLimit(map[string]interface{}{"limit": 10.0}); got != 10 || capped {
		t.Errorf("10 = %d capped=%v, want 10", got, capped)
	}
}

func TestQuerySnapshotsCountFailureMarksTotalUnknown(t *testing.T) {
	n := 50
	snaps := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		name := "s" + strings.Repeat("x", 0) + fmt.Sprintf("%02d", i)
		snaps = append(snaps, map[string]interface{}{
			"id": "tank@" + name, "snapshot_name": name, "dataset": "tank", "pool": "tank",
			"properties": map[string]interface{}{},
		})
	}
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method != "pool.snapshot.query" {
			return nil, nil, false
		}
		_, options := parseQueryParams(t, params)
		if options["count"] == true {
			return nil, map[string]interface{}{"code": 1, "message": "count failed"}, true
		}
		return snaps, nil, true
	}}
	r := queryFilterRegistry(t, fake)
	out, err := r.CallTool(t.Context(), "query_snapshots", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	if resp["total_snapshots"] != nil {
		t.Errorf("total_snapshots = %v, want null/omitted when the count query fails; body=%s", resp["total_snapshots"], out)
	}
	note, _ := resp["note"].(string)
	if !strings.Contains(strings.ToLower(note), "unknown") {
		t.Errorf("missing note that the snapshot total is unknown: %s", out)
	}
	if resp["snapshot_count"] != 50.0 {
		t.Errorf("snapshot_count = %v, want the page size 50", resp["snapshot_count"])
	}
}

func decodeList(t *testing.T, out string) []map[string]interface{} {
	t.Helper()
	var list []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("parse list: %v; body=%s", err, out)
	}
	return list
}

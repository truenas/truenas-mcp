package tools

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// catalogApps is a small catalog: one app only in stable, one only in
// community and one in both.
var catalogApps = []interface{}{
	map[string]interface{}{"name": "stableapp", "title": "Stable App", "train": "stable", "latest_version": "1.0.0"},
	map[string]interface{}{"name": "communityapp", "title": "Community App", "train": "community", "latest_version": "2.0.0"},
	map[string]interface{}{"name": "bothapp", "title": "Both App", "train": "community", "latest_version": "3.0.0"},
	map[string]interface{}{"name": "bothapp", "title": "Both App", "train": "stable", "latest_version": "3.0.0"},
	map[string]interface{}{"name": "nextcloud", "title": "Nextcloud", "train": "stable", "latest_version": "1.0.0",
		"categories": []interface{}{"productivity", "storage"}},
}

// catalogHandle answers app.available by applying the same filter operators
// the 25.10 middleware uses: "=" (equality), "~" (Python re.match, anchored
// at the start), "rin" (literal substring or list membership) and "Crin"
// (case-insensitive rin). catalog.get_app_details answers only for an app
// that is in the requested train, with an ENOENT error and a traceback
// otherwise, as the middleware does.
func catalogHandle(t *testing.T) func(string, json.RawMessage) (interface{}, map[string]interface{}, bool) {
	return func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		var raw []json.RawMessage
		if err := json.Unmarshal(params, &raw); err != nil {
			t.Errorf("bad %s params %s", method, params)
			return nil, nil, false
		}
		switch method {
		case "app.available":
			var filters [][]interface{}
			_ = json.Unmarshal(raw[0], &filters)
			out := []interface{}{}
			for _, a := range catalogApps {
				app := a.(map[string]interface{})
				keep := true
				for _, f := range filters {
					if len(f) < 3 {
						keep = false
						break
					}
					field, _ := f[0].(string)
					op, _ := f[1].(string)
					if !catalogFilterMatch(app[field], op, f[2]) {
						keep = false
						break
					}
				}
				if keep {
					out = append(out, app)
				}
			}
			return out, nil, true
		case "catalog.get_app_details":
			var name string
			var opts map[string]interface{}
			_ = json.Unmarshal(raw[0], &name)
			_ = json.Unmarshal(raw[1], &opts)
			train, _ := opts["train"].(string)
			for _, a := range catalogApps {
				app := a.(map[string]interface{})
				if app["name"] == name && app["train"] == train {
					return map[string]interface{}{"name": name, "title": app["title"], "latest_version": app["latest_version"]}, nil, true
				}
			}
			return nil, middlewareError("ENOENT", "Unable to locate '"+name+"' at '/catalog/trains/"+train+"/"+name+"'"), true
		}
		return nil, nil, false
	}
}

func callCatalogTool(t *testing.T, tool string, args map[string]interface{}) (string, error, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{handle: catalogHandle(t)}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	defer mgr.Shutdown()
	out, err := NewRegistry(client, mgr).CallTool(t.Context(), tool, args)
	return out, err, fake
}

// detailsTrains returns the train sent with each catalog.get_app_details call.
func detailsTrains(t *testing.T, fake *fakeMiddleware) []string {
	t.Helper()
	var trains []string
	for _, c := range fake.called("catalog.get_app_details") {
		var raw []json.RawMessage
		var opts map[string]interface{}
		if json.Unmarshal(c.Params, &raw) != nil || len(raw) != 2 || json.Unmarshal(raw[1], &opts) != nil {
			t.Fatalf("bad catalog.get_app_details params %s", c.Params)
		}
		train, _ := opts["train"].(string)
		trains = append(trains, train)
	}
	return trains
}

// Without a train, search_app_catalog searches every train, so an app that
// is only in the community train is found.
func TestSearchAppCatalogDefaultsToAllTrains(t *testing.T) {
	out, err, fake := callCatalogTool(t, "search_app_catalog", map[string]interface{}{"query": "communityapp"})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	apps, _ := resp["apps"].([]interface{})
	if len(apps) != 1 || apps[0].(map[string]interface{})["train"] != "community" {
		t.Errorf("apps = %v, want communityapp in the community train", resp["apps"])
	}
	for _, c := range fake.called("app.available") {
		if strings.Contains(string(c.Params), `"train"`) {
			t.Errorf("no train given, but the query filters on train: %s", c.Params)
		}
	}

	// "all" behaves the same.
	out, err, _ = callCatalogTool(t, "search_app_catalog", map[string]interface{}{"query": "app", "train": "all"})
	if err != nil {
		t.Fatal(err)
	}
	if n := decode(t, out)["count"]; n != 4.0 {
		t.Errorf("train all: count = %v, want 4", n)
	}
}

// An explicit train is still honored, and the middleware filters on it so
// limit counts only apps in that train.
func TestSearchAppCatalogHonorsTrain(t *testing.T) {
	out, err, fake := callCatalogTool(t, "search_app_catalog", map[string]interface{}{"query": "communityapp", "train": "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No apps found") {
		t.Errorf("train stable: got %s, want no apps", out)
	}
	calls := fake.called("app.available")
	if len(calls) != 1 || !strings.Contains(string(calls[0].Params), `["train","=","stable"]`) {
		t.Errorf("app.available params = %v, want a train filter", calls)
	}

	out, err, _ = callCatalogTool(t, "search_app_catalog", map[string]interface{}{"query": "app", "train": "community"})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	if resp["count"] != 2.0 {
		t.Errorf("train community: count = %v, want 2", resp["count"])
	}
	for _, a := range resp["apps"].([]interface{}) {
		if a.(map[string]interface{})["train"] != "community" {
			t.Errorf("train community: got app %v", a)
		}
	}
}

// Without a train, get_app_catalog_details looks up the app's train, so a
// community-only app works without train=community.
func TestGetAppCatalogDetailsResolvesTrain(t *testing.T) {
	for _, c := range []struct {
		app, want string
		several   bool
	}{
		{"communityapp", "community", false},
		{"stableapp", "stable", false},
		{"bothapp", "stable", true}, // stable is preferred
	} {
		out, err, fake := callCatalogTool(t, "get_app_catalog_details", map[string]interface{}{"app_name": c.app})
		if err != nil {
			t.Errorf("%s: %v", c.app, err)
			continue
		}
		resp := decode(t, out)
		if resp["name"] != c.app || resp["train"] != c.want {
			t.Errorf("%s: name=%v train=%v, want train %s", c.app, resp["name"], resp["train"], c.want)
		}
		if got := detailsTrains(t, fake); len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: get_app_details trains = %v, want [%s]", c.app, got, c.want)
		}
		trains, has := resp["available_trains"].([]interface{})
		if c.several != has {
			t.Errorf("%s: available_trains = %v", c.app, resp["available_trains"])
		}
		if c.several && (len(trains) != 2 || trains[0] != "stable" || trains[1] != "community" || resp["train_note"] == nil) {
			t.Errorf("%s: available_trains = %v, train_note = %v", c.app, trains, resp["train_note"])
		}
	}
}

// An explicit train is used as given, without a lookup.
func TestGetAppCatalogDetailsHonorsTrain(t *testing.T) {
	out, err, fake := callCatalogTool(t, "get_app_catalog_details", map[string]interface{}{"app_name": "bothapp", "train": "community"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := decode(t, out); resp["train"] != "community" {
		t.Errorf("train = %v, want community", resp["train"])
	}
	if got := detailsTrains(t, fake); len(got) != 1 || got[0] != "community" {
		t.Errorf("get_app_details trains = %v, want [community]", got)
	}
	if n := len(fake.called("app.available")); n != 0 {
		t.Errorf("app.available called %d times with an explicit train", n)
	}
}

// An app that is in no train, or not in the train given, gets a short error
// that names the fix, without the middleware's traceback.
func TestGetAppCatalogDetailsCleanErrors(t *testing.T) {
	for _, c := range []struct {
		args map[string]interface{}
		want string
	}{
		{map[string]interface{}{"app_name": "nosuchapp"}, "not found in any catalog train"},
		{map[string]interface{}{"app_name": "communityapp", "train": "stable"}, "not found in the stable catalog train"},
	} {
		_, err, fake := callCatalogTool(t, "get_app_catalog_details", c.args)
		if err == nil {
			t.Errorf("%v: no error", c.args)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, c.want) || !strings.Contains(msg, "search_app_catalog") {
			t.Errorf("%v: error %q, want %q and a pointer to search_app_catalog", c.args, msg, c.want)
		}
		for _, bad := range []string{"Traceback", "Trace:", "ENOENT", "/catalog/trains"} {
			if strings.Contains(msg, bad) {
				t.Errorf("%v: error contains %q: %q", c.args, bad, msg)
			}
		}
		if _, ok := c.args["train"]; !ok {
			if n := len(fake.called("catalog.get_app_details")); n != 0 {
				t.Errorf("%v: get_app_details called %d times for an unknown app", c.args, n)
			}
		}
	}
}

// If the train lookup itself fails, the stable train is used as before.
func TestGetAppCatalogDetailsLookupFailureFallsBackToStable(t *testing.T) {
	handle := catalogHandle(t)
	fake := &fakeMiddleware{handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "app.available" {
			return nil, middlewareError("EFAULT", "catalog unavailable"), true
		}
		return handle(method, params)
	}}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	defer mgr.Shutdown()
	out, err := NewRegistry(client, mgr).CallTool(t.Context(), "get_app_catalog_details", map[string]interface{}{"app_name": "stableapp"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := decode(t, out); resp["train"] != "stable" {
		t.Errorf("train = %v, want stable", resp["train"])
	}
}

// The schemas no longer default train to stable.
func TestCatalogTrainSchemaDefaults(t *testing.T) {
	r := NewRegistry(nil, nil)
	search := r.tools["search_app_catalog"].Definition.InputSchema["properties"].(map[string]interface{})["train"].(map[string]interface{})
	if search["default"] != "all" {
		t.Errorf("search_app_catalog train default = %v, want all", search["default"])
	}
	details := r.tools["get_app_catalog_details"].Definition.InputSchema["properties"].(map[string]interface{})["train"].(map[string]interface{})
	if d, has := details["default"]; has {
		t.Errorf("get_app_catalog_details train default = %v, want none", d)
	}
	install := r.tools["install_app"].Definition.InputSchema["properties"].(map[string]interface{})["train"].(map[string]interface{})
	if d, has := install["default"]; has {
		t.Errorf("install_app train default = %v, want none", d)
	}
}

// search_app_catalog sends the query as a case-insensitive literal substring
// (Crin), not as a regex, so characters like + need no escaping. limit <= 0
// is rejected and nothing is sent to the middleware.
func TestSearchAppCatalogLiteralQueryAndRejectsNonPositiveLimit(t *testing.T) {
	out, err, fake := callCatalogTool(t, "search_app_catalog", map[string]interface{}{"query": "c++"})
	if err != nil {
		t.Fatalf("c++ query: %v (%s)", err, out)
	}
	calls := fake.called("app.available")
	if len(calls) != 1 {
		t.Fatalf("app.available called %d times", len(calls))
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(calls[0].Params, &raw); err != nil || len(raw) == 0 {
		t.Fatalf("params = %s (%v)", calls[0].Params, err)
	}
	var filters [][]interface{}
	if err := json.Unmarshal(raw[0], &filters); err != nil {
		t.Fatalf("filters = %s (%v)", raw[0], err)
	}
	found := false
	for _, f := range filters {
		if len(f) >= 3 && f[0] == "name" && f[1] == "Crin" && f[2] == "c++" {
			found = true
		}
		if len(f) >= 3 && f[0] == "name" && f[1] == "~" {
			t.Errorf("query still used the ~ regex operator: %s", calls[0].Params)
		}
	}
	if !found {
		t.Errorf("app.available filters = %s, want name Crin %q", calls[0].Params, "c++")
	}

	for _, limit := range []float64{0, -1} {
		_, err, fake := callCatalogTool(t, "search_app_catalog", map[string]interface{}{"query": "app", "limit": limit})
		if err == nil {
			t.Errorf("limit %v: no error", limit)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "limit") || !strings.Contains(msg, "nothing was executed") {
			t.Errorf("limit %v: error %q, want limit and nothing was executed", limit, msg)
		}
		if n := len(fake.called("app.available")); n != 0 {
			t.Errorf("limit %v: app.available called %d times, want 0", limit, n)
		}
	}
}

func installCatalogRegistry(t *testing.T) (*Registry, *fakeMiddleware) {
	t.Helper()
	existing := map[string]bool{
		"tank/apps/jellyfin/config": true,
		"tank/apps/jellyfin/data":   true,
	}
	base := catalogHandle(t)
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"pool.query": []interface{}{map[string]interface{}{"name": "tank", "status": "ONLINE"}},
			"app.query":  []interface{}{},
			"app.create": 101,
		},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			switch method {
			case "app.available":
				return base(method, params)
			case "catalog.get_app_details":
				res, rpcErr, ok := base(method, params)
				if !ok || rpcErr != nil {
					return res, rpcErr, ok
				}
				details := hostPathCatalogDetails()
				if m, ok := res.(map[string]interface{}); ok {
					if name, ok := m["name"]; ok {
						details["name"] = name
					}
				}
				return details, nil, true
			case "pool.dataset.query":
				var args []interface{}
				if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
					return []interface{}{}, nil, true
				}
				filters, _ := args[0].([]interface{})
				name := ""
				if len(filters) > 0 {
					if f, ok := filters[0].([]interface{}); ok && len(f) >= 3 {
						if s, ok := f[2].(string); ok {
							name = s
						}
					}
				}
				if existing[name] {
					return []interface{}{map[string]interface{}{"name": name}}, nil, true
				}
				return []interface{}{}, nil, true
			default:
				return nil, nil, false
			}
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake
}

func installCatalogArgs(catalogApp string, extra map[string]interface{}) map[string]interface{} {
	args := map[string]interface{}{
		"app_name":    catalogApp,
		"catalog_app": catalogApp,
		"values":      validHostPathValues(),
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func appCreateTrain(t *testing.T, fake *fakeMiddleware) string {
	t.Helper()
	calls := fake.called("app.create")
	if len(calls) != 1 {
		t.Fatalf("app.create called %d times, want 1", len(calls))
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(calls[0].Params, &raw); err != nil || len(raw) == 0 {
		t.Fatalf("app.create params = %s (%v)", calls[0].Params, err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw[0], &payload); err != nil {
		t.Fatalf("app.create payload = %s (%v)", raw[0], err)
	}
	train, _ := payload["train"].(string)
	return train
}

// Without a train, install_app looks up the catalog train, so a
// community-only app is installed from community rather than stable.
func TestInstallAppResolvesTrain(t *testing.T) {
	for _, c := range []struct {
		app, want string
	}{
		{"communityapp", "community"},
		{"stableapp", "stable"},
		{"bothapp", "stable"},
	} {
		r, fake := installCatalogRegistry(t)
		out, err := r.CallTool(t.Context(), "install_app", installCatalogArgs(c.app, nil))
		if err != nil {
			t.Errorf("%s: %v (%s)", c.app, err, out)
			continue
		}
		resp := decode(t, out)
		if resp["train"] != c.want {
			t.Errorf("%s: train = %v, want %s", c.app, resp["train"], c.want)
		}
		if got := appCreateTrain(t, fake); got != c.want {
			t.Errorf("%s: app.create train = %q, want %s", c.app, got, c.want)
		}
	}

	r, fake := installCatalogRegistry(t)
	out, err := r.CallTool(t.Context(), "install_app", installCatalogArgs("communityapp", map[string]interface{}{"dry_run": true}))
	if err != nil {
		t.Fatalf("dry-run: %v (%s)", err, out)
	}
	if n := len(fake.called("app.create")); n != 0 {
		t.Errorf("dry-run called app.create %d times", n)
	}
	preview := decode(t, out)
	actions, _ := preview["planned_actions"].([]interface{})
	found := false
	for _, a := range actions {
		m, _ := a.(map[string]interface{})
		details, _ := m["details"].(map[string]interface{})
		if details["train"] == "community" {
			found = true
		}
	}
	if !found {
		t.Errorf("dry-run preview = %s, want train community", out)
	}
}

// An explicit train is used as given, without a lookup.
func TestInstallAppHonorsTrain(t *testing.T) {
	r, fake := installCatalogRegistry(t)
	out, err := r.CallTool(t.Context(), "install_app", installCatalogArgs("bothapp", map[string]interface{}{"train": "community"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp := decode(t, out); resp["train"] != "community" {
		t.Errorf("train = %v, want community", resp["train"])
	}
	if got := appCreateTrain(t, fake); got != "community" {
		t.Errorf("app.create train = %q, want community", got)
	}
	if n := len(fake.called("app.available")); n != 0 {
		t.Errorf("app.available called %d times with an explicit train", n)
	}
}

// catalogFilterMatch applies one middleware filter operator as 25.10's
// filter_list does: "~" is re.match (anchored at the start), "rin" is
// operator.contains on a string or list, and "Crin" is case-insensitive rin.
func catalogFilterMatch(field interface{}, op string, want interface{}) bool {
	switch op {
	case "=":
		return field == want
	case "~":
		s, _ := field.(string)
		pat, _ := want.(string)
		re, err := regexp.Compile(`\A(?:` + pat + `)`)
		return err == nil && re.MatchString(s)
	case "rin":
		return catalogRin(field, want, false)
	case "Crin":
		return catalogRin(field, want, true)
	default:
		return false
	}
}

func catalogRin(field, want interface{}, insensitive bool) bool {
	fold := func(s string) string {
		if insensitive {
			return strings.ToLower(s)
		}
		return s
	}
	switch x := field.(type) {
	case string:
		w, ok := want.(string)
		return ok && strings.Contains(fold(x), fold(w))
	case []interface{}:
		w := want
		if ws, ok := want.(string); ok && insensitive {
			w = fold(ws)
		}
		for _, item := range x {
			if s, ok := item.(string); ok && insensitive {
				if fold(s) == w {
					return true
				}
				continue
			}
			if item == want {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func searchNameFilter(t *testing.T, fake *fakeMiddleware) (op, query string) {
	t.Helper()
	calls := fake.called("app.available")
	if len(calls) != 1 {
		t.Fatalf("app.available called %d times", len(calls))
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(calls[0].Params, &raw); err != nil || len(raw) == 0 {
		t.Fatalf("params = %s (%v)", calls[0].Params, err)
	}
	var filters [][]interface{}
	if err := json.Unmarshal(raw[0], &filters); err != nil {
		t.Fatalf("filters = %s (%v)", raw[0], err)
	}
	for _, f := range filters {
		if len(f) >= 3 && f[0] == "name" {
			op, _ = f[1].(string)
			query, _ = f[2].(string)
			return op, query
		}
	}
	t.Fatalf("no name filter in %s", calls[0].Params)
	return "", ""
}

func searchApps(t *testing.T, out string) []string {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil
	}
	raw, _ := resp["apps"].([]interface{})
	var names []string
	for _, a := range raw {
		m, _ := a.(map[string]interface{})
		if n, ok := m["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names
}

func hasAppName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// A substring that is not a prefix, such as "cloud" in "nextcloud", must
// be found. The 25.10 "~" operator is re.match and would miss it.
func TestSearchAppCatalogCloudFindsNextcloud(t *testing.T) {
	for _, q := range []string{"cloud", "CLOUD"} {
		out, err, fake := callCatalogTool(t, "search_app_catalog", map[string]interface{}{"query": q})
		if err != nil {
			t.Errorf("query %q: %v (%s)", q, err, out)
			continue
		}
		if op, got := searchNameFilter(t, fake); op != "Crin" || got != q {
			t.Errorf("query %q: name filter = %s %q, want Crin %q", q, op, got, q)
		}
		if !hasAppName(searchApps(t, out), "nextcloud") {
			t.Errorf("query %q: apps = %v, want nextcloud", q, searchApps(t, out))
		}
	}
}

// category is an exact membership test on the categories list, not a regex.
func TestSearchAppCatalogCategoryIsExactMembership(t *testing.T) {
	out, err, fake := callCatalogTool(t, "search_app_catalog", map[string]interface{}{"category": "productivity"})
	if err != nil {
		t.Fatalf("category productivity: %v (%s)", err, out)
	}
	calls := fake.called("app.available")
	if len(calls) != 1 {
		t.Fatalf("app.available called %d times", len(calls))
	}
	if !strings.Contains(string(calls[0].Params), `["categories","rin","productivity"]`) {
		t.Errorf("app.available params = %s, want categories rin productivity", calls[0].Params)
	}
	if !hasAppName(searchApps(t, out), "nextcloud") {
		t.Errorf("productivity: apps = %v, want nextcloud", searchApps(t, out))
	}

	out, err, _ = callCatalogTool(t, "search_app_catalog", map[string]interface{}{"category": "media"})
	if err != nil {
		t.Fatalf("category media: %v", err)
	}
	if hasAppName(searchApps(t, out), "nextcloud") {
		t.Errorf("category media unexpectedly included nextcloud: %s", out)
	}
}

// When the train lookup itself fails, install_app uses stable and says so.
func TestInstallAppLookupFailureFallsBackToStableWithNote(t *testing.T) {
	for _, dry := range []bool{true, false} {
		r, fake := installCatalogRegistry(t)
		orig := fake.handle
		fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method == "app.available" {
				return nil, middlewareError("EFAULT", "catalog unavailable"), true
			}
			return orig(method, params)
		}
		args := installCatalogArgs("stableapp", nil)
		if dry {
			args["dry_run"] = true
		}
		out, err := r.CallTool(t.Context(), "install_app", args)
		if err != nil {
			t.Errorf("dry_run=%v: %v (%s)", dry, err, out)
			continue
		}
		if dry {
			if n := len(fake.called("app.create")); n != 0 {
				t.Errorf("dry_run: app.create called %d times", n)
			}
			preview := decode(t, out)
			state, _ := preview["current_state"].(map[string]interface{})
			note, _ := state["train_note"].(string)
			if !strings.Contains(strings.ToLower(note), "stable") || !strings.Contains(strings.ToLower(note), "lookup") {
				t.Errorf("dry_run preview train_note = %q, want a lookup-failed / stable note: %s", note, out)
			}
			continue
		}
		resp := decode(t, out)
		if resp["train"] != "stable" {
			t.Errorf("train = %v, want stable", resp["train"])
		}
		note, _ := resp["train_note"].(string)
		if !strings.Contains(strings.ToLower(note), "stable") || !strings.Contains(strings.ToLower(note), "lookup") {
			t.Errorf("train_note = %q, want a lookup-failed / stable note: %s", note, out)
		}
		if got := appCreateTrain(t, fake); got != "stable" {
			t.Errorf("app.create train = %q, want stable", got)
		}
	}
}

package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

func TestCreateDatasetVolblocksize(t *testing.T) {
	r := NewRegistry(nil, nil)
	preview := func(args map[string]interface{}) (map[string]interface{}, error) {
		args["dry_run"] = true
		out, err := handleCreateDataset(nil, args)
		if err != nil {
			return nil, err
		}
		var got map[string]interface{}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		return got["payload"].(map[string]interface{}), nil
	}

	for _, v := range []string{"512", "512B", "1K", "2K", "4K", "8K", "16K", "32K", "64K", "128K"} {
		payload, err := preview(map[string]interface{}{"name": "tank/vol", "type": "VOLUME", "volsize": float64(1 << 30), "volblocksize": v})
		if err != nil {
			t.Errorf("volblocksize %q rejected: %v", v, err)
			continue
		}
		if payload["volblocksize"] != v {
			t.Errorf("volblocksize %q: payload has %v", v, payload["volblocksize"])
		}
	}

	// Not in the middleware's list, wrong case, bytes, wrong type, empty.
	for _, v := range []interface{}{"16k", "16384", "256K", "", float64(16384), true, nil} {
		_, err := preview(map[string]interface{}{"name": "tank/vol", "type": "VOLUME", "volsize": float64(1 << 30), "volblocksize": v})
		if err == nil || !strings.Contains(err.Error(), "volblocksize must be") || !strings.Contains(err.Error(), "128K") {
			t.Errorf("volblocksize %#v: err = %v", v, err)
		}
	}

	// FILESYSTEM (explicit or default) rejects it instead of ignoring it.
	for _, args := range []map[string]interface{}{
		{"name": "tank/fs", "volblocksize": "16K"},
		{"name": "tank/fs", "type": "FILESYSTEM", "volblocksize": "16K"},
		{"name": "tank/fs", "type": "FILESYSTEM", "volblocksize": float64(16)},
	} {
		if _, err := preview(args); err == nil || !strings.Contains(err.Error(), "only to VOLUME") {
			t.Errorf("%v: err = %v", args, err)
		}
	}

	// The schema advertises the same enum.
	props := r.tools["create_dataset"].Definition.InputSchema["properties"].(map[string]interface{})
	enum, _ := props["volblocksize"].(map[string]interface{})["enum"].([]string)
	if strings.Join(enum, ",") != strings.Join(volblocksizes, ",") {
		t.Errorf("schema enum = %v, want %v", enum, volblocksizes)
	}
}

func TestCreateDatasetUserProperties(t *testing.T) {
	prop := func(k, v interface{}) interface{} { return map[string]interface{}{"key": k, "value": v} }
	call := func(up interface{}) error {
		_, err := handleCreateDataset(nil, map[string]interface{}{"name": "tank/x", "dry_run": true, "user_properties": up})
		return err
	}

	for name, up := range map[string]interface{}{
		"one":      []interface{}{prop("org:note", "hello")},
		"two":      []interface{}{prop("custom:backup_policy", "daily"), prop("a:b:c", "")},
		"empty":    []interface{}{},
		"no value": []interface{}{prop("org:note", "")},
	} {
		if err := call(up); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	const secret = "hunter2-do-not-echo"
	bad := map[string]struct {
		up   interface{}
		want string
	}{
		"not a list":       {map[string]interface{}{"key": "a:b", "value": "c"}, "must be an array"},
		"item not object":  {[]interface{}{"org:note=" + secret}, "user_properties[0] must be an object"},
		"no colon":         {[]interface{}{prop("note", secret)}, "user_properties[0].key must be a string in namespace:property form"},
		"key not string":   {[]interface{}{prop(float64(1), "x")}, "user_properties[0].key"},
		"key missing":      {[]interface{}{map[string]interface{}{"value": secret}}, "user_properties[0].key"},
		"value not string": {[]interface{}{prop("org:a", "ok"), prop("org:b", float64(3))}, "user_properties[1].value must be a string"},
		"value missing":    {[]interface{}{map[string]interface{}{"key": "org:a"}}, "user_properties[0].value"},
		"extra field":      {[]interface{}{map[string]interface{}{"key": "org:a", "value": "x", "inherit": true}}, "only the fields key and value"},
	}
	for name, c := range bad {
		err := call(c.up)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error echoes a value: %v", name, err)
		}
	}
}

// TestCreateDatasetEncryptionDescriptionsAreHonest keeps the schema
// consistent with what the tool does: it can create an encrypted dataset
// (encryption=true plus encryption_options), so no description may still
// say it cannot, and each one must point at the working route. It also
// keeps the promises small: the tool never returns or recovers a key.
func TestCreateDatasetEncryptionDescriptionsAreHonest(t *testing.T) {
	tool := NewRegistry(nil, nil).tools["create_dataset"]
	props := tool.Definition.InputSchema["properties"].(map[string]interface{})
	stale := []string{"not effective yet", "cannot yet", "cannot enable encryption", "cannot send", "not supported by this tool", "does not create an encrypted dataset", "independently encrypted"}
	for _, key := range []string{"inherit_encryption", "encryption", "encryption_options"} {
		desc := strings.ToLower(props[key].(map[string]interface{})["description"].(string))
		for _, s := range stale {
			if strings.Contains(desc, s) {
				t.Errorf("%s description still says encryption is unsupported or over-promises (%q): %q", key, s, desc)
			}
		}
		if !strings.Contains(desc, "encryption=true") && key != "encryption" {
			t.Errorf("%s description does not mention encryption=true: %q", key, desc)
		}
	}
	// inherit_encryption points at the supported route and states the conflict.
	inherit := props["inherit_encryption"].(map[string]interface{})["description"].(string)
	for _, want := range []string{"encryption_options", "rejected"} {
		if !strings.Contains(inherit, want) {
			t.Errorf("inherit_encryption description lacks %q: %q", want, inherit)
		}
	}
	sub := props["encryption_options"].(map[string]interface{})["properties"].(map[string]interface{})
	for k, v := range sub {
		d := strings.ToLower(v.(map[string]interface{})["description"].(string))
		for _, s := range stale {
			if strings.Contains(d, s) {
				t.Errorf("encryption_options.%s description still says %q: %q", k, s, d)
			}
		}
	}
	lower := strings.ToLower(tool.Definition.Description)
	if !strings.Contains(lower, "supports native zfs encryption") {
		t.Error("tool description does not say encryption is supported")
	}
	for _, s := range stale {
		if strings.Contains(lower, s) {
			t.Errorf("tool description still says %q", s)
		}
	}
}

func TestDownloadUpdateDoesNotEchoIgnoredArguments(t *testing.T) {
	const wantNote = "train and version were ignored"
	ignoredArgs := func() map[string]interface{} {
		return map[string]interface{}{"train": "NIGHTLY-TRAIN", "version": "99.99-BOGUS"}
	}
	checkNoEcho := func(t *testing.T, text string) {
		t.Helper()
		if strings.Contains(text, "NIGHTLY-TRAIN") || strings.Contains(text, "99.99-BOGUS") {
			t.Errorf("requested train/version echoed:\n%s", text)
		}
	}

	t.Run("already downloaded reports the real version", func(t *testing.T) {
		client, _ := newFakeClientWith(t, map[string]interface{}{
			"update.status": map[string]interface{}{"update_download_progress": map[string]interface{}{"percent": float64(100), "version": "25.10.1"}},
		}, nil)
		out, err := NewRegistry(client, nil).handleDownloadUpdate(client, ignoredArgs())
		if err != nil {
			t.Fatal(err)
		}
		checkNoEcho(t, out)
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["version"] != "25.10.1" || resp["already_downloaded"] != true {
			t.Errorf("response = %v", resp)
		}
		if _, ok := resp["train"]; ok {
			t.Errorf("response has a train: %v", resp)
		}
		if w, _ := resp["warning"].(string); !strings.Contains(w, wantNote) {
			t.Errorf("warning = %q", w)
		}
	})

	t.Run("started download", func(t *testing.T) {
		client, fake := newFakeClientWith(t, map[string]interface{}{"update.status": map[string]interface{}{}, "update.download": 7}, nil)
		mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second})
		out, err := NewRegistry(client, mgr).handleDownloadUpdate(client, ignoredArgs())
		if err != nil {
			t.Fatal(err)
		}
		checkNoEcho(t, out)
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"train", "version"} {
			if _, ok := resp[k]; ok {
				t.Errorf("response has %q: %v", k, resp)
			}
		}
		if w, _ := resp["warning"].(string); !strings.Contains(w, wantNote) {
			t.Errorf("warning = %q", w)
		}
		if resp["job_id"] != float64(7) || len(fake.called("update.download")) != 1 {
			t.Errorf("download not started as expected: %v", resp)
		}
		if params := string(fake.called("update.download")[0].Params); strings.Contains(params, "BOGUS") || strings.Contains(params, "NIGHTLY") {
			t.Errorf("update.download params carry the ignored arguments: %s", params)
		}
		task, err := mgr.Get(resp["task_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := task.Arguments["train"]; ok {
			t.Errorf("task arguments keep train: %v", task.Arguments)
		}
		if _, ok := task.Arguments["version"]; ok {
			t.Errorf("task arguments keep version: %v", task.Arguments)
		}
	})

	t.Run("no ignored arguments, no warning", func(t *testing.T) {
		client, _ := newFakeClientWith(t, map[string]interface{}{
			"update.status": map[string]interface{}{"update_download_progress": map[string]interface{}{"percent": float64(100), "version": "25.10.1"}},
		}, nil)
		out, err := NewRegistry(client, nil).handleDownloadUpdate(client, map[string]interface{}{})
		if err != nil || strings.Contains(out, "warning") {
			t.Errorf("out = %s, err = %v", out, err)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		client, _ := newFakeClient(t)
		res, err := (&downloadUpdateDryRun{}).ExecuteDryRun(client, ignoredArgs())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(res)
		checkNoEcho(t, string(b))
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], wantNote) {
			t.Errorf("warnings = %v", res.Warnings)
		}
		res, err = (&downloadUpdateDryRun{}).ExecuteDryRun(client, map[string]interface{}{})
		if err != nil || len(res.Warnings) != 0 {
			t.Errorf("no-arg dry run: %v, %v", res, err)
		}
	})
}

func TestCreateDatasetEncryptionWarningRequiresEncrypted(t *testing.T) {
	for _, tt := range []struct {
		name     string
		dataset  map[string]interface{}
		wantWarn bool
		wantLoad bool
	}{
		{"encrypted and loaded", map[string]interface{}{"encrypted": true, "key_loaded": true}, true, true},
		{"encrypted, key not loaded", map[string]interface{}{"encrypted": true, "key_loaded": false}, true, false},
		{"not encrypted", map[string]interface{}{"encrypted": false, "key_loaded": true}, false, true},
		{"no encryption fields", map[string]interface{}{}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ds := map[string]interface{}{"id": "tank/x", "name": "tank/x", "type": "FILESYSTEM", "pool": "tank"}
			for k, v := range tt.dataset {
				ds[k] = v
			}
			client, _ := newFakeClientWith(t, map[string]interface{}{"pool.dataset.create": ds}, nil)
			out, err := handleCreateDataset(client, map[string]interface{}{"name": "tank/x"})
			if err != nil {
				t.Fatal(err)
			}
			var resp map[string]interface{}
			if err := json.Unmarshal([]byte(out), &resp); err != nil {
				t.Fatal(err)
			}
			if _, got := resp["encryption_warning"]; got != tt.wantWarn {
				t.Errorf("encryption_warning present = %v, want %v (%s)", got, tt.wantWarn, out)
			}
			if _, got := resp["key_loaded"]; got != tt.wantLoad {
				t.Errorf("key_loaded present = %v, want %v", got, tt.wantLoad)
			}
		})
	}
}

func TestCreateDatasetUserPropertiesDescriptionNamesManagedKeys(t *testing.T) {
	r := NewRegistry(nil, nil)
	props := r.tools["create_dataset"].Definition.InputSchema["properties"].(map[string]interface{})
	desc := props["user_properties"].(map[string]interface{})["description"].(string)
	for _, want := range []string{"org.freenas:description", "org.freenas:quota_warning", "org.freenas:quota_critical",
		"org.freenas:refquota_warning", "org.freenas:refquota_critical", "org.truenas:managedby", "lowercase", "must not repeat"} {
		if !strings.Contains(desc, want) {
			t.Errorf("user_properties description missing %q", want)
		}
	}
	opts := props["encryption_options"].(map[string]interface{})["properties"].(map[string]interface{})
	for _, k := range []string{"generate_key", "passphrase"} {
		d := opts[k].(map[string]interface{})["description"].(string)
		if strings.HasPrefix(d, "Not effective yet") || strings.Contains(d, "recommended") {
			t.Errorf("encryption_options.%s description = %q", k, d)
		}
	}
}

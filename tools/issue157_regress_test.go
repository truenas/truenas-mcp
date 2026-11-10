package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// Official 25.10 catalog storage-slot shapes, matching middleware
// construct_schema fixtures (type, ix_volume_config.dataset_name,
// host_path_config.path). Used as app.config for installed official apps.

func issue157IxVolume(datasetName string) map[string]interface{} {
	return map[string]interface{}{
		"type": "ix_volume",
		"ix_volume_config": map[string]interface{}{
			"dataset_name": datasetName,
		},
	}
}

func issue157IxVolumeConfig() map[string]interface{} {
	return map[string]interface{}{
		"storage": map[string]interface{}{
			"config":             issue157IxVolume("config"),
			"data":               issue157IxVolume("data"),
			"additional_storage": []interface{}{},
		},
	}
}

func issue157HostPathConfig() map[string]interface{} {
	return cloneValues(validHostPathValues())
}

func issue157InstalledApp(custom bool) map[string]interface{} {
	app := hostPathInstalledApp()
	app["custom_app"] = custom
	return app
}

func issue157Registry(t *testing.T, currentConfig map[string]interface{}, customApp bool) (*Registry, *fakeMiddleware) {
	t.Helper()
	existing := map[string]bool{
		"tank/apps/jellyfin/config": true,
		"tank/apps/jellyfin/data":   true,
		"tank/apps/jellyfin/media":  true,
	}
	if currentConfig == nil {
		currentConfig = map[string]interface{}{}
	}
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"catalog.get_app_details": hostPathCatalogDetails(),
			"pool.query":              []interface{}{map[string]interface{}{"name": "tank", "status": "ONLINE"}},
			"app.query":               []interface{}{issue157InstalledApp(customApp)},
			"app.config":              currentConfig,
			"app.create":              101,
			"app.update":              102,
		},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method != "pool.dataset.query" {
				return nil, nil, false
			}
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
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake
}

func callUpdateApp(t *testing.T, r *Registry, values map[string]interface{}, extra map[string]interface{}) (string, error) {
	t.Helper()
	args := map[string]interface{}{"app_name": "jellyfin", "values": values}
	for k, v := range extra {
		args[k] = v
	}
	if dry, _ := extra["dry_run"].(bool); dry {
		return r.CallTool(t.Context(), "update_app", args)
	}
	if plan := r.tools["update_app"].Plan; plan != nil {
		if _, err := plan(t.Context(), r.client, args); err != nil {
			return "", err
		}
	}
	return r.CallTool(t.Context(), "update_app", approve(t, r, "update_app", args))
}

func updateAppPayload(t *testing.T, fake *fakeMiddleware) map[string]interface{} {
	t.Helper()
	calls := fake.called("app.update")
	if len(calls) != 1 {
		t.Fatalf("app.update called %d times, want 1", len(calls))
	}
	var params []interface{}
	if err := json.Unmarshal(calls[0].Params, &params); err != nil {
		t.Fatalf("decode app.update params: %v", err)
	}
	if len(params) < 2 {
		t.Fatalf("app.update params %v, want [app_name, options]", params)
	}
	opts, ok := params[1].(map[string]interface{})
	if !ok {
		t.Fatalf("app.update options %T, want object", params[1])
	}
	values, ok := opts["values"].(map[string]interface{})
	if !ok {
		t.Fatalf("app.update values %T, want object", opts["values"])
	}
	if _, ok := opts["allow_storage_change"]; ok {
		t.Errorf("allow_storage_change was forwarded to app.update: %v", opts)
	}
	return values
}

func storageOf(values map[string]interface{}) map[string]interface{} {
	s, _ := values["storage"].(map[string]interface{})
	return s
}

func entryType(storage map[string]interface{}, slot string) string {
	e, _ := storage[slot].(map[string]interface{})
	typ, _ := e["type"].(string)
	return typ
}

func ixDataset(storage map[string]interface{}, slot string) string {
	e, _ := storage[slot].(map[string]interface{})
	cfg, _ := e["ix_volume_config"].(map[string]interface{})
	name, _ := cfg["dataset_name"].(string)
	return name
}

func hostPath(storage map[string]interface{}, slot string) string {
	e, _ := storage[slot].(map[string]interface{})
	cfg, _ := e["host_path_config"].(map[string]interface{})
	p, _ := cfg["path"].(string)
	return p
}

func hasStorageChangeWarning(text, slot, oldLoc, newLoc string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, strings.ToLower(slot)) &&
		strings.Contains(low, strings.ToLower(oldLoc)) &&
		strings.Contains(low, strings.ToLower(newLoc)) &&
		strings.Contains(low, "not migrated")
}

func TestIssue157UpdateAppStorage(t *testing.T) {
	t.Run("omit storage leaves ix_volume unchanged", func(t *testing.T) {
		r, fake := issue157Registry(t, issue157IxVolumeConfig(), false)
		out, err := callUpdateApp(t, r, map[string]interface{}{"TZ": "UTC"}, nil)
		if err != nil {
			t.Fatalf("omitting storage was refused: %v\nout=%s", err, out)
		}
		values := updateAppPayload(t, fake)
		if _, ok := values["storage"]; ok {
			t.Errorf("omitted storage was added to the app.update payload: %v", values["storage"])
		}
		if values["TZ"] != "UTC" {
			t.Errorf("TZ=%v, want UTC", values["TZ"])
		}
	})

	t.Run("unchanged ix_volume is not converted", func(t *testing.T) {
		current := issue157IxVolumeConfig()
		r, fake := issue157Registry(t, current, false)
		out, err := callUpdateApp(t, r, cloneValues(current), nil)
		if err != nil {
			t.Fatalf("identical ix_volume storage was refused: %v\nout=%s", err, out)
		}
		storage := storageOf(updateAppPayload(t, fake))
		if entryType(storage, "config") != "ix_volume" || ixDataset(storage, "config") != "config" {
			t.Errorf("storage.config was converted: %v", storage["config"])
		}
		if entryType(storage, "data") != "ix_volume" || ixDataset(storage, "data") != "data" {
			t.Errorf("storage.data was converted: %v", storage["data"])
		}
	})

	t.Run("type change is blocked without allow_storage_change", func(t *testing.T) {
		r, fake := issue157Registry(t, issue157IxVolumeConfig(), false)
		values := cloneValues(issue157IxVolumeConfig())
		setStoragePath(values, "config", "/mnt/tank/apps/jellyfin/config")
		out, err := callUpdateApp(t, r, values, nil)
		if err == nil {
			t.Fatalf("type change was accepted without the flag: %s", out)
		}
		msg := err.Error()
		if !strings.Contains(msg, "allow_storage_change") {
			t.Errorf("error %q, want allow_storage_change", err)
		}
		if !hasStorageChangeWarning(msg, "config", "config", "/mnt/tank/apps/jellyfin/config") &&
			!hasStorageChangeWarning(msg, "storage.config", "ix_volume", "/mnt/tank/apps/jellyfin/config") {
			t.Errorf("error %q, want the slot, old and new locations, and that data is not migrated", err)
		}
		if !strings.Contains(strings.ToLower(msg), "not migrated") {
			t.Errorf("error %q, want it to state that data is not migrated", err)
		}
		if n := len(fake.called("app.update")); n != 0 {
			t.Errorf("app.update called %d times", n)
		}
	})

	t.Run("type change with allow_storage_change is warned and sent", func(t *testing.T) {
		current := issue157IxVolumeConfig()
		r, fake := issue157Registry(t, current, false)
		values := cloneValues(current)
		setStoragePath(values, "config", "/mnt/tank/apps/jellyfin/config")

		preview, err := callUpdateApp(t, r, cloneValues(values), map[string]interface{}{
			"allow_storage_change": true,
			"dry_run":              true,
		})
		if err != nil {
			t.Fatalf("dry_run with the flag: %v\n%s", err, preview)
		}
		if !hasStorageChangeWarning(preview, "config", "config", "/mnt/tank/apps/jellyfin/config") &&
			!strings.Contains(strings.ToLower(preview), "not migrated") {
			t.Errorf("preview %s, want the slot, old and new locations, and that data is not migrated", preview)
		}
		if !strings.Contains(preview, "/mnt/tank/apps/jellyfin/config") {
			t.Errorf("preview %s, want the new host path", preview)
		}

		// Confirmation prompt (no token) must carry the same warning.
		promptOut, promptErr := r.CallTool(t.Context(), "update_app", map[string]interface{}{
			"app_name":             "jellyfin",
			"values":               cloneValues(values),
			"allow_storage_change": true,
		})
		if promptErr != nil && !strings.Contains(promptErr.Error(), "confirmation") && !strings.Contains(promptOut, "Warnings") {
			t.Fatalf("confirmation preview: %v\n%s", promptErr, promptOut)
		}
		combined := promptOut
		if promptErr != nil {
			combined += promptErr.Error()
		}
		if !strings.Contains(strings.ToLower(combined), "not migrated") {
			t.Errorf("confirmation prompt %s, want the data-not-migrated warning", combined)
		}
		if n := len(fake.called("app.update")); n != 0 {
			t.Errorf("preview called app.update %d times", n)
		}

		out, err := callUpdateApp(t, r, values, map[string]interface{}{"allow_storage_change": true})
		if err != nil {
			t.Fatalf("type change with the flag: %v\nout=%s", err, out)
		}
		storage := storageOf(updateAppPayload(t, fake))
		if entryType(storage, "config") != "host_path" || hostPath(storage, "config") != "/mnt/tank/apps/jellyfin/config" {
			t.Errorf("storage.config=%v, want the new host_path", storage["config"])
		}
		if entryType(storage, "data") != "ix_volume" || ixDataset(storage, "data") != "data" {
			t.Errorf("untouched storage.data was converted: %v", storage["data"])
		}
	})

	t.Run("host-path change is blocked without allow_storage_change", func(t *testing.T) {
		r, fake := issue157Registry(t, issue157HostPathConfig(), false)
		values := cloneValues(issue157HostPathConfig())
		setStoragePath(values, "data", "/mnt/tank/apps/jellyfin/media")
		out, err := callUpdateApp(t, r, values, nil)
		if err == nil {
			t.Fatalf("host-path change was accepted without the flag: %s", out)
		}
		msg := err.Error()
		if !strings.Contains(msg, "allow_storage_change") {
			t.Errorf("error %q, want allow_storage_change", err)
		}
		if !strings.Contains(msg, "/mnt/tank/apps/jellyfin/data") || !strings.Contains(msg, "/mnt/tank/apps/jellyfin/media") {
			t.Errorf("error %q, want the old and new host paths", err)
		}
		if !strings.Contains(strings.ToLower(msg), "not migrated") {
			t.Errorf("error %q, want it to state that data is not migrated", err)
		}
		if n := len(fake.called("app.update")); n != 0 {
			t.Errorf("app.update called %d times", n)
		}
	})

	t.Run("host-path change with allow_storage_change is warned and sent", func(t *testing.T) {
		r, fake := issue157Registry(t, issue157HostPathConfig(), false)
		values := cloneValues(issue157HostPathConfig())
		setStoragePath(values, "data", "/mnt/tank/apps/jellyfin/media")
		preview, err := callUpdateApp(t, r, cloneValues(values), map[string]interface{}{
			"allow_storage_change": true,
			"dry_run":              true,
		})
		if err != nil {
			t.Fatalf("dry_run with the flag: %v\n%s", err, preview)
		}
		if !strings.Contains(preview, "/mnt/tank/apps/jellyfin/data") || !strings.Contains(preview, "/mnt/tank/apps/jellyfin/media") {
			t.Errorf("preview %s, want the old and new host paths", preview)
		}
		if !strings.Contains(strings.ToLower(preview), "not migrated") {
			t.Errorf("preview %s, want it to state that data is not migrated", preview)
		}
		out, err := callUpdateApp(t, r, values, map[string]interface{}{"allow_storage_change": true})
		if err != nil {
			t.Fatalf("host-path change with the flag: %v\nout=%s", err, out)
		}
		storage := storageOf(updateAppPayload(t, fake))
		if hostPath(storage, "data") != "/mnt/tank/apps/jellyfin/media" {
			t.Errorf("storage.data=%v, want the new host path", storage["data"])
		}
		if hostPath(storage, "config") != "/mnt/tank/apps/jellyfin/config" {
			t.Errorf("untouched storage.config changed: %v", storage["config"])
		}
	})

	t.Run("new entry must be host_path under an existing dataset", func(t *testing.T) {
		current := issue157IxVolumeConfig()

		t.Run("ix_volume refused", func(t *testing.T) {
			r, fake := issue157Registry(t, current, false)
			values := cloneValues(current)
			values["storage"].(map[string]interface{})["additional_storage"] = []interface{}{
				issue157IxVolume("extra"),
			}
			out, err := callUpdateApp(t, r, values, nil)
			if err == nil {
				t.Fatalf("new ix_volume entry was accepted: %s", out)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "host_path") {
				t.Errorf("error %q, want it to require host_path", err)
			}
			if n := len(fake.called("app.update")); n != 0 {
				t.Errorf("app.update called %d times", n)
			}
		})

		t.Run("missing dataset refused", func(t *testing.T) {
			r, fake := issue157Registry(t, current, false)
			values := cloneValues(current)
			values["storage"].(map[string]interface{})["additional_storage"] = []interface{}{
				hostPathVolume("/mnt/tank/apps/jellyfin/missing"),
			}
			out, err := callUpdateApp(t, r, values, map[string]interface{}{})
			if err == nil {
				t.Fatalf("new host_path on a missing dataset was accepted: %s", out)
			}
			msg := strings.ToLower(err.Error())
			if !strings.Contains(msg, "missing") && !strings.Contains(msg, "must exist") {
				t.Errorf("error %q, want a missing-dataset refusal", err)
			}
			if n := len(fake.called("app.update")); n != 0 {
				t.Errorf("app.update called %d times", n)
			}
		})

		t.Run("host_path on an existing dataset accepted", func(t *testing.T) {
			r, fake := issue157Registry(t, current, false)
			values := cloneValues(current)
			values["storage"].(map[string]interface{})["additional_storage"] = []interface{}{
				hostPathVolume("/mnt/tank/apps/jellyfin/media"),
			}
			out, err := callUpdateApp(t, r, values, nil)
			if err != nil {
				t.Fatalf("new host_path entry was refused: %v\nout=%s", err, out)
			}
			storage := storageOf(updateAppPayload(t, fake))
			arr, _ := storage["additional_storage"].([]interface{})
			if len(arr) != 1 {
				t.Fatalf("additional_storage=%v, want one host_path entry", storage["additional_storage"])
			}
			item, _ := arr[0].(map[string]interface{})
			if typ, _ := item["type"].(string); typ != "host_path" {
				t.Errorf("new entry type=%q", typ)
			}
			if entryType(storage, "config") != "ix_volume" {
				t.Errorf("existing ix_volume config was converted: %v", storage["config"])
			}
		})
	})

	t.Run("partial storage object is filled from the current config", func(t *testing.T) {
		current := issue157IxVolumeConfig()
		r, fake := issue157Registry(t, current, false)
		// Only replace data; config and additional_storage are omitted.
		partial := map[string]interface{}{
			"storage": map[string]interface{}{
				"data": hostPathVolume("/mnt/tank/apps/jellyfin/data"),
			},
		}
		out, err := callUpdateApp(t, r, partial, map[string]interface{}{"allow_storage_change": true})
		if err != nil {
			t.Fatalf("partial storage update: %v\nout=%s", err, out)
		}
		payload := updateAppPayload(t, fake)
		storage := storageOf(payload)
		if entryType(storage, "config") != "ix_volume" || ixDataset(storage, "config") != "config" {
			t.Errorf("omitted storage.config was not filled from current: %v", storage["config"])
		}
		if entryType(storage, "data") != "host_path" || hostPath(storage, "data") != "/mnt/tank/apps/jellyfin/data" {
			t.Errorf("storage.data=%v, want the provided host_path", storage["data"])
		}
		if _, ok := storage["additional_storage"]; !ok {
			t.Errorf("omitted additional_storage was not filled from current: %v", storage)
		}
	})

	t.Run("install_app still requires host_path for every entry", func(t *testing.T) {
		r, fake := hostPathRegistry(t)
		values := cloneValues(validHostPathValues())
		delete(values["storage"].(map[string]interface{}), "config")
		out, err := callHostPathTool(t, r, "install_app", values)
		if err == nil {
			t.Fatalf("install_app accepted omitted storage: %s", out)
		}
		if n := len(fake.called("app.create")); n != 0 {
			t.Errorf("app.create called %d times", n)
		}

		r, fake = hostPathRegistry(t)
		ix := cloneValues(validHostPathValues())
		ix["storage"].(map[string]interface{})["config"] = issue157IxVolume("config")
		out, err = callHostPathTool(t, r, "install_app", ix)
		if err == nil {
			t.Fatalf("install_app accepted ix_volume: %s", out)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "host_path") && !strings.Contains(err.Error(), "ix_volume") {
			t.Errorf("error %q, want a host_path requirement", err)
		}
		if n := len(fake.called("app.create")); n != 0 {
			t.Errorf("app.create called %d times", n)
		}
	})

	t.Run("custom apps skip catalog-slot storage rules", func(t *testing.T) {
		// custom_app is a separate validate_payload path; catalog-slot
		// comparison and host_path rules must not be applied.
		r, fake := issue157Registry(t, map[string]interface{}{"name": "jellyfin"}, true)
		values := map[string]interface{}{
			"storage": map[string]interface{}{
				"config": issue157IxVolume("config"),
			},
		}
		out, err := callUpdateApp(t, r, values, nil)
		if err != nil {
			t.Fatalf("custom app update was refused by catalog-slot rules: %v\nout=%s", err, out)
		}
		payload := updateAppPayload(t, fake)
		if entryType(storageOf(payload), "config") != "ix_volume" {
			t.Errorf("custom app values were rewritten: %v", payload)
		}
	})
}

// Official 25.10 additional_storage share and host_path shapes from
// catalog questions (jellyfin): nfs_config.server/path, cifs_config
// server/path/username/password/domain, and top-level read_only.

const issue157CIFSPassword = "review-cifs-secret"

func issue157NFSEntry(server, sharePath, mountPath string) map[string]interface{} {
	return map[string]interface{}{
		"type":       "nfs",
		"read_only":  false,
		"mount_path": mountPath,
		"nfs_config": map[string]interface{}{
			"server": server,
			"path":   sharePath,
		},
	}
}

func issue157CIFSEntry(server, sharePath, mountPath string) map[string]interface{} {
	return map[string]interface{}{
		"type":       "cifs",
		"read_only":  false,
		"mount_path": mountPath,
		"cifs_config": map[string]interface{}{
			"server":   server,
			"path":     sharePath,
			"username": "media",
			"password": issue157CIFSPassword,
			"domain":   "WORKGROUP",
		},
	}
}

func issue157HostPathWithReadOnly(p string, readOnly bool) map[string]interface{} {
	return map[string]interface{}{
		"type":      "host_path",
		"read_only": readOnly,
		"host_path_config": map[string]interface{}{
			"path":       p,
			"acl_enable": false,
		},
	}
}

func issue157ConfigWithAdditional(entry map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"storage": map[string]interface{}{
			"config":             issue157IxVolume("config"),
			"data":               issue157IxVolume("data"),
			"additional_storage": []interface{}{entry},
		},
	}
}

func previewWarnings(preview string) []string {
	var out struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(preview), &out); err != nil {
		return nil
	}
	return out.Warnings
}

func warningMentionsReadOnlyChange(text string, from, to bool) bool {
	fromS, toS := "true", "false"
	if !from {
		fromS = "false"
	}
	if to {
		toS = "true"
	}
	warnings := previewWarnings(text)
	if len(warnings) == 0 {
		// Confirmation / elicitation text lists plan warnings after "Warnings:".
		warnings = []string{text}
	}
	for _, w := range warnings {
		low := strings.ToLower(w)
		if !strings.Contains(low, "read_only") {
			continue
		}
		// Require a warning that names the field and both values, not just
		// a raw values dump that happens to contain the keys.
		if strings.Contains(low, fromS) && strings.Contains(low, toS) &&
			(strings.Contains(low, "changing") || strings.Contains(low, "from")) {
			return true
		}
	}
	return false
}

func TestIssue157ReviewStorageNits(t *testing.T) {
	t.Run("nfs path change requires allow_storage_change", func(t *testing.T) {
		current := issue157ConfigWithAdditional(issue157NFSEntry("files.example.invalid", "/export/media", "/mnt/nfs"))
		r, fake := issue157Registry(t, current, false)
		values := cloneValues(current)
		values["storage"].(map[string]interface{})["additional_storage"] = []interface{}{
			issue157NFSEntry("files.example.invalid", "/export/other", "/mnt/nfs"),
		}
		out, err := callUpdateApp(t, r, values, nil)
		if err == nil {
			t.Fatalf("NFS path change was accepted without the flag: %s", out)
		}
		msg := err.Error()
		if !strings.Contains(msg, "allow_storage_change") {
			t.Errorf("error %q, want allow_storage_change", err)
		}
		if !strings.Contains(msg, "/export/media") || !strings.Contains(msg, "/export/other") {
			t.Errorf("error %q, want the old and new NFS paths", err)
		}
		if !strings.Contains(msg, "files.example.invalid") {
			t.Errorf("error %q, want the NFS server", err)
		}
		if !strings.Contains(strings.ToLower(msg), "not migrated") {
			t.Errorf("error %q, want it to state that data is not migrated", err)
		}
		if n := len(fake.called("app.update")); n != 0 {
			t.Errorf("app.update called %d times", n)
		}

		preview, err := callUpdateApp(t, r, cloneValues(values), map[string]interface{}{
			"allow_storage_change": true,
			"dry_run":              true,
		})
		if err != nil {
			t.Fatalf("dry_run with the flag: %v\n%s", err, preview)
		}
		if !strings.Contains(preview, "/export/media") || !strings.Contains(preview, "/export/other") {
			t.Errorf("preview %s, want the old and new NFS paths", preview)
		}
		if !strings.Contains(strings.ToLower(preview), "not migrated") {
			t.Errorf("preview %s, want it to state that data is not migrated", preview)
		}
	})

	t.Run("cifs server change requires allow_storage_change", func(t *testing.T) {
		current := issue157ConfigWithAdditional(issue157CIFSEntry("files.example.invalid", "/media", "/mnt/smb"))
		r, fake := issue157Registry(t, current, false)
		values := cloneValues(current)
		values["storage"].(map[string]interface{})["additional_storage"] = []interface{}{
			issue157CIFSEntry("other.example.invalid", "/media", "/mnt/smb"),
		}
		out, err := callUpdateApp(t, r, values, nil)
		if err == nil {
			t.Fatalf("CIFS server change was accepted without the flag: %s", out)
		}
		msg := err.Error()
		if !strings.Contains(msg, "allow_storage_change") {
			t.Errorf("error %q, want allow_storage_change", err)
		}
		if !strings.Contains(msg, "files.example.invalid") || !strings.Contains(msg, "other.example.invalid") {
			t.Errorf("error %q, want the old and new CIFS servers", err)
		}
		if strings.Contains(msg, issue157CIFSPassword) {
			t.Errorf("error %q leaked the CIFS password", err)
		}
		if !strings.Contains(strings.ToLower(msg), "not migrated") {
			t.Errorf("error %q, want it to state that data is not migrated", err)
		}
		if n := len(fake.called("app.update")); n != 0 {
			t.Errorf("app.update called %d times", n)
		}
	})

	t.Run("read_only is preserved on a same-location nested partial", func(t *testing.T) {
		current := map[string]interface{}{
			"storage": map[string]interface{}{
				"config":             issue157IxVolume("config"),
				"data":               issue157HostPathWithReadOnly("/mnt/tank/apps/jellyfin/data", true),
				"additional_storage": []interface{}{},
			},
		}
		r, fake := issue157Registry(t, current, false)
		partial := map[string]interface{}{
			"storage": map[string]interface{}{
				"data": map[string]interface{}{
					"type": "host_path",
					"host_path_config": map[string]interface{}{
						"path": "/mnt/tank/apps/jellyfin/data",
					},
				},
			},
		}
		out, err := callUpdateApp(t, r, partial, nil)
		if err != nil {
			t.Fatalf("same-location nested partial was refused: %v\nout=%s", err, out)
		}
		payload := updateAppPayload(t, fake)
		data, _ := storageOf(payload)["data"].(map[string]interface{})
		if ro, ok := data["read_only"].(bool); !ok || !ro {
			t.Errorf("storage.data=%v, want read_only:true to be preserved from the current config", data)
		}
		if entryType(storageOf(payload), "config") != "ix_volume" {
			t.Errorf("omitted storage.config was not filled: %v", storageOf(payload)["config"])
		}
		cfg, _ := data["host_path_config"].(map[string]interface{})
		if path, _ := cfg["path"].(string); path != "/mnt/tank/apps/jellyfin/data" {
			t.Errorf("storage.data path=%v, want the provided path", cfg)
		}
	})

	t.Run("read_only flip is warned without allow_storage_change", func(t *testing.T) {
		current := map[string]interface{}{
			"storage": map[string]interface{}{
				"config":             issue157IxVolume("config"),
				"data":               issue157HostPathWithReadOnly("/mnt/tank/apps/jellyfin/data", true),
				"additional_storage": []interface{}{},
			},
		}
		r, fake := issue157Registry(t, current, false)
		values := cloneValues(current)
		values["storage"].(map[string]interface{})["data"] = issue157HostPathWithReadOnly("/mnt/tank/apps/jellyfin/data", false)

		preview, err := callUpdateApp(t, r, cloneValues(values), map[string]interface{}{"dry_run": true})
		if err != nil {
			t.Fatalf("read_only flip dry_run was refused: %v\n%s", err, preview)
		}
		if !warningMentionsReadOnlyChange(preview, true, false) {
			t.Errorf("preview warnings are %v, want a plan warning of read_only true to false", previewWarnings(preview))
		}

		promptOut, promptErr := r.CallTool(t.Context(), "update_app", map[string]interface{}{
			"app_name": "jellyfin",
			"values":   cloneValues(values),
		})
		combined := promptOut
		if promptErr != nil {
			combined += promptErr.Error()
		}
		if !warningMentionsReadOnlyChange(combined, true, false) {
			t.Errorf("confirmation prompt %s, want a warning of read_only true to false", combined)
		}

		if n := len(fake.called("app.update")); n != 0 {
			t.Errorf("preview called app.update %d times", n)
		}

		out, err := callUpdateApp(t, r, values, nil)
		if err != nil {
			t.Fatalf("read_only flip without the flag was refused: %v\nout=%s", err, out)
		}
		data, _ := storageOf(updateAppPayload(t, fake))["data"].(map[string]interface{})
		if ro, ok := data["read_only"].(bool); !ok || ro {
			t.Errorf("storage.data=%v, want read_only:false", data)
		}
	})
}

package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
)

// plexAppConfig2510 is a 25.10 app.config / get_current_app_config values
// object: user-specified YAML, not the app.query row. Catalog storage
// follows the official apps questions.yaml shape used on origin/r2510
// (TS-25.10.8) and in middleware plugins/apps tests
// (test_list_with_show_if_conditions, test_nested_show_if): storage.<slot>
// or additional_storage entries with type host_path + host_path_config.path,
// or type ix_volume + ix_volume_config.dataset_name. app.query does not
// include this object unless extra.retrieve_config is true.
func plexAppConfig2510() map[string]interface{} {
	return map[string]interface{}{
		"TZ": "America/Los_Angeles",
		"run_as": map[string]interface{}{
			"user":  568.0,
			"group": 568.0,
		},
		"network": map[string]interface{}{
			"web_port": 32400.0,
		},
		"storage": map[string]interface{}{
			"config": map[string]interface{}{
				"type": "host_path",
				"host_path_config": map[string]interface{}{
					"path":       "/mnt/tank/apps/plex/config",
					"acl_enable": false,
				},
			},
			"data": map[string]interface{}{
				"type": "ix_volume",
				"ix_volume_config": map[string]interface{}{
					"dataset_name": "data",
				},
			},
			"additional_storage": []interface{}{
				map[string]interface{}{
					"type": "host_path",
					"host_path_config": map[string]interface{}{
						"path": "/mnt/tank/media",
					},
				},
				map[string]interface{}{
					"type": "ix_volume",
					"ix_volume_config": map[string]interface{}{
						"dataset_name": "scratch",
					},
				},
				map[string]interface{}{
					"type": "host_path",
					"host_path_config": map[string]interface{}{
						"path": "/mnt/tank/transcode",
					},
				},
			},
		},
		"resources": map[string]interface{}{
			"limits": map[string]interface{}{
				"cpus":   2.0,
				"memory": 4096.0,
			},
		},
		// A values key that must never appear in the delete preview.
		"password": "super-secret-app-password",
	}
}

// plexIxVolumeOnlyConfig2510 is a 25.10 values object whose storage slots
// are all ix_volume. Those are not host paths; app.delete leaves the
// datasets in place unless options.remove_ix_volumes is set (this tool
// does not send that option).
func plexIxVolumeOnlyConfig2510() map[string]interface{} {
	return map[string]interface{}{
		"TZ": "UTC",
		"storage": map[string]interface{}{
			"config": map[string]interface{}{
				"type": "ix_volume",
				"ix_volume_config": map[string]interface{}{
					"dataset_name": "config",
				},
			},
			"data": map[string]interface{}{
				"type": "ix_volume",
				"ix_volume_config": map[string]interface{}{
					"dataset_name": "data",
				},
			},
		},
	}
}

const staleQueryHostPath = "/mnt/stale/from-query-without-retrieve_config"

func deleteAppPreviewRegistry(t *testing.T, config interface{}, queryApp map[string]interface{}, configErr map[string]interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	if queryApp == nil {
		queryApp = appEntry("plex", "RUNNING")
		queryApp["version"] = "1.2.3"
	}
	fake := &fakeMiddleware{
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			switch method {
			case "app.query":
				return []interface{}{queryApp}, nil, true
			case "app.config":
				if configErr != nil {
					return nil, configErr, true
				}
				return config, nil, true
			}
			return nil, nil, false
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	return NewRegistry(client, nil), fake
}

func deleteAppDryRun(t *testing.T, r *Registry) string {
	t.Helper()
	out, err := r.CallTool(t.Context(), "delete_app", map[string]interface{}{
		"app_name": "plex", "remove_images": true, "dry_run": true,
	})
	if err != nil {
		t.Fatalf("delete_app dry run: %v\n%s", err, out)
	}
	return out
}

func deleteAppPreviewState(t *testing.T, out string) (state map[string]interface{}, warnings []string) {
	t.Helper()
	var preview struct {
		CurrentState map[string]interface{} `json:"current_state"`
		Warnings     []string               `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		t.Fatalf("preview is not JSON: %v\n%s", err, out)
	}
	if preview.CurrentState == nil {
		t.Fatalf("preview has no current_state: %s", out)
	}
	return preview.CurrentState, preview.Warnings
}

func storagePathsOf(t *testing.T, state map[string]interface{}) []string {
	t.Helper()
	raw, ok := state["storage_paths"]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("storage_paths is %T, want a list: %v", raw, raw)
	}
	var paths []string
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("storage_paths entry is %T, want string: %v", item, item)
		}
		paths = append(paths, s)
	}
	return paths
}

func assertAppConfigCalled(t *testing.T, fake *fakeMiddleware, appName string) {
	t.Helper()
	calls := fake.called("app.config")
	if len(calls) == 0 {
		t.Fatal("delete_app preview did not call app.config")
	}
	want, _ := json.Marshal([]interface{}{appName})
	if !jsonBytesEqual(t, calls[0].Params, want) {
		t.Errorf("app.config params %s, want %s", calls[0].Params, want)
	}
}

func TestDeleteAppPreviewListsHostPathStorageFromAppConfig(t *testing.T) {
	// app.query is the 25.10 default (no extra.retrieve_config), so the
	// row has no config. The real values come from app.config.
	r, fake := deleteAppPreviewRegistry(t, plexAppConfig2510(), nil, nil)
	out := deleteAppDryRun(t, r)
	assertAppConfigCalled(t, fake, "plex")

	state, warnings := deleteAppPreviewState(t, out)
	got := storagePathsOf(t, state)
	want := []string{
		"/mnt/tank/apps/plex/config",
		"/mnt/tank/media",
		"/mnt/tank/transcode",
	}
	if len(got) != len(want) {
		t.Fatalf("storage_paths = %v, want %v\n%s", got, want, out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("storage_paths = %v, want %v (sorted host paths only)", got, want)
		}
	}
	if strings.Contains(out, staleQueryHostPath) {
		t.Errorf("preview used a path that was not in app.config:\n%s", out)
	}
	if strings.Contains(out, "super-secret-app-password") {
		t.Errorf("preview leaked a values secret:\n%s", out)
	}
	joined := strings.Join(warnings, "\n")
	for _, path := range want {
		if !strings.Contains(joined, path) {
			t.Errorf("warnings should list preserved path %s:\n%s", path, joined)
		}
	}
	// ix_volume dataset names are not host paths and must not be invented
	// as ones. The names themselves may appear in other fields; the path
	// list and the preserved-path warnings must not treat them as paths.
	for _, name := range []string{"data", "scratch"} {
		for _, path := range got {
			if strings.Contains(path, name) && !strings.Contains(path, "media") {
				// "data" is also a substring of no host path in want.
				if path != "/mnt/tank/media" {
					t.Errorf("storage_paths invented an ix_volume path: %s", path)
				}
			}
		}
		for _, w := range warnings {
			if strings.Contains(w, "/mnt/") && strings.HasSuffix(w, name) {
				t.Errorf("warnings invented an ix_volume path: %s", w)
			}
		}
	}
}

func TestDeleteAppPreviewIgnoresQueryRowConfigWithoutRetrieveConfig(t *testing.T) {
	// A misleading app.query row that looks like the old persistence
	// layout. 25.10 app.query does not return this unless
	// extra.retrieve_config is set, and even then config is the values
	// object, not persistence.hostPath. The preview must read app.config.
	row := appEntry("plex", "STOPPED")
	row["version"] = "1.2.3"
	row["config"] = map[string]interface{}{
		"persistence": map[string]interface{}{
			"data": map[string]interface{}{"hostPath": staleQueryHostPath},
		},
	}
	r, fake := deleteAppPreviewRegistry(t, plexAppConfig2510(), row, nil)
	out := deleteAppDryRun(t, r)
	assertAppConfigCalled(t, fake, "plex")

	state, _ := deleteAppPreviewState(t, out)
	got := storagePathsOf(t, state)
	for _, path := range got {
		if path == staleQueryHostPath {
			t.Fatalf("preview used config from app.query without retrieve_config: %v", got)
		}
	}
	want := map[string]bool{
		"/mnt/tank/apps/plex/config": true,
		"/mnt/tank/media":            true,
		"/mnt/tank/transcode":        true,
	}
	if len(got) != 3 {
		t.Fatalf("storage_paths = %v, want the three app.config host paths", got)
	}
	for _, path := range got {
		if !want[path] {
			t.Errorf("unexpected storage path %q in %v", path, got)
		}
	}
}

func TestDeleteAppPreviewIxVolumeOnlyHasNoHostPaths(t *testing.T) {
	r, fake := deleteAppPreviewRegistry(t, plexIxVolumeOnlyConfig2510(), nil, nil)
	out := deleteAppDryRun(t, r)
	assertAppConfigCalled(t, fake, "plex")

	state, _ := deleteAppPreviewState(t, out)
	got := storagePathsOf(t, state)
	if len(got) != 0 {
		t.Fatalf("ix_volume-only storage_paths = %v, want none (do not invent host paths)", got)
	}
	if strings.Contains(out, "/mnt/") {
		t.Errorf("ix_volume-only preview invented a host path:\n%s", out)
	}
}

func TestDeleteAppPreviewFailsClosedWhenAppConfigFails(t *testing.T) {
	r, fake := deleteAppPreviewRegistry(t, nil, nil, map[string]interface{}{
		"code":    1.0,
		"message": "app not found",
	})
	out, err := r.CallTool(t.Context(), "delete_app", map[string]interface{}{
		"app_name": "plex", "dry_run": true,
	})
	if err == nil {
		t.Fatalf("delete_app dry run succeeded when app.config failed:\n%s", out)
	}
	if strings.Contains(err.Error(), "internal error") {
		t.Errorf("app.config failure panicked: %v", err)
	}
	if !strings.Contains(err.Error(), "app.config") && !strings.Contains(err.Error(), "app config") {
		t.Errorf("error should mention the failed app.config read: %v", err)
	}
	if n := len(fake.called("app.delete")); n != 0 {
		t.Errorf("failed preview sent app.delete %d times", n)
	}
	var reply *mcp.ReplyError
	if errors.As(err, &reply) && strings.Contains(reply.Reply, "confirmation_token") {
		t.Errorf("failed preview issued a token: %s", reply.Reply)
	}
}

func TestDeleteAppPreviewFailsClosedWhenAppConfigIsNotAnObject(t *testing.T) {
	r, _ := deleteAppPreviewRegistry(t, nil, nil, nil)
	out, err := r.CallTool(t.Context(), "delete_app", map[string]interface{}{
		"app_name": "plex", "dry_run": true,
	})
	if err == nil {
		t.Fatalf("delete_app dry run succeeded with a null app.config:\n%s", out)
	}
	if strings.Contains(err.Error(), "internal error") {
		t.Errorf("null app.config panicked: %v", err)
	}
	if !strings.Contains(err.Error(), "app config") && !strings.Contains(err.Error(), "app.config") {
		t.Errorf("error should mention the failed app.config parse: %v", err)
	}
}

func TestDeleteAppPlanHashIgnoresStoragePaths(t *testing.T) {
	// Confirmation tokens hash method + CallParams + Bind only.
	// Filling current_state.storage_paths changes the preview contract
	// but must not change the plan hash, so a token stays valid.
	withPaths, fake := deleteAppPreviewRegistry(t, plexAppConfig2510(), nil, nil)
	planA, err := planDeleteApp(t.Context(), withPaths.client, map[string]interface{}{
		"app_name": "plex", "remove_images": true,
	})
	if err != nil {
		t.Fatalf("plan with host paths: %v", err)
	}
	if n := len(fake.called("app.delete")); n != 0 {
		t.Fatalf("planning sent app.delete")
	}

	empty, _ := deleteAppPreviewRegistry(t, map[string]interface{}{}, nil, nil)
	planB, err := planDeleteApp(t.Context(), empty.client, map[string]interface{}{
		"app_name": "plex", "remove_images": true,
	})
	if err != nil {
		t.Fatalf("plan with empty config: %v", err)
	}

	hashA, err := planA.planHash()
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := planB.planHash()
	if err != nil {
		t.Fatal(err)
	}
	if hashA != hashB {
		t.Errorf("plan hash changed when storage_paths changed: %x vs %x", hashA, hashB)
	}
	bindA, err := confirmationPlanHashBound(planA.Method, planA.CallParams, planA.Bind)
	if err != nil {
		t.Fatal(err)
	}
	if hashA != bindA {
		t.Errorf("plan hash is not method+params+bind only")
	}
}

func TestDeleteAppPreviewUsesACLPathWhenACLEnableIsSet(t *testing.T) {
	cfg := map[string]interface{}{
		"storage": map[string]interface{}{
			"config": map[string]interface{}{
				"type": "host_path",
				"host_path_config": map[string]interface{}{
					"path":       "/mnt/tank/apps/plex/config",
					"acl_enable": true,
					"acl": map[string]interface{}{
						"path": "/mnt/tank/apps/plex/acl-data",
					},
				},
			},
		},
	}
	r, fake := deleteAppPreviewRegistry(t, cfg, nil, nil)
	out := deleteAppDryRun(t, r)
	assertAppConfigCalled(t, fake, "plex")
	state, _ := deleteAppPreviewState(t, out)
	got := storagePathsOf(t, state)
	if len(got) != 1 || got[0] != "/mnt/tank/apps/plex/acl-data" {
		t.Fatalf("storage_paths = %v, want only the acl.path (apps bind-mount acl.path when acl_enable is set)\n%s", got, out)
	}
	for _, path := range got {
		if path == "/mnt/tank/apps/plex/config" {
			t.Fatalf("storage_paths used host_path_config.path while acl_enable is set: %v", got)
		}
	}
}

func TestDeleteAppPreviewListsBareHostPathAndComposeBindMounts(t *testing.T) {
	cfg := map[string]interface{}{
		"storage": map[string]interface{}{
			"config": map[string]interface{}{
				"type": "host_path",
				"path": "/mnt/tank/apps/plex/bare",
			},
			"legacy": map[string]interface{}{
				"type":     "hostPath",
				"hostPath": "/mnt/tank/apps/plex/legacy",
			},
		},
		"services": map[string]interface{}{
			"plex": map[string]interface{}{
				"volumes": []interface{}{
					map[string]interface{}{
						"type":   "bind",
						"source": "/mnt/tank/media",
						"target": "/data",
					},
					"/mnt/tank/transcode:/transcode:ro",
					"namedvol:/data",
				},
			},
		},
	}
	r, fake := deleteAppPreviewRegistry(t, cfg, nil, nil)
	out := deleteAppDryRun(t, r)
	assertAppConfigCalled(t, fake, "plex")
	state, _ := deleteAppPreviewState(t, out)
	got := storagePathsOf(t, state)
	want := []string{
		"/mnt/tank/apps/plex/bare",
		"/mnt/tank/apps/plex/legacy",
		"/mnt/tank/media",
		"/mnt/tank/transcode",
	}
	if len(got) != len(want) {
		t.Fatalf("storage_paths = %v, want %v\n%s", got, want, out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("storage_paths = %v, want %v (sorted host paths from bare hostpath and compose bind mounts)\n%s", got, want, out)
		}
	}
	for _, path := range got {
		if path == "namedvol" || path == "namedvol:/data" {
			t.Errorf("storage_paths included a named volume: %v", got)
		}
	}
}

func TestDeleteAppPreviewIgnoresComposeCommandArrayLookalikes(t *testing.T) {
	// Short-syntax bind-mount detection must only look at service volumes
	// arrays. A command argument like "/x:y" starts with '/' and contains
	// ':', but it is not a mount and must not appear in storage_paths.
	cfg := map[string]interface{}{
		"services": map[string]interface{}{
			"app": map[string]interface{}{
				"command":    []interface{}{"/x:y", "/bin/sh:-c"},
				"entrypoint": []interface{}{"/usr/local/bin/start:now"},
				"volumes": []interface{}{
					"/mnt/tank/data:/data",
					map[string]interface{}{
						"type":   "bind",
						"source": "/mnt/tank/media",
						"target": "/media",
					},
				},
			},
		},
	}
	r, fake := deleteAppPreviewRegistry(t, cfg, nil, nil)
	out := deleteAppDryRun(t, r)
	assertAppConfigCalled(t, fake, "plex")
	state, warnings := deleteAppPreviewState(t, out)
	got := storagePathsOf(t, state)
	want := []string{"/mnt/tank/data", "/mnt/tank/media"}
	if len(got) != len(want) {
		t.Fatalf("storage_paths = %v, want %v\n%s", got, want, out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("storage_paths = %v, want %v (volumes bind mounts only)\n%s", got, want, out)
		}
	}
	joined := strings.Join(append(append([]string{}, got...), warnings...), "\n")
	for _, lookalike := range []string{"/x:y", "/x", "/bin/sh:-c", "/bin/sh", "/usr/local/bin/start:now", "/usr/local/bin/start"} {
		if strings.Contains(joined, lookalike) {
			t.Errorf("preview listed command/entrypoint argument %q as a preserved path:\n%s", lookalike, out)
		}
	}
}

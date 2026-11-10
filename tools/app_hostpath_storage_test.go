package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// Catalog schema with two storage volumes that default to ix_volume, plus
// an optional additional_storage list that also allows nfs.
func hostPathTestSchema() map[string]interface{} {
	entry := func(enum []interface{}, defaultType string) map[string]interface{} {
		return map[string]interface{}{
			"type": "dict",
			"attrs": []interface{}{
				map[string]interface{}{
					"variable": "type",
					"schema": map[string]interface{}{
						"type":     "string",
						"required": true,
						"default":  defaultType,
						"enum":     enum,
					},
				},
				map[string]interface{}{
					"variable": "host_path_config",
					"schema": map[string]interface{}{
						"type": "dict",
						"attrs": []interface{}{
							map[string]interface{}{
								"variable": "path",
								"schema":   map[string]interface{}{"type": "hostpath", "required": true},
							},
						},
					},
				},
				map[string]interface{}{
					"variable": "ix_volume_config",
					"schema": map[string]interface{}{
						"type": "dict",
						"attrs": []interface{}{
							map[string]interface{}{
								"variable": "dataset_name",
								"schema":   map[string]interface{}{"type": "string"},
							},
						},
					},
				},
			},
		}
	}
	volumeEnum := []interface{}{
		map[string]interface{}{"value": "ix_volume"},
		map[string]interface{}{"value": "host_path"},
	}
	extraEnum := []interface{}{
		map[string]interface{}{"value": "ix_volume"},
		map[string]interface{}{"value": "host_path"},
		map[string]interface{}{"value": "nfs"},
	}
	return map[string]interface{}{
		"groups": []interface{}{
			map[string]interface{}{"name": "Storage Configuration"},
		},
		"questions": []interface{}{
			map[string]interface{}{
				"variable": "storage",
				"group":    "Storage Configuration",
				"schema": map[string]interface{}{
					"type": "dict",
					"attrs": []interface{}{
						map[string]interface{}{"variable": "config", "schema": entry(volumeEnum, "ix_volume")},
						map[string]interface{}{"variable": "data", "schema": entry(volumeEnum, "ix_volume")},
						map[string]interface{}{
							"variable": "additional_storage",
							"schema": map[string]interface{}{
								"type":    "list",
								"default": []interface{}{},
								"items": []interface{}{
									map[string]interface{}{"schema": entry(extraEnum, "host_path")},
								},
							},
						},
					},
				},
			},
		},
	}
}

func hostPathCatalogDetails() map[string]interface{} {
	return map[string]interface{}{
		"name":           "jellyfin",
		"latest_version": "1.2.0",
		"versions": map[string]interface{}{
			"1.2.0": map[string]interface{}{"schema": hostPathTestSchema()},
		},
	}
}

func hostPathInstalledApp() map[string]interface{} {
	return map[string]interface{}{
		"name":    "jellyfin",
		"id":      "jellyfin",
		"version": "1.2.0",
		"metadata": map[string]interface{}{
			"name":  "jellyfin",
			"train": "community",
		},
	}
}

func hostPathVolume(p string) map[string]interface{} {
	return map[string]interface{}{
		"type": "host_path",
		"host_path_config": map[string]interface{}{
			"path":       p,
			"acl_enable": false,
		},
	}
}

func validHostPathValues() map[string]interface{} {
	return map[string]interface{}{
		"storage": map[string]interface{}{
			"config":             hostPathVolume("/mnt/tank/apps/jellyfin/config"),
			"data":               hostPathVolume("/mnt/tank/apps/jellyfin/data"),
			"additional_storage": []interface{}{},
		},
	}
}

func cloneValues(src map[string]interface{}) map[string]interface{} {
	b, _ := json.Marshal(src)
	var out map[string]interface{}
	json.Unmarshal(b, &out)
	return out
}

func setStoragePath(values map[string]interface{}, volume, p string) {
	storage := values["storage"].(map[string]interface{})
	storage[volume] = hostPathVolume(p)
}

func hostPathFake(t *testing.T) (*truenas.Client, *fakeMiddleware) {
	t.Helper()
	existing := map[string]bool{
		"tank/apps/jellyfin/config": true,
		"tank/apps/jellyfin/data":   true,
	}
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"catalog.get_app_details": hostPathCatalogDetails(),
			"pool.query":              []interface{}{map[string]interface{}{"name": "tank", "status": "ONLINE"}},
			"app.query":               []interface{}{hostPathInstalledApp()},
			"app.config":              map[string]interface{}{},
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
	return client, fake
}

func hostPathRegistry(t *testing.T) (*Registry, *fakeMiddleware) {
	t.Helper()
	client, fake := hostPathFake(t)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake
}

func callHostPathTool(t *testing.T, r *Registry, tool string, values map[string]interface{}) (string, error) {
	t.Helper()
	args := map[string]interface{}{"app_name": "jellyfin", "values": values}
	if tool == "install_app" {
		args["catalog_app"] = "jellyfin"
		args["train"] = "community"
	}
	if safetyOf(tool).Access == Destructive {
		// update_app: the real handler, as after the user approved it.
		args = approve(t, r, tool, args)
	}
	return r.CallTool(t.Context(), tool, args)
}

func writeCalls(fake *fakeMiddleware) (creates, updates int) {
	return len(fake.called("app.create")), len(fake.called("app.update"))
}

// TestAppHostPathStorageEnforced is the regression for install_app / update_app
// host-path enforcement (schema-positive checks and normalized paths).
func TestAppHostPathStorageEnforced(t *testing.T) {
	pathCases := []struct {
		name, path, want string
	}{
		{"traversal parent of /mnt", "/mnt/../etc", "invalid path"},
		{"traversal past the pool", "/mnt/tank/../../etc", "invalid path"},
		{"double slash", "/mnt/tank//x", "invalid path"},
		{"pool root", "/mnt/tank", "invalid path"},
	}

	for _, tool := range []string{"install_app", "update_app"} {
		tool := tool
		for _, tc := range pathCases {
			tc := tc
			t.Run(tool+" "+tc.name, func(t *testing.T) {
				r, fake := hostPathRegistry(t)
				values := cloneValues(validHostPathValues())
				setStoragePath(values, "config", tc.path)
				out, err := callHostPathTool(t, r, tool, values)
				if err == nil {
					t.Fatalf("accepted %s: %s", tc.path, out)
				}
				if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
					t.Errorf("error %q, want it to mention %q (not a mere missing-dataset failure)", err, tc.want)
				}
				creates, updates := writeCalls(fake)
				if creates != 0 || updates != 0 {
					t.Errorf("app.create=%d app.update=%d, want no write after a failed check", creates, updates)
				}
			})
		}

		t.Run(tool+" omits storage that defaults to ix_volume", func(t *testing.T) {
			r, fake := hostPathRegistry(t)
			values := cloneValues(validHostPathValues())
			delete(values["storage"].(map[string]interface{}), "config")
			out, err := callHostPathTool(t, r, tool, values)
			if err == nil {
				t.Fatalf("omitted storage.config was accepted: %s", out)
			}
			msg := strings.ToLower(err.Error())
			if !strings.Contains(msg, "host_path") && !strings.Contains(msg, "storage.config") {
				t.Errorf("error %q, want it to mention the omitted host_path entry", err)
			}
			creates, updates := writeCalls(fake)
			if creates != 0 || updates != 0 {
				t.Errorf("app.create=%d app.update=%d, want no write", creates, updates)
			}
		})

		t.Run(tool+" rejects nfs storage type", func(t *testing.T) {
			r, fake := hostPathRegistry(t)
			values := cloneValues(validHostPathValues())
			values["storage"].(map[string]interface{})["config"] = map[string]interface{}{
				"type": "nfs",
				"nfs_config": map[string]interface{}{
					"server": "files.example.invalid",
					"path":   "/export/media",
				},
			}
			out, err := callHostPathTool(t, r, tool, values)
			if err == nil {
				t.Fatalf("nfs storage was accepted: %s", out)
			}
			msg := strings.ToLower(err.Error())
			if !strings.Contains(msg, "host_path") && !strings.Contains(msg, "nfs") {
				t.Errorf("error %q, want it to require host_path", err)
			}
			creates, updates := writeCalls(fake)
			if creates != 0 || updates != 0 {
				t.Errorf("app.create=%d app.update=%d, want no write", creates, updates)
			}
		})
	}

	t.Run("accepted host_path under existing datasets", func(t *testing.T) {
		r, fake := hostPathRegistry(t)
		out, err := callHostPathTool(t, r, "install_app", validHostPathValues())
		if err != nil {
			t.Fatalf("install_app: %v", err)
		}
		if len(fake.called("app.create")) != 1 {
			t.Errorf("install_app did not call app.create; out=%s", out)
		}
		if len(fake.called("app.update")) != 0 {
			t.Errorf("install_app called app.update")
		}

		r, fake = hostPathRegistry(t)
		out, err = callHostPathTool(t, r, "update_app", validHostPathValues())
		if err != nil {
			t.Fatalf("update_app: %v", err)
		}
		if len(fake.called("app.update")) != 1 {
			t.Errorf("update_app did not call app.update; out=%s", out)
		}
		if len(fake.called("app.create")) != 0 {
			t.Errorf("update_app called app.create")
		}
	})
}

func hostPathACLConfig(path, aclPath string) map[string]interface{} {
	return map[string]interface{}{
		"type": "host_path",
		"host_path_config": map[string]interface{}{
			"path":       path,
			"acl_enable": true,
			"acl": map[string]interface{}{
				"path":    aclPath,
				"entries": []interface{}{},
				"options": map[string]interface{}{"force": true},
			},
		},
	}
}

// hostPathFieldSchema mirrors the real catalog shapes (truenas/apps): the
// list item's variable name (share_entry, module, conf) is not a key in
// values, and nextcloud custom_confs items are typed hostpath, not hostpathfile.
func hostPathFieldSchema() map[string]interface{} {
	schema := hostPathTestSchema()
	questions, _ := schema["questions"].([]interface{})
	storageQ := questions[0].(map[string]interface{})
	storageSchema := storageQ["schema"].(map[string]interface{})
	attrs, _ := storageSchema["attrs"].([]interface{})
	attrs = append(attrs, map[string]interface{}{
		"variable": "shares",
		"schema": map[string]interface{}{
			"type":    "list",
			"default": []interface{}{},
			"items": []interface{}{
				map[string]interface{}{
					"variable": "share_entry",
					"schema": map[string]interface{}{
						"type": "dict",
						"attrs": []interface{}{
							map[string]interface{}{
								"variable": "host_path",
								"schema":   map[string]interface{}{"type": "hostpath", "required": true},
							},
						},
					},
				},
			},
		},
	})
	storageSchema["attrs"] = attrs
	questions = append(questions,
		map[string]interface{}{
			"variable": "rsyncd",
			"schema": map[string]interface{}{
				"type": "dict",
				"attrs": []interface{}{
					map[string]interface{}{
						"variable": "rsync_modules",
						"schema": map[string]interface{}{
							"type":    "list",
							"default": []interface{}{},
							"items": []interface{}{
								map[string]interface{}{
									"variable": "module",
									"schema": map[string]interface{}{
										"type": "dict",
										"attrs": []interface{}{
											map[string]interface{}{
												"variable": "host_path",
												"schema":   map[string]interface{}{"type": "hostpath", "required": true},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		map[string]interface{}{
			"variable": "network",
			"schema": map[string]interface{}{
				"type": "dict",
				"attrs": []interface{}{
					map[string]interface{}{
						"variable": "nginx",
						"schema": map[string]interface{}{
							"type": "dict",
							"attrs": []interface{}{
								map[string]interface{}{
									"variable": "custom_confs",
									"schema": map[string]interface{}{
										"type":    "list",
										"default": []interface{}{},
										"items": []interface{}{
											map[string]interface{}{
												"variable": "conf",
												"schema":   map[string]interface{}{"type": "hostpath", "required": true},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	)
	schema["questions"] = questions
	return schema
}

func hostPathReviewFake(t *testing.T, schema map[string]interface{}, versions map[string]interface{}) (*Registry, *fakeMiddleware) {
	t.Helper()
	if schema == nil {
		schema = hostPathFieldSchema()
	}
	if versions == nil {
		versions = map[string]interface{}{
			"1.2.0": map[string]interface{}{"schema": schema},
		}
	}
	details := map[string]interface{}{
		"name":           "jellyfin",
		"latest_version": "1.2.0",
		"versions":       versions,
	}
	existing := map[string]bool{
		"tank/apps/jellyfin/config": true,
		"tank/apps/jellyfin/data":   true,
	}
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"catalog.get_app_details": details,
			"pool.query":              []interface{}{map[string]interface{}{"name": "tank", "status": "ONLINE"}},
			"app.query":               []interface{}{hostPathInstalledApp()},
			"app.config":              map[string]interface{}{},
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

// TestAppHostPathReviewFollowups covers the review follow-ups for acl.path,
// hostpath-typed fields outside storage dicts, reserved datasets, and related nits.
func TestAppHostPathReviewFollowups(t *testing.T) {
	mustRefuse := func(t *testing.T, tool string, values map[string]interface{}, want string, extra func(*Registry, *fakeMiddleware, map[string]interface{})) {
		t.Helper()
		r, fake := hostPathReviewFake(t, nil, nil)
		if extra != nil {
			extra(r, fake, values)
		}
		args := map[string]interface{}{"app_name": "jellyfin", "values": values}
		if tool == "install_app" {
			args["catalog_app"] = "jellyfin"
			args["train"] = "community"
		}
		out, err := r.CallTool(t.Context(), tool, args)
		if err == nil {
			t.Fatalf("%s accepted: %s", tool, out)
		}
		if want != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("%s error %q, want it to mention %q", tool, err, want)
		}
		creates, updates := writeCalls(fake)
		if creates != 0 || updates != 0 {
			t.Errorf("%s app.create=%d app.update=%d, want no write", tool, creates, updates)
		}
	}

	t.Run("acl.path /etc is rejected even with force true", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["config"] = hostPathACLConfig(
			"/mnt/tank/apps/jellyfin/config", "/etc")
		mustRefuse(t, "install_app", values, "invalid path", nil)
		mustRefuse(t, "update_app", values, "invalid path", nil)
	})

	t.Run("acl.path at the pool root is rejected", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["config"] = hostPathACLConfig(
			"/mnt/tank/apps/jellyfin/config", "/mnt/tank")
		mustRefuse(t, "install_app", values, "invalid path", nil)
		mustRefuse(t, "update_app", values, "invalid path", nil)
	})

	t.Run("dataset check uses the mounted acl.path", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["config"] = hostPathACLConfig(
			"/mnt/tank/apps/jellyfin/config", "/mnt/tank/apps/jellyfin/missing")
		mustRefuse(t, "install_app", values, "create_dataset", nil)
	})

	t.Run("webdav shares[].host_path /etc is rejected", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["shares"] = []interface{}{
			map[string]interface{}{"name": "share1", "host_path": "/etc"},
		}
		mustRefuse(t, "install_app", values, "invalid path", nil)
	})

	t.Run("rsyncd rsync_modules[].host_path /etc is rejected", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["rsyncd"] = map[string]interface{}{
			"rsync_modules": []interface{}{
				map[string]interface{}{"name": "module1", "host_path": "/etc"},
			},
		}
		mustRefuse(t, "install_app", values, "invalid path", nil)
	})

	t.Run("nextcloud custom_confs /etc is rejected", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["network"] = map[string]interface{}{
			"nginx": map[string]interface{}{"custom_confs": []interface{}{"/etc"}},
		}
		mustRefuse(t, "install_app", values, "invalid path", nil)
	})

	t.Run("reserved .system and ix-applications paths are rejected", func(t *testing.T) {
		for _, p := range []string{"/mnt/tank/.system", "/mnt/tank/ix-applications", "/mnt/tank/.system/foo"} {
			values := cloneValues(validHostPathValues())
			setStoragePath(values, "config", p)
			mustRefuse(t, "install_app", values, "invalid path", nil)
		}
	})

	t.Run("unknown storage type nfs is rejected", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["extra"] = map[string]interface{}{
			"type": "nfs",
			"nfs_config": map[string]interface{}{
				"server": "files.example.invalid",
				"path":   "/export/media",
			},
		}
		mustRefuse(t, "install_app", values, "host_path", nil)
	})

	t.Run("install dry-run uses the requested version schema", func(t *testing.T) {
		oldSchema := hostPathTestSchema()
		// 1.0.0 also requires a media volume that latest does not.
		qs := oldSchema["questions"].([]interface{})
		st := qs[0].(map[string]interface{})["schema"].(map[string]interface{})
		attrs := st["attrs"].([]interface{})
		entry := attrs[0].(map[string]interface{})["schema"]
		st["attrs"] = append(attrs, map[string]interface{}{"variable": "media", "schema": entry})
		versions := map[string]interface{}{
			"1.0.0": map[string]interface{}{"schema": oldSchema},
			"1.2.0": map[string]interface{}{"schema": hostPathTestSchema()},
		}
		r, fake := hostPathReviewFake(t, nil, versions)
		args := map[string]interface{}{
			"app_name":    "jellyfin",
			"catalog_app": "jellyfin",
			"train":       "community",
			"version":     "1.0.0",
			"values":      validHostPathValues(),
			"dry_run":     true,
		}
		out, err := r.CallTool(t.Context(), "install_app", args)
		if err == nil {
			t.Fatalf("dry-run used latest schema and accepted a 1.0.0 config missing media: %s", out)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "media") && !strings.Contains(strings.ToLower(err.Error()), "host_path") {
			t.Errorf("error %q, want it to mention the missing 1.0.0 media volume", err)
		}
		if n := len(fake.called("app.create")); n != 0 {
			t.Errorf("app.create called %d times", n)
		}
	})

	t.Run("acl_enable with a valid acl.path is accepted", func(t *testing.T) {
		r, fake := hostPathReviewFake(t, nil, nil)
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["config"] = hostPathACLConfig(
			"/mnt/tank/apps/jellyfin/config", "/mnt/tank/apps/jellyfin/data")
		out, err := r.CallTool(t.Context(), "install_app", map[string]interface{}{
			"app_name": "jellyfin", "catalog_app": "jellyfin", "train": "community", "values": values,
		})
		if err != nil {
			t.Fatalf("valid acl.path was refused: %v (out=%s)", err, out)
		}
		if n := len(fake.called("app.create")); n != 1 {
			t.Errorf("app.create called %d times, want 1", n)
		}
	})

	t.Run("dataset lookup RPC error is a lookup failure", func(t *testing.T) {
		r, fake := hostPathReviewFake(t, nil, nil)
		orig := fake.handle
		fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method == "pool.dataset.query" {
				return nil, map[string]interface{}{"code": -32001, "message": "middleware timeout"}, true
			}
			if orig != nil {
				return orig(method, params)
			}
			return nil, nil, false
		}
		_, err := r.CallTool(t.Context(), "install_app", map[string]interface{}{
			"app_name": "jellyfin", "catalog_app": "jellyfin", "train": "community", "values": validHostPathValues(),
		})
		if err == nil {
			t.Fatal("install_app accepted a values set when dataset lookup failed")
		}
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "look up") && !strings.Contains(msg, "lookup") {
			t.Errorf("error %q, want a lookup failure, not a missing-dataset message", err)
		}
		if strings.Contains(msg, "missing:") {
			t.Errorf("error %q reports Missing, want a lookup failure", err)
		}
		if n := len(fake.called("app.create")); n != 0 {
			t.Errorf("app.create called %d times", n)
		}
	})

	t.Run("update_app missing-dataset message says update", func(t *testing.T) {
		r, fake := hostPathReviewFake(t, nil, nil)
		values := cloneValues(validHostPathValues())
		setStoragePath(values, "data", "/mnt/tank/apps/jellyfin/missing")
		_, err := r.CallTool(t.Context(), "update_app", approve(t, r, "update_app", map[string]interface{}{"app_name": "jellyfin", "values": values}))
		if err == nil {
			t.Fatal("update_app accepted a missing dataset")
		}
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "update") {
			t.Errorf("error %q, want it to say update", err)
		}
		if strings.Contains(msg, "installation") {
			t.Errorf("error %q still says installation", err)
		}
		if n := len(fake.called("app.update")); n != 0 {
			t.Errorf("app.update called %d times", n)
		}
	})

	t.Run("unknown install version is an error", func(t *testing.T) {
		r, fake := hostPathReviewFake(t, nil, nil)
		_, err := r.CallTool(t.Context(), "install_app", map[string]interface{}{
			"app_name": "jellyfin", "catalog_app": "jellyfin", "train": "community",
			"version": "9.9.9", "values": validHostPathValues(),
		})
		if err == nil {
			t.Fatal("install_app accepted a version that is not in the catalog")
		}
		if !strings.Contains(err.Error(), "9.9.9") {
			t.Errorf("error %q, want the missing version", err)
		}
		if n := len(fake.called("app.create")); n != 0 {
			t.Errorf("app.create called %d times", n)
		}
	})
}

// TestAppHostPathCatalogFollowups covers the post-merge review nits: fail
// closed on an unusable schema, nextcloud custom_confs mounted as files,
// and an allow list for off-schema storage types.
func TestAppHostPathCatalogFollowups(t *testing.T) {
	callInstall := func(t *testing.T, schema map[string]interface{}, values map[string]interface{}, dry bool) (string, error, *fakeMiddleware) {
		t.Helper()
		r, fake := hostPathReviewFake(t, schema, nil)
		args := map[string]interface{}{
			"app_name": "jellyfin", "catalog_app": "jellyfin", "train": "community", "values": values,
		}
		if dry {
			args["dry_run"] = true
		}
		out, err := r.CallTool(t.Context(), "install_app", args)
		return out, err, fake
	}
	mustRefuseWrite := func(t *testing.T, schema map[string]interface{}, values map[string]interface{}, want string, dry bool) {
		t.Helper()
		out, err, fake := callInstall(t, schema, values, dry)
		if err == nil {
			t.Fatalf("dry_run=%v accepted: %s", dry, out)
		}
		if want != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("dry_run=%v error %q, want it to mention %q", dry, err, want)
		}
		creates, updates := writeCalls(fake)
		if creates != 0 || updates != 0 {
			t.Errorf("dry_run=%v app.create=%d app.update=%d, want no write", dry, creates, updates)
		}
	}

	bareEtc := cloneValues(validHostPathValues())
	bareEtc["storage"].(map[string]interface{})["shares"] = []interface{}{
		map[string]interface{}{"name": "share1", "host_path": "/etc"},
	}

	t.Run("missing questions list refuses including dry-run", func(t *testing.T) {
		schema := hostPathTestSchema()
		delete(schema, "questions")
		mustRefuseWrite(t, schema, bareEtc, "questions", false)
		mustRefuseWrite(t, schema, bareEtc, "questions", true)
	})

	t.Run("empty questions list refuses", func(t *testing.T) {
		schema := hostPathTestSchema()
		schema["questions"] = []interface{}{}
		mustRefuseWrite(t, schema, bareEtc, "questions", false)
	})

	t.Run("non-list questions refuses", func(t *testing.T) {
		schema := hostPathTestSchema()
		schema["questions"] = "not-a-list"
		mustRefuseWrite(t, schema, bareEtc, "questions", false)
	})

	t.Run("unparseable questions item refuses", func(t *testing.T) {
		schema := hostPathTestSchema()
		schema["questions"] = []interface{}{"not-a-map"}
		mustRefuseWrite(t, schema, bareEtc, "questions", false)
	})

	t.Run("unparseable list items refuses", func(t *testing.T) {
		schema := hostPathTestSchema()
		qs := schema["questions"].([]interface{})
		st := qs[0].(map[string]interface{})["schema"].(map[string]interface{})
		attrs := st["attrs"].([]interface{})
		st["attrs"] = append(attrs, map[string]interface{}{
			"variable": "shares",
			"schema": map[string]interface{}{
				"type":  "list",
				"items": "not-parseable",
			},
		})
		mustRefuseWrite(t, schema, validHostPathValues(), "items", false)
	})

	t.Run("nextcloud custom_confs file under an existing dataset is accepted", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["network"] = map[string]interface{}{
			"nginx": map[string]interface{}{
				"custom_confs": []interface{}{"/mnt/tank/apps/jellyfin/config/custom.conf"},
			},
		}
		out, err, fake := callInstall(t, hostPathFieldSchema(), values, false)
		if err != nil {
			t.Fatalf("valid custom_confs file was refused: %v (out=%s)", err, out)
		}
		if n := len(fake.called("app.create")); n != 1 {
			t.Errorf("app.create called %d times, want 1", n)
		}
	})

	t.Run("nextcloud custom_confs /etc is rejected", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["network"] = map[string]interface{}{
			"nginx": map[string]interface{}{"custom_confs": []interface{}{"/etc/nginx/custom.conf"}},
		}
		mustRefuseWrite(t, hostPathFieldSchema(), values, "invalid path", false)
	})

	t.Run("off-schema storage types NFS anonymous and volume are rejected", func(t *testing.T) {
		for _, typ := range []string{"NFS", "anonymous", "volume"} {
			values := cloneValues(validHostPathValues())
			values["storage"].(map[string]interface{})["extra"] = map[string]interface{}{"type": typ}
			mustRefuseWrite(t, hostPathTestSchema(), values, "host_path", false)
		}
	})

	t.Run("question with no schema map refuses", func(t *testing.T) {
		schema := hostPathTestSchema()
		qs := schema["questions"].([]interface{})
		qs = append(qs, map[string]interface{}{"variable": "orphaned"})
		schema["questions"] = qs
		mustRefuseWrite(t, schema, validHostPathValues(), "schema map", false)
		mustRefuseWrite(t, schema, validHostPathValues(), "schema map", true)
	})

	t.Run("question schema that is not a map refuses", func(t *testing.T) {
		schema := hostPathTestSchema()
		qs := schema["questions"].([]interface{})
		qs = append(qs, map[string]interface{}{"variable": "orphaned", "schema": "not-a-map"})
		schema["questions"] = qs
		mustRefuseWrite(t, schema, validHostPathValues(), "schema map", false)
	})

	t.Run("attrs entry that is not a map refuses", func(t *testing.T) {
		schema := hostPathTestSchema()
		qs := schema["questions"].([]interface{})
		st := qs[0].(map[string]interface{})["schema"].(map[string]interface{})
		attrs := st["attrs"].([]interface{})
		st["attrs"] = append(attrs, "not-a-map")
		mustRefuseWrite(t, schema, validHostPathValues(), "attrs", false)
	})

	t.Run("off-schema unknown type with volume config is rejected", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["extra"] = map[string]interface{}{
			"type": "bind",
			"host_path_config": map[string]interface{}{
				"path": "/mnt/tank/apps/jellyfin/config",
			},
		}
		mustRefuseWrite(t, hostPathTestSchema(), values, "host_path", false)
	})
}

// addHostPathDatasets wraps a host-path fake so extra datasets exist.
func addHostPathDatasets(fake *fakeMiddleware, names ...string) {
	extra := map[string]bool{}
	for _, n := range names {
		extra[n] = true
	}
	orig := fake.handle
	fake.handle = func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "pool.dataset.query" {
			var args []interface{}
			if err := json.Unmarshal(params, &args); err == nil && len(args) > 0 {
				if filters, ok := args[0].([]interface{}); ok && len(filters) > 0 {
					if f, ok := filters[0].([]interface{}); ok && len(f) >= 3 {
						if s, ok := f[2].(string); ok && extra[s] {
							return []interface{}{map[string]interface{}{"name": s}}, nil, true
						}
					}
				}
			}
		}
		if orig != nil {
			return orig(method, params)
		}
		return nil, nil, false
	}
}

// TestAppHostPathClassifyAndSchemaNits covers the remaining #127 review nits
// around file-vs-directory classification for hostpath values.
func TestAppHostPathClassifyAndSchemaNits(t *testing.T) {
	callInstall := func(t *testing.T, schema map[string]interface{}, values map[string]interface{}, extraDS []string) (string, error, *fakeMiddleware) {
		t.Helper()
		r, fake := hostPathReviewFake(t, schema, nil)
		if len(extraDS) > 0 {
			addHostPathDatasets(fake, extraDS...)
		}
		out, err := r.CallTool(t.Context(), "install_app", map[string]interface{}{
			"app_name": "jellyfin", "catalog_app": "jellyfin", "train": "community", "values": values,
		})
		return out, err, fake
	}
	mustRefuse := func(t *testing.T, schema map[string]interface{}, values map[string]interface{}, want string, extraDS []string) {
		t.Helper()
		out, err, fake := callInstall(t, schema, values, extraDS)
		if err == nil {
			t.Fatalf("accepted: %s", out)
		}
		if want != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("error %q, want it to mention %q", err, want)
		}
		creates, updates := writeCalls(fake)
		if creates != 0 || updates != 0 {
			t.Errorf("app.create=%d app.update=%d, want no write", creates, updates)
		}
	}
	mustAccept := func(t *testing.T, schema map[string]interface{}, values map[string]interface{}, extraDS []string) {
		t.Helper()
		out, err, fake := callInstall(t, schema, values, extraDS)
		if err != nil {
			t.Fatalf("refused: %v (out=%s)", err, out)
		}
		if n := len(fake.called("app.create")); n != 1 {
			t.Errorf("app.create called %d times, want 1", n)
		}
	}

	t.Run("webdav dotted directory dataset at the pool is accepted", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["shares"] = []interface{}{
			map[string]interface{}{"name": "share1", "host_path": "/mnt/tank/share.1"},
		}
		mustAccept(t, hostPathFieldSchema(), values, []string{"tank/share.1"})
	})

	t.Run("webdav dotted directory at the pool with no dataset is a missing-dataset error", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["shares"] = []interface{}{
			map[string]interface{}{"name": "share1", "host_path": "/mnt/tank/share.1"},
		}
		mustRefuse(t, hostPathFieldSchema(), values, "create_dataset", nil)
	})

	t.Run("missing dataset directly under the pool uses the missing-dataset check", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["shares"] = []interface{}{
			map[string]interface{}{"name": "share1", "host_path": "/mnt/tank/webdav"},
		}

		out, err, fake := callInstall(t, hostPathFieldSchema(), values, nil)
		if err == nil {
			t.Fatalf("real call accepted a missing pool-root dataset: %s", out)
		}
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "create_dataset") || !strings.Contains(msg, "tank/webdav") || !strings.Contains(msg, "missing") {
			t.Errorf("real call error %q, want the missing-dataset error with a create_dataset hint for tank/webdav", err)
		}
		if strings.Contains(msg, "not a file under a dataset") {
			t.Errorf("real call used the hard classification error: %v", err)
		}
		creates, updates := writeCalls(fake)
		if creates != 0 || updates != 0 {
			t.Errorf("app.create=%d app.update=%d, want no write", creates, updates)
		}

		r, fake := hostPathReviewFake(t, hostPathFieldSchema(), nil)
		out, err = r.CallTool(t.Context(), "install_app", map[string]interface{}{
			"app_name": "jellyfin", "catalog_app": "jellyfin", "train": "community",
			"values": values, "dry_run": true,
		})
		if err != nil {
			t.Fatalf("dry-run returned an error: %v (out=%s)", err, out)
		}
		low := strings.ToLower(out)
		if !strings.Contains(low, "critical") || !strings.Contains(low, "create_dataset") || !strings.Contains(low, "tank/webdav") {
			t.Errorf("dry-run output %q, want a CRITICAL missing-dataset warning for tank/webdav", out)
		}
		if strings.Contains(low, "not a file under a dataset") {
			t.Errorf("dry-run used the hard classification error: %s", out)
		}
		if n := len(fake.called("app.create")); n != 0 {
			t.Errorf("dry-run app.create called %d times, want 0", n)
		}
	})

	t.Run("rsyncd dotted directory dataset is accepted when the parent is not a dataset", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["rsyncd"] = map[string]interface{}{
			"rsync_modules": []interface{}{
				map[string]interface{}{"name": "module1", "host_path": "/mnt/tank/apps/jellyfin/media.data"},
			},
		}
		mustAccept(t, hostPathFieldSchema(), values, []string{"tank/apps/jellyfin/media.data"})
	})

	t.Run("nextcloud custom_confs dotless file under an existing dataset is accepted", func(t *testing.T) {
		values := cloneValues(validHostPathValues())
		values["network"] = map[string]interface{}{
			"nginx": map[string]interface{}{
				"custom_confs": []interface{}{"/mnt/tank/apps/jellyfin/config/nginx_custom"},
			},
		}
		mustAccept(t, hostPathFieldSchema(), values, nil)
	})

	t.Run("hostpathdirectory dotted dataset is accepted when the parent is not a dataset", func(t *testing.T) {
		schema := hostPathTestSchema()
		qs := schema["questions"].([]interface{})
		st := qs[0].(map[string]interface{})["schema"].(map[string]interface{})
		attrs := st["attrs"].([]interface{})
		st["attrs"] = append(attrs, map[string]interface{}{
			"variable": "media_dir",
			"schema":   map[string]interface{}{"type": "hostpathdirectory", "required": true},
		})
		values := cloneValues(validHostPathValues())
		values["storage"].(map[string]interface{})["media_dir"] = "/mnt/tank/apps/jellyfin/media.data"
		mustAccept(t, schema, values, []string{"tank/apps/jellyfin/media.data"})
	})
}

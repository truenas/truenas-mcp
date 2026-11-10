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

func callHostPathTool(r *Registry, tool string, values map[string]interface{}) (string, error) {
	args := map[string]interface{}{"app_name": "jellyfin", "values": values}
	if tool == "install_app" {
		args["catalog_app"] = "jellyfin"
		args["train"] = "community"
	}
	return r.CallTool(tool, args)
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
				out, err := callHostPathTool(r, tool, values)
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
			out, err := callHostPathTool(r, tool, values)
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
			out, err := callHostPathTool(r, tool, values)
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
		out, err := callHostPathTool(r, "install_app", validHostPathValues())
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
		out, err = callHostPathTool(r, "update_app", validHostPathValues())
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
